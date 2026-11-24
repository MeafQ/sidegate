package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Build is what a sidegated was linked as: its version, and the releases address it trusts for updates,
// which only a release has.
type Build struct{ Version, Releases string }

// releaseVersion is a release's three numbers, which order releases. scripts/release-version.sh, which says what
// is published as one, keeps the same rule: testdata/release-tags.json is both's table.
type releaseVersion [3]uint64

var releaseVersionText = regexp.MustCompile(`^v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)

func parseReleaseVersion(text string) (releaseVersion, bool) {
	var v releaseVersion
	match := releaseVersionText.FindStringSubmatch(text)
	if match == nil {
		return v, false
	}
	for i := range v {
		v[i], _ = strconv.ParseUint(match[i+1], 10, 64)
	}
	return v, true
}

func (v releaseVersion) above(other releaseVersion) bool { return slices.Compare(v[:], other[:]) > 0 }

// release is the build's version if it is a release: a release's version, with an https address to update from.
func (b Build) release() (releaseVersion, bool) {
	v, ok := parseReleaseVersion(b.Version)
	address, err := url.Parse(b.Releases)
	return v, ok && err == nil && address.Scheme == "https" && address.Host != ""
}

// guide is a release's own copy of docs/operate.md, in the repository whose releases address it was linked with.
func (b Build) guide() string {
	return strings.TrimSuffix(b.Releases, "/releases") + "/blob/" + b.Version + "/docs/operate.md"
}

// versionOf asks a sidegated what it was linked as.
func versionOf(ctx context.Context, binary string) (Build, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "version").Output()
	if err != nil {
		return Build{}, fmt.Errorf("%s version: %w", binary, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if lines[0] == "" || len(lines) > 2 {
		return Build{}, fmt.Errorf("%s version said %q", binary, out)
	}
	return Build{Version: lines[0], Releases: strings.Join(lines[1:], "")}, nil
}

const (
	manifestName  = "sidegate.json"
	manifestLimit = 64 << 10
	manifestTime  = 15 * time.Second
	bundleTime    = 5 * time.Minute
	maxRedirects  = 5
)

// CheckFailure is why the last check learned nothing.
type CheckFailure string

const (
	CheckUnreachable      CheckFailure = "unreachable"
	CheckClockWrong       CheckFailure = "clockWrong"
	CheckFilesUnavailable CheckFailure = "filesUnavailable" // 404: no release, or one without its files.
	CheckMalformed        CheckFailure = "malformed"
)

// checkError is a manifest whose failure isn't the connection's.
type checkError struct {
	failure CheckFailure
	err     error
}

func (e checkError) Error() string { return e.err.Error() }
func (e checkError) Unwrap() error { return e.err }

func checkFailureOf(err error) CheckFailure {
	var known checkError
	var certificate x509.CertificateInvalidError
	switch {
	case errors.As(err, &known):
		return known.failure
	case errors.As(err, &certificate) && certificate.Reason == x509.Expired:
		return CheckClockWrong
	}
	return CheckUnreachable
}

// manifest is a release's sidegate.json.
type manifest struct {
	Version string `json:"version"`
	Bundle  string `json:"bundle"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Core    string `json:"core"`
}

func bundleName(version string) string { return "sidegate-" + version + "-arm64.tar.gz" }

var sha256Text = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (m manifest) validate() error {
	switch _, ok := parseReleaseVersion(m.Version); {
	case !ok:
		return fmt.Errorf("version %q isn't a release's", m.Version)
	case m.Bundle != bundleName(m.Version):
		return fmt.Errorf("bundle %q isn't %s", m.Bundle, bundleName(m.Version))
	case m.Size <= 0 || m.Size > maxBundleFile:
		return fmt.Errorf("size %d isn't a bundle's", m.Size)
	case !sha256Text.MatchString(m.SHA256) || !sha256Text.MatchString(m.Core):
		return errors.New("its digests aren't SHA-256")
	}
	return nil
}

// releaseSource reads the one releases address, over HTTPS alone. A request is a plain GET that says
// "sidegated" and nothing else of the gateway.
type releaseSource struct {
	address string
	client  *http.Client
}

// newReleaseSource trusts the system's roots when roots is nil.
func newReleaseSource(address string, roots *x509.CertPool) releaseSource {
	return releaseSource{address: address, client: &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			TLSClientConfig:     &tls.Config{RootCAs: roots},
			TLSHandshakeTimeout: 10 * time.Second,
			DisableCompression:  true,
			IdleConnTimeout:     10 * time.Second,
			ForceAttemptHTTP2:   true,
		},
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			switch {
			case r.URL.Scheme != "https":
				return fmt.Errorf("a redirect to %s:// is refused", r.URL.Scheme)
			case len(via) > maxRedirects:
				return fmt.Errorf("more than %d redirects", maxRedirects)
			}
			r.Header.Del("Referer")
			return nil
		},
	}}
}

// get answers with a 200's body.
func (s releaseSource) get(ctx context.Context, path string) (io.ReadCloser, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, s.address+path, nil)
	if err != nil {
		return nil, err
	}
	if r.URL.Scheme != "https" {
		return nil, fmt.Errorf("%s isn't an https address", s.address)
	}
	r.Header.Set("User-Agent", "sidegated")
	resp, err := s.client.Do(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		err := fmt.Errorf("%s: HTTP %d", r.URL, resp.StatusCode)
		if resp.StatusCode == http.StatusNotFound {
			err = checkError{CheckFilesUnavailable, err}
		}
		return nil, err
	}
	return resp.Body, nil
}

// latest is the manifest of the release marked Latest.
func (s releaseSource) latest(ctx context.Context) (manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, manifestTime)
	defer cancel()
	body, err := s.get(ctx, "/latest/download/"+manifestName)
	if err != nil {
		return manifest{}, err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, manifestLimit+1))
	if err != nil {
		return manifest{}, err
	}
	var m manifest
	if len(raw) > manifestLimit {
		err = fmt.Errorf("it is over %d bytes", manifestLimit)
	} else if err = json.Unmarshal(raw, &m); err == nil {
		err = m.validate()
	}
	if err != nil {
		return manifest{}, checkError{CheckMalformed, fmt.Errorf("%s: %w", manifestName, err)}
	}
	return m, nil
}

// download puts m's bundle at path once its size and checksum are m's, reading at most one byte past its size.
func (s releaseSource) download(ctx context.Context, m manifest, path string) error {
	ctx, cancel := context.WithTimeout(ctx, bundleTime)
	defer cancel()
	failed := func(err error) error { return updateFailure{ReasonDownloadFailed, err} }
	body, err := s.get(ctx, "/download/"+m.Version+"/"+m.Bundle)
	if err != nil {
		return failed(err)
	}
	defer body.Close()
	part := path + ".part"
	file, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return failed(err)
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(body, m.Size+1))
	if err = errors.Join(err, file.Close()); err != nil {
		return failed(err)
	}
	switch sum := hex.EncodeToString(hash.Sum(nil)); {
	case n != m.Size:
		return updateFailure{ReasonChecksum, fmt.Errorf("%s isn't %d bytes long, as %s says", m.Bundle, m.Size, manifestName)}
	case sum != m.SHA256:
		return updateFailure{ReasonChecksum, fmt.Errorf("%s has SHA-256 %s, not %s", m.Bundle, sum, m.SHA256)}
	}
	return os.Rename(part, path)
}
