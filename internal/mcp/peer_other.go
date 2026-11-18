//go:build !darwin && !linux

package mcp

import (
	"errors"
	"net"
)

// peerUID can't be told here, so the hook never trusts a listener and every
// prompt shows in its tab.
func peerUID(net.Conn) (uint32, error) {
	return 0, errors.New("peer credentials aren't available on this system")
}
