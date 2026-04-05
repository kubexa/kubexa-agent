package grpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"

	agentv1 "github.com/kubexa/kubexa-agent/gen/agent/v1"
	"github.com/kubexa/kubexa-agent/internal/buffer"
	"github.com/kubexa/kubexa-agent/internal/k8s"
)

// PodReporter forwards pod events to AgentService.StreamPodEvents using a spillover buffer (memory + optional Redis).
type PodReporter struct {
	client    *Client
	clusterID string
	spill     *buffer.SpilloverBuffer
}

func NewPodReporter(client *Client, clusterID string, spill *buffer.SpilloverBuffer) *PodReporter {
	return &PodReporter{
		client:    client,
		clusterID: clusterID,
		spill:     spill,
	}
}

// Run ingests events into the buffer and drains to gRPC with reconnect on failure.
func (r *PodReporter) Run(ctx context.Context, events <-chan *k8s.PodEvent) {
	ingestDone := make(chan struct{})
	go r.ingest(ctx, events, ingestDone)

	const minDelay = time.Second
	const maxDelay = 30 * time.Second
	delay := minDelay

	for {
		if ctx.Err() != nil {
			return
		}
		err := r.runStreamFromBuffer(ctx, ingestDone, &delay, minDelay)
		if err == nil {
			return
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}

		log.Warn().Err(err).Msg("pod events gRPC stream disconnected, will be retried")

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func (r *PodReporter) ingest(ctx context.Context, events <-chan *k8s.PodEvent, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev == nil || ev.Pod == nil {
				continue
			}
			pe := r.toProto(ev)
			payload, err := proto.Marshal(pe)
			if err != nil {
				log.Error().Err(err).Msg("pod event marshal for buffer failed")
				continue
			}
			item := &buffer.Item{
				ID:        uuid.NewString(),
				Type:      buffer.EventTypePod,
				Payload:   payload,
				CreatedAt: ev.Timestamp,
			}
			if err := r.spill.Push(ctx, item); err != nil {
				log.Warn().Err(err).Msg("pod outbound buffer push failed")
			}
		}
	}
}

func (r *PodReporter) runStreamFromBuffer(
	ctx context.Context,
	ingestDone <-chan struct{},
	reconnectBackoff *time.Duration,
	backoffMin time.Duration,
) error {
	stub, err := r.client.Stub()
	if err != nil {
		return err
	}
	stream, err := stub.StreamPodEvents(ctx)
	if err != nil {
		return err
	}
	if reconnectBackoff != nil {
		*reconnectBackoff = backoffMin
	}

	for {
		select {
		case <-ctx.Done():
			return r.closePodStream(stream, ctx.Err())
		default:
		}

		item, err := r.spill.Pop(ctx)
		if err != nil {
			if errors.Is(err, buffer.ErrBufferEmpty) {
				select {
				case <-ctx.Done():
					return r.closePodStream(stream, ctx.Err())
				case <-ingestDone:
					if n, _ := r.spill.Len(ctx); n == 0 {
						return r.closePodStream(stream, nil)
					}
				case <-time.After(40 * time.Millisecond):
				}
				continue
			}
			return err
		}

		var pe agentv1.PodEvent
		if err := proto.Unmarshal(item.Payload, &pe); err != nil {
			_ = r.spill.Ack(ctx, item)
			log.Warn().Err(err).Str("id", item.ID).Msg("drop corrupted buffered pod event")
			continue
		}

		if err := stream.Send(&pe); err != nil {
			_ = stream.CloseSend()
			_, _ = stream.CloseAndRecv()
			_ = r.spill.Nack(ctx, item)
			return err
		}
		if err := r.spill.Ack(ctx, item); err != nil {
			log.Warn().Err(err).Str("id", item.ID).Msg("pod buffer ack failed")
		}
	}
}

func (r *PodReporter) closePodStream(
	stream agentv1.AgentService_StreamPodEventsClient,
	cause error,
) error {
	_ = stream.CloseSend()
	ack, recvErr := stream.CloseAndRecv()

	if cause != nil {
		return cause
	}

	if recvErr != nil && !errors.Is(recvErr, io.EOF) {
		if st, ok := status.FromError(recvErr); ok && st.Code() == codes.Canceled {
			return recvErr
		}
		return recvErr
	}

	if ack != nil && !ack.GetOk() {
		return fmt.Errorf("pod events stream rejected: %s", ack.GetMessage())
	}

	return nil
}

func (r *PodReporter) toProto(ev *k8s.PodEvent) *agentv1.PodEvent {
	pod := ev.Pod
	out := &agentv1.PodEvent{
		ClusterId:       r.clusterID,
		Namespace:       pod.Namespace,
		PodName:         pod.Name,
		Phase:           string(pod.Status.Phase),
		EventType:       string(ev.Type),
		Timestamp:       ev.Timestamp.UnixMilli(),
		Labels:          copyStringMap(pod.Labels),
		Annotations:     copyStringMap(pod.Annotations),
		Containers:      containerStatusesProto(pod.Status.ContainerStatuses),
		ResourceVersion: pod.ResourceVersion,
		NodeName:        pod.Spec.NodeName,
	}

	if ev.Type == k8s.EventModified && ev.OldPod != nil {
		out.PreviousPhase = string(ev.OldPod.Status.Phase)
		if hints := k8s.PodChangeHints(ev.OldPod, pod); len(hints) > 0 {
			out.ChangeHints = hints
		}
	}

	return out
}

func copyStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func containerStatusesProto(cs []corev1.ContainerStatus) []*agentv1.ContainerStatus {
	if len(cs) == 0 {
		return nil
	}
	out := make([]*agentv1.ContainerStatus, 0, len(cs))
	for i := range cs {
		st := &cs[i]
		out = append(out, &agentv1.ContainerStatus{
			Name:         st.Name,
			Ready:        st.Ready,
			RestartCount: st.RestartCount,
			Image:        st.Image,
			State:        k8sContainerState(st),
			Reason:       k8sContainerReason(st),
		})
	}
	return out
}

func k8sContainerState(st *corev1.ContainerStatus) string {
	if st == nil {
		return ""
	}
	switch {
	case st.State.Running != nil:
		return "running"
	case st.State.Waiting != nil:
		return "waiting"
	case st.State.Terminated != nil:
		return "terminated"
	default:
		return "unknown"
	}
}

func k8sContainerReason(st *corev1.ContainerStatus) string {
	if st == nil {
		return ""
	}
	if st.State.Waiting != nil {
		return st.State.Waiting.Reason
	}
	if st.State.Terminated != nil {
		return st.State.Terminated.Reason
	}
	return ""
}
