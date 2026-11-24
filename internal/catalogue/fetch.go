package catalogue

import (
	"cmp"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"sidegate/internal/domain"
)

const (
	MinRefresh     = 15 * time.Minute
	MaxRefresh     = 7 * 24 * time.Hour
	DefaultRefresh = 6 * time.Hour

	// DefaultUserAgent is what a new source sends. Panels pick a format by User-Agent, and many answer
	// a name they don't know with 404, so it names a client they serve.
	DefaultUserAgent = "Happ/3.0.0"
)

// Never is the deadline of a paused source.
var Never = time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC)

var backoffSteps = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

// FetchState is the catalogue's memory of one source's fetches: HTTP
// validators, consecutive failures, the provider's Retry-After floor, its refresh
// hint, and its current metadata. The fields are private. The controller persists
// the opaque encoding in the same state.db transaction as the lineage changes the
// fetch produced, so a crash can never keep a validator whose body was not applied.
// The zero value is a source that has never been fetched.
type FetchState struct {
	etag, lastModified string
	checkedAt          time.Time
	updatedAt          time.Time
	failures           uint32
	failure            FailureCode
	retryAfter         time.Time
	metadata           domain.ProviderMetadata
}

// Due is when a remote source should next be fetched. A paused source is due Never. A
// source never fetched is due at once. Otherwise it is the later of the
// Retry-After floor and the last check plus the interval on success, or the
// capped 1/5/15/60 minute step after failures. The interval is the owner's, else
// the provider hint, else DefaultRefresh, clamped to [MinRefresh, MaxRefresh].
// Jitter of up to 5% is keyed by the source ID, so Due is a pure function
// and a boot does not fetch every source at once.
func (s FetchState) Due(id domain.SourceID, source domain.Remote) time.Time {
	if source.Paused {
		return Never
	}
	if s.checkedAt.IsZero() {
		return s.retryAfter
	}
	wait := s.interval(source)
	if s.failures > 0 {
		step := backoffSteps[min(int(s.failures), len(backoffSteps))-1]
		wait = min(step, wait)
	}
	due := s.checkedAt.Add(wait + jitter(id, wait))
	if s.retryAfter.After(due) {
		return s.retryAfter
	}
	return due
}

func (s FetchState) interval(source domain.Remote) time.Duration {
	d := source.RefreshEvery
	if d <= 0 {
		d = s.metadata.SuggestedRefresh
	}
	if d <= 0 {
		d = DefaultRefresh
	}
	return min(max(d, MinRefresh), MaxRefresh)
}

func jitter(id domain.SourceID, d time.Duration) time.Duration {
	h := fnv.New64a()
	h.Write([]byte(id))
	return time.Duration(h.Sum64()%1001) * (d / 20) / 1000
}

// ManualDue is the earliest time a manual refresh may start: now, unless the
// provider's Retry-After deadline is later.
func (s FetchState) ManualDue(now time.Time) time.Time {
	if s.retryAfter.After(now) {
		return s.retryAfter
	}
	return now
}

// Provider is the metadata from the last validated 200 or 304.
func (s FetchState) Provider() domain.ProviderMetadata { return s.metadata }

// Status is the display-safe part of the state.
type Status struct {
	CheckedAt, UpdatedAt time.Time
	Failures             uint32
	FailureCode          FailureCode
}

func (s FetchState) Status() Status {
	return Status{CheckedAt: s.checkedAt, UpdatedAt: s.updatedAt, Failures: s.failures, FailureCode: s.failure}
}

type fetchStateWire struct {
	ETag         string                  `json:"etag,omitempty"`
	LastModified string                  `json:"last_modified,omitempty"`
	CheckedAt    time.Time               `json:"checked_at"`
	UpdatedAt    time.Time               `json:"updated_at"`
	Failures     uint32                  `json:"failures,omitempty"`
	Failure      FailureCode             `json:"failure,omitempty"`
	RetryAfter   time.Time               `json:"retry_after"`
	Metadata     domain.ProviderMetadata `json:"metadata"`
}

func (s FetchState) MarshalBinary() ([]byte, error) {
	return json.Marshal(fetchStateWire{
		ETag: s.etag, LastModified: s.lastModified, CheckedAt: s.checkedAt, UpdatedAt: s.updatedAt,
		Failures: s.failures, Failure: s.failure, RetryAfter: s.retryAfter, Metadata: s.metadata,
	})
}

