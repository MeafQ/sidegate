package catalogue

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"sidegate/internal/domain"
)

func testFetcher(t *testing.T, limits Limits) *Fetcher {
	t.Helper()
	f, err := NewFetcher(limits, CountryPolicy{LabelRules: []CountryRule{{Regex: `(?i)germany`, Country: "de"}}})
	if err != nil {
		t.Fatal(err)
	}
	f.allow = func(netip.Addr) bool { return true } // httptest listens on loopback.
	f.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: testRoots}
	f.now = func() time.Time { return t0 }
	return f
}

// testRoots trusts the one certificate every httptest TLS server serves.
var testRoots = func() *x509.CertPool {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}()

var testBasis = Basis{Source: "source-a", Revision: 3}

const testUserAgent = "Happ/1.0"

func fetch(t *testing.T, f *Fetcher, location string, hwid string, state FetchState) Refresh {
	t.Helper()
	return f.Fetch(context.Background(), NewRequest(testBasis, testUserAgent, domain.NewSecret(location), domain.NewSecret(hwid), state))
}

// recorder serves scripted responses and keeps the request headers it saw.
type recorder struct {
	mu      sync.Mutex
	headers []http.Header
	handle  func(w http.ResponseWriter, r *http.Request, call int)
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec.mu.Lock()
	call := len(rec.headers)
	rec.headers = append(rec.headers, r.Header.Clone())
	rec.mu.Unlock()
	rec.handle(w, r, call)
}

