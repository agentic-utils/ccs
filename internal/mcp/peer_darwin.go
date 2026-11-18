package mcp

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID is the uid of the process on the other end of a unix socket, as
// the kernel tells it (LOCAL_PEERCRED): who is really listening or
// connecting, whatever the socket file looked like.
func peerUID(c net.Conn) (uint32, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errNotUnix
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var serr error
	if err := raw.Control(func(fd uintptr) {
		var cred *unix.Xucred
		cred, serr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if serr == nil {
			uid = cred.Uid
		}
	}); err != nil {
		return 0, err
	}
	return uid, serr
}