func (s *FetchState) UnmarshalBinary(data []byte) error {
	var w fetchStateWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*s = FetchState{
		etag: w.ETag, lastModified: w.LastModified, checkedAt: w.CheckedAt, updatedAt: w.UpdatedAt,
		failures: w.Failures, failure: w.Failure, retryAfter: w.RetryAfter, metadata: w.Metadata,
	}
	return nil
}

// Basis is what a refresh was computed from. The controller drops a Refresh whose
// source was deleted or whose revision moved on while it was in flight.
type Basis struct {
	Source   domain.SourceID
	Revision domain.SourceRevision
}

// Request is one fetch, built from intent, resolved secrets, and the last state.
type Request struct {
	basis     Basis
	location  domain.Secret
	userAgent string
	hwid      domain.Secret
	state     FetchState
}

func NewRequest(basis Basis, userAgent string, location, hwid domain.Secret, state FetchState) Request {
	return Request{
		basis:     basis,
		location:  location,
		userAgent: userAgent,
		hwid:      hwid,
		state:     state,
	}
}

func (r Request) Basis() Basis { return r.basis }

type Outcome uint8

const (
	Updated Outcome = iota + 1
	Unchanged
	FetchFailed
)

// Refresh is created only by Fetcher. Document is set only when Outcome is
// Updated. Next replaces the source's FetchState when the controller commits.
type Refresh struct {
	Basis    Basis
	Outcome  Outcome
	Document Document
	Next     FetchState
}

// FailureCode is why a fetch failed: one of the constants below, or HTTPFailure's. It is a safe
// code, never a URL, header, or body snippet. The page has words for each constant.
type FailureCode string

const (
	FailInvalidLocation  FailureCode = "invalid_location"
	FailAddressBlocked   FailureCode = "address_blocked"
	FailTooManyRedirects FailureCode = "too_many_redirects"
	FailRedirectRefused  FailureCode = "redirect_refused"
	FailTimeout          FailureCode = "timeout"
	FailNetwork          FailureCode = "network"
	FailTooLarge         FailureCode = "too_large"
	FailBadEncoding      FailureCode = "bad_encoding"
	FailUnexpected304    FailureCode = "unexpected_304"
	FailMalformed        FailureCode = "malformed"
	FailUnknownFormat    FailureCode = "unknown_format"
	FailTooManyEntries   FailureCode = "too_many_entries"
	FailNoUsableEntries  FailureCode = "no_usable_entries"
)

// HTTPFailure is the code for an answer with a status other than 200 or 304.
func HTTPFailure(status int) FailureCode { return FailureCode("http_" + strconv.Itoa(status)) }

var (
	errAddressBlocked   = errors.New(string(FailAddressBlocked))
	errTooManyRedirects = errors.New(string(FailTooManyRedirects))
	errRedirectRefused  = errors.New(string(FailRedirectRefused))
)

// deviceHeaders identify this gateway to panels that limit devices (Remnawave
// HWID). Each source has its own device ID.
var deviceHeaders = []string{"X-Hwid", "X-Device-Os", "X-Device-Model"}

// Fetcher is safe for a fixed pool of workers. It holds no per-source state.
type Fetcher struct {
	client    *http.Client
	limits    Limits
	countries *countryResolver
	resolver  *net.Resolver
	allow     func(netip.Addr) bool
	now       func() time.Time
}

// NewFetcher fails only on an invalid country rule.
func NewFetcher(limits Limits, countries CountryPolicy) (*Fetcher, error) {
	resolver, err := newCountryResolver(countries)
	if err != nil {
		return nil, err
	}
	f := &Fetcher{limits: limits.withDefaults(), countries: resolver, resolver: net.DefaultResolver, allow: publicAddr, now: time.Now}
	f.client = &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil, // Environment proxies are never inherited.
			DialContext:           f.dial,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: f.limits.Timeout,
			DisableCompression:    true,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: f.checkRedirect,
	}
	return f, nil
}

