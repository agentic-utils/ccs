package proc

import (
	"net"

	"golang.org/x/sys/unix"
)

// PeerUID is the uid of the process on the other end of a unix socket, as
// the kernel tells it (SO_PEERCRED).
func PeerUID(c net.Conn) (uint32, error) {
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
		var cred *unix.Ucred
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if serr == nil {
			uid = cred.Uid
		}
	}); err != nil {
		return 0, err
	}
	return uid, serr
}
