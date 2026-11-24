package runtime

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// coreAPI is a unary gRPC client for the Xray API. Its methods are the reads the actor and
// telemetry make: listings, balancer readback, routing tests, probe users and stats. A pass's
// writes go through corePass, which sends them with call. It speaks HTTP/2 without TLS through
// net/http and encodes the small protobuf messages by hand, so the daemon links no gRPC or
// protobuf runtime and spawns no process per read.
type coreAPI struct {
	base   string
	client *http.Client
}

const apiCallTimeout = 3 * time.Second

func newCoreAPI(addr netip.AddrPort) coreAPI {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	return coreAPI{
		base:   "http://" + addr.String() + "/",
		client: &http.Client{Transport: &http.Transport{Protocols: &protocols, DisableCompression: true}},
	}
}

type grpcError struct {
	code    string
	message string
}

func (e grpcError) Error() string { return "grpc status " + e.code + ": " + e.message }

// grpcUnknown is the status gRPC gives an error a handler returned.
const grpcUnknown = "2"

func (a coreAPI) call(ctx context.Context, method string, request []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, apiCallTimeout)
	defer cancel()
	body := make([]byte, 5, 5+len(request))
	binary.BigEndian.PutUint32(body[1:], uint32(len(request)))
	body = append(body, request...)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+method, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	status := resp.Trailer.Get("Grpc-Status")
	message := resp.Trailer.Get("Grpc-Message")
	if status == "" {
		status, message = resp.Header.Get("Grpc-Status"), resp.Header.Get("Grpc-Message")
	}
	if resp.StatusCode != http.StatusOK || status != "0" {
		return nil, fmt.Errorf("%s: %w", method, grpcError{code: status, message: message})
	}
	if len(payload) < 5 || payload[0] != 0 {
		return nil, fmt.Errorf("%s: malformed gRPC frame", method)
	}
	n := binary.BigEndian.Uint32(payload[1:5])
	if int(n) != len(payload)-5 {
		return nil, fmt.Errorf("%s: frame length %d, body %d", method, n, len(payload)-5)
	}
	return payload[5:], nil
}

const (
	handlerService = "xray.app.proxyman.command.HandlerService/"
	routingService = "xray.app.router.command.RoutingService/"
	statsService   = "xray.app.stats.command.StatsService/"
)

func (a coreAPI) listInbounds(ctx context.Context) ([]string, error) {
	resp, err := a.call(ctx, handlerService+"ListInbounds", pbBool(nil, 1, true))
	if err != nil {
		return nil, err
	}
	return tagsOf(resp, 1, 1)
}

func (a coreAPI) listOutbounds(ctx context.Context) ([]string, error) {
	resp, err := a.call(ctx, handlerService+"ListOutbounds", nil)
	if err != nil {
		return nil, err
	}
	return tagsOf(resp, 1, 1)
}

// routeRule is one routing rule as ListRule reads it back: its tag and its
// outbound. A rule that feeds a balancer has no outbound.
type routeRule struct {
	RuleTag, Outbound string
}

func (a coreAPI) listRules(ctx context.Context) ([]routeRule, error) {
	resp, err := a.call(ctx, routingService+"ListRule", nil)
	if err != nil {
		return nil, err
	}
	var rules []routeRule
	err = pbFields(resp, func(num int, item []byte) error {
		if num != 1 {
			return nil
		}
		var r routeRule
		err := pbFields(item, func(num int, v []byte) error {
			switch num {
			case 1:
				r.Outbound = string(v)
			case 2:
				r.RuleTag = string(v)
			}
			return nil
		})
		rules = append(rules, r)
		return err
	})
	return rules, err
}

// Network values of xray.common.net.Network.
const (
	networkTCP = 2
	networkUDP = 3
)

