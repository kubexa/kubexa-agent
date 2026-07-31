package stream

import (
	"context"
	"testing"
	"time"

	"github.com/kubexa/kubexa-agent/internal/logger"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

type fakeResponder struct {
	got    chan *agentv1.ResourceQuery
	result *agentv1.ResourceQueryResult
}

func (f *fakeResponder) Execute(_ context.Context, q *agentv1.ResourceQuery) *agentv1.ResourceQueryResult {
	f.got <- q
	if f.result != nil {
		return f.result
	}
	return &agentv1.ResourceQueryResult{QueryId: q.GetQueryId()}
}

func TestHandleGatewayMessageDispatchesResourceQuery(t *testing.T) {
	resp := &fakeResponder{got: make(chan *agentv1.ResourceQuery, 1)}
	m := newTestManagerWithResponder(t, resp)

	m.handleGatewayMessage(context.Background(), &agentv1.GatewayMessage{
		Payload: &agentv1.GatewayMessage_ResourceQuery{
			ResourceQuery: &agentv1.ResourceQuery{
				QueryId: "q1",
				Ref:     &agentv1.ResourceRef{Version: "v1", Resource: "pods"},
				Verb:    agentv1.QueryVerb_QUERY_VERB_LIST,
			},
		},
	})

	select {
	case got := <-resp.got:
		if got.GetQueryId() != "q1" {
			t.Fatalf("query_id = %q, want q1", got.GetQueryId())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the responder was never called")
	}
}

func TestResourceQueryResultIsSentBack(t *testing.T) {
	resp := &fakeResponder{
		got:    make(chan *agentv1.ResourceQuery, 1),
		result: &agentv1.ResourceQueryResult{QueryId: "q2", Payload: []byte("[]")},
	}
	m := newTestManagerWithResponder(t, resp)

	m.handleGatewayMessage(context.Background(), &agentv1.GatewayMessage{
		Payload: &agentv1.GatewayMessage_ResourceQuery{
			ResourceQuery: &agentv1.ResourceQuery{QueryId: "q2"},
		},
	})

	sent := waitForSentMessage(t, m)
	p, ok := sent.Payload.(*agentv1.AgentMessage_ResourceQueryResult)
	if !ok {
		t.Fatalf("sent payload = %T, want *AgentMessage_ResourceQueryResult", sent.Payload)
	}
	if p.ResourceQueryResult.GetQueryId() != "q2" {
		t.Errorf("query_id = %q, want q2", p.ResourceQueryResult.GetQueryId())
	}
}

func TestQueryDoesNotBlockTheRecvLoop(t *testing.T) {
	// A query that never returns must not stop the next gateway message from
	// being handled. Executing inline would stall acks, backpressure and
	// shutdown for up to the query timeout.
	block := make(chan struct{})
	slow := &blockingResponder{release: block}
	m := newTestManagerWithResponder(t, slow)

	done := make(chan struct{})
	go func() {
		m.handleGatewayMessage(context.Background(), &agentv1.GatewayMessage{
			Payload: &agentv1.GatewayMessage_ResourceQuery{
				ResourceQuery: &agentv1.ResourceQuery{QueryId: "slow"},
			},
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(block)
		t.Fatal("handleGatewayMessage blocked on the query; it must dispatch asynchronously")
	}
	close(block)
}

func TestNilResponderIsIgnored(t *testing.T) {
	m := newTestManagerWithResponder(t, nil)
	// Must not panic.
	m.handleGatewayMessage(context.Background(), &agentv1.GatewayMessage{
		Payload: &agentv1.GatewayMessage_ResourceQuery{
			ResourceQuery: &agentv1.ResourceQuery{QueryId: "q3"},
		},
	})
}

type blockingResponder struct{ release chan struct{} }

func (b *blockingResponder) Execute(_ context.Context, q *agentv1.ResourceQuery) *agentv1.ResourceQueryResult {
	<-b.release
	return &agentv1.ResourceQueryResult{QueryId: q.GetQueryId()}
}

// newTestManagerWithResponder builds the minimal streamManager needed to
// exercise handleGatewayMessage's resource-query case, mirroring
// newConfigTestManager in manager_config_test.go.
//
// ready must be set: Send falls through to bufferMessage when it is false,
// and bufferMessage needs a queue this manager deliberately does not have.
func newTestManagerWithResponder(t *testing.T, resp QueryResponder) *streamManager {
	t.Helper()
	m := &streamManager{
		log:       logger.New("stream-query-test"),
		responder: resp,
		sendCh:    make(chan *agentv1.AgentMessage, 4),
	}
	m.ready.Store(true)
	return m
}

func waitForSentMessage(t *testing.T, m *streamManager) *agentv1.AgentMessage {
	t.Helper()
	select {
	case msg := <-m.sendCh:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("no AgentMessage was enqueued")
		return nil
	}
}
