// Command dev-grpc-server is a local Kubexa gateway mock for agent development.
// It accepts the AgentService.Connect bidirectional stream and prints incoming
// messages to stdout.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	"github.com/kubexa/kubexa-agent/pkg/protoversion"
)

const serverVersion = "dev-grpc-server"

func main() {
	listen := flag.String("listen", "127.0.0.1:50051", "gRPC listen address")
	acceptToken := flag.String("accept-token", "", "if set, reject handshakes whose tenant_token does not match")
	flag.Parse()

	gw := &devGateway{acceptToken: strings.TrimSpace(*acceptToken)}

	srv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(srv, gw)

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen %q: %v", *listen, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Println("shutting down gRPC server...")
		srv.GracefulStop()
	}()

	log.Printf("demo gateway listening on %s (insecure gRPC)", *listen)
	log.Printf("start agent: make run-dev  (config gateway.address=%s)", *listen)
	if err := srv.Serve(lis); err != nil && ctx.Err() == nil {
		log.Fatalf("serve: %v", err)
	}
}

// devGateway implements agent.v1.AgentService for local testing.
type devGateway struct {
	agentv1.UnimplementedAgentServiceServer
	acceptToken string
}

func (g *devGateway) Connect(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
	ctx := stream.Context()
	peer := peerAddr(ctx)
	md := incomingMD(ctx)

	log.Printf("→ stream opened peer=%s", peer)
	if len(md) > 0 {
		log.Printf("  metadata: %s", formatMD(md))
	}

	msg, err := stream.Recv()
	if err != nil {
		return err
	}
	if msg.GetHandshake() == nil {
		return status.Error(codes.InvalidArgument, "first message must be handshake")
	}

	hs := msg.GetHandshake()
	sessionID, rejectReason := g.validateHandshake(hs, md)
	if rejectReason != "" {
		log.Printf("✗ handshake rejected: %s", rejectReason)
		resp := &agentv1.GatewayMessage{
			MessageId: uuid.NewString(),
			Payload: &agentv1.GatewayMessage_Handshake{
				Handshake: &agentv1.HandshakeResponse{
					Accepted:         false,
					RejectionReason:    rejectReason,
					ServerVersion:      serverVersion,
				},
			},
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
		return status.Error(codes.PermissionDenied, rejectReason)
	}

	negotiated, err := protoversion.ValidateAgentRequest(hs.GetProtoVersion(), hs.GetSupportedProtoVersions())
	if err != nil {
		rejectReason := err.Error()
		log.Printf("✗ handshake rejected: %s", rejectReason)
		resp := &agentv1.GatewayMessage{
			MessageId: uuid.NewString(),
			Payload: &agentv1.GatewayMessage_Handshake{
				Handshake: &agentv1.HandshakeResponse{
					Accepted:        false,
					RejectionReason: rejectReason,
					ServerVersion:   serverVersion,
				},
			},
		}
		if sendErr := stream.Send(resp); sendErr != nil {
			return sendErr
		}
		return status.Error(codes.FailedPrecondition, rejectReason)
	}

	log.Printf("✓ handshake accepted session=%s cluster=%q agent_version=%q proto_version=%q caps={logs:%v state:%v metrics:%v}",
		sessionID,
		hs.GetClusterId(),
		hs.GetAgentVersion(),
		negotiated,
		hs.GetCaps().GetLogs(),
		hs.GetCaps().GetState(),
		hs.GetCaps().GetMetrics(),
	)

	if err := stream.Send(&agentv1.GatewayMessage{
		MessageId: uuid.NewString(),
		Payload: &agentv1.GatewayMessage_Handshake{
			Handshake: &agentv1.HandshakeResponse{
				Accepted:               true,
				SessionId:              sessionID,
				ServerVersion:          serverVersion,
				ProtoVersion:           negotiated,
				SupportedProtoVersions: protoversion.Supported,
			},
		},
	}); err != nil {
		return err
	}

	printAgentMessage(msg, sessionID)

	for {
		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				log.Printf("← stream closed peer=%s", peer)
				return nil
			}
			return err
		}
		printAgentMessage(msg, sessionID)
	}
}

