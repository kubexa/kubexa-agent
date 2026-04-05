package k8s

import (
	"bufio"
	"context"
	"io"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/kubexa/kubexa-agent/internal/config"
)

// ─────────────────────────────────────────
// Types
// ─────────────────────────────────────────

// LogLine single log line from a pod
type LogLine struct {
	Namespace string
	PodName   string
	Container string
	Line      string
	Timestamp time.Time
}

// streamKey key to track active streams
type streamKey struct {
	namespace string
	pod       string
	container string
}

// ─────────────────────────────────────────
// LogStreamer
// ─────────────────────────────────────────

// LogStreamer config'de defined pods's logs to a channel.
// tail and send to a channel.
type LogStreamer struct {
	client  *Client
	cfg     config.LogConfig
	logChan chan *LogLine

	mu      sync.Mutex
	active  map[streamKey]context.CancelFunc // active streams
	stopped bool
}

func NewLogStreamer(client *Client, cfg config.LogConfig) *LogStreamer {
	return &LogStreamer{
		client:  client,
		cfg:     cfg,
		logChan: make(chan *LogLine, cfg.BufferSize),
		active:  make(map[streamKey]context.CancelFunc),
	}
}

// ─────────────────────────────────────────
// Start
// ─────────────────────────────────────────

// Start for each log target, find pods and start stream.
// Works with PodWatcher: new pod, StartPodStream is called, pod deleted, StopPodStream.
// StartPodStream is called, pod deleted, StopPodStream.
func (s *LogStreamer) Start(ctx context.Context) error {
	if !s.cfg.Enabled {
		log.Info().Msg("log streaming disabled")
		return nil
	}

	for _, target := range s.cfg.Targets {
		pods, err := s.findPods(ctx, target)
		if err != nil {
			log.Error().
				Err(err).
				Str("namespace", target.Namespace).
				Str("selector", target.LabelSelector).
				Msg("pod list not found")
			continue
		}

		for i := range pods {
			s.startPodStream(ctx, &pods[i], target.ContainerNames)
		}
	}

	return nil
}

// ─────────────────────────────────────────
// Pod Stream Management
// ─────────────────────────────────────────

// StartPodStream starts log stream for a specific pod.
// If pod is already being watched, don't start again.
func (s *LogStreamer) StartPodStream(
	ctx context.Context,
	pod *corev1.Pod,
	containerNames []string,
) {
	if !s.cfg.Enabled {
		return
	}
	s.startPodStream(ctx, pod, containerNames)
}

// StopPodStream stops log stream for a specific pod.
func (s *LogStreamer) StopPodStream(namespace, podName string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stopped := 0
	for key, cancel := range s.active {
		if key.namespace == namespace && key.pod == podName {
			cancel()
			delete(s.active, key)
			stopped++
		}
	}

	if stopped > 0 {
		log.Info().
			Str("namespace", namespace).
			Str("pod", podName).
			Int("containers", stopped).
			Msg("pod log stream stopped")
	}
}

func (s *LogStreamer) startPodStream(
	ctx context.Context,
	pod *corev1.Pod,
	containerNames []string,
) {
	containers := resolveContainers(pod, containerNames)

	for _, container := range containers {
		key := streamKey{
			namespace: pod.Namespace,
			pod:       pod.Name,
			container: container,
		}

		s.mu.Lock()
		if _, exists := s.active[key]; exists {
			s.mu.Unlock()
			continue // already being watched
		}

		streamCtx, cancel := context.WithCancel(ctx)
		s.active[key] = cancel
		s.mu.Unlock()

		go s.streamContainer(streamCtx, key)
	}
}

// ─────────────────────────────────────────
// Container Stream
// ─────────────────────────────────────────

func (s *LogStreamer) streamContainer(ctx context.Context, key streamKey) {
	log.Info().
		Str("namespace", key.namespace).
		Str("pod", key.pod).
		Str("container", key.container).
		Msg("container log stream started")

	defer func() {
		s.mu.Lock()
		delete(s.active, key)
		s.mu.Unlock()

		log.Info().
			Str("namespace", key.namespace).
			Str("pod", key.pod).
			Str("container", key.container).
			Msg("container log stream stopped")
	}()

	backoff := newBackoff(1*time.Second, 30*time.Second)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := s.readLogs(ctx, key)
		if err == nil || ctx.Err() != nil {
			return
		}

		delay := backoff.next()
		log.Warn().
			Err(err).
			Str("pod", key.pod).
			Str("container", key.container).
			Dur("retry_in", delay).
			Msg("log stream kesildi, yeniden denenecek")

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
			backoff.reset()
		}
	}
}

