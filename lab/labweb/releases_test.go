package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var releaseFiles = []string{"install.sh", "sidegate.json", "SHA256SUMS", "XRAY-LICENSE"}

// releaseTree writes v0.0.2, v0.0.9 and v0.0.10, plus a directory and a file that aren't
// tags, so Latest has to order by number.
func releaseTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tag := range []string{"v0.0.2", "v0.0.9", "v0.0.10"} {
		for _, f := range append(releaseFiles, "sidegate-"+tag+"-arm64.tar.gz") {
			body := bytes.Repeat([]byte(tag+" "+f+"\n"), 20)
			write(t, filepath.Join(dir, tag, f), body)
		}
	}
	write(t, filepath.Join(dir, "v1.0.0-rc1", "install.sh"), []byte("not a tag"))
	write(t, filepath.Join(dir, "v9.9.9"), []byte("a file, not a release"))
	return dir
}

func write(t *testing.T, name string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func file(t *testing.T, dir, tag, f string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, tag, f))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// eachProtocol runs f against the stand-in over HTTP/1.1 and HTTP/2, since curl and Go's
// client take HTTP/2 when the server offers it, as labweb's TLS listener does.
func eachProtocol(t *testing.T, dir string, f func(t *testing.T, s *httptest.Server)) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("h2=%v", h2), func(t *testing.T) {
			s := standIn(t, dir, h2)
			client := s.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			f(t, s)
			want := 1
			if h2 {
				want = 2
			}
			resp, err := client.Get(s.URL + releasesPath + "latest/download/install.sh")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.ProtoMajor != want {
				t.Fatalf("spoke HTTP/%d, want HTTP/%d", resp.ProtoMajor, want)
			}
		})
	}
}

// standIn serves dir's releases over TLS, offering HTTP/2 when h2.
func standIn(t *testing.T, dir string, h2 bool) *httptest.Server {
	mux := http.NewServeMux()
	addReleases(mux, dir)
	s := httptest.NewUnstartedServer(mux)
	s.EnableHTTP2 = h2
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

// hops follows redirects by hand from latest/download/<f>, and returns each Location and the
// last response, unread. A redirect to http ends it with no response.
func hops(t *testing.T, s *httptest.Server, f string) ([]string, *http.Response) {
	t.Helper()
	return hopsFrom(t, s, releasesPath+"latest/download/"+f)
}

func hopsFrom(t *testing.T, s *httptest.Server, path string) ([]string, *http.Response) {
	t.Helper()
	client := s.Client()
	url := s.URL + path
	var seen []string
	for range 5 {
		resp, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusFound {
			t.Cleanup(func() { resp.Body.Close() })
			return seen, resp
		}
		resp.Body.Close()
		url = resp.Header.Get("Location")
		seen = append(seen, url)
		if strings.HasPrefix(url, "/") {
			url = s.URL + url
		}
		if strings.HasPrefix(url, "http://") {
			return seen, nil
		}
	}
	t.Fatalf("more than 5 redirects: %v", seen)
	return nil, nil
}

// slowTest keeps real waits out of the quick check, which is -short. The full run keeps them.
func slowTest(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("real waits: the full, non-short run covers this")
	}
}

func mustMode(t *testing.T, dir string, fields ...string) {
	t.Helper()
	if err := setMode(dir, fields); err != nil {
		t.Fatal(err)
	}
}

func TestGoodServesLatestThroughTwoRedirects(t *testing.T) {
	dir := releaseTree(t)
	eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
		seen, resp := hops(t, s, "install.sh")
		want := []string{releasesPath + "download/v0.0.10/install.sh", s.URL + assetsPath + "v0.0.10/install.sh"}
		if strings.Join(seen, " ") != strings.Join(want, " ") {
			t.Fatalf("redirects %v, want %v", seen, want)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != 200 || !bytes.Equal(body, file(t, dir, "v0.0.10", "install.sh")) {
			t.Fatalf("status %d err %v body %q", resp.StatusCode, err, body)
		}
		if resp.ContentLength != int64(len(body)) {
			t.Fatalf("Content-Length %d for %d bytes", resp.ContentLength, len(body))
		}

		_, resp = hopsFrom(t, s, releasesPath+"download/v0.0.2/sidegate-v0.0.2-arm64.tar.gz")
		body, _ = io.ReadAll(resp.Body)
		if !bytes.Equal(body, file(t, dir, "v0.0.2", "sidegate-v0.0.2-arm64.tar.gz")) {
			t.Fatalf("an older tag's bundle: %q", body)
		}
	})
}

