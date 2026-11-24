package runtime

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
)

func TestVLESSRequestHeader(t *testing.T) {
	var user [16]byte
	user[0], user[15] = 0xaa, 0xbb
	got, err := vlessRequest(user, "tcp", "203.0.113.80:80")
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{0}, user[:]...), 0, 1, 0, 80, 1, 203, 0, 113, 80)
	if !bytes.Equal(got, want) {
		t.Fatalf("header % x\nwant   % x", got, want)
	}
	got, err = vlessRequest(user, "udp", "example.com:53")
	if err != nil {
		t.Fatal(err)
	}
	if tail := got[17:]; !bytes.Equal(tail, append([]byte{0, 2, 0, 53, 2, 11}, "example.com"...)) {
		t.Fatalf("domain request % x", tail)
	}
	if _, err := vlessRequest(user, "icmp", "1.2.3.4:0"); err == nil {
		t.Error("icmp accepted")
	}
}

func TestVLESSResponseHeaderIsStrippedLazily(t *testing.T) {
	client, server := net.Pipe()
	go func() {
		server.Write([]byte{0, 2, 'x', 'y'})
		server.Write([]byte("payload"))
		server.Close()
	}()
	c := &vlessConn{Conn: client}
	c.reader = bufio.NewReader(client)
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Fatalf("read %q", got)
	}
}

func TestVLESSDatagramsAreLengthFramed(t *testing.T) {
	client, server := net.Pipe()
	c := &vlessPacketConn{&vlessConn{Conn: client}}
	c.reader = bufio.NewReader(client)
	go func() {
		buf := make([]byte, 7)
		io.ReadFull(server, buf)
		server.Write([]byte{0, 0, 0, 3, 'a', 'b', 'c'})
	}()
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "abc" {
		t.Fatalf("read %q, %v", buf[:n], err)
	}
}

func TestDialUDPFramesADatagramThroughTheInbound(t *testing.T) {
	inbound, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inbound.Close()
	heard := make(chan []byte, 1)
	go func() {
		conn, err := inbound.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 26+2+5)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		heard <- buf
		conn.Write([]byte{0, 0, 0, 2, 'o', 'k'})
	}()
	route := ProbeRoute{inbound: netip.MustParseAddrPort(inbound.Addr().String())}
	route.user[0] = 0xaa
	conn, err := route.Dial(context.Background(), "udp", "8.8.8.8:53")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	got := <-heard
	if want := append(append([]byte{0}, route.user[:]...), 0, 2, 0, 53, 1, 8, 8, 8, 8, 0, 5, 'h', 'e', 'l', 'l', 'o'); !bytes.Equal(got[:len(want)], want) {
		t.Fatalf("sent % x\nwant % x", got, want)
	}
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "ok" {
		t.Fatalf("read %q, %v", buf[:n], err)
	}
}
