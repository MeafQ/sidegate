package daemon

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultListenSocket sits apart from RunDir, which only root may enter, so that nginx's
// workers can reach it.
const DefaultListenSocket = "/run/sidegate-http/http.sock"

// DefaultBasePath is where the page and its API live on the gateway's own web address. The
// nginx hook serves it there.
const DefaultBasePath = "/sidegate/"

// Bootstrap is bootstrap.json under DataDir: paths and integration endpoints
// only. DataDir holds state.db, metrics.db, and releases; RunDir holds
// the rendered core config and its warning log.
// Mutable sources, settings, keys, and selections have one authoritative
// state.db record.
type Bootstrap struct {
	DataDir      string `json:"dataDir"`
	RunDir       string `json:"runDir"`
	ListenSocket string `json:"listenSocket"`
	// ListenGroup may connect to the socket, for nginx's workers. Empty means root only.
	ListenGroup string `json:"listenGroup"`
	BasePath    string `json:"basePath"`
	// NetworkCredentialFile holds the Network Application's API key. Without one the
	// daemon leaves UniFi alone and the page says so.
	NetworkCredentialFile string `json:"networkCredentialFile"`
	// NginxHookDir is unifi-core's directory that nginx includes our hook from. Empty means
	// the daemon leaves nginx alone.
	NginxHookDir    string `json:"nginxHookDir"`
	NetworkEndpoint string `json:"networkEndpoint"`
	NetworkSite     string `json:"networkSite"`
	// ClientDNS is the resolver a VPN Client is created with. The app refuses a client
	// without one, and no traffic of ours asks it anything.
	ClientDNS       string        `json:"clientDns"`
	ProbeTargets    []TargetSpec  `json:"probeTargets"`
	BaselineTargets []TargetSpec  `json:"baselineTargets"`
	CountryRules    []CountryRule `json:"countryRules"`
	// Timings overrides single durations of defaultTimings. An unknown field or a
	// non-positive value fails LoadBootstrap.
	Timings timingOverrides `json:"timings"`
}

// TargetSpec is one approved probe URL. Status zero accepts anything below 500.
type TargetSpec struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
}

// CountryRule assigns a country to every server whose label matches Regex; the owner's
// rules apply in order and the first match wins.
type CountryRule struct {
	Regex   string `json:"regex"`
	Country string `json:"country"`
}

// Probe targets are independent HTTPS sites, not only generate_204, which censors may
// whitelist. Baselines are plain HTTP: cheap, and any answer proves the WAN.
var (
	defaultProbeTargets = []TargetSpec{
		{URL: "https://www.gstatic.com/generate_204", Status: 204},
		{URL: "https://cp.cloudflare.com/generate_204", Status: 204},
		{URL: "https://www.apple.com/library/test/success.html", Status: 200},
		{URL: "https://www.google.com/generate_204", Status: 204},
	}
	defaultBaselineTargets = []TargetSpec{
		{URL: "http://www.gstatic.com/generate_204"},
		{URL: "http://cp.cloudflare.com/generate_204"},
		{URL: "http://captive.apple.com/hotspot-detect.html"},
	}
)

func bootstrapPath(dataDir string) string { return filepath.Join(dataDir, "bootstrap.json") }

// LoadBootstrap reads bootstrap.json under dataDir. A missing file means the defaults.
func LoadBootstrap(dataDir string) (Bootstrap, error) {
	b := Bootstrap{DataDir: dataDir}
	data, err := os.ReadFile(bootstrapPath(dataDir))
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &b); err != nil {
			return Bootstrap{}, fmt.Errorf("bootstrap.json: %w", err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return Bootstrap{}, err
	}
	b.DataDir = cmp.Or(b.DataDir, dataDir)
	b.RunDir = cmp.Or(b.RunDir, "/run/sidegate")
	b.ListenSocket = cmp.Or(b.ListenSocket, DefaultListenSocket)
	b.BasePath = cmp.Or(b.BasePath, DefaultBasePath)
	if !strings.HasPrefix(b.BasePath, "/") || !strings.HasSuffix(b.BasePath, "/") {
		return Bootstrap{}, errors.New("bootstrap.json: basePath must start and end with a slash")
	}
	b.NetworkEndpoint = cmp.Or(b.NetworkEndpoint, "https://127.0.0.1/proxy/network")
	b.NetworkSite = cmp.Or(b.NetworkSite, "default")
	b.ClientDNS = cmp.Or(b.ClientDNS, "1.1.1.1")
	if len(b.ProbeTargets) == 0 {
		b.ProbeTargets = defaultProbeTargets
	}
	if len(b.BaselineTargets) == 0 {
		b.BaselineTargets = defaultBaselineTargets
	}
	return b, nil
}