func (rec *recorder) seen(i int) http.Header {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.headers[i]
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFetchUpdatedThenUnchanged(t *testing.T) {
	body := fixture(t, "marzban.b64")
	title := "base64:" + base64.StdEncoding.EncodeToString([]byte("Marzban 🚀"))
	rec := &recorder{handle: func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 0 {
			h := w.Header()
			h.Set("ETag", `"v1"`)
			h.Set("Last-Modified", "Tue, 03 Nov 2026 10:00:00 GMT")
			h.Set("Subscription-Userinfo", "upload=1024; download=2048; total=107374182400; expire=1798761600")
			h.Set("Profile-Update-Interval", "12")
			h.Set("Profile-Title", title)
			h.Set("Profile-Announce", "Maintenance%20on%20Friday")
			h.Set("Support-Url", "https://t.example.net/support")
			h.Set("Profile-Web-Page-Url", "https://panel.example.net/source/abc")
			h.Set("Subscription-Refill-Date", "1796083200")
			w.Write(body)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.Header().Set("Subscription-Userinfo", "upload=4096; download=8192; total=107374182400; expire=1798761600")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write(body)
	}}
	srv := httptest.NewTLSServer(rec)
	defer srv.Close()
	f := testFetcher(t, Limits{})

	first := fetch(t, f, srv.URL+"/source/token", "hwid-123", FetchState{})
	if first.Outcome != Updated || first.Basis != (Basis{Source: "source-a", Revision: 3}) {
		t.Fatalf("first = %v %+v, want Updated for source-a@3 (status %+v)", first.Outcome, first.Basis, first.Next.Status())
	}
	if got := len(first.Document.Entries()); got != 4 {
		t.Fatalf("entries = %d, want 4", got)
	}
	if c := first.Document.Entries()[0].Country; c.Code != "DE" || c.Source != domain.CountryRule {
		t.Fatalf("country = %+v, want DE from the label rule", c)
	}
	if c := first.Document.Entries()[1].Country; c.Code != "NL" || c.Source != domain.CountryConfigFlag {
		t.Fatalf("country = %+v, want NL from the flag", c)
	}
	h := rec.seen(0)
	if h.Get("User-Agent") != "Happ/1.0" || h.Get("X-Hwid") != "hwid-123" || h.Get("If-None-Match") != "" {
		t.Fatalf("first request headers = %v", h)
	}
	meta := first.Next.Provider()
	expires, refills := time.Unix(1798761600, 0).UTC(), time.Unix(1796083200, 0).UTC()
	if meta.Title != "Marzban 🚀" || meta.Announcement != "Maintenance on Friday" ||
		meta.SupportURL != "https://t.example.net/support" || meta.ProfileURL != "https://panel.example.net/source/abc" ||
		meta.SuggestedRefresh != 12*time.Hour || *meta.Quota.Upload != 1024 || *meta.Quota.Download != 2048 ||
		*meta.Quota.Limit != 107374182400 || !meta.Quota.Expires.Equal(expires) || !meta.Quota.Refills.Equal(refills) ||
		!meta.Quota.ObservedAt.Equal(t0) {
		t.Fatalf("metadata = %+v", meta)
	}
	if s := first.Next.Status(); !s.CheckedAt.Equal(t0) || !s.UpdatedAt.Equal(t0) || s.Failures != 0 {
		t.Fatalf("status = %+v", s)
	}

	f.now = func() time.Time { return t0.Add(time.Hour) }
	second := fetch(t, f, srv.URL+"/source/token", "hwid-123", first.Next)
	h = rec.seen(1)
	if h.Get("If-None-Match") != `"v1"` || h.Get("If-Modified-Since") != "Tue, 03 Nov 2026 10:00:00 GMT" {
		t.Fatalf("validators not sent: %v", h)
	}
	if second.Outcome != Unchanged || len(second.Document.Entries()) != 0 {
		t.Fatalf("second = %v with %d entries, want Unchanged and no document", second.Outcome, len(second.Document.Entries()))
	}
	meta = second.Next.Provider()
	if meta.Title != "Marzban 🚀" || *meta.Quota.Upload != 4096 || meta.SuggestedRefresh != 12*time.Hour {
		t.Fatalf("304 metadata = %+v, want the old title and hint with the new quota", meta)
	}
	if s := second.Next.Status(); !s.CheckedAt.Equal(t0.Add(time.Hour)) || !s.UpdatedAt.Equal(t0) {
		t.Fatalf("status = %+v", s)
	}
	fetch(t, f, srv.URL+"/source/token", "", second.Next)
	if h := rec.seen(2); h.Get("If-None-Match") != `"v1"` || h.Get("X-Hwid") != "" {
		t.Fatalf("third request headers = %v, want kept validators and no HWID", h)
	}
}

func TestFetchFailuresKeepTheLastGoodState(t *testing.T) {
	tuicOnly := "tuic://d4d4d4d4-0000-4000-8000-000000000003:pw@tuic.example.net:443#tuic\n"
	rec := &recorder{handle: func(w http.ResponseWriter, r *http.Request, call int) {
		switch call {
		case 0:
			w.Header().Set("ETag", `"good"`)
			w.Header().Set("Profile-Title", "Good")
			w.Write(fixture(t, "marzban.b64"))
		case 1:
			w.Header().Set("ETag", `"bad"`)
			w.Write([]byte(tuicOnly))
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}}
	srv := httptest.NewTLSServer(rec)
	defer srv.Close()
	f := testFetcher(t, Limits{})
	good := fetch(t, f, srv.URL, "", FetchState{})
	unusable := fetch(t, f, srv.URL, "", good.Next)
	if unusable.Outcome != FetchFailed || unusable.Next.Status().FailureCode != FailNoUsableEntries {
		t.Fatalf("unusable = %v %+v", unusable.Outcome, unusable.Next.Status())
	}
	bad := fetch(t, f, srv.URL, "", unusable.Next)
	if bad.Next.Status().FailureCode != "http_502" || bad.Next.Status().Failures != 2 {
		t.Fatalf("status = %+v, want two failures ending in http_502", bad.Next.Status())
	}
	if bad.Next.Provider().Title != "Good" {
		t.Fatal("a failure dropped the last good metadata")
	}
	fetch(t, f, srv.URL, "", bad.Next)
	if got := rec.seen(3).Get("If-None-Match"); got != `"good"` {
		t.Fatalf("If-None-Match = %q, want the validator of the last applied body", got)
	}
}

func TestFetchRetryAfter(t *testing.T) {
	for _, value := range []string{"600", t0.Add(10 * time.Minute).Format(http.TimeFormat)} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", value)
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		f := testFetcher(t, Limits{})
		r := fetch(t, f, srv.URL, "", FetchState{})
		srv.Close()
		floor := t0.Add(10 * time.Minute)
		if r.Outcome != FetchFailed || !r.Next.ManualDue(t0).Equal(floor) || r.Next.Due(testBasis.Source, domain.Remote{UserAgent: testUserAgent}).Before(floor) {
			t.Fatalf("Retry-After %q: manual %v, due %v; want both at or after %v", value, r.Next.ManualDue(t0), r.Next.Due(testBasis.Source, domain.Remote{UserAgent: testUserAgent}), floor)
		}
		if got := r.Next.ManualDue(floor.Add(time.Second)); !got.Equal(floor.Add(time.Second)) {
			t.Fatalf("an expired floor still delays manual refresh: %v", got)
		}
	}
}

func TestFetchGuards(t *testing.T) {
	big := strings.Repeat("vless://"+linkUUID+"@a.example.net:443?security=tls#x\n", 200)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(big))
	zw.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(big)) })
	mux.HandleFunc("/gzip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(gz.Bytes())
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	mux.HandleFunc("/ftp", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://files.example.net/source", http.StatusFound)
	})
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a plain HTTP server was asked")
	}))
	defer plain.Close()
	mux.HandleFunc("/plain", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/source", http.StatusFound)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	small := testFetcher(t, Limits{DecodedBytes: 4096})
	cases := []struct {
		name, location string
		code           FailureCode
		f              *Fetcher
	}{
		{"decoded size", srv.URL + "/big", FailTooLarge, small},
		{"gzip expansion", srv.URL + "/gzip", FailTooLarge, small},
		{"redirect loop", srv.URL + "/loop", FailTooManyRedirects, testFetcher(t, Limits{})},
		{"redirect off the web", srv.URL + "/ftp", FailRedirectRefused, testFetcher(t, Limits{})},
		{"redirect to plain HTTP", srv.URL + "/plain", FailRedirectRefused, testFetcher(t, Limits{})},
		{"plain HTTP", plain.URL + "/source", FailInvalidLocation, testFetcher(t, Limits{})},
		{"credentials in authority", strings.Replace(srv.URL, "https://", "https://user:pass@", 1), FailInvalidLocation, testFetcher(t, Limits{})},
		{"non-http scheme", "file:///etc/passwd", FailInvalidLocation, testFetcher(t, Limits{})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := fetch(t, c.f, c.location, "", FetchState{})
			if r.Outcome != FetchFailed || r.Next.Status().FailureCode != c.code {
				t.Fatalf("outcome %v, status %+v; want %s", r.Outcome, r.Next.Status(), c.code)
			}
		})
	}
	t.Run("gzip within limits", func(t *testing.T) {
		if r := fetch(t, testFetcher(t, Limits{}), srv.URL+"/gzip", "", FetchState{}); r.Outcome != Updated {
			t.Fatalf("outcome %v, status %+v", r.Outcome, r.Next.Status())
		}
	})
}

