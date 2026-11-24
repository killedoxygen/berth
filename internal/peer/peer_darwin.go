//go:build darwin

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
	var inner error
	err = raw.Control(func(fd uintptr) {
		pid, inner = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if inner != nil {
			return
		}
		var x *unix.Xucred
		x, inner = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if inner == nil {
			uid = int(x.Uid)
		}
	})
	if err == nil {
		err = inner
	}
	return pid, uid, err
}
