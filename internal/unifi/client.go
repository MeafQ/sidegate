// Package unifi keeps the Network Application's VPN Clients in step with the
// daemon's slots. The JSON of the internal API is parsed here and nowhere else.
// Recorded shapes live in testdata/contract.json, which lab/net/verify.sh writes.
package unifi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sidegate/internal/domain"
)

// Config is the daemon's route to the Network Application on its own gateway.
type Config struct {
	// BaseURL is https on loopback, such as https://127.0.0.1/proxy/network. The
	// daemon runs on the gateway, so any other host is a mistake it must refuse.
	BaseURL string
	Site    string
	APIKey  domain.Secret
	// Timeout bounds each request, 10 s when zero.
	Timeout time.Duration
	// Transport replaces the default one. Only the lab sets it, because a plain
	// Network Application has no UniFi Core to validate an API key.
	Transport http.RoundTripper
}

// Client talks to the internal API. It logs nothing, and its errors carry only
// what the app's message code says, never a request or response body, and never
// the key it was built with.
type Client struct {
	base    string
	site    string
	key     domain.Secret
	http    *http.Client
	timeout time.Duration
}

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("network application URL: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("network application URL %q is not https", cfg.BaseURL)
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("network application URL %q is not on loopback", cfg.BaseURL)
	}
	if cfg.Site == "" {
		return nil, errors.New("network application site is empty")
	}
	if cfg.APIKey.Reveal() == "" {
		return nil, errors.New("network application API key is empty")
	}
	transport := cfg.Transport
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = nil
		// The certificate names the gateway, not 127.0.0.1, and New has proved the
		// connection never leaves the host.
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		transport = t
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		base: strings.TrimRight(cfg.BaseURL, "/"),
		site: cfg.Site,
		key:  cfg.APIKey,
		http: &http.Client{
			Transport: transport,
			// A redirect leads to a login page, not to the object asked for.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		timeout: timeout,
	}, nil
}

// FailureKind says what a caller can do about an Error.
type FailureKind uint8

const (
	// Unavailable means the app did not answer. Keep serving and try again later.
	Unavailable FailureKind = iota + 1
	// Unauthorized means the key was refused. Retrying won't help.
	Unauthorized
	// Schema means an answer this client can't read, so it writes nothing.
	Schema
	// Rejected means the app read the request and refused it.
	Rejected
)

type Error struct {
	Kind   FailureKind
	Op     string
	Status int
	Code   string // The app's message code, such as api.err.SubnetOverlapped.
	Field  string // The field the app blames, when it names one.
	Reason string
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", e.Op, map[FailureKind]string{
		Unavailable: "network application unavailable", Unauthorized: "API key refused",
		Schema: "unreadable answer", Rejected: "rejected",
	}[e.Kind])
	if e.Code != "" {
		fmt.Fprintf(&b, " (%s", e.Code)
		if e.Field != "" {
			fmt.Fprintf(&b, " on %s", e.Field)
		}
		b.WriteString(")")
	}
	if e.Status != 0 {
		fmt.Fprintf(&b, " HTTP %d", e.Status)
	}
	if e.Reason != "" {
		fmt.Fprintf(&b, ": %s", e.Reason)
	}
	return b.String()
}

func isKind(err error, kind FailureKind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == kind
}

// do sends one request and returns the raw answer of a 2xx. A 2xx body may still
// hold an rc:error, which the callers of the legacy API check.
func (c *Client) do(ctx context.Context, op, method, path string, body any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, &Error{Kind: Schema, Op: op, Reason: "encoding the request failed"}
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, &Error{Kind: Schema, Op: op, Reason: "building the request failed"}
	}
	req.Header.Set("X-API-KEY", c.key.Reveal())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{Kind: Unavailable, Op: op, Reason: c.transportReason(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, &Error{Kind: Unavailable, Op: op, Status: resp.StatusCode, Reason: c.transportReason(err)}
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		code, _ := c.appMessage(raw)
		return nil, &Error{Kind: Unauthorized, Op: op, Status: resp.StatusCode, Code: code}
	case resp.StatusCode >= 500:
		return nil, &Error{Kind: Unavailable, Op: op, Status: resp.StatusCode}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, &Error{Kind: Schema, Op: op, Status: resp.StatusCode, Reason: "redirected"}
	case resp.StatusCode >= 400:
		code, field := c.appMessage(raw)
		if code == "" {
			return nil, &Error{Kind: Schema, Op: op, Status: resp.StatusCode, Reason: "no message code"}
		}
		return nil, &Error{Kind: Rejected, Op: op, Status: resp.StatusCode, Code: code, Field: field}
	}
	return raw, nil
}

