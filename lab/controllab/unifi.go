package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// unifiStandIn answers the Network Application calls internal/unifi makes, in the shapes
// internal/unifi/testdata/contract.json recorded from the real app. It holds one VPN Client that
// isn't ours, and gives our first client two traffic routes, so the page has a board and a kill
// switch to show. What the real app accepts is lab/net's to prove. It counts every write it is
// sent, which GET /lab/writes answers, so a step can prove what reached UniFi.
type unifiStandIn struct {
	mu       sync.Mutex
	networks []map[string]any
	routes   []map[string]any
	serial   int
	writes   []string
}

func (u *unifiStandIn) id(kind string) string {
	u.serial++
	return fmt.Sprintf("lab%s%020d", kind, u.serial)
}

func (u *unifiStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/proxy/network")
	if path == "/lab/writes" {
		json.NewEncoder(w).Encode(u.writes)
		return
	}
	if r.Method != http.MethodGet {
		u.writes = append(u.writes, r.Method+" "+path)
	}
	var body map[string]any
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body)
	}
	legacy := func(data any) {
		json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"rc": "ok"}, "data": data})
	}
	const networks, trafficRoutes = "/api/s/default/rest/networkconf", "/v2/api/site/default/trafficroutes"
	switch {
	case path == networks && r.Method == http.MethodGet:
		legacy(u.networks)
	case path == networks && r.Method == http.MethodPost:
		body["_id"] = u.id("net")
		if _, stated := body["enabled"]; !stated {
			body["enabled"] = true
		}
		u.networks = append(u.networks, body)
		if len(u.routes) == 0 {
			id := body["_id"]
			u.routes = []map[string]any{
				{"_id": u.id("route"), "description": "Laptops", "network_id": id, "enabled": true, "kill_switch_enabled": true, "matching_target": "INTERNET"},
				{"_id": u.id("route"), "description": "Television", "network_id": id, "enabled": true, "kill_switch_enabled": false, "matching_target": "INTERNET"},
			}
		}
		legacy([]map[string]any{body})
	case strings.HasPrefix(path, networks+"/") && r.Method == http.MethodPut:
		id := strings.TrimPrefix(path, networks+"/")
		for _, n := range u.networks {
			if n["_id"] == id {
				for k, v := range body {
					n[k] = v
				}
				legacy([]map[string]any{n})
				return
			}
		}
		http.NotFound(w, r)
	case strings.HasPrefix(path, networks+"/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(path, networks+"/")
		for i, n := range u.networks {
			if n["_id"] == id {
				u.networks = append(u.networks[:i], u.networks[i+1:]...)
				break
			}
		}
		legacy([]map[string]any{})
	case path == "/api/s/default/rest/routing":
		legacy([]map[string]any{})
	case path == "/v2/api/site/default/object-oriented-network-configs":
		json.NewEncoder(w).Encode([]map[string]any{})
	case path == trafficRoutes:
		json.NewEncoder(w).Encode(u.routes)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func standIn(ctx context.Context) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	u := &unifiStandIn{networks: []map[string]any{
		{"_id": "labforeign00000000000001", "name": "Office WireGuard", "purpose": "vpn-client", "vpn_type": "wireguard-client", "enabled": true},
	}}
	listener, err := net.Listen("tcp", "127.0.0.1:8443")
	if err != nil {
		return err
	}
	server := &http.Server{Handler: u, TLSConfig: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}}
	go func() {
		<-ctx.Done()
		server.Close()
	}()
	return server.ServeTLS(listener, "", "")
}
