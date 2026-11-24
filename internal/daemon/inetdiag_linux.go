package daemon

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const (
	netlinkSockDiag  = 4
	sockDiagByFamily = 20
	sockDestroy      = 21
	tcpEstablished   = 1
	tcpCloseWait     = 8
)

func openDiag() (int, error) {
	return syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, netlinkSockDiag)
}

// diagRequest is one struct nlmsghdr and struct inet_diag_req_v2, for TCP.
func diagRequest(kind, flags uint16, family, ext uint8, states uint32) []byte {
	req := make([]byte, 16+56)
	binary.LittleEndian.PutUint32(req[0:], uint32(len(req)))
	binary.LittleEndian.PutUint16(req[4:], kind)
	binary.LittleEndian.PutUint16(req[6:], flags)
	req[16], req[17], req[18] = family, syscall.IPPROTO_TCP, ext
	binary.LittleEndian.PutUint32(req[20:], states)
	return req
}

// dumpTCP asks inet_diag for established TCP sockets with tcp_info: one netlink dump
// per family, never a process per socket.
func dumpTCP(families []uint8) ([]tcpSocket, error) {
	fd, err := openDiag()
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	var out []tcpSocket
	for _, family := range families {
		sockets, err := dumpSockets(fd, family, 1<<(attrTCPInfo-1), 1<<tcpEstablished)
		if err != nil {
			return nil, err
		}
		out = append(out, sockets...)
	}
	return out, nil
}

func dumpSockets(fd int, family, ext uint8, states uint32) ([]tcpSocket, error) {
	req := diagRequest(sockDiagByFamily, syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP, family, ext, states)
	if err := syscall.Sendto(fd, req, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, err
	}
	var out []tcpSocket
	buf := make([]byte, 32<<10)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return nil, err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			switch m.Header.Type {
			case syscall.NLMSG_DONE:
				return out, nil
			case syscall.NLMSG_ERROR:
				return nil, fmt.Errorf("inet_diag: netlink error")
			case sockDiagByFamily:
				if sock, ok := parseDiagMessage(m.Data); ok {
					out = append(out, sock)
				}
			}
		}
	}
}

// closeFlows destroys the established TCP sockets the core holds with this mark. The client gets a
// RST, the core's read fails, and the core drops its upstream socket for that flow. A half-closed
// flow counts, since a download can outlive the client's FIN. Slot inbounds listen on IPv4 only.
func closeFlows(pid int, mark uint32) (int, error) {
	core, err := coreSockets(pid)
	if err != nil {
		return 0, err
	}
	fd, err := openDiag()
	if err != nil {
		return 0, err
	}
	defer syscall.Close(fd)
	sockets, err := dumpSockets(fd, afInet, 0, 1<<tcpEstablished|1<<tcpCloseWait)
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, s := range flowsOf(sockets, mark, core) {
		switch err := destroySocket(fd, s); {
		case err == nil:
			closed++
		case !errors.Is(err, syscall.ENOENT): // It closed on its own since the dump.
			return closed, err
		}
	}
	return closed, nil
}

func destroySocket(fd int, s tcpSocket) error {
	req := diagRequest(sockDestroy, syscall.NLM_F_REQUEST|syscall.NLM_F_ACK, afInet, 0, 0)
	copy(req[24:], s.id[:])
	if err := syscall.Sendto(fd, req, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	n, _, err := syscall.Recvfrom(fd, buf, 0)
	if err != nil {
		return err
	}
	msgs, err := syscall.ParseNetlinkMessage(buf[:n])
	if err != nil {
		return err
	}
	if len(msgs) != 1 || msgs[0].Header.Type != syscall.NLMSG_ERROR || len(msgs[0].Data) < 4 {
		return fmt.Errorf("inet_diag: unexpected answer to a destroy")
	}
	if code := int32(binary.LittleEndian.Uint32(msgs[0].Data)); code != 0 {
		return syscall.Errno(-code)
	}
	return nil
}

// coreSockets lists the socket inodes pid holds open.
func coreSockets(pid int) (map[uint32]bool, error) {
	dir := "/proc/" + strconv.Itoa(pid) + "/fd"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make(map[uint32]bool, len(entries))
	for _, e := range entries {
		link, err := os.Readlink(dir + "/" + e.Name())
		if err != nil {
			continue
		}
		if rest, ok := strings.CutPrefix(link, "socket:["); ok {
			if inode, err := strconv.ParseUint(strings.TrimSuffix(rest, "]"), 10, 32); err == nil {
				out[uint32(inode)] = true
			}
		}
	}
	return out, nil
}