func TestMissingIs404(t *testing.T) {
	dir := releaseTree(t)
	empty := t.TempDir()
	eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
		for _, path := range []string{
			releasesPath + "latest/download/nothing.txt",
			releasesPath + "download/v0.0.3/install.sh",
			releasesPath + "download/v1.0.0-rc1/install.sh",
			assetsPath + "v0.0.10/nothing.txt",
		} {
			if _, resp := hopsFrom(t, s, path); resp.StatusCode != 404 {
				t.Errorf("%s: %d, want 404", path, resp.StatusCode)
			}
		}
	})
	mux := http.NewServeMux()
	addReleases(mux, empty)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", releasesPath+"latest/download/install.sh", nil))
	if rec.Code != 404 {
		t.Fatalf("no releases: %d, want 404", rec.Code)
	}
}

func TestMode404(t *testing.T) {
	dir := releaseTree(t)
	mustMode(t, dir, "sidegate.json", "404")
	eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
		seen, resp := hops(t, s, "sidegate.json")
		if len(seen) != 1 || resp.StatusCode != 404 {
			t.Fatalf("redirects %v, status %d; want one redirect, then 404", seen, resp.StatusCode)
		}
		_, resp = hops(t, s, "install.sh")
		if resp.StatusCode != 200 {
			t.Fatalf("install.sh, which the mode doesn't name: %d", resp.StatusCode)
		}
	})
}

func TestModeChangedFlipsOneByte(t *testing.T) {
	dir := releaseTree(t)
	mustMode(t, dir, "*.tar.gz", "changed")
	bundle := "sidegate-v0.0.10-arm64.tar.gz"
	want := file(t, dir, "v0.0.10", bundle)
	eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
		_, resp := hops(t, s, bundle)
		got, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != 200 || len(got) != len(want) || resp.ContentLength != int64(len(want)) {
			t.Fatalf("status %d err %v: %d bytes, Content-Length %d, want %d", resp.StatusCode, err, len(got), resp.ContentLength, len(want))
		}
		diff := 0
		for i := range got {
			if got[i] != want[i] {
				diff++
			}
		}
		if diff != 1 || sha256.Sum256(got) == sha256.Sum256(want) {
			t.Fatalf("%d bytes differ, want 1", diff)
		}
		_, resp = hops(t, s, "SHA256SUMS")
		got, _ = io.ReadAll(resp.Body)
		if !bytes.Equal(got, file(t, dir, "v0.0.10", "SHA256SUMS")) {
			t.Fatal("SHA256SUMS, which the mode doesn't name, changed")
		}
	})
}

func TestModeCutDropsTheConnection(t *testing.T) {
	dir := releaseTree(t)
	full := len(file(t, dir, "v0.0.10", "install.sh"))
	for _, c := range []struct {
		n    []string
		want int
	}{{nil, full / 2}, {[]string{"7"}, 7}, {[]string{"0"}, 0}, {[]string{"-3"}, full - 3}} {
		mustMode(t, dir, append([]string{"install.sh", "cut"}, c.n...)...)
		eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
			_, resp := hops(t, s, "install.sh")
			if resp.ContentLength != int64(full) {
				t.Fatalf("cut %v: Content-Length %d, want the whole %d", c.n, resp.ContentLength, full)
			}
			got, err := io.ReadAll(resp.Body)
			if err == nil || len(got) != c.want {
				t.Fatalf("cut %v: read %d bytes, err %v; want %d bytes and an error", c.n, len(got), err, c.want)
			}
		})
	}
	mustMode(t, dir, "install.sh", "cut", "7")
	write(t, filepath.Join(dir, "v0.0.10", "install.sh"), []byte("tiny"))
	eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
		if _, resp := hops(t, s, "install.sh"); resp.StatusCode != 500 {
			t.Fatalf("a file cut short of the mode set on it: %d, want 500", resp.StatusCode)
		}
	})
}

func TestReleaseModeRefusesASizeAFileCantHonour(t *testing.T) {
	dir := releaseTree(t)
	full := len(file(t, dir, "v0.0.10", "install.sh"))
	older := len(file(t, dir, "v0.0.2", "install.sh"))
	write(t, filepath.Join(dir, "v0.0.10", "empty.txt"), nil)
	mustMode(t, dir, "install.sh", "404")
	for _, fields := range [][]string{
		{"install.sh", "cut", fmt.Sprint(full)},
		{"install.sh", "cut", fmt.Sprint(-full - 1)},
		{"install.sh", "short", fmt.Sprint(full)},
		{"install.sh", "cut", fmt.Sprint((older + full) / 2)},
		{"empty.txt", "cut"},
		{"empty.txt", "short", "0"},
		{"empty.txt", "changed"},
		{"install.sh", "cutt"},
	} {
		err := setMode(dir, fields)
		if err == nil {
			t.Fatalf("release-mode %q: no error", fields)
		}
		if !strings.Contains(err.Error(), `"install.sh" 404 is still in effect`) {
			t.Errorf("release-mode %q: %v; want it to say the earlier mode is still in effect", fields, err)
		}
	}
	eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
		if seen, resp := hops(t, s, "install.sh"); len(seen) != 1 || resp.StatusCode != 404 {
			t.Fatalf("after the refusals: redirects %v, status %d; want the earlier 404", seen, resp.StatusCode)
		}
	})
}