// Fetch never returns the location, a header, or body text in any outcome.
func (f *Fetcher) Fetch(ctx context.Context, req Request) Refresh {
	now := f.now()
	out := Refresh{Basis: req.basis}
	location, err := url.Parse(req.location.Reveal())
	fail := func(resp *http.Response, code FailureCode) Refresh {
		out.Outcome, out.Next = FetchFailed, advance(req.state, resp, location, code, now)
		return out
	}
	if err != nil || !sourceURL(location) {
		return fail(nil, FailInvalidLocation)
	}
	ctx, cancel := context.WithTimeout(ctx, f.limits.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
	if err != nil {
		return fail(nil, FailInvalidLocation)
	}
	httpReq.Header.Set("User-Agent", orDefault(req.userAgent, DefaultUserAgent))
	httpReq.Header.Set("Accept", "*/*")
	httpReq.Header.Set("Accept-Encoding", "gzip")
	if !req.hwid.IsZero() {
		httpReq.Header.Set("X-Hwid", req.hwid.Reveal())
		httpReq.Header.Set("X-Device-Os", "Linux")
		httpReq.Header.Set("X-Device-Model", "sidegate")
	}
	if req.state.etag != "" {
		httpReq.Header.Set("If-None-Match", req.state.etag)
	}
	if req.state.lastModified != "" {
		httpReq.Header.Set("If-Modified-Since", req.state.lastModified)
	}
	resp, err := f.client.Do(httpReq)
	if err != nil {
		return fail(nil, transportFailure(err))
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		if sent := resp.Request.Header; sent.Get("If-None-Match") == "" && sent.Get("If-Modified-Since") == "" {
			return fail(resp, FailUnexpected304) // A redirect to another origin drops the validators.
		}
		out.Outcome, out.Next = Unchanged, advance(req.state, resp, location, "", now)
		return out
	default:
		return fail(resp, HTTPFailure(resp.StatusCode))
	}
	body, code := f.readBody(resp)
	if code != "" {
		return fail(resp, code)
	}
	doc, err := parse(body, Detect, f.limits, f.countries)
	if err != nil {
		return fail(resp, parseFailure(err))
	}
	out.Outcome, out.Document, out.Next = Updated, doc, advance(req.state, resp, location, "", now)
	return out
}

// ParseText applies the fetcher's limits and country rules to pasted text, so both
// origins produce the same Document.
func (f *Fetcher) ParseText(text string) (Document, error) {
	return parse([]byte(text), Detect, f.limits, f.countries)
}

func (f *Fetcher) readBody(resp *http.Response) ([]byte, FailureCode) {
	if resp.ContentLength > f.limits.CompressedBytes {
		return nil, FailTooLarge
	}
	var r io.Reader = &capReader{r: resp.Body, left: f.limits.CompressedBytes}
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, FailBadEncoding
		}
		r = gz
	default:
		return nil, FailBadEncoding
	}
	body, err := io.ReadAll(&capReader{r: r, left: f.limits.DecodedBytes})
	switch {
	case errors.Is(err, ErrTooLarge):
		return nil, FailTooLarge
	case errors.Is(err, gzip.ErrHeader), errors.Is(err, gzip.ErrChecksum), errors.Is(err, io.ErrUnexpectedEOF):
		return nil, FailBadEncoding
	case err != nil:
		return nil, transportFailure(err)
	}
	return body, ""
}

// capReader fails with ErrTooLarge instead of truncating.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, ErrTooLarge
	}
	return n, err
}

func parseFailure(err error) FailureCode {
	switch {
	case errors.Is(err, ErrTooLarge):
		return FailTooLarge
	case errors.Is(err, ErrTooManyEntries):
		return FailTooManyEntries
	case errors.Is(err, ErrNoUsableEntries):
		return FailNoUsableEntries
	case errors.Is(err, ErrUnknownFormat):
		return FailUnknownFormat
	}
	return FailMalformed
}

func transportFailure(err error) FailureCode {
	switch {
	case errors.Is(err, errAddressBlocked):
		return FailAddressBlocked
	case errors.Is(err, errTooManyRedirects):
		return FailTooManyRedirects
	case errors.Is(err, errRedirectRefused):
		return FailRedirectRefused
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return FailTimeout
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return FailTimeout
	}
	return FailNetwork
}

// webURL is a link a provider's header may carry, which the page only shows.
func webURL(u *url.URL) bool {
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

// sourceURL is a location the fetcher asks. Plain HTTP would hand the source's token, and
// the servers it lists, to anyone on the path.
func sourceURL(u *url.URL) bool {
	return u.Scheme == "https" && u.Host != "" && u.User == nil
}

// checkRedirect allows a bounded chain of redirects that stay on HTTPS. Leaving the
// origin drops device headers and validators, which belong to the first origin only.
func (f *Fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > f.limits.Redirects {
		return errTooManyRedirects
	}
	first := via[0].URL
	if !sourceURL(req.URL) {
		return errRedirectRefused
	}
	if req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
		for _, h := range append(deviceHeaders, "If-None-Match", "If-Modified-Since") {
			req.Header.Del(h)
		}
	}
	return nil
}

