package grpc

import (
	"context"
	"io"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/kubexa/kubexa-agent/gen/agent/v1"
	"github.com/kubexa/kubexa-agent/internal/k8s"
)

// RunAgentControl keeps a bidirectional AgentControl stream to the backend and answers ListNamespaces / ListPods.
// If the server returns Unimplemented, it logs once and stops (no retry storm).
//
// Backend implementors: on a given AgentControl stream, do not call Send and Recv concurrently (grpc-go constraint).
func RunAgentControl(ctx context.Context, grpcClient *Client, k8sClient *k8s.Client) {
	go func() {
		backoff := time.Second
		const maxBackoff = 30 * time.Second
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			err := runAgentControlStream(ctx, grpcClient, k8sClient)
			if err == nil {
				backoff = time.Second
			} else if isUnimplemented(err) {
				log.Info().Msg("AgentControl not implemented by backend; on-demand cluster queries disabled")
				return
			} else {
				log.Warn().Err(err).Msg("AgentControl stream error; reconnecting")
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff < maxBackoff {
					backoff *= 2
				}
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}()
}

func runAgentControlStream(ctx context.Context, grpcClient *Client, k8sClient *k8s.Client) error {
	stub, err := grpcClient.Stub()
	if err != nil {
		return err
	}
	stream, err := stub.AgentControl(ctx)
	if err != nil {
		return err
	}
	log.Info().Msg("AgentControl stream connected")

	for {
		in, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		reqID := in.GetRequestId()
		switch m := in.GetMsg().(type) {
		case *agentv1.AgentControlMessage_ListNamespacesReq:
			_ = m // empty
			out := listNamespacesReply(ctx, k8sClient, reqID)
			if err := stream.Send(out); err != nil {
				return err
			}

		case *agentv1.AgentControlMessage_ListPodsReq:
			req := m.ListPodsReq
			out := listPodsReply(ctx, k8sClient, reqID, req)
			if resp := out.GetListPodsResp(); resp != nil {
				ns := ""
				if req != nil {
					ns = req.GetNamespace()
				}
				if e := resp.GetError(); e != "" {
					log.Warn().
						Str("request_id", reqID).
						Str("namespace", ns).
						Str("error", e).
						Msg("AgentControl: ListPods failed")
				} else {
					log.Info().
						Str("request_id", reqID).
						Str("namespace", ns).
						Int("pods", len(resp.GetPods())).
						Msg("AgentControl: ListPods ok")
				}
			}
			if err := stream.Send(out); err != nil {
				return err
			}

		default:
			log.Debug().
				Str("request_id", reqID).
				Msg("AgentControl: ignored message (not a request)")
		}
	}
}

func listNamespacesReply(ctx context.Context, k8sClient *k8s.Client, reqID string) *agentv1.AgentControlMessage {
	names, err := k8s.ListNamespaceNames(ctx, k8sClient.Clientset)
	if err != nil {
		return &agentv1.AgentControlMessage{
			RequestId: reqID,
			Msg: &agentv1.AgentControlMessage_ListNamespacesResp{
				ListNamespacesResp: &agentv1.ListNamespacesResponse{
					Error: err.Error(),
				},
			},
		}
	}
	return &agentv1.AgentControlMessage{
		RequestId: reqID,
		Msg: &agentv1.AgentControlMessage_ListNamespacesResp{
			ListNamespacesResp: &agentv1.ListNamespacesResponse{
				Namespaces: names,
			},
		},
	}
}

func listPodsReply(ctx context.Context, k8sClient *k8s.Client, reqID string, req *agentv1.ListPodsRequest) *agentv1.AgentControlMessage {
	if req == nil {
		return &agentv1.AgentControlMessage{
			RequestId: reqID,
			Msg: &agentv1.AgentControlMessage_ListPodsResp{
				ListPodsResp: &agentv1.ListPodsResponse{Error: "missing ListPodsRequest body"},
			},
		}
	}

	pods, err := k8s.ListPodsInNamespace(ctx, k8sClient.Clientset, req.GetNamespace(), req.GetLabelSelector())
	if err != nil {
		return &agentv1.AgentControlMessage{
			RequestId: reqID,
			Msg: &agentv1.AgentControlMessage_ListPodsResp{
				ListPodsResp: &agentv1.ListPodsResponse{Error: err.Error()},
			},
		}
	}

	summaries := make([]*agentv1.PodSummary, 0, len(pods))
	for i := range pods {
		p := &pods[i]
		summaries = append(summaries, &agentv1.PodSummary{
			Name:            p.Name,
			Namespace:       p.Namespace,
			Phase:           string(p.Status.Phase),
			Labels:          copyStrMapForControl(p.Labels),
			ResourceVersion: p.ResourceVersion,
			NodeName:        p.Spec.NodeName,
		})
	}

	return &agentv1.AgentControlMessage{
		RequestId: reqID,
		Msg: &agentv1.AgentControlMessage_ListPodsResp{
			ListPodsResp: &agentv1.ListPodsResponse{
				Pods: summaries,
			},
		},
	}
}

func copyStrMapForControl(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func isUnimplemented(err error) bool {
	s, ok := status.FromError(err)
	return ok && s.Code() == codes.Unimplemented
}