func TestReleaseModeCarriesAPatternWithSpaces(t *testing.T) {
	dir := releaseTree(t)
	write(t, filepath.Join(dir, "v0.0.10", "install .sh"), []byte("a name with a space"))
	mustMode(t, dir, "install .sh", "404")
	eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
		if seen, resp := hops(t, s, "install%20.sh"); len(seen) != 1 || resp.StatusCode != 404 {
			t.Fatalf("install .sh: redirects %v, status %d; want one redirect, then 404", seen, resp.StatusCode)
		}
		if _, resp := hops(t, s, "install.sh"); resp.StatusCode != 200 {
			t.Fatalf("install.sh, which the mode doesn't name: %d", resp.StatusCode)
		}
	})
}

func TestModeShortEndsCleanlyWithoutALength(t *testing.T) {
	dir := releaseTree(t)
	whole := file(t, dir, "v0.0.10", "install.sh")
	for _, c := range []struct {
		n    []string
		want int
	}{{nil, len(whole) / 2}, {[]string{"0"}, 0}, {[]string{"-3"}, len(whole) - 3}} {
		mustMode(t, dir, append([]string{"install.sh", "short"}, c.n...)...)
		eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
			_, resp := hops(t, s, "install.sh")
			got, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != 200 || resp.ContentLength != -1 || !bytes.Equal(got, whole[:c.want]) {
				t.Fatalf("short %v: status %d, Content-Length %d, %d bytes, err %v; want %d bytes, no length, no error",
					c.n, resp.StatusCode, resp.ContentLength, len(got), err, c.want)
			}
			if resp.ProtoMajor == 1 && (len(resp.TransferEncoding) != 0 || !resp.Close) {
				t.Fatalf("short %v over HTTP/1.1: Transfer-Encoding %v, close %v; want close-delimited", c.n, resp.TransferEncoding, resp.Close)
			}
		})
	}
}

func TestModeSlowPacesTheBody(t *testing.T) {
	dir := releaseTree(t)
	want := file(t, dir, "v0.0.10", "sidegate.json")
	mustMode(t, dir, "sidegate.json", "slow", fmt.Sprint(len(want)*10/3))
	eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
		_, resp := hops(t, s, "sidegate.json")
		start := time.Now()
		got, err := io.ReadAll(resp.Body)
		took := time.Since(start)
		if err != nil || !bytes.Equal(got, want) || resp.ContentLength != int64(len(want)) {
			t.Fatalf("err %v, %d of %d bytes", err, len(got), len(want))
		}
		if took < 2*slowTick || took > 2*time.Second {
			t.Fatalf("took %v for what comes in 300ms at that rate", took)
		}
	})
}

func TestModeSlowSendsNBytesASecond(t *testing.T) {
	slowTest(t)
	dir := releaseTree(t)
	write(t, filepath.Join(dir, "v0.0.10", "small.bin"), bytes.Repeat([]byte{'x'}, 64))
	mustMode(t, dir, "small.bin", "slow", "1")
	const deadline = 8 * time.Second
	eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
		t.Parallel()
		seen, _ := hops(t, s, "small.bin")
		client := *s.Client()
		client.Timeout = deadline
		resp, err := client.Get(seen[len(seen)-1])
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)
		if err == nil || len(got) < 6 || len(got) > 9 {
			t.Fatalf("slow 1: %d of 64 bytes in %v, err %v; want about 8 and the read still going", len(got), deadline, err)
		}
	})
}

func TestModeEndlessNeverStops(t *testing.T) {
	dir := releaseTree(t)
	want := file(t, dir, "v0.0.10", "sidegate.json")
	mustMode(t, dir, "sidegate.json", "endless")
	eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
		_, resp := hops(t, s, "sidegate.json")
		if resp.ContentLength != -1 {
			t.Fatalf("Content-Length %d, want none", resp.ContentLength)
		}
		read := int64(len(zeros) * 4)
		got, err := io.ReadAll(io.LimitReader(resp.Body, read))
		if err != nil || int64(len(got)) != read {
			t.Fatalf("read %d of %d bytes: %v", len(got), read, err)
		}
		if !bytes.Equal(got[:len(want)], want) || bytes.ContainsFunc(got[len(want):], func(r rune) bool { return r != 0 }) {
			t.Fatal("want the file, then zeros")
		}
	})
}