// routeOf asks the core's router which outbound a new connection on inbound to
// port would take now. It runs the real decision: rule order and conditions, the
// balancer, and its override. ok is false when the router picks nothing, so the
// connection would go to the default outbound, block. The router says so with an
// error from its handler, which gRPC carries as Unknown; any other status, such as
// Unavailable, is a call that failed.
func (a coreAPI) routeOf(ctx context.Context, inbound string, network uint64, port uint32) (outbound string, ok bool, err error) {
	routing := pbVarint(pbVarint(pbString(nil, 1, inbound), 2<<3), network)
	routing = pbVarint(pbVarint(routing, 6<<3), uint64(port))
	resp, err := a.call(ctx, routingService+"TestRoute", pbString(pbBytes(nil, 1, routing), 2, "outbound"))
	var status grpcError
	if errors.As(err, &status) && status.code == grpcUnknown {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	err = pbFields(resp, func(num int, v []byte) error {
		if num == 12 {
			outbound = string(v)
		}
		return nil
	})
	return outbound, outbound != "", err
}

// inboundUsers returns the emails of tag's users.
func (a coreAPI) inboundUsers(ctx context.Context, tag string) ([]string, error) {
	resp, err := a.call(ctx, handlerService+"GetInboundUsers", pbString(nil, 1, tag))
	if err != nil {
		return nil, err
	}
	return tagsOf(resp, 1, 2)
}

type balancerInfo struct {
	Override string
}

func (a coreAPI) balancer(ctx context.Context, tag string) (balancerInfo, error) {
	resp, err := a.call(ctx, routingService+"GetBalancerInfo", pbString(nil, 1, tag))
	if err != nil {
		return balancerInfo{}, err
	}
	var info balancerInfo
	err = pbFields(resp, func(num int, msg []byte) error {
		if num != 1 {
			return nil
		}
		return pbFields(msg, func(num int, source []byte) error {
			if num != 5 {
				return nil
			}
			return pbFields(source, func(num int, v []byte) error {
				if num == 2 {
					info.Override = string(v)
				}
				return nil
			})
		})
	})
	return info, err
}

// queryStats returns cumulative counters whose names start with pattern. It never
// resets them: the metrics writer owns the watermark that turns them into deltas.
func (a coreAPI) queryStats(ctx context.Context, pattern string) (map[string]int64, error) {
	resp, err := a.call(ctx, statsService+"QueryStats", pbString(nil, 1, pattern))
	if err != nil {
		return nil, err
	}
	stats := map[string]int64{}
	err = pbFields(resp, func(num int, stat []byte) error {
		if num != 1 {
			return nil
		}
		var name string
		var value uint64
		err := pbScan(stat, func(num int, v uint64, b []byte) error {
			switch num {
			case 1:
				name = string(b)
			case 2:
				value = v
			}
			return nil
		})
		stats[name] = int64(value)
		return err
	})
	return stats, err
}

func typedMessage(name string, value []byte) []byte {
	return pbBytes(pbString(nil, 1, name), 2, value)
}

// tagsOf collects string field inner of every repeated message field outer.
func tagsOf(msg []byte, outer, inner int) ([]string, error) {
	var tags []string
	err := pbFields(msg, func(num int, source []byte) error {
		if num != outer {
			return nil
		}
		found := false
		err := pbFields(source, func(num int, v []byte) error {
			if num == inner {
				tags, found = append(tags, string(v)), true
			}
			return nil
		})
		if !found {
			tags = append(tags, "")
		}
		return err
	})
	return tags, err
}

// Protobuf wire format, the subset these messages use: varints and
// length-delimited fields.

func pbVarint(b []byte, v uint64) []byte { return binary.AppendUvarint(b, v) }

func pbBytes(b []byte, field int, v []byte) []byte {
	b = pbVarint(b, uint64(field)<<3|2)
	b = pbVarint(b, uint64(len(v)))
	return append(b, v...)
}

func pbString(b []byte, field int, v string) []byte {
	if v == "" {
		return b
	}
	return pbBytes(b, field, []byte(v))
}

func pbBool(b []byte, field int, v bool) []byte {
	if !v {
		return b
	}
	return pbVarint(pbVarint(b, uint64(field)<<3), 1)
}

var errProtobuf = errors.New("malformed protobuf")

// pbScan walks msg's fields: varints arrive in v, length-delimited fields in b.
func pbScan(msg []byte, fn func(num int, v uint64, b []byte) error) error {
	for len(msg) > 0 {
		key, n := binary.Uvarint(msg)
		if n <= 0 {
			return errProtobuf
		}
		msg = msg[n:]
		num := int(key >> 3)
		switch key & 7 {
		case 0:
			v, n := binary.Uvarint(msg)
			if n <= 0 {
				return errProtobuf
			}
			msg = msg[n:]
			if err := fn(num, v, nil); err != nil {
				return err
			}
		case 1:
			if len(msg) < 8 {
				return errProtobuf
			}
			msg = msg[8:]
		case 2:
			l, n := binary.Uvarint(msg)
			if n <= 0 || uint64(len(msg)-n) < l {
				return errProtobuf
			}
			if err := fn(num, 0, msg[n:n+int(l)]); err != nil {
				return err
			}
			msg = msg[n+int(l):]
		case 5:
			if len(msg) < 4 {
				return errProtobuf
			}
			msg = msg[4:]
		default:
			return errProtobuf
		}
	}
	return nil
}

// pbFields walks only the length-delimited fields of msg.
func pbFields(msg []byte, fn func(num int, b []byte) error) error {
	return pbScan(msg, func(num int, _ uint64, b []byte) error {
		if b == nil {
			return nil
		}
		return fn(num, b)
	})
}

// isNotFound reports an API error that means the named object does not exist: the handler's own
// error, which gRPC carries as Unknown. Any other status, such as Unavailable, is a call that failed.
func isNotFound(err error) bool {
	var g grpcError
	return errors.As(err, &g) && g.code == grpcUnknown && (strings.Contains(g.message, "not found") ||
		strings.Contains(g.message, "cannot find") || strings.Contains(g.message, "not exist"))
}
