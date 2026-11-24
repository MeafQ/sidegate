package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The stand-in for GitHub's releases. releaseDir holds one directory per tag, named vX.Y.Z,
// with that release's files in it. The highest tag is Latest, as the release workflow marks
// it. A file goes through the hops GitHub's does: latest/download/<f> redirects to
// download/<tag>/<f>, which redirects to the asset host's path.
const (
	releaseDir    = "/etc/unifi-lab/releases"
	releasesPath  = "/OWNER/sidegate/releases/"
	assetsPath    = "/release-assets/"
	modeFile      = "mode"
	servedFile    = "served"
	slowPerSecond = 16 << 10
	slowTick      = 100 * time.Millisecond
)

var tagPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// A mode makes the files whose name matches its pattern misbehave. releaseDir/mode holds at
// most one, written by `labweb release-mode` as a JSON list of its arguments, so a pattern with
// a space comes back whole. With none, every file is served as it is.
type mode struct {
	pattern string
	kind    modeKind
	n       int64
	hasN    bool
}

type modeKind string

const (
	notFound  modeKind = "404"     // download/<tag>/<f> answers 404.
	changed   modeKind = "changed" // One byte in the middle is flipped; the length stays.
	cut       modeKind = "cut"     // The full Content-Length, then the connection drops after n bytes, or all but -n, or half.
	short     modeKind = "short"   // The same first bytes as cut, with no Content-Length, ended cleanly: close-delimited on HTTP/1.1.
	slow      modeKind = "slow"    // n bytes a second, 16 KiB by default.
	endless   modeKind = "endless" // The file, then zeros without end, with no Content-Length.
	plainHTTP modeKind = "http"    // download/<tag>/<f> redirects to the asset over http.
	empty     modeKind = "empty"   // 200 with an empty body.
)

var takesN = map[modeKind]bool{notFound: false, changed: false, cut: true, short: true, slow: true, endless: false, plainHTTP: false, empty: false}

func parseMode(fields []string) (mode, error) {
	if len(fields) < 2 || len(fields) > 3 {
		return mode{}, fmt.Errorf("a mode is <file pattern> <%s> [n]", strings.Join(modeNames(), "|"))
	}
	m := mode{pattern: fields[0], kind: modeKind(fields[1])}
	if _, err := path.Match(m.pattern, ""); err != nil {
		return mode{}, fmt.Errorf("pattern %q: %v", m.pattern, err)
	}
	withN, known := takesN[m.kind]
	if !known {
		return mode{}, fmt.Errorf("unknown mode %q: one of %s", fields[1], strings.Join(modeNames(), ", "))
	}
	if len(fields) == 3 {
		if !withN {
			return mode{}, fmt.Errorf("mode %s takes no number", m.kind)
		}
		n, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || (m.kind == slow && n <= 0) {
			return mode{}, fmt.Errorf("mode %s: %q isn't a usable number", m.kind, fields[2])
		}
		m.n, m.hasN = n, true
	}
	return m, nil
}

func (m mode) String() string {
	s := fmt.Sprintf("%q %s", m.pattern, m.kind)
	if m.hasN {
		s += fmt.Sprintf(" %d", m.n)
	}
	return s
}