func TestFetchRefusesNonPublicAddresses(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a loopback server was reached")
	}))
	defer srv.Close()
	f, err := NewFetcher(Limits{}, CountryPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	r := f.Fetch(context.Background(), NewRequest(testBasis, testUserAgent, domain.NewSecret(srv.URL+"/s3cret-token"), domain.Secret{}, FetchState{}))
	if r.Next.Status().FailureCode != FailAddressBlocked {
		t.Fatalf("status = %+v, want %s", r.Next.Status(), FailAddressBlocked)
	}
	for _, a := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "::1", "fd00::1", "fe80::1", "::ffff:10.0.0.1", "0.0.0.0", "224.0.0.1"} {
		if publicAddr(netip.MustParseAddr(a).Unmap()) {
			t.Errorf("%s counts as public", a)
		}
	}
	if !publicAddr(netip.MustParseAddr("1.1.1.1")) || !publicAddr(netip.MustParseAddr("2606:4700::1111")) {
		t.Error("public addresses refused")
	}
}

func TestFetchCrossOriginRedirectDropsDeviceHeaders(t *testing.T) {
	target := &recorder{handle: func(w http.ResponseWriter, r *http.Request, call int) {
		w.Write(fixture(t, "marzban.b64"))
	}}
	other := httptest.NewTLSServer(target)
	defer other.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/moved", http.StatusFound)
	}))
	defer origin.Close()
	state := advance(FetchState{}, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Etag": {`"v1"`}}}, nil, "", t0)
	r := fetch(t, testFetcher(t, Limits{}), origin.URL, "hwid-123", state)
	if r.Outcome != Updated {
		t.Fatalf("outcome %v, status %+v", r.Outcome, r.Next.Status())
	}
	h := target.seen(0)
	if h.Get("X-Hwid") != "" || h.Get("If-None-Match") != "" || h.Get("User-Agent") != "Happ/1.0" {
		t.Fatalf("cross-origin headers = %v, want the user agent only", h)
	}
}

