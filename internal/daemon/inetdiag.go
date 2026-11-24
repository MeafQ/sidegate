package daemon

import (
	"encoding/binary"
	"net/netip"
	"time"
)

const (
	afInet  uint8 = 2
	afInet6 uint8 = 10

	diagMsgHeader = 72 // struct inet_diag_msg.
	attrTCPInfo   = 2  // INET_DIAG_INFO.
	attrMark      = 15 // INET_DIAG_MARK, sent to a reader with CAP_NET_ADMIN.
)

// flowsOf picks the sockets a close may destroy: the core's own, with this slot's mark. The core's
// sockets to servers hold no mark, and they end when the core's side of each flow does.
func flowsOf(sockets []tcpSocket, mark uint32, core map[uint32]bool) []tcpSocket {
	var out []tcpSocket
	for _, s := range sockets {
		if s.Mark == mark && core[s.Inode] {
			out = append(out, s)
		}
	}
	return out
}

// parseDiagMessage reads one struct inet_diag_msg and its attributes. tcp_info grew over
// kernel versions, so a field the kernel didn't send stays nil rather than reads as zero.
func parseDiagMessage(msg []byte) (tcpSocket, bool) {
	if len(msg) < diagMsgHeader {
		return tcpSocket{}, false
	}
	family := msg[0]
	port := binary.BigEndian.Uint16(msg[6:8])
	var addr netip.Addr
	switch family {
	case afInet:
		addr = netip.AddrFrom4([4]byte(msg[24:28]))
	case afInet6:
		addr = netip.AddrFrom16([16]byte(msg[24:40])).Unmap()
	default:
		return tcpSocket{}, false
	}
	sock := tcpSocket{
		Remote: netip.AddrPortFrom(addr, port),
		UID:    binary.LittleEndian.Uint32(msg[64:68]),
		Inode:  binary.LittleEndian.Uint32(msg[68:72]),
		id:     [48]byte(msg[4:52]),
	}
	for rest := msg[diagMsgHeader:]; len(rest) >= 4; {
		length := int(binary.LittleEndian.Uint16(rest[:2]))
		kind := binary.LittleEndian.Uint16(rest[2:4])
		if length < 4 || length > len(rest) {
			break
		}
		switch {
		case kind == attrTCPInfo:
			parseTCPInfo(rest[4:length], &sock)
		case kind == attrMark && length >= 8:
			sock.Mark = binary.LittleEndian.Uint32(rest[4:8])
		}
		rest = rest[min((length+3)&^3, len(rest)):]
	}
	return sock, true
}

// parseTCPInfo reads struct tcp_info by the offsets every kernel since 4.x agrees on.
func parseTCPInfo(info []byte, sock *tcpSocket) {
	u32 := func(off int) *uint32 {
		if off+4 > len(info) {
			return nil
		}
		v := binary.LittleEndian.Uint32(info[off:])
		return &v
	}
	u64 := func(off int) *uint64 {
		if off+8 > len(info) {
			return nil
		}
		v := binary.LittleEndian.Uint64(info[off:])
		return &v
	}
	micros := func(off int) *time.Duration {
		v := u32(off)
		if v == nil {
			return nil
		}
		d := time.Duration(*v) * time.Microsecond
		return &d
	}
	sock.Unacked = u32(24)
	if v := u32(56); v != nil {
		d := time.Duration(*v) * time.Millisecond
		sock.LastAckRecv = &d
	}
	sock.SRTT, sock.RTTVar = micros(68), micros(72)
	sock.TotalRetrans = u32(100)
	sock.BytesAcked = u64(120)
	sock.SegsOut = u32(136)
	sock.DeliveryRate = u64(160)
}
