package stream

import (
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxConsecutiveTransientFailures = 10

// permanentGatewayError indicates the gateway rejected the agent in a non-recoverable way.
type permanentGatewayError struct {
	code    codes.Code
	message string
}

func (e *permanentGatewayError) Error() string {
	if e == nil {
		return "permanent gateway error"
	}
	if e.message != "" {
		return e.message
	}
	return fmt.Sprintf("permanent gateway error: %s", e.code.String())
}

// circuitBreaker tracks consecutive gateway failures for backoff and shutdown decisions.
type circuitBreaker struct {
	consecutiveTransient int
}

func (cb *circuitBreaker) reset() {
	if cb == nil {
		return
	}
	cb.consecutiveTransient = 0
}

func (cb *circuitBreaker) recordSuccess() {
	cb.reset()
}

func (cb *circuitBreaker) recordTransient() int {
	if cb == nil {
		return 0
	}
	cb.consecutiveTransient++
	return cb.consecutiveTransient
}

func classifyGRPCError(err error) (permanent bool, transient bool) {
	if err == nil {
		return false, false
	}
	var perm *permanentGatewayError
	if errors.As(err, &perm) {
		return true, false
	}
	st, ok := status.FromError(err)
	if !ok {
		return false, true
	}
	switch st.Code() { //nolint:exhaustive
	case codes.Unauthenticated, codes.PermissionDenied, codes.Unimplemented:
		return true, false
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		return false, true
	default:
		return false, false
	}
}

func permanentFromCode(code codes.Code, detail string) error {
	msg := fmt.Sprintf("gateway rejected connection: %s", code.String())
	if detail != "" {
		msg = fmt.Sprintf("%s: %s", msg, detail)
	}
	return &permanentGatewayError{code: code, message: msg}
}

func handshakeRejected(reason string) error {
	msg := "gateway rejected handshake"
	if reason != "" {
		msg = fmt.Sprintf("%s: %s", msg, reason)
	}
	return &permanentGatewayError{code: codes.PermissionDenied, message: msg}
}