func failed(prev FetchState, status int, at time.Time) FetchState {
	return advance(prev, &http.Response{StatusCode: status, Header: http.Header{}}, nil, HTTPFailure(status), at)
}

func TestDueBackoffAndCadence(t *testing.T) {
	ok := advance(FetchState{}, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Profile-Update-Interval": {"2"}}}, nil, "", t0)
	within := func(t *testing.T, got time.Time, want time.Duration) {
		t.Helper()
		if d := got.Sub(t0); d < want || d > want+want/20 {
			t.Fatalf("due after %v, want %v plus at most 5%% jitter", d, want)
		}
	}
	t.Run("provider hint", func(t *testing.T) { within(t, ok.Due("x", domain.Remote{}), 2*time.Hour) })
	t.Run("owner interval wins", func(t *testing.T) {
		within(t, ok.Due("x", domain.Remote{RefreshEvery: 3 * time.Hour}), 3*time.Hour)
	})
	t.Run("clamped", func(t *testing.T) {
		within(t, ok.Due("x", domain.Remote{RefreshEvery: time.Minute}), MinRefresh)
		within(t, ok.Due("x", domain.Remote{RefreshEvery: 30 * 24 * time.Hour}), MaxRefresh)
	})
	t.Run("default", func(t *testing.T) {
		within(t, FetchState{checkedAt: t0}.Due("x", domain.Remote{}), DefaultRefresh)
	})
	t.Run("never fetched", func(t *testing.T) {
		if got := (FetchState{}).Due("x", domain.Remote{}); !got.IsZero() {
			t.Fatalf("due = %v, want now", got)
		}
	})
	t.Run("paused", func(t *testing.T) {
		if got := ok.Due("x", domain.Remote{Paused: true}); !got.Equal(Never) {
			t.Fatalf("due = %v, want Never", got)
		}
	})
	t.Run("failure steps", func(t *testing.T) {
		s := ok
		for _, want := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, time.Hour} {
			s = failed(s, http.StatusBadGateway, t0)
			within(t, s.Due("x", domain.Remote{RefreshEvery: 6 * time.Hour}), want)
		}
	})
	t.Run("steps capped by interval", func(t *testing.T) {
		s := failed(failed(failed(failed(ok, 502, t0), 502, t0), 502, t0), 502, t0)
		within(t, s.Due("x", domain.Remote{RefreshEvery: MinRefresh}), MinRefresh)
	})
	t.Run("auth failure jumps to the last step", func(t *testing.T) {
		within(t, failed(ok, http.StatusForbidden, t0).Due("x", domain.Remote{}), time.Hour)
	})
	t.Run("success resets failures", func(t *testing.T) {
		s := advance(failed(ok, 502, t0), &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}, nil, "", t0)
		if s.Status().Failures != 0 || s.Status().FailureCode != "" {
			t.Fatalf("status = %+v", s.Status())
		}
		within(t, s.Due("x", domain.Remote{}), DefaultRefresh)
	})
	t.Run("jitter is keyed by id", func(t *testing.T) {
		a := ok.Due("a", domain.Remote{})
		if !a.Equal(ok.Due("a", domain.Remote{})) {
			t.Fatal("Due is not pure")
		}
		spread := map[time.Time]bool{}
		for _, id := range []domain.SourceID{"a", "b", "c", "d", "e"} {
			spread[ok.Due(id, domain.Remote{})] = true
		}
		if len(spread) < 3 {
			t.Fatalf("five sources share %d deadlines", len(spread))
		}
	})
}