// dial resolves once, refuses the whole name if any address is not public, and
// connects to the checked address, so a second lookup cannot rebind it.
func (f *Fetcher) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else if addrs, err = f.resolver.LookupNetIP(ctx, "ip", host); err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, errAddressBlocked
	}
	for _, a := range addrs {
		if !f.allow(a.Unmap()) {
			return nil, errAddressBlocked
		}
	}
	var d net.Dialer
	var lastErr error
	for _, a := range addrs {
		conn, err := d.DialContext(ctx, network, netip.AddrPortFrom(a.Unmap(), uint16(port)).String())
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
}

var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
	teredo    = netip.MustParsePrefix("2001::/32")
)

// publicAddr refuses loopback, private, link-local (including cloud metadata),
// multicast, CGNAT, documentation, and reserved ranges. An IPv6 address that
// carries an IPv4 destination (NAT64, 6to4, Teredo) is judged by that IPv4.
func publicAddr(a netip.Addr) bool {
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsUnspecified() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	b := a.As16()
	switch {
	case nat64.Contains(a):
		return publicAddr(netip.AddrFrom4([4]byte(b[12:16])))
	case sixToFour.Contains(a):
		return publicAddr(netip.AddrFrom4([4]byte(b[2:6])))
	case teredo.Contains(a):
		return publicAddr(netip.AddrFrom4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]})) // The client address, inverted.
	}
	return true
}

// advance folds one response into the state. 304 is success and keeps prior
// validators and metadata for fields the response omits; a validated 200 replaces
// them, and absent fields clear the old value. Failures keep the last good
// metadata and bump the failure count. 429/503 Retry-After, including the date
// form, sets a floor that normal cadence never shortens. 401/403/404 jump to the
// last backoff step. Userinfo upload/download/total/expire, refill date,
// profile-update-interval (hours), plain or base64 title, announcement, support
// and profile URLs are bounded and optional. Provider links are display text,
// never executable downloads or probe targets.
func advance(prev FetchState, resp *http.Response, location *url.URL, failure FailureCode, now time.Time) FetchState {
	next := prev
	next.checkedAt = now
	if !prev.retryAfter.After(now) {
		next.retryAfter = time.Time{}
	}
	if failure != "" {
		next.failures = min(prev.failures, 1<<16) + 1
		next.failure = failure
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusTooManyRequests, http.StatusServiceUnavailable:
				if at, ok := retryAfter(resp.Header.Get("Retry-After"), now); ok && at.After(next.retryAfter) {
					next.retryAfter = at
				}
			case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
				next.failures = max(next.failures, uint32(len(backoffSteps)))
			}
		}
		return next
	}
	next.failures, next.failure = 0, ""
	meta := providerMetadata(resp.Header, now, location)
	etag := boundedHeader(resp.Header.Get("ETag"), 1024)
	lastModified := boundedHeader(resp.Header.Get("Last-Modified"), 64)
	if resp.StatusCode == http.StatusNotModified {
		next.etag, next.lastModified = orDefault(etag, prev.etag), orDefault(lastModified, prev.lastModified)
		next.metadata = mergeMetadata(prev.metadata, meta)
		return next
	}
	next.updatedAt = now
	next.etag, next.lastModified, next.metadata = etag, lastModified, meta
	return next
}

func retryAfter(value string, now time.Time) (time.Time, bool) {
	value = strings.TrimSpace(value)
	var at time.Time
	if seconds, err := strconv.ParseUint(value, 10, 32); err == nil {
		at = now.Add(time.Duration(seconds) * time.Second)
	} else if t, err := http.ParseTime(value); err == nil {
		at = t
	} else {
		return time.Time{}, false
	}
	if limit := now.Add(MaxRefresh); at.After(limit) {
		at = limit
	}
	return at, true
}

const (
	maxTitle        = 256
	maxAnnouncement = 4096
	maxURL          = 2048
)

