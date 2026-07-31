package query

import (
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// TestQueryMessagesRoundTripOnTheStream pins the two new oneof arms. A field
// number collision would silently reinterpret an existing payload, so this
// asserts that an encoded query survives a decode as itself and that the
// neighbouring arms still decode as themselves.
func TestQueryMessagesRoundTripOnTheStream(t *testing.T) {
	q := &agentv1.GatewayMessage{
		MessageId: "m1",
		Payload: &agentv1.GatewayMessage_ResourceQuery{
			ResourceQuery: &agentv1.ResourceQuery{
				QueryId:   "q1",
				Ref:       &agentv1.ResourceRef{Group: "apps", Version: "v1", Resource: "deployments"},
				Verb:      agentv1.QueryVerb_QUERY_VERB_LIST,
				View:      agentv1.QueryView_QUERY_VIEW_TABLE,
				Namespace: "stage",
				Limit:     100,
				TimeoutMs: 5000,
			},
		},
	}
	raw, err := proto.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got agentv1.GatewayMessage
	if err := proto.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p, ok := got.Payload.(*agentv1.GatewayMessage_ResourceQuery)
	if !ok {
		t.Fatalf("payload = %T, want *GatewayMessage_ResourceQuery", got.Payload)
	}
	if p.ResourceQuery.GetQueryId() != "q1" {
		t.Errorf("query_id = %q, want q1", p.ResourceQuery.GetQueryId())
	}
	if p.ResourceQuery.GetRef().GetResource() != "deployments" {
		t.Errorf("resource = %q, want deployments", p.ResourceQuery.GetRef().GetResource())
	}

	// A Shutdown on the same oneof must still decode as a Shutdown.
	sd := &agentv1.GatewayMessage{Payload: &agentv1.GatewayMessage_Shutdown{
		Shutdown: &agentv1.Shutdown{Reason: "bye"},
	}}
	rawSd, err := proto.Marshal(sd)
	if err != nil {
		t.Fatalf("marshal shutdown: %v", err)
	}
	var gotSd agentv1.GatewayMessage
	if err := proto.Unmarshal(rawSd, &gotSd); err != nil {
		t.Fatalf("unmarshal shutdown: %v", err)
	}
	if _, ok := gotSd.Payload.(*agentv1.GatewayMessage_Shutdown); !ok {
		t.Fatalf("shutdown payload = %T, want *GatewayMessage_Shutdown", gotSd.Payload)
	}
}

func TestQueryResultRoundTripsOnTheAgentMessage(t *testing.T) {
	r := &agentv1.AgentMessage{
		MessageId: "m2",
		Payload: &agentv1.AgentMessage_ResourceQueryResult{
			ResourceQueryResult: &agentv1.ResourceQueryResult{
				QueryId: "q1",
				Payload: []byte(`{"kind":"Table"}`),
				Error: &agentv1.QueryError{
					Code:    agentv1.QueryErrorCode_QUERY_ERROR_POLICY_DENIED,
					Message: "denied",
				},
			},
		},
	}
	raw, err := proto.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got agentv1.AgentMessage
	if err := proto.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p, ok := got.Payload.(*agentv1.AgentMessage_ResourceQueryResult)
	if !ok {
		t.Fatalf("payload = %T, want *AgentMessage_ResourceQueryResult", got.Payload)
	}
	if p.ResourceQueryResult.GetError().GetCode() != agentv1.QueryErrorCode_QUERY_ERROR_POLICY_DENIED {
		t.Errorf("code = %v, want POLICY_DENIED", p.ResourceQueryResult.GetError().GetCode())
	}

	// The catalog arm shares this oneof and must still decode as itself.
	cat := &agentv1.AgentMessage{Payload: &agentv1.AgentMessage_Catalog{
		Catalog: &agentv1.ResourceCatalog{Fingerprint: "fp"},
	}}
	rawCat, err := proto.Marshal(cat)
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	var gotCat agentv1.AgentMessage
	if err := proto.Unmarshal(rawCat, &gotCat); err != nil {
		t.Fatalf("unmarshal catalog: %v", err)
	}
	if _, ok := gotCat.Payload.(*agentv1.AgentMessage_Catalog); !ok {
		t.Fatalf("catalog payload = %T, want *AgentMessage_Catalog", gotCat.Payload)
	}
}
