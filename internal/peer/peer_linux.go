//go:build linux

// Package peer identifies the process on the other end of a Unix socket.
package peer

import (
	"net"

	"golang.org/x/sys/unix"
)

// Cred returns the peer's pid and uid.
func Cred(c *net.UnixConn) (pid, uid int, err error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *unix.Ucred
	var inner error
	err = raw.Control(func(fd uintptr) {
		cred, inner = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err == nil {
		err = inner
	}
	if err != nil {
		return 0, 0, err
	}
	return int(cred.Pid), int(cred.Uid), nil
}
