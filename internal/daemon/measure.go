package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"sidegate/internal/domain"
	gateway "sidegate/internal/runtime"
)

const (
	pingRequests = 2 // Reused requests a full probe makes. A heartbeat makes none.
	probeBodyCap = 64 << 10
)

// measurement is what one probe saw, before anyone judges it.
type measurement struct {
	err        error // The first request that errored, whichever one it was.
	neverAsked bool  // The core's own inbound didn't answer, so the server wasn't asked.
	newConn    time.Duration
	ping       *time.Duration
	pingSpread *time.Duration
	noReuse    bool
}

// probeTransport forces HTTP/1.1, because a multiplexed HTTP/2 connection would give
// times that don't compare across servers. Each request crosses one TLS stream.
func probeTransport(dial func(ctx context.Context, network, address string) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		Proxy:              nil,
		DisableKeepAlives:  false,
		DisableCompression: true,
		MaxConnsPerHost:    1,
		TLSNextProto:       map[string]func(string, *tls.Conn) http.RoundTripper{},
		DialContext:        dial,
	}
}

// measure makes one request on a new connection, then reused more on that connection.
// ctx is the one deadline over all of them. A follow-up that errors fails the probe.
// A connection that simply wasn't reused gives no Ping and fails nothing.
func (s *signalLoop) measure(ctx context.Context, route gateway.ProbeRoute, target probeTarget, reused int) measurement {
	var dialFailed atomic.Bool
	transport := probeTransport(func(ctx context.Context, _, address string) (net.Conn, error) {
		conn, err := s.dial(route, ctx, "tcp", address)
		dialFailed.Store(err != nil)
		return conn, err
	})
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	failed := func(err error) measurement {
		return measurement{err: err, neverAsked: dialFailed.Load() || errors.Is(err, errBadTargetURL)}
	}
	first, err := fetch(ctx, client, target)
	if err != nil {
		return failed(err)
	}
	m := measurement{newConn: first.toFirstByte}
	if first.pastCap {
		m.noReuse = reused > 0
		return m
	}
	var pings []time.Duration
	for range reused {
		next, err := fetch(ctx, client, target)
		if err != nil {
			return failed(err)
		}
		if !next.reused || next.pastCap {
			m.noReuse = true
			return m
		}
		pings = append(pings, next.toFirstByte)
	}
	if len(pings) > 0 {
		ping, spread := domain.Median(pings), slices.Max(pings)-slices.Min(pings)
		m.ping, m.pingSpread = &ping, &spread
	}
	return m
}

// fetched is one request's result.
type fetched struct {
	toFirstByte time.Duration
	reused      bool // Every connection this request took was already open.
	pastCap     bool
}

var errBadTargetURL = errors.New("bad target URL")

// fetch times one request to the first response byte, since client.Do also waits for the
// rest of the headers. net/http may retry a GET on a new connection after a reused one
// dies, so GotConn can fire twice, and one new connection means the request wasn't reused.
func fetch(ctx context.Context, client *http.Client, target probeTarget) (fetched, error) {
	// The transport's callbacks run on its goroutines, and can outlive client.Do on a timeout.
	var mu sync.Mutex
	result := fetched{reused: true}
	began := time.Now()
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			mu.Lock()
			defer mu.Unlock()
			result.reused = result.reused && info.Reused
		},
		GotFirstResponseByte: func() {
			mu.Lock()
			defer mu.Unlock()
			result.toFirstByte = time.Since(began)
		},
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, target.URL, nil)
	if err != nil {
		return fetched{}, fmt.Errorf("%w: %w", errBadTargetURL, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fetched{}, err
	}
	defer resp.Body.Close()
	good := resp.StatusCode < 500
	if target.ExpectedStatus != 0 {
		good = resp.StatusCode == target.ExpectedStatus
	}
	if !good {
		return fetched{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	limit := target.MaxBytes
	if limit <= 0 {
		limit = probeBodyCap
	}
	// Only a body read to EOF lets the connection go back for the next request.
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fetched{}, err
	}
	mu.Lock()
	defer mu.Unlock()
	result.pastCap = n > limit
	return result, nil
}
