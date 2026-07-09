package stream

import (
	"errors"
	"testing"
)

func TestHandshakeRejectedInvalidTenantTokenIsRecoverable(t *testing.T) {
	t.Parallel()

	err := handshakeRejected(recoverableRejectionInvalidTenantToken)
	var recoverable *recoverableHandshakeError
	if !errors.As(err, &recoverable) {
		t.Fatalf("error type = %T, want *recoverableHandshakeError", err)
	}

	permanent, transient := classifyGRPCError(err)
	if permanent || !transient {
		t.Fatalf("classify = permanent:%v transient:%v, want false/true", permanent, transient)
	}
}

func TestHandshakeRejectedOtherReasonIsPermanent(t *testing.T) {
	t.Parallel()

	err := handshakeRejected("cluster not registered")
	var perm *permanentGatewayError
	if !errors.As(err, &perm) {
		t.Fatalf("error type = %T, want *permanentGatewayError", err)
	}

	permanent, transient := classifyGRPCError(err)
	if !permanent || transient {
		t.Fatalf("classify = permanent:%v transient:%v, want true/false", permanent, transient)
	}
}