func TestModeHTTPRedirectsToPlainHTTP(t *testing.T) {
	dir := releaseTree(t)
	mustMode(t, dir, "install.sh", "http")
	eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
		seen, _ := hops(t, s, "install.sh")
		host := strings.TrimPrefix(s.URL, "https://")
		want := []string{releasesPath + "download/v0.0.10/install.sh", "http://" + host + assetsPath + "v0.0.10/install.sh"}
		if strings.Join(seen, " ") != strings.Join(want, " ") {
			t.Fatalf("redirects %v, want %v", seen, want)
		}
	})
}

func TestModeEmptyAnswers200WithNothing(t *testing.T) {
	dir := releaseTree(t)
	mustMode(t, dir, "install.sh", "empty")
	eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
		_, resp := hops(t, s, "install.sh")
		got, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != 200 || len(got) != 0 || resp.ContentLength != 0 {
			t.Fatalf("status %d, Content-Length %d, %d bytes, err %v", resp.StatusCode, resp.ContentLength, len(got), err)
		}
	})
}

func TestReleaseModeRefusesWhatItCantServe(t *testing.T) {
	dir := releaseTree(t)
	for _, fields := range [][]string{
		{"install.sh"},
		{"install.sh", "cutt"},
		{"install.sh", "cut", "half"},
		{"install.sh", "slow", "0"},
		{"install.sh", "404", "3"},
		{"[", "404"},
		{"install.sh", "cut", "1", "2"},
	} {
		if err := setMode(dir, fields); err == nil || !strings.Contains(err.Error(), "no mode is in effect") {
			t.Errorf("release-mode %v: %v; want an error that says no mode is in effect", fields, err)
		}
		if _, err := os.Stat(filepath.Join(dir, modeFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("release-mode %v left a mode behind", fields)
		}
	}
	mustMode(t, dir, "install.sh", "404")
	mustMode(t, dir)
	if _, err := os.Stat(filepath.Join(dir, modeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("release-mode with no mode kept the last one")
	}
	mustMode(t, dir)
}

// What the stand-in answered is on record as the mode that was asked for, or as anything but it.
func TestEachAnswerIsRecorded(t *testing.T) {
	for _, c := range []struct {
		mode []string // Or, with none, what is written as the mode.
		text string
		want string
	}{
		{want: "install.sh whole 200\n"},
		{mode: []string{"install.sh", "short", "3"}, want: "install.sh short 3 200\n"},
		{mode: []string{"install.sh", "cut", "3"}, want: "install.sh cut 3 200\n"},
		{mode: []string{"install.sh", "empty"}, want: "install.sh empty 200\n"},
		{mode: []string{"install.sh", "404"}, want: "install.sh 404 404\n"},
		{mode: []string{"install.sh", "http"}, want: "install.sh http 302\n"},
		{mode: []string{"SHA256SUMS", "404"}, want: "install.sh whole 200\n"},
		{text: `["install.sh", "sloww"]`, want: "install.sh error 500\n"},
		{text: `["install.sh", "short", "99999999"]`, want: "install.sh error 500\n"},
	} {
		dir := releaseTree(t)
		eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
			os.Remove(filepath.Join(dir, servedFile))
			if c.text != "" {
				write(t, filepath.Join(dir, modeFile), []byte(c.text))
			} else {
				mustMode(t, dir, c.mode...)
			}
			_, resp := hops(t, s, "install.sh")
			if resp != nil {
				io.Copy(io.Discard, resp.Body)
			}
			if got := string(file(t, dir, "", servedFile)); got != c.want {
				t.Errorf("%v %s: recorded %q, want %q", c.mode, c.text, got, c.want)
			}
		})
	}
}

func TestAHandWrittenBadModeIs500(t *testing.T) {
	dir := releaseTree(t)
	write(t, filepath.Join(dir, modeFile), []byte(`["install.sh", "sloww"]`))
	eachProtocol(t, dir, func(t *testing.T, s *httptest.Server) {
		seen, resp := hops(t, s, "install.sh")
		if len(seen) != 1 || resp.StatusCode != 500 {
			t.Fatalf("redirects %v, status %d; want 500 at the tag's hop", seen, resp.StatusCode)
		}
	})
}