func (s *LogStreamer) readLogs(ctx context.Context, key streamKey) error {
	sinceSeconds := int64(10) // last 10 seconds of logs
	req := s.client.Clientset.CoreV1().Pods(key.namespace).GetLogs(
		key.pod,
		&corev1.PodLogOptions{
			Container:    key.container,
			Follow:       true,
			SinceSeconds: &sinceSeconds,
			Timestamps:   false,
		},
	)

	stream, err := req.Stream(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := stream.Close(); cerr != nil {
			log.Warn().Err(cerr).Str("pod", key.pod).Str("container", key.container).Msg("log stream close")
		}
	}()

	scanner := bufio.NewScanner(stream)
	// increase buffer for large log lines (1MB)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		line := scanner.Text()
		if line == "" {
			continue
		}

		logLine := &LogLine{
			Namespace: key.namespace,
			PodName:   key.pod,
			Container: key.container,
			Line:      line,
			Timestamp: time.Now(),
		}

		select {
		case s.logChan <- logLine:
		case <-ctx.Done():
			return nil
		default:
			log.Warn().
				Str("pod", key.pod).
				Str("container", key.container).
				Msg("log channel full, line dropped")
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		return err
	}

	return nil
}

// ─────────────────────────────────────────
// Logs Channel
// ─────────────────────────────────────────

// Logs log channel.
func (s *LogStreamer) Logs() <-chan *LogLine {
	return s.logChan
}

// ActiveStreams number of active streams.
func (s *LogStreamer) ActiveStreams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// ShouldStream checks if the pod matches any log target.
// If it matches, return true and the relevant container names.
func (s *LogStreamer) ShouldStream(pod *corev1.Pod) (bool, []string) {
	for _, target := range s.cfg.Targets {
		if matchesTarget(pod, target) {
			return true, target.ContainerNames
		}
	}
	return false, nil
}

// ─────────────────────────────────────────
// Stop
// ─────────────────────────────────────────

func (s *LogStreamer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stopped {
		return
	}
	s.stopped = true

	for key, cancel := range s.active {
		cancel()
		delete(s.active, key)
	}

	log.Info().Msg("log streamer stopped")
}

// ─────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────

// findPods return pod list based on target config.
func (s *LogStreamer) findPods(
	ctx context.Context,
	target config.LogTarget,
) ([]corev1.Pod, error) {
	opts := metav1.ListOptions{}
	if target.LabelSelector != "" {
		opts.LabelSelector = target.LabelSelector
	}

	podList, err := s.client.Clientset.CoreV1().
		Pods(target.Namespace).
		List(ctx, opts)
	if err != nil {
		return nil, err
	}

	return podList.Items, nil
}

// resolveContainers return container list of the pod.
// if containerNames is empty, return all containers.
func resolveContainers(pod *corev1.Pod, containerNames []string) []string {
	if len(containerNames) > 0 {
		return containerNames
	}

	var result []string
	for _, c := range pod.Spec.Containers {
		result = append(result, c.Name)
	}
	return result
}

// matchesTarget checks if the pod matches the log target.
func matchesTarget(pod *corev1.Pod, target config.LogTarget) bool {
	if pod.Namespace != target.Namespace {
		return false
	}
	if target.LabelSelector == "" {
		return true
	}

	selector, err := labels.Parse(target.LabelSelector)
	if err != nil {
		return false
	}

	return selector.Matches(labels.Set(pod.Labels))
}

// ─────────────────────────────────────────
// Backoff
// ─────────────────────────────────────────

type backoff struct {
	current time.Duration
	base    time.Duration
	max     time.Duration
}

func newBackoff(base, max time.Duration) *backoff {
	return &backoff{current: base, base: base, max: max}
}

func (b *backoff) next() time.Duration {
	d := b.current
	b.current *= 2
	if b.current > b.max {
		b.current = b.max
	}
	return d
}

func (b *backoff) reset() {
	b.current = b.base
}