func TestFetchStateRoundTrip(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{
		"Etag":                  {`"v9"`},
		"Subscription-Userinfo": {"upload=0; download=5; total=0"},
		"Profile-Title":         {"Title"},
	}}
	s := failed(advance(FetchState{}, resp, nil, "", t0), http.StatusServiceUnavailable, t0.Add(time.Minute))
	s.retryAfter = t0.Add(time.Hour)
	data, err := s.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var back FetchState
	if err := back.UnmarshalBinary(data); err != nil {
		t.Fatal(err)
	}
	bs, ss := back.Status(), s.Status()
	if !bs.CheckedAt.Equal(ss.CheckedAt) || !bs.UpdatedAt.Equal(ss.UpdatedAt) || bs.Failures != ss.Failures || bs.FailureCode != ss.FailureCode {
		t.Fatalf("status %+v, want %+v", bs, ss)
	}
	var source domain.Remote
	if !back.Due("x", source).Equal(s.Due("x", source)) || !back.ManualDue(t0).Equal(s.ManualDue(t0)) || back.etag != `"v9"` {
		t.Fatal("round trip changed scheduling or validators")
	}
	meta := back.Provider()
	if meta.Title != "Title" || *meta.Quota.Upload != 0 || *meta.Quota.Download != 5 || meta.Quota.Limit == nil || *meta.Quota.Limit != 0 {
		t.Fatalf("metadata = %+v, want upload 0, download 5, and a total of 0, which is unlimited", meta)
	}
	missing := providerMetadata(http.Header{"Subscription-Userinfo": {"upload=1; download=2"}}, t0, nil)
	if missing.Quota.Limit != nil {
		t.Fatalf("no total reads as %d, want unknown", *missing.Quota.Limit)
	}
	if err := back.UnmarshalBinary([]byte("not json")); err == nil {
		t.Fatal("garbage decoded")
	}
}

func TestNewFetcherRejectsBadCountryRules(t *testing.T) {
	for _, rule := range []CountryRule{{Regex: "(", Country: "DE"}, {Regex: "x", Country: "Germany"}} {
		if _, err := NewFetcher(Limits{}, CountryPolicy{LabelRules: []CountryRule{rule}}); err == nil {
			t.Errorf("rule %+v accepted", rule)
		}
	}
}

func TestFetchRefusesA304ToAnUnconditionalRequest(t *testing.T) {
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer other.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/moved", http.StatusFound)
	}))
	defer origin.Close()
	state := advance(FetchState{}, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Etag": {`"v1"`}}}, nil, "", t0)
	r := fetch(t, testFetcher(t, Limits{}), origin.URL, "", state)
	if r.Outcome != FetchFailed || r.Next.Status().FailureCode != FailUnexpected304 {
		t.Fatalf("outcome %v, status %+v, want FetchFailed %s", r.Outcome, r.Next.Status(), FailUnexpected304)
	}
}

