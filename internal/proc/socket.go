package proc

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

var errNotUnix = errors.New("not a unix socket")

// OwnerOf is a file's owner's uid; MaxUint32 when it can't be told.
func OwnerOf(fi os.FileInfo) uint32 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Uid
	}
	return ^uint32(0)
}

// TrustedDir refuses a folder that isn't this user's own: a real directory
// (not a symlink) owned by this user and writable by no one else.
func TrustedDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm()&0o022 != 0 || OwnerOf(fi) != uint32(os.Getuid()) {
		return fmt.Errorf("%s isn't a private folder of this user's", dir)
	}
	return nil
}

// TrustedSocket refuses a socket that isn't this user's own: its folder
// must pass TrustedDir, and the socket be a real socket owned by this user
// that only they can use. Other macOS users are what this keeps out; a
// process running as this user could change Claude Code's settings or run
// claude itself, so it's beyond what any check here could stop
// (docs/remote.md).
func TrustedSocket(path string) error {
	if err := TrustedDir(filepath.Dir(path)); err != nil {
		return err
	}
	sock, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if sock.Mode()&os.ModeSocket == 0 || sock.Mode().Perm()&0o077 != 0 || OwnerOf(sock) != uint32(os.Getuid()) {
		return fmt.Errorf("%s isn't this user's private socket", path)
	}
	return nil
}

// SameUser says the other end of c runs as this user, as the kernel tells
// it (PeerUID); false when it can't be told.
func SameUser(c net.Conn) bool {
	uid, err := PeerUID(c)
	return err == nil && uid == uint32(os.Getuid())
}