// record adds one answer to releaseDir/served before it goes out, so a test can tell the misbehaviour it asked
// for from any other: the file, how it was answered ("whole", a mode as release-mode was given it, or "error") and
// the status. A redirect that isn't the misbehaviour itself isn't an answer.
func record(dir, file, how string, status int) {
	f, err := os.OpenFile(filepath.Join(dir, servedFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		_, err = fmt.Fprintf(f, "%s %s %d\n", file, how, status)
		err = errors.Join(err, f.Close())
	}
	if err != nil {
		log.Printf("recording an answer: %v", err)
	}
}

func (m mode) how() string {
	how := cmp.Or(string(m.kind), "whole")
	if m.hasN {
		how += " " + strconv.FormatInt(m.n, 10)
	}
	return how
}

// cutAt is where cut and short end the body of a file of size bytes.
func (m mode) cutAt(size int64) (int64, error) {
	at := size / 2
	if m.hasN && m.n >= 0 {
		at = m.n
	} else if m.hasN {
		at = size + m.n
	}
	if at < 0 || at >= size {
		return 0, fmt.Errorf("%s ends at byte %d, outside its %d bytes", m.kind, at, size)
	}
	return at, nil
}

// fits says whether a file of size bytes can carry out the mode.
func (m mode) fits(size int64) error {
	switch m.kind {
	case changed:
		if size == 0 {
			return errors.New("changed has no byte to flip in an empty file")
		}
	case cut, short:
		_, err := m.cutAt(size)
		return err
	}
	return nil
}

func modeNames() []string {
	names := make([]string, 0, len(takesN))
	for k := range takesN {
		names = append(names, string(k))
	}
	slices.Sort(names)
	return names
}

// setMode writes the one mode, or with no fields removes it. The rename means a request never
// reads half a mode. A refused mode leaves the earlier one, and the error says which that is.
func setMode(dir string, fields []string) error {
	file := filepath.Join(dir, modeFile)
	if len(fields) == 0 {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := writeMode(dir, fields); err != nil {
		return fmt.Errorf("%v; %s", err, inEffect(dir))
	}
	return nil
}

func writeMode(dir string, fields []string) error {
	m, err := parseMode(fields)
	if err != nil {
		return err
	}
	if err := m.honouredIn(dir); err != nil {
		return err
	}
	saved, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file := filepath.Join(dir, modeFile)
	if err := os.WriteFile(file+".tmp", saved, 0o644); err != nil {
		return err
	}
	return os.Rename(file+".tmp", file)
}

// honouredIn refuses a mode that a file it names, in any release, can't carry out.
func (m mode) honouredIn(dir string) error {
	for _, tag := range releaseTags(dir) {
		files, err := os.ReadDir(filepath.Join(dir, tag))
		if err != nil {
			return err
		}
		for _, f := range files {
			if match, _ := path.Match(m.pattern, f.Name()); !match || f.IsDir() {
				continue
			}
			info, err := f.Info()
			if err != nil {
				return err
			}
			if err := m.fits(info.Size()); err != nil {
				return fmt.Errorf("%s/%s: %v", tag, f.Name(), err)
			}
		}
	}
	return nil
}

func inEffect(dir string) string {
	m, err := loadMode(dir)
	switch {
	case err != nil:
		return fmt.Sprintf("the mode in effect can't be read: %v", err)
	case m.kind == "":
		return "no mode is in effect"
	default:
		return m.String() + " is still in effect"
	}
}

// loadMode reads the mode in effect. Its kind is empty when there is none.
func loadMode(dir string) (mode, error) {
	raw, err := os.ReadFile(filepath.Join(dir, modeFile))
	if os.IsNotExist(err) {
		return mode{}, nil
	}
	if err != nil {
		return mode{}, err
	}
	var fields []string
	if err := json.Unmarshal(raw, &fields); err != nil {
		return mode{}, fmt.Errorf("%s isn't a JSON list of strings: %v", modeFile, err)
	}
	m, err := parseMode(fields)
	if err != nil {
		return mode{}, fmt.Errorf("%s: %v", modeFile, err)
	}
	return m, nil
}

// modeFor returns the mode that applies to file. Its kind is empty when file is served as it is.
func modeFor(dir, file string) (mode, error) {
	m, err := loadMode(dir)
	if err != nil {
		return mode{}, err
	}
	if match, _ := path.Match(m.pattern, file); !match {
		return mode{}, nil
	}
	return m, nil
}

func releaseTags(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var tags []string
	for _, e := range entries {
		if _, ok := tagNumbers(e.Name()); ok && e.IsDir() {
			tags = append(tags, e.Name())
		}
	}
	return tags
}

func latestTag(dir string) (string, bool) {
	var best string
	var bestN [3]int
	for _, tag := range releaseTags(dir) {
		n, _ := tagNumbers(tag)
		if best == "" || slices.Compare(n[:], bestN[:]) > 0 {
			best, bestN = tag, n
		}
	}
	return best, best != ""
}

func tagNumbers(tag string) ([3]int, bool) {
	var n [3]int
	m := tagPattern.FindStringSubmatch(tag)
	if m == nil {
		return n, false
	}
	for i := range n {
		v, err := strconv.Atoi(m[i+1])
		if err != nil {
			return n, false
		}
		n[i] = v
	}
	return n, true
}

func addReleases(mux *http.ServeMux, dir string) {
	mux.HandleFunc("GET "+releasesPath+"latest/download/{file}", func(w http.ResponseWriter, r *http.Request) {
		tag, ok := latestTag(dir)
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, releasesPath+"download/"+tag+"/"+url.PathEscape(r.PathValue("file")), http.StatusFound)
	})
	mux.HandleFunc("GET "+releasesPath+"download/{tag}/{file}", func(w http.ResponseWriter, r *http.Request) {
		tag, file := r.PathValue("tag"), r.PathValue("file")
		m, err := modeFor(dir, file)
		if err != nil {
			record(dir, file, "error", http.StatusInternalServerError)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		name, ok := releaseFile(dir, tag, file)
		if _, err := os.Stat(name); !ok || err != nil || m.kind == notFound {
			record(dir, file, m.how(), http.StatusNotFound)
			http.NotFound(w, r)
			return
		}
		scheme := "https"
		if m.kind == plainHTTP {
			scheme = "http"
			record(dir, file, m.how(), http.StatusFound)
		}
		http.Redirect(w, r, scheme+"://"+r.Host+assetsPath+url.PathEscape(tag)+"/"+url.PathEscape(file), http.StatusFound)
	})
	mux.HandleFunc("GET "+assetsPath+"{tag}/{file}", func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, r, dir, r.PathValue("tag"), r.PathValue("file"))
	})
}

