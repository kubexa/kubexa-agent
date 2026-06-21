package stream

import (
	"fmt"
)

// ConnState is the gRPC stream manager connection state.
type ConnState int

const (
	// StateIdle is the initial state before the first connection attempt.
	StateIdle ConnState = iota
	// StateConnecting is dialing the gateway.
	StateConnecting
	// StateHandshaking is waiting for HandshakeResponse on the stream.
	StateHandshaking
	// StateReady indicates an active bidirectional stream after a successful handshake.
	StateReady
	// StateTransientFailure indicates a recoverable connection or stream error.
	StateTransientFailure
	// StateShutdown is terminal; Run returns after entering this state.
	StateShutdown
)

// String returns the canonical state name.
func (s ConnState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateConnecting:
		return "connecting"
	case StateHandshaking:
		return "handshaking"
	case StateReady:
		return "ready"
	case StateTransientFailure:
		return "transient_failure"
	case StateShutdown:
		return "shutdown"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}