func (g *devGateway) validateHandshake(hs *agentv1.HandshakeRequest, md metadata.MD) (sessionID, rejectReason string) {
	token := strings.TrimSpace(hs.GetTenantToken())
	if token == "" {
		token = firstMD(md, "x-tenant-token")
	}
	if g.acceptToken != "" && token != g.acceptToken {
		return "", fmt.Sprintf("unexpected tenant_token (want %q)", g.acceptToken)
	}
	if token == "" {
		return "", "missing tenant_token"
	}
	return uuid.NewString(), ""
}

func printAgentMessage(msg *agentv1.AgentMessage, sessionID string) {
	if msg == nil {
		return
	}

	base := map[string]any{
		"received_at": time.Now().UTC().Format(time.RFC3339Nano),
		"message_id":  msg.GetMessageId(),
		"session_id":  sessionID,
	}
	if meta := msg.GetMeta(); meta != nil {
		base["agent_id"] = meta.GetAgentId()
		base["cluster_id"] = meta.GetClusterId()
		if meta.GetTenantId() != "" {
			base["tenant_id"] = meta.GetTenantId()
		}
		if meta.GetTimestamp() != 0 {
			base["meta_timestamp_ms"] = meta.GetTimestamp()
		}
	}

	switch p := msg.Payload.(type) {
	case *agentv1.AgentMessage_Handshake:
		hs := p.Handshake
		event := cloneMap(base)
		event["type"] = "handshake"
		event["agent_version"] = hs.GetAgentVersion()
		event["proto_version"] = hs.GetProtoVersion()
		event["cluster_id"] = hs.GetClusterId()
		event["tenant_token"] = redactToken(hs.GetTenantToken())
		event["capabilities"] = map[string]bool{
			"logs":    hs.GetCaps().GetLogs(),
			"state":   hs.GetCaps().GetState(),
			"metrics": hs.GetCaps().GetMetrics(),
		}
		printJSONLine(event)
	case *agentv1.AgentMessage_Logs:
		for _, e := range p.Logs.GetEntries() {
			event := cloneMap(base)
			event["type"] = "log"
			event["namespace"] = e.GetNamespace()
			event["pod"] = e.GetPodName()
			event["container"] = e.GetContainer()
			event["level"] = logLevelName(e.GetLevel())
			event["timestamp"] = time.Unix(0, e.GetTimestamp()).UTC().Format(time.RFC3339Nano)
			if labels := e.GetLabels(); len(labels) > 0 {
				event["labels"] = labels
			}
			if body := parseLogBody(e.GetMessage(), e.GetRaw()); body != nil {
				event["body"] = body
			}
			printJSONLine(event)
		}
	case *agentv1.AgentMessage_State:
		ev := p.State
		event := cloneMap(base)
		event["type"] = "state"
		event["event_type"] = strings.TrimPrefix(ev.GetType().String(), "EVENT_TYPE_")
		event["kind"] = strings.TrimPrefix(ev.GetKind().String(), "RESOURCE_KIND_")
		event["namespace"] = ev.GetNamespace()
		event["name"] = ev.GetName()
		event["timestamp"] = time.Unix(0, ev.GetTimestamp()).UTC().Format(time.RFC3339Nano)
		if labels := ev.GetLabels(); len(labels) > 0 {
			event["labels"] = labels
		}
		if raw := ev.GetObject(); len(raw) > 0 {
			event["object"] = parseJSONValue(raw)
		}
		printJSONLine(event)
	case *agentv1.AgentMessage_KubeMetrics:
		ev := p.KubeMetrics
		event := cloneMap(base)
		event["type"] = "kube_metrics"
		event["resource_type"] = strings.TrimPrefix(ev.GetResourceType().String(), "KUBE_METRICS_RESOURCE_TYPE_")
		event["namespace"] = ev.GetNamespace()
		event["name"] = ev.GetName()
		event["cpu_nanocores"] = ev.GetCpuNanocores()
		event["memory_bytes"] = ev.GetMemoryBytes()
		event["window_seconds"] = ev.GetWindowSeconds()
		if ts := ev.GetTimestamp(); ts != 0 {
			event["timestamp"] = time.UnixMilli(ts).UTC().Format(time.RFC3339Nano)
			event["timestamp_ms"] = ts
		}
		printJSONLine(event)
	case *agentv1.AgentMessage_PrometheusMetrics:
		ev := p.PrometheusMetrics
		event := cloneMap(base)
		event["type"] = "prometheus_metrics"
		event["target_url"] = ev.GetTargetUrl()
		if ts := ev.GetScrapedAt(); ts != 0 {
			event["scraped_at"] = time.UnixMilli(ts).UTC().Format(time.RFC3339Nano)
			event["scraped_at_ms"] = ts
		}
		if raw := ev.GetMetricFamilyJson(); raw != "" {
			event["metric_family"] = parseJSONString(raw)
		}
		printJSONLine(event)
	case *agentv1.AgentMessage_Metrics:
		//nolint:staticcheck // legacy MetricBatch payload; dev server prints all wire formats
		printLegacyMetricBatch(p.Metrics, base)
	case *agentv1.AgentMessage_Heartbeat:
		h := p.Heartbeat
		health := h.GetHealth()
		event := cloneMap(base)
		event["type"] = "heartbeat"
		event["timestamp_ms"] = h.GetTimestampUnixMs()
		event["health"] = map[string]any{
			"status":           health.GetStatus(),
			"queue_depth":      health.GetQueueDepth(),
			"dropped_messages": health.GetDroppedMessages(),
		}
		printJSONLine(event)
	default:
		event := cloneMap(base)
		event["type"] = "unknown"
		printJSONLine(event)
	}
}