func releaseFile(dir, tag, file string) (string, bool) {
	_, ok := tagNumbers(tag)
	return filepath.Join(dir, tag, file), ok
}

func serveAsset(w http.ResponseWriter, r *http.Request, dir, tag, file string) {
	name, ok := releaseFile(dir, tag, file)
	body, err := os.ReadFile(name)
	if !ok || err != nil {
		record(dir, file, "error", http.StatusNotFound)
		http.NotFound(w, r)
		return
	}
	m, err := modeFor(dir, file)
	if err == nil {
		err = m.fits(int64(len(body)))
	}
	if err != nil {
		record(dir, file, "error", http.StatusInternalServerError)
		http.Error(w, fmt.Sprintf("%s: %v", file, err), http.StatusInternalServerError)
		return
	}
	record(dir, file, m.how(), http.StatusOK)
	w.Header().Set("Content-Type", "application/octet-stream")
	switch m.kind {
	case changed:
		body = slices.Clone(body)
		body[len(body)/2] ^= 0xff
	case empty:
		body = nil
	case cut, short:
		at, _ := m.cutAt(int64(len(body)))
		if m.kind == short {
			w.Header().Set("Transfer-Encoding", "identity")
			w.Write(body[:at])
			http.NewResponseController(w).Flush()
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body[:at])
		http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler)
	case endless:
		w.Write(body)
		for {
			if _, err := w.Write(zeros); err != nil {
				return
			}
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	switch m.kind {
	case slow:
		rate := float64(slowPerSecond)
		if m.hasN {
			rate = float64(m.n)
		}
		rc := http.NewResponseController(w)
		w.WriteHeader(http.StatusOK)
		rc.Flush()
		start := time.Now()
		tick := time.NewTicker(slowTick)
		defer tick.Stop()
		for sent := 0; sent < len(body); {
			select {
			case <-tick.C:
			case <-r.Context().Done():
				return
			}
			due := int(min(float64(len(body)), rate*time.Since(start).Seconds()))
			if due == sent {
				continue
			}
			if _, err := w.Write(body[sent:due]); err != nil {
				return
			}
			rc.Flush()
			sent = due
		}
	default:
		w.Write(body)
	}
}