func TestFetchIgnoresDatesItCannotStore(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Subscription-Userinfo", "upload=1; download=2; expire=9223372036854775807")
		w.Header().Set("Subscription-Refill-Date", "253402300800") // 10000-01-01.
		w.Write([]byte(vless("DE")))
	}))
	defer srv.Close()
	r := fetch(t, testFetcher(t, Limits{}), srv.URL, "", FetchState{})
	if r.Outcome != Updated {
		t.Fatalf("outcome %v, status %+v", r.Outcome, r.Next.Status())
	}
	q := r.Next.Provider().Quota
	if q.Expires != nil || q.Refills != nil || q.Upload == nil || *q.Upload != 1 {
		t.Fatalf("quota = %+v, want the counters without the dates", q)
	}
	encoded, err := r.Next.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary = %v", err)
	}
	var back FetchState
	if err := back.UnmarshalBinary(encoded); err != nil {
		t.Fatalf("UnmarshalBinary = %v", err)
	}
}

func TestPublicAddrChecksEmbeddedIPv4(t *testing.T) {
	for _, a := range []string{"64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "64:ff9b::c0a8:101", "2002:7f00:1::1", "2002:c0a8:101::1", "2002:a9fe:a9fe::",
		"2001:0:4136:e378:8000:63bf:80ff:fffe", // Teredo for client 127.0.0.1.
	} {
		if publicAddr(netip.MustParseAddr(a)) {
			t.Errorf("%s embeds a non-public IPv4 but counts as public", a)
		}
	}
	for _, a := range []string{"64:ff9b::101:101", "2002:101:101::1", "2001:0:4136:e378:8000:63bf:fefe:fefe"} {
		if !publicAddr(netip.MustParseAddr(a)) {
			t.Errorf("%s embeds 1.1.1.1 but is refused", a)
		}
	}
}

func TestProviderMetadataReadsEitherAnnouncementHeader(t *testing.T) {
	location, _ := url.Parse("https://panel.example.net/api/source/Secret-Token1")
	for _, c := range []struct {
		name, profile, happ, want string
	}{
		{"none", "", "", ""},
		{"the profile's", "Friday", "", "Friday"},
		{"Happ's", "", "base64:" + base64.StdEncoding.EncodeToString([]byte("Пятница")), "Пятница"},
		{"both", "Friday", "Saturday", "Friday"},
		{"Happ's that names the location", "", "Your link: Secret-Token1", ""},
	} {
		h := http.Header{}
		h.Set("Profile-Announce", c.profile)
		h.Set("announce", c.happ)
		if got := providerMetadata(h, t0, location).Announcement; got != c.want {
			t.Errorf("%s: announcement = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestProviderMetadataNeverNamesTheLocation(t *testing.T) {
	location, err := url.Parse("https://panel.example.net/api/source/Secret-Token1?key=abc123")
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	h.Set("Profile-Web-Page-Url", "https://panel.example.net/api/source/Secret-Token1")
	h.Set("Support-Url", "https://panel.example.net/support?key=abc123")
	h.Set("Profile-Announce", "Your link: Secret-Token1")
	h.Set("Profile-Title", "Acme")
	meta := providerMetadata(h, t0, location)
	if meta.ProfileURL != "" {
		t.Errorf("profile URL kept: %s", meta.ProfileURL)
	}
	if meta.Announcement != "" {
		t.Errorf("announcement kept: %s", meta.Announcement)
	}
	if meta.Title != "Acme" {
		t.Errorf("title = %q", meta.Title)
	}
	location, _ = url.Parse("https://panel.example.net/source/token")
	h = http.Header{}
	h.Set("Support-Url", "https://t.me/support")
	if got := providerMetadata(h, t0, location).SupportURL; got != "https://t.me/support" {
		t.Errorf("an unrelated link was dropped: %q", got)
	}
}
