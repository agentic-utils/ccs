package mcp

import "github.com/agentic-utils/ccs/internal/proc"

// The socket checks live in proc, shared with internal/profiling.
var (
	ownerOf       = proc.OwnerOf
	peerUID       = proc.PeerUID
	trustedSocket = proc.TrustedSocket
)