// providerMetadata reads what the provider says about itself. A panel may send the
// source's own URL as its web page, so nothing naming the location's secret part
// is kept: metadata reaches the page, and the location must not.
func providerMetadata(h http.Header, now time.Time, location *url.URL) domain.ProviderMetadata {
	clean := func(v string) string {
		if namesLocation(v, location) {
			return ""
		}
		return v
	}
	meta := domain.ProviderMetadata{
		Title:        clean(headerText(h.Get("Profile-Title"), maxTitle)),
		Announcement: clean(headerText(cmp.Or(h.Get("Profile-Announce"), h.Get("Announce")), maxAnnouncement)),
		SupportURL:   clean(headerURL(h.Get("Support-Url"))),
		ProfileURL:   clean(headerURL(h.Get("Profile-Web-Page-Url"))),
	}
	if hours, err := strconv.ParseUint(strings.TrimSpace(h.Get("Profile-Update-Interval")), 10, 16); err == nil && hours > 0 {
		meta.SuggestedRefresh = min(time.Duration(hours)*time.Hour, MaxRefresh)
	}
	if info := boundedHeader(h.Get("Subscription-Userinfo"), 2048); info != "" {
		meta.Quota.Upload = userinfoValue(info, "upload", false)
		meta.Quota.Download = userinfoValue(info, "download", false)
		meta.Quota.Limit = userinfoValue(info, "total", false)
		if expire := userinfoValue(info, "expire", true); expire != nil {
			meta.Quota.Expires = unixTime(*expire)
		}
	}
	if refill, err := strconv.ParseUint(strings.TrimSpace(h.Get("Subscription-Refill-Date")), 10, 63); err == nil && refill > 0 {
		meta.Quota.Refills = unixTime(refill)
	}
	if meta.Quota.Upload != nil || meta.Quota.Download != nil || meta.Quota.Limit != nil ||
		meta.Quota.Expires != nil || meta.Quota.Refills != nil {
		meta.Quota.ObservedAt = now
	}
	return meta
}

// namesLocation reports whether v contains the part of a source's location that
// grants access: its query, or its path's last segment.
func namesLocation(v string, location *url.URL) bool {
	if v == "" || location == nil {
		return false
	}
	unescaped, err := url.PathUnescape(v)
	if err != nil {
		unescaped = v
	}
	for _, secret := range []string{location.RawQuery, path.Base(location.Path)} {
		if secret != "" && secret != "/" && secret != "." && (strings.Contains(v, secret) || strings.Contains(unescaped, secret)) {
			return true
		}
	}
	return false
}

// mergeMetadata keeps every field a 304 left out.
func mergeMetadata(prev, next domain.ProviderMetadata) domain.ProviderMetadata {
	out := prev
	for _, f := range []struct {
		dst *string
		src string
	}{
		{&out.Title, next.Title}, {&out.Announcement, next.Announcement},
		{&out.SupportURL, next.SupportURL}, {&out.ProfileURL, next.ProfileURL},
	} {
		if f.src != "" {
			*f.dst = f.src
		}
	}
	if next.SuggestedRefresh > 0 {
		out.SuggestedRefresh = next.SuggestedRefresh
	}
	if !next.Quota.ObservedAt.IsZero() {
		out.Quota = next.Quota
	}
	return out
}

// lastStorable is the last second time.Time can encode: RFC 3339 has four-digit years.
var lastStorable = time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC).Unix()

// unixTime reads an optional provider date, and drops one FetchState could not
// persist.
func unixTime(seconds uint64) *time.Time {
	if seconds > uint64(lastStorable) {
		return nil
	}
	t := time.Unix(int64(seconds), 0).UTC()
	return &t
}

// userinfoValue reads one key of "upload=1; download=2; total=3; expire=4".
// Zero is a value except for total and expire, where panels use it for none.
func userinfoValue(info, key string, zeroIsNone bool) *uint64 {
	for part := range strings.SplitSeq(info, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), key) {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 63)
		if err != nil || (zeroIsNone && n == 0) {
			return nil
		}
		return &n
	}
	return nil
}

func boundedHeader(v string, limit int) string {
	v = strings.TrimSpace(v)
	if len(v) > limit {
		return ""
	}
	return v
}

// headerText decodes a "base64:" value or a percent-encoded one, and keeps it
// only if it is printable text within limit runes.
func headerText(v string, limit int) string {
	v = boundedHeader(v, limit*4)
	if len(v) >= len("base64:") && strings.EqualFold(v[:len("base64:")], "base64:") {
		encoded := v[len("base64:"):]
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			decoded, err = base64.URLEncoding.DecodeString(encoded)
		}
		if err != nil {
			return ""
		}
		v = string(decoded)
	} else if unescaped, err := url.PathUnescape(v); err == nil {
		v = unescaped
	}
	v = strings.TrimSpace(strings.Trim(v, `"`))
	if v == "" || len([]rune(v)) > limit || strings.ContainsFunc(v, func(r rune) bool {
		return r == unicode.ReplacementChar || (unicode.IsControl(r) && r != '\n')
	}) {
		return ""
	}
	return v
}

func headerURL(v string) string {
	v = boundedHeader(v, maxURL)
	u, err := url.Parse(v)
	if err != nil || !webURL(u) {
		return ""
	}
	return u.String()
}
