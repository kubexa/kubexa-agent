// Package devbackend provides a minimal AgentService gRPC server for local agent testing.
package devbackend

import (
	"io"
	"strings"

	"github.com/rs/zerolog/log"

	agentv1 "github.com/kubexa/kubexa-agent/gen/agent/v1"
)

// Server logs incoming pod events and log chunks and responds with Ack.
type Server struct {
	agentv1.UnimplementedAgentServiceServer
	// AgentControlDemo: after agent connects, send sample ListNamespaces + ListPods probes (local testing).
	AgentControlDemo bool
	// DemoPodNamespace: preferred namespace for demo ListPods (empty = pick automatically from ListNamespaces).
	DemoPodNamespace string
}

// StreamPodEvents receives client-streamed pod events and prints them.
func (s *Server) StreamPodEvents(stream agentv1.AgentService_StreamPodEventsServer) error {
	n := 0
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			log.Info().Int("count", n).Msg("StreamPodEvents: client closed send; acking")
			return stream.SendAndClose(&agentv1.Ack{Ok: true, Message: "ok"})
		}
		if err != nil {
			return err
		}
		n++
		log.Info().
			Str("rpc", "StreamPodEvents").
			Int("seq", n).
			Str("cluster_id", ev.GetClusterId()).
			Str("namespace", ev.GetNamespace()).
			Str("pod", ev.GetPodName()).
			Str("phase", ev.GetPhase()).
			Str("previous_phase", ev.GetPreviousPhase()).
			Str("event_type", ev.GetEventType()).
			Str("resource_version", ev.GetResourceVersion()).
			Str("node_name", ev.GetNodeName()).
			Strs("change_hints", ev.GetChangeHints()).
			Int64("ts_ms", ev.GetTimestamp()).
			Int("containers", len(ev.GetContainers())).
			Msg("received pod event")
	}
}

// StreamLogs receives client-streamed log lines and prints them.
func (s *Server) StreamLogs(stream agentv1.AgentService_StreamLogsServer) error {
	n := 0
	for {
		ch, err := stream.Recv()
		if err == io.EOF {
			log.Info().Int("count", n).Msg("StreamLogs: client closed send; acking")
			return stream.SendAndClose(&agentv1.Ack{Ok: true, Message: "ok"})
		}
		if err != nil {
			return err
		}
		n++
		line := ch.GetLine()
		if len(line) > 2048 {
			line = line[:2048] + "…(truncated)"
		}
		log.Info().
			Str("rpc", "StreamLogs").
			Int("seq", n).
			Str("cluster_id", ch.GetClusterId()).
			Str("namespace", ch.GetNamespace()).
			Str("pod", ch.GetPodName()).
			Str("container", ch.GetContainer()).
			Int64("ts_ms", ch.GetTimestamp()).
			Str("line", line).
			Msg("received log chunk")
	}
}

// WatchConfig stub: logs the request, pushes one empty config, then waits until the client disconnects.
func (s *Server) WatchConfig(req *agentv1.ConfigRequest, stream agentv1.AgentService_WatchConfigServer) error {
	log.Info().
		Str("rpc", "WatchConfig").
		Str("cluster_id", req.GetClusterId()).
		Str("agent_version", req.GetAgentVersion()).
		Msg("WatchConfig: sending empty AgentConfig (dev)")

	if err := stream.Send(&agentv1.AgentConfig{}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

// AgentControl handles backend→agent queries over the bidirectional stream (agent is the gRPC client).
// grpc-go does not allow concurrent Recv and Send on the same server stream; keep all stream I/O on this goroutine.
func (s *Server) AgentControl(stream agentv1.AgentService_AgentControlServer) error {
	log.Info().Msg("AgentControl: stream opened")
	defer log.Info().Msg("AgentControl: stream closed")

	if s.AgentControlDemo {
		if err := stream.Send(&agentv1.AgentControlMessage{
			RequestId: "demo-list-namespaces",
			Msg: &agentv1.AgentControlMessage_ListNamespacesReq{
				ListNamespacesReq: &agentv1.ListNamespacesRequest{},
			},
		}); err != nil {
			return err
		}
	}

	demoPodsSent := false

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch p := msg.GetMsg().(type) {
		case *agentv1.AgentControlMessage_ListNamespacesResp:
			resp := p.ListNamespacesResp
			log.Info().
				Str("request_id", msg.GetRequestId()).
				Strs("namespaces", resp.GetNamespaces()).
				Str("error", resp.GetError()).
				Msg("AgentControl: ListNamespaces response")

			if s.AgentControlDemo && !demoPodsSent && resp.GetError() == "" {
				demoPodsSent = true
				ns := pickDemoPodNamespace(resp.GetNamespaces(), s.DemoPodNamespace)
				log.Info().
					Str("chosen_namespace", ns).
					Str("preferred", s.DemoPodNamespace).
					Msg("AgentControl demo: sending ListPods for namespace")
				if err := stream.Send(&agentv1.AgentControlMessage{
					RequestId: "demo-list-pods",
					Msg: &agentv1.AgentControlMessage_ListPodsReq{
						ListPodsReq: &agentv1.ListPodsRequest{Namespace: ns},
					},
				}); err != nil {
					return err
				}
			}
		case *agentv1.AgentControlMessage_ListPodsResp:
			resp := p.ListPodsResp
			pods := resp.GetPods()
			const maxNamesInLog = 100
			names := make([]string, 0, len(pods))
			for i, pod := range pods {
				if i >= maxNamesInLog {
					break
				}
				names = append(names, pod.GetName())
			}
			evt := log.Info().
				Str("request_id", msg.GetRequestId()).
				Int("pod_count", len(pods)).
				Strs("pod_names", names).
				Str("error", resp.GetError())
			omitted := len(pods) - len(names)
			if omitted > 0 {
				evt = evt.Int("pod_names_omitted_from_log", omitted)
			}
			evt.Msg("AgentControl: ListPods response (full list over gRPC; log truncates names at 100)")
		default:
			log.Debug().
				Str("request_id", msg.GetRequestId()).
				Msg("AgentControl: received non-response message on server recv (ignored)")
		}
	}
}

// pickDemoPodNamespace chooses a namespace that exists in the cluster for a meaningful ListPods demo.
func pickDemoPodNamespace(names []string, preferred string) string {
	if preferred != "" {
		for _, n := range names {
			if n == preferred {
				return preferred
			}
		}
	}
	for _, n := range names {
		if !strings.HasPrefix(n, "kube-") {
			return n
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return "default"
}
