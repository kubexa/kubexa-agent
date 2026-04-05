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

	agentv1 "github.com/kubexa/kubexa-agent/gen/agent/v1"
	"github.com/kubexa/kubexa-agent/internal/buffer"
	"github.com/kubexa/kubexa-agent/internal/k8s"
)

// LogReporter forwards log lines to AgentService.StreamLogs via spillover buffer (memory + optional Redis).
type LogReporter struct {
	client    *Client
	clusterID string
	spill     *buffer.SpilloverBuffer
}

func NewLogReporter(client *Client, clusterID string, spill *buffer.SpilloverBuffer) *LogReporter {
	return &LogReporter{
		client:    client,
		clusterID: clusterID,
		spill:     spill,
	}
}

// Run ingests log lines into the buffer and drains to gRPC with reconnect on failure.
func (r *LogReporter) Run(ctx context.Context, logs <-chan *k8s.LogLine) {
	ingestDone := make(chan struct{})
	go r.ingest(ctx, logs, ingestDone)

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

		log.Warn().Err(err).Msg("log gRPC stream disconnected, will be retried")

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

func (r *LogReporter) ingest(ctx context.Context, lines <-chan *k8s.LogLine, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-lines:
			if !ok {
				return
			}
			if line == nil {
				continue
			}
			ch := r.toChunk(line)
			payload, err := proto.Marshal(ch)
			if err != nil {
				log.Error().Err(err).Msg("log chunk marshal for buffer failed")
				continue
			}
			item := &buffer.Item{
				ID:        uuid.NewString(),
				Type:      buffer.EventTypeLog,
				Payload:   payload,
				CreatedAt: line.Timestamp,
			}
			if err := r.spill.Push(ctx, item); err != nil {
				log.Warn().Err(err).Msg("log outbound buffer push failed")
			}
		}
	}
}

func (r *LogReporter) runStreamFromBuffer(
	ctx context.Context,
	ingestDone <-chan struct{},
	reconnectBackoff *time.Duration,
	backoffMin time.Duration,
) error {
	stub, err := r.client.Stub()
	if err != nil {
		return err
	}
	stream, err := stub.StreamLogs(ctx)
	if err != nil {
		return err
	}
	if reconnectBackoff != nil {
		*reconnectBackoff = backoffMin
	}

	for {
		select {
		case <-ctx.Done():
			return r.closeStream(stream, ctx.Err())
		default:
		}

		item, err := r.spill.Pop(ctx)
		if err != nil {
			if errors.Is(err, buffer.ErrBufferEmpty) {
				select {
				case <-ctx.Done():
					return r.closeStream(stream, ctx.Err())
				case <-ingestDone:
					if n, _ := r.spill.Len(ctx); n == 0 {
						return r.closeStream(stream, nil)
					}
				case <-time.After(40 * time.Millisecond):
				}
				continue
			}
			return err
		}

		var ch agentv1.LogChunk
		if err := proto.Unmarshal(item.Payload, &ch); err != nil {
			_ = r.spill.Ack(ctx, item)
			log.Warn().Err(err).Str("id", item.ID).Msg("drop corrupted buffered log chunk")
			continue
		}

		if err := stream.Send(&ch); err != nil {
			_ = stream.CloseSend()
			_, _ = stream.CloseAndRecv()
			_ = r.spill.Nack(ctx, item)
			return err
		}
		if err := r.spill.Ack(ctx, item); err != nil {
			log.Warn().Err(err).Str("id", item.ID).Msg("log buffer ack failed")
		}
	}
}

func (r *LogReporter) closeStream(
	stream agentv1.AgentService_StreamLogsClient,
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
		return fmt.Errorf("log stream rejected: %s", ack.GetMessage())
	}

	return nil
}

func (r *LogReporter) toChunk(line *k8s.LogLine) *agentv1.LogChunk {
	return &agentv1.LogChunk{
		ClusterId: r.clusterID,
		Namespace: line.Namespace,
		PodName:   line.PodName,
		Container: line.Container,
		Line:      line.Line,
		Timestamp: line.Timestamp.UnixMilli(),
	}
}
