//go:build !linux

package daemon

import "errors"

// inet_diag exists only on the gateway. Elsewhere passive socket signals are absent,
// and absence is never a zero.
func dumpTCP([]uint8) ([]tcpSocket, error) { return nil, errors.New("inet_diag needs linux") }

func coreSockets(int) (map[uint32]bool, error) { return nil, errors.New("inet_diag needs linux") }

func closeFlows(int, uint32) (int, error) { return 0, errors.New("inet_diag needs linux") }
