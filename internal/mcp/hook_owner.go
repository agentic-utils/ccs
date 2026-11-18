package mcp

import (
	"errors"
	"os"
	"syscall"
)

var errNotUnix = errors.New("not a unix socket")

// ownerOf is a file's owner's uid; MaxUint32 when it can't be told.
func ownerOf(fi os.FileInfo) uint32 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Uid
	}
	return ^uint32(0)
}
