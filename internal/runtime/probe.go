package runtime

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
)

// Dial opens a stream to address through the probe inbound, so the core routes it
// out through r.Server's outbound. network is "tcp" or "udp"; a UDP stream frames
// each datagram with a two-byte length, as VLESS does. The loopback hop has no
// TLS. The response header is read lazily on the first Read, so a dial costs no
// round trip.
func (r ProbeRoute) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	header, err := vlessRequest(r.user, network, address)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", r.inbound.String())
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(header); err != nil {
		conn.Close()
		return nil, err
	}
	c := &vlessConn{Conn: conn, reader: bufio.NewReader(conn)}
	if network == "udp" {
		return &vlessPacketConn{vlessConn: c}, nil
	}
	return c, nil
}

func vlessRequest(user [16]byte, network, address string) ([]byte, error) {
	command := map[string]byte{"tcp": 1, "udp": 2}[network]
	if command == 0 {
		return nil, fmt.Errorf("probe network %q is not tcp or udp", network)
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("probe port %q: %w", portText, err)
	}
	b := append([]byte{0}, user[:]...)
	b = append(b, 0, command)
	b = binary.BigEndian.AppendUint16(b, uint16(port))
	switch addr, err := netip.ParseAddr(host); {
	case err == nil && addr.Is4():
		b = append(append(b, 1), addr.AsSlice()...)
	case err == nil:
		b = append(append(b, 3), addr.AsSlice()...)
	case len(host) == 0 || len(host) > 255:
		return nil, fmt.Errorf("probe host %q", host)
	default:
		b = append(append(b, 2, byte(len(host))), host...)
	}
	return b, nil
}

// vlessConn strips the response header (version, addon length, addons) before
// the first byte of payload.
type vlessConn struct {
	net.Conn
	reader *bufio.Reader
	once   sync.Once
	err    error
}

func (c *vlessConn) readHeader() error {
	c.once.Do(func() {
		var h [2]byte
		if _, err := io.ReadFull(c.reader, h[:]); err != nil {
			c.err = err
			return
		}
		if h[0] != 0 {
			c.err = errors.New("vless: unexpected response version")
			return
		}
		_, c.err = c.reader.Discard(int(h[1]))
	})
	return c.err
}

func (c *vlessConn) Read(p []byte) (int, error) {
	if err := c.readHeader(); err != nil {
		return 0, err
	}
	return c.reader.Read(p)
}

type vlessPacketConn struct{ *vlessConn }

func (c *vlessPacketConn) Write(p []byte) (int, error) {
	if len(p) > 0xffff {
		return 0, errors.New("vless: datagram too large")
	}
	if _, err := c.Conn.Write(binary.BigEndian.AppendUint16(nil, uint16(len(p)))); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

func (c *vlessPacketConn) Read(p []byte) (int, error) {
	if err := c.readHeader(); err != nil {
		return 0, err
	}
	var l [2]byte
	if _, err := io.ReadFull(c.reader, l[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(l[:]))
	if n > len(p) {
		return 0, io.ErrShortBuffer
	}
	return io.ReadFull(c.reader, p[:n])
}