// printLegacyMetricBatch logs deprecated MetricBatch payloads for older agents.
//
//nolint:staticcheck // dev server intentionally handles legacy AgentMessage.Metrics wire format
func printLegacyMetricBatch(batch *agentv1.MetricBatch, base map[string]any) {
	for _, fam := range batch.GetFamilies() {
		for _, m := range fam.GetMetrics() {
			event := cloneMap(base)
			event["type"] = "metric"
			event["family"] = fam.GetName()
			event["help"] = fam.GetHelp()
			event["metric_type"] = strings.TrimPrefix(fam.GetType().String(), "METRIC_TYPE_")
			event["labels"] = m.GetLabels()
			event["value"] = m.GetValue()
			if m.GetTimestamp() != 0 {
				event["timestamp_ms"] = m.GetTimestamp()
			}
			printJSONLine(event)
		}
	}
}

func cloneMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func printJSONLine(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func logLevelName(level agentv1.LogLevel) string {
	name := strings.TrimPrefix(level.String(), "LOG_LEVEL_")
	if name == "UNSPECIFIED" {
		return "unspecified"
	}
	return strings.ToLower(name)
}

func parseLogBody(message string, raw []byte) any {
	if trimmed := strings.TrimSpace(message); trimmed != "" {
		if parsed := parseJSONString(trimmed); parsed != nil {
			return parsed
		}
		return trimmed
	}
	if len(raw) > 0 {
		if parsed := parseJSONValue(raw); parsed != nil {
			return parsed
		}
		return string(raw)
	}
	return nil
}

func parseJSONString(s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil
	}
	return v
}

func parseJSONValue(raw []byte) any {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

func peerAddr(ctx context.Context) string {
	if p, ok := peerFromContext(ctx); ok {
		return p
	}
	return "unknown"
}

func peerFromContext(ctx context.Context) (string, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return "", false
	}
	return p.Addr.String(), true
}

func incomingMD(ctx context.Context) metadata.MD {
	md, _ := metadata.FromIncomingContext(ctx)
	return md
}

func formatMD(md metadata.MD) string {
	var parts []string
	for k, vals := range md {
		key := strings.ToLower(k)
		for _, v := range vals {
			if key == "x-tenant-token" || key == "authorization" {
				v = redactToken(v)
			}
			parts = append(parts, fmt.Sprintf("%s=%q", k, v))
		}
	}
	return strings.Join(parts, " ")
}

func firstMD(md metadata.MD, key string) string {
	vals := md.Get(key)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

func redactToken(token string) string {
	if token == "" {
		return "(empty)"
	}
	if len(token) <= 8 {
		return "***"
	}
	return token[:4] + "…" + token[len(token)-4:]
}