// told is how text from the app or the transport enters an Error, through
// appMessage and transportReason alone. The app may quote the key it was sent
// anywhere in an answer, and the transport quotes a malformed one, so the key is
// cut out and the rest kept.
func (c *Client) told(text string) string {
	return strings.ReplaceAll(text, c.key.Reveal(), "<API key>")
}

// transportReason drops the URL, which says nothing a caller can use, and keeps
// a timeout readable.
func (c *Client) transportReason(err error) string {
	var u *url.Error
	if errors.As(err, &u) {
		err = u.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	return c.told(err.Error())
}

// appMessage reads the message code of a legacy meta envelope or a v2 error.
func (c *Client) appMessage(raw []byte) (code, field string) {
	var answer struct {
		Meta struct {
			Msg   string `json:"msg"`
			Field string `json:"field_name"`
		} `json:"meta"`
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &answer) != nil {
		return "", ""
	}
	if answer.Meta.Msg != "" {
		return c.told(answer.Meta.Msg), c.told(answer.Meta.Field)
	}
	return c.told(answer.Code), ""
}

// legacy calls an /api/s/<site> endpoint and returns its data array.
func (c *Client) legacy(ctx context.Context, op, method, path string, body any) (json.RawMessage, error) {
	raw, err := c.do(ctx, op, method, "/api/s/"+url.PathEscape(c.site)+path, body)
	if err != nil {
		return nil, err
	}
	var answer struct {
		Meta struct {
			RC string `json:"rc"`
		} `json:"meta"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, &Error{Kind: Schema, Op: op, Reason: "answer is not JSON"}
	}
	switch answer.Meta.RC {
	case "ok":
		return answer.Data, nil
	case "error":
		code, field := c.appMessage(raw)
		return nil, &Error{Kind: Rejected, Op: op, Code: code, Field: field}
	}
	return nil, &Error{Kind: Schema, Op: op, Reason: "answer has no rc"}
}

// redacted holds a secret read from the app, so that no print of a network shows it.
type redacted string

func (redacted) String() string { return "<redacted>" }

// network is one rest/networkconf object, narrowed to the fields this client reads.
type network struct {
	ID            string   `json:"_id"`
	Name          string   `json:"name"`
	Purpose       string   `json:"purpose"`
	VPNType       string   `json:"vpn_type"`
	Mode          string   `json:"wireguard_client_mode"`
	PeerIP        string   `json:"wireguard_client_peer_ip"`
	PeerPort      int      `json:"wireguard_client_peer_port"`
	PeerPublicKey string   `json:"wireguard_client_peer_public_key"`
	PublicKey     string   `json:"wireguard_public_key"`
	PrivateKey    redacted `json:"x_wireguard_private_key"`
	Subnet        string   `json:"ip_subnet"`
	DNS           string   `json:"dhcpd_dns_1"`
	Filename      string   `json:"wireguard_client_configuration_filename"`
	// Enabled is a pointer so an absent field stays distinct from false. The daemon
	// reads it and never writes it, because a disabled client sends traffic direct.
	Enabled *bool `json:"enabled"`
}

const purposeVPNClient = "vpn-client"

// Check makes one read, to learn whether the app answers and accepts this client's key.
func (c *Client) Check(ctx context.Context) error {
	_, err := c.networks(ctx)
	return err
}

func (c *Client) networks(ctx context.Context) ([]network, error) {
	const op = "list networks"
	data, err := c.legacy(ctx, op, http.MethodGet, "/rest/networkconf", nil)
	if err != nil {
		return nil, err
	}
	var nets []network
	if err := json.Unmarshal(data, &nets); err != nil {
		return nil, &Error{Kind: Schema, Op: op, Reason: "networks are not in the recorded shape"}
	}
	for _, n := range nets {
		if n.ID == "" || n.Purpose == "" {
			return nil, &Error{Kind: Schema, Op: op, Reason: "a network has no _id or purpose"}
		}
	}
	return nets, nil
}

func (c *Client) createNetwork(ctx context.Context, fields map[string]any) (domain.NetworkID, error) {
	const op = "create VPN Client"
	data, err := c.legacy(ctx, op, http.MethodPost, "/rest/networkconf", fields)
	if err != nil {
		return "", err
	}
	var created []struct {
		ID string `json:"_id"`
	}
	if err := json.Unmarshal(data, &created); err != nil || len(created) != 1 || created[0].ID == "" {
		return "", &Error{Kind: Schema, Op: op, Reason: "the answer names no created object"}
	}
	return domain.NetworkID(created[0].ID), nil
}

func (c *Client) updateNetwork(ctx context.Context, id string, fields map[string]any) error {
	_, err := c.legacy(ctx, "update VPN Client", http.MethodPut, "/rest/networkconf/"+url.PathEscape(id), fields)
	return err
}

func (c *Client) deleteNetwork(ctx context.Context, id string) error {
	_, err := c.legacy(ctx, "delete VPN Client", http.MethodDelete, "/rest/networkconf/"+url.PathEscape(id), nil)
	return err
}

// Route is a Policy Table entry that targets a network.
type Route struct {
	ID, Name string
	Kind     RouteKind
	Target   domain.NetworkID
	Enabled  bool
	// KillSwitch is read from the route, and this product never writes it. It is nil on
	// a route with no such field, such as a static route, and on a traffic route that
	// names this network some other way than network_id.
	KillSwitch *bool
}

type RouteKind string

const (
	TrafficRoute RouteKind = "traffic route"
	StaticRoute  RouteKind = "static route"
	PolicyRoute  RouteKind = "policy route"
)

// routes reads every Policy Table store. The UI writes these under different
// fields per kind and the app may add more, so a reference is any string value
// equal to a network's _id, at any depth. A false match can only refuse a delete.
//
// Traffic routes are the store a delete takes with it, so they are read last, right
// before it. The app has no conditional delete, so a route saved after that read
// still goes with the client.
func (c *Client) routes(ctx context.Context, targets map[string]bool) ([]Route, error) {
	stores := []struct {
		kind RouteKind
		read func() (json.RawMessage, error)
	}{
		{StaticRoute, func() (json.RawMessage, error) {
			return c.legacy(ctx, "list static routes", http.MethodGet, "/rest/routing", nil)
		}},
		{PolicyRoute, func() (json.RawMessage, error) {
			return c.v2(ctx, "list policy routes", "/object-oriented-network-configs")
		}},
		{TrafficRoute, func() (json.RawMessage, error) {
			return c.v2(ctx, "list traffic routes", "/trafficroutes")
		}},
	}
	var found []Route
	for _, store := range stores {
		raw, err := store.read()
		if err != nil {
			return nil, err
		}
		var entries []map[string]any
		// UseNumber, because a route is sent back whole and a float64 would round a large integer.
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&entries); err != nil {
			return nil, &Error{Kind: Schema, Op: "read " + string(store.kind) + "s", Reason: "not a list"}
		}
		for _, entry := range entries {
			hits := map[string]bool{}
			references(entry, targets, hits, true)
			for target := range hits {
				enabled, stated := entry["enabled"].(bool)
				route := Route{ID: text(entry, "_id"), Name: routeName(entry), Kind: store.kind, Target: domain.NetworkID(target), Enabled: enabled || !stated}
				if blocks, ok := entry[killSwitchField].(bool); ok && store.kind == TrafficRoute && text(entry, "network_id") == target {
					route.KillSwitch = &blocks
				}
				found = append(found, route)
			}
		}
	}
	return found, nil
}

func (c *Client) v2(ctx context.Context, op, path string) (json.RawMessage, error) {
	return c.do(ctx, op, http.MethodGet, "/v2/api/site/"+url.PathEscape(c.site)+path, nil)
}

const killSwitchField = "kill_switch_enabled"

func references(value any, targets, hits map[string]bool, top bool) {
	switch v := value.(type) {
	case string:
		if targets[v] {
			hits[v] = true
		}
	case []any:
		for _, item := range v {
			references(item, targets, hits, false)
		}
	case map[string]any:
		for key, item := range v {
			if top && key == "_id" {
				continue
			}
			references(item, targets, hits, false)
		}
	}
}

func text(entry map[string]any, key string) string {
	s, _ := entry[key].(string)
	return s
}

func routeName(entry map[string]any) string {
	for _, key := range []string{"description", "name"} {
		if s := text(entry, key); s != "" {
			return s
		}
	}
	return text(entry, "_id")
}
