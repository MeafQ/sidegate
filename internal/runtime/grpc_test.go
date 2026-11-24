package runtime

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"
)

// fakeCore answers unary gRPC calls the way Xray's API does: a length-prefixed
// protobuf body and grpc-status in the trailers.
func fakeCore(t *testing.T, answer func(method string, req []byte) ([]byte, string)) coreAPI {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.ProtoMajor != 2 || r.Header.Get("Content-Type") != "application/grpc" || len(body) < 5 {
			http.Error(w, "not grpc", http.StatusBadRequest)
			return
		}
		resp, status := answer(r.URL.Path[1:], body[5:])
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
		frame := binary.BigEndian.AppendUint32([]byte{0}, uint32(len(resp)))
		w.Write(append(frame, resp...))
		w.Header().Set("Grpc-Status", status)
		if status != "0" {
			w.Header().Set("Grpc-Message", "app/router: cannot find tag")
		}
	}))
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	return newCoreAPI(netip.MustParseAddrPort(srv.Listener.Addr().String()))
}

func TestBalancerReadbackOverH2C(t *testing.T) {
	var asked string
	api := fakeCore(t, func(method string, req []byte) ([]byte, string) {
		tags, _ := tagsOf(pbBytes(nil, 1, req), 1, 1)
		asked = method + " " + tags[0]
		override := pbBytes(nil, 5, pbString(nil, 2, "o.srvB.0011223344556677"))
		principle := pbBytes(nil, 6, pbString(pbString(nil, 1, "o.srvA.0011223344556677"), 1, "o.srvB.0011223344556677"))
		return pbBytes(nil, 1, append(override, principle...)), "0"
	})
	info, err := api.balancer(context.Background(), "b.slot1")
	if err != nil {
		t.Fatal(err)
	}
	if asked != routingService+"GetBalancerInfo b.slot1" {
		t.Errorf("request %q", asked)
	}
	if info.Override != "o.srvB.0011223344556677" {
		t.Errorf("readback %+v", info)
	}
}

func TestMissingBalancerIsNotFound(t *testing.T) {
	api := fakeCore(t, func(string, []byte) ([]byte, string) { return nil, "2" })
	_, err := api.balancer(context.Background(), "b.gone")
	if !isNotFound(err) {
		t.Fatalf("err %v is not a not-found", err)
	}
}

func TestQueryStatsAndListings(t *testing.T) {
	api := fakeCore(t, func(method string, _ []byte) ([]byte, string) {
		switch method {
		case statsService + "QueryStats":
			stat := func(name string, v uint64) []byte {
				return pbBytes(nil, 1, pbVarint(pbVarint(pbString(nil, 1, name), 2<<3), v))
			}
			return append(stat("outbound>>>o.a>>>traffic>>>uplink", 300), stat("outbound>>>o.a>>>traffic>>>downlink", 1<<40)...), "0"
		case routingService + "ListRule":
			return append(pbBytes(nil, 1, pbString(pbString(nil, 1, "o.a"), 2, "p.a")), pbBytes(nil, 1, nil)...), "0"
		}
		return nil, "12"
	})
	stats, err := api.queryStats(context.Background(), "outbound>>>")
	if err != nil {
		t.Fatal(err)
	}
	if stats["outbound>>>o.a>>>traffic>>>uplink"] != 300 || stats["outbound>>>o.a>>>traffic>>>downlink"] != 1<<40 {
		t.Errorf("stats %v", stats)
	}
	rules, err := api.listRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rules, []routeRule{{RuleTag: "p.a", Outbound: "o.a"}, {}}) {
		t.Errorf("rules %q", rules)
	}
}

func TestRouteOfAsksTheRouterAboutOneInboundNetworkAndPort(t *testing.T) {
	var asked []byte
	api := fakeCore(t, func(method string, req []byte) ([]byte, string) {
		if method != routingService+"TestRoute" {
			return nil, "12"
		}
		asked = req
		if bytes.Contains(req, []byte("s.gone")) {
			return nil, "2"
		}
		return pbString(pbString(nil, 1, "s.slot1.udp"), 12, "o.srvA.0011223344556677"), "0"
	})
	tag, ok, err := api.routeOf(context.Background(), "s.slot1.udp", networkUDP, 443)
	if err != nil || !ok || tag != "o.srvA.0011223344556677" {
		t.Fatalf("routeOf = %q, %v, %v", tag, ok, err)
	}
	routing := pbVarint(pbVarint(pbVarint(pbVarint(pbString(nil, 1, "s.slot1.udp"), 2<<3), networkUDP), 6<<3), 443)
	if want := pbString(pbBytes(nil, 1, routing), 2, "outbound"); !bytes.Equal(asked, want) {
		t.Errorf("request %x, want %x", asked, want)
	}
	if _, ok, err := api.routeOf(context.Background(), "s.gone", networkTCP, 443); ok || err != nil {
		t.Errorf("a router that picks nothing gave ok=%v err=%v; it must read as routing nowhere", ok, err)
	}
}

// Only the router's own answer, an error from its handler, which gRPC carries as Unknown, says no rule
// matched. Any other status is a call that failed.
func TestARouteTestTheAPICouldntAnswerIsAnError(t *testing.T) {
	for _, status := range []string{"14", "4", "13", "12"} {
		api := fakeCore(t, func(string, []byte) ([]byte, string) { return nil, status })
		if _, ok, err := api.routeOf(context.Background(), "s.slot1.tcp", networkTCP, 443); ok || err == nil {
			t.Errorf("status %s gave ok=%v err=%v; it must be an error", status, ok, err)
		}
	}
}

func TestMalformedProtobufIsAnError(t *testing.T) {
	if err := pbScan([]byte{0x0a, 0x05, 'a'}, func(int, uint64, []byte) error { return nil }); err == nil {
		t.Error("truncated length-delimited field accepted")
	}
}
