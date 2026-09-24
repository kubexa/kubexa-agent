// Command agent is the kubexa-agent process entrypoint.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/kubexa/kubexa-agent/internal/capability"
	"github.com/kubexa/kubexa-agent/internal/collector/logs"
	metricscollector "github.com/kubexa/kubexa-agent/internal/collector/metrics"
	"github.com/kubexa/kubexa-agent/internal/collector/state"
	"github.com/kubexa/kubexa-agent/internal/exec"
	execpolicy "github.com/kubexa/kubexa-agent/internal/exec/policy"
	"github.com/kubexa/kubexa-agent/internal/health"
	"github.com/kubexa/kubexa-agent/internal/ingestrules"
	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/k8s/k8sconfig"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/internal/metrics"
	"github.com/kubexa/kubexa-agent/internal/mutate"
	mutatepolicy "github.com/kubexa/kubexa-agent/internal/mutate/policy"
	"github.com/kubexa/kubexa-agent/internal/nodeops"
	agentpprof "github.com/kubexa/kubexa-agent/internal/pprof"
	"github.com/kubexa/kubexa-agent/internal/query"
	"github.com/kubexa/kubexa-agent/internal/query/policy"
	"github.com/kubexa/kubexa-agent/internal/queue"
	"github.com/kubexa/kubexa-agent/internal/stream"
	"github.com/kubexa/kubexa-agent/pkg/buildinfo"
	"github.com/kubexa/kubexa-agent/pkg/config"
	"github.com/kubexa/kubexa-agent/pkg/config/k8sresource"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	commonv1 "github.com/kubexa/kubexa-agent/proto/gen/go/common/v1"
)

const (
	defaultConfigPath      = "/config/config.yaml"
	defaultShutdownTimeout = 30 * time.Second
)

// Collector is a long-running data collection component (logs, state, metrics).
// Implementations will be wired in later steps.
type Collector interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Name() string
}

// effectiveConfigSetter is the narrow, structural view of the stream manager
// this file needs: stream.Manager (the interface stream.New returns)
// deliberately does not expose SetOnEffectiveConfig -- most callers, and the
// mock in internal/health, only need Run/Send/Connected/SessionID/
// IsThrottled -- so the hook is reached by asserting a Manager value against
// this interface instead. Declared at package level, rather than inline in
// serve(), so TestStreamManagerSatisfiesEffectiveConfigSetter can assert the
// exact same interface serve() uses, against the exact same stream.New
// constructor, rather than a redeclared copy that could quietly drift from
// the real one.
type effectiveConfigSetter interface {
	SetOnEffectiveConfig(func(*config.Config) ([]string, error))
}

// shutdownSignaler is the narrow, structural view of the stream manager this
// file needs to let a gateway-requested Shutdown drain the agent through the
// SAME exit path a real SIGTERM already takes, rather than a second
// implementation only Shutdown would ever exercise: stream.Manager
// deliberately does not expose it, mirroring effectiveConfigSetter above, so
// the mock in internal/health -- which implements only Manager -- is
// unaffected. serve() asserts streamMgr against this interface and treats a
// failed assertion as a boot error, for the same reason
// effectiveConfigSetter does: a silent skip here would leave the platform's
// restart-on-boot-only-config feature permanently inert, with no error and
// no log line to say so.
type shutdownSignaler interface {
	ShutdownRequested() <-chan struct{}
	ShutdownReason() string
}

// sweepRequester is the narrow, structural view of the capability reporter
// the swap callback needs: *capability.Reporter, when the state collector is
// enabled and buildCollectors constructs one. Collector itself does not
// expose RequestSweep -- log and metrics collectors have no sweep to force
// -- so the callback finds it by asserting each of the already-built
// collectors against this interface instead of buildCollectors returning the
// reporter by name.
type sweepRequester interface {
	RequestSweep()
}

// collectorConfigSetter is the narrow, structural view of the stream manager
// serve() uses to tell it which config the collectors were actually built
// from. Declared at package level for the same reason
// effectiveConfigSetter is, and asserted the same way: a failed assertion is
// a boot error, because a manager that never learns this reports every
// revision as applied with restart_required false forever -- a control that
// changes nothing, reported as working. See
// TestStreamManagerSatisfiesCollectorConfigSetter.
type collectorConfigSetter interface {
	SetCollectorConfig(*config.Config)
}

// effectiveConfigSource is what boot reads to decide what to build the
// collectors FROM: a signal that the first handshake has been processed, and
// the config that produced. Kept separate from collectorConfigSetter so the
// test doubles in boot_test.go need not implement a setter they never call.
type effectiveConfigSource interface {
	ConfigSettled() <-chan struct{}
	EffectiveConfig() *config.Config
}

// managedConfigBootWait is how long boot waits for the gateway's first
// handshake before building the collectors from the file config.
//
// Bounded, and short: an agent whose gateway is unreachable -- a cluster
// with no egress yet, a gateway mid-rollout -- must still collect. Ten
// seconds is long enough for a handshake on a working link (the handshake
// timeout itself defaults to the same order) and short enough that the
// health server, which starts before this wait, answers a readiness probe
// with "collectors have not started yet" rather than the kubelet counting
// liveness failures.
const managedConfigBootWait = 10 * time.Second

// errReconcilerNotReady is what a watch reconcile gets while the collectors
// are still being built. The gateway retries on an error; it cannot retry a
// success that did nothing.
var errReconcilerNotReady = errors.New("watch reconciler is not ready yet: the agent is still starting its collectors")

// reconcilerHolder breaks the dependency cycle between the stream manager
// and the collectors.
//
// Measured: the collectors carry the reconciler (the state collector IS the
// stream.WatchReconciler) and the stream manager needs it at construction --
// while the collectors must now be built from the config the stream manager
// resolves at the handshake. "Manager first, then collectors" is therefore
// not an ordering, it is a cycle. The holder is the same indirection this
// branch already uses twice (SetOnEffectiveConfig, sweepRequester): built
// first, passed to stream.New, populated once the real collector exists.
//
// It has three states, and the middle one is the point:
//
//   - unpopulated (boot has not reached the collectors): Reconcile records
//     the desired set and returns errReconcilerNotReady. Never a nil
//     dereference, and never a silent success -- a caller that cannot
//     converge must say so.
//   - populated with nil (collect.state is disabled, so no collector
//     satisfies WatchReconciler at all): Reconcile succeeds and does
//     nothing, which is exactly what the manager's own nil check did before.
//   - populated with a collector: a pass-through.
//
// The recorded set is REPLAYED when Set populates the holder, and that is
// not belt-and-braces: nothing else would ever re-send it. Measured in the
// backend, internal/clusterwatch/reconciler.go:188-217 skips a session whose
// signature matches lastSent, and that signature is recorded on a successful
// SEND -- delivery to the stream, not application by the agent. The stream
// manager logs the rejected reconcile and returns. So the 15s periodic
// reconciler does not re-send the same set: healing would need the desired
// set itself to change (a lease added or expired, 90s TTL) or a reconnect.
// Without the replay, a watch a user asked for in the boot window can stay
// dead for up to 90 seconds.
//
// Drop-and-replay-LATEST, not a queue: each reconcile is a complete desired
// set ("converge on exactly this"), so only the most recent one means
// anything, and replaying an older one after a newer would converge
// backwards.
//
// One mutex covers resolution, the pending set and the forwarded call --
// rather than the lock-free atomic the other two holders use -- so a replay
// cannot interleave with a live reconcile and leave the informers converged
// on the older of the two. Reconcile's only caller is the stream manager's
// receive loop, which is already serial, so the lock costs nothing; Set runs
// once, at boot.
type reconcilerHolder struct {
	mu          sync.Mutex
	resolved    bool
	inner       stream.WatchReconciler
	pending     []k8sresource.Descriptor
	havePending bool
}

// Set resolves the holder and replays the most recent desired set that was
// refused while it was unresolved, if there was one. r may be nil, meaning
// this agent builds no state collector; that is a resolution, not an
// absence, and it discards any pending set because there is nothing that
// could ever apply it. Called once, by serve(), before the collectors start.
//
// The returned error is the replayed Reconcile's own; serve() logs it. A
// failed replay is no worse than the state before this holder existed.
func (h *reconcilerHolder) Set(ctx context.Context, r stream.WatchReconciler) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	pending, have := h.pending, h.havePending
	h.resolved, h.inner = true, r
	h.pending, h.havePending = nil, false
	if !have || r == nil {
		return nil
	}
	return r.Reconcile(ctx, pending)
}

// Reconcile implements stream.WatchReconciler.
func (h *reconcilerHolder) Reconcile(ctx context.Context, desired []k8sresource.Descriptor) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.resolved {
		h.pending, h.havePending = desired, true
		return errReconcilerNotReady
	}
	if h.inner == nil {
		return nil
	}
	return h.inner.Reconcile(ctx, desired)
}

// scrapeHealthHolder is reconcilerHolder's sibling for the heartbeat's
// scrape health: the metrics collector supplies it, and the metrics
// collector no longer exists when stream.New is called.
//
// Unresolved and resolved-to-none answer identically here -- nil, meaning
// "this agent does not report scrape health" -- because that is already the
// honest reading of an absent scrape_targets list, and during the boot
// window it is literally true: nothing is scraping yet. It must never become
// an empty list, which a consumer would read as "every kind is OK".
type scrapeHealthHolder struct {
	slot atomic.Pointer[scrapeHealthSlot]
}

type scrapeHealthSlot struct{ inner stream.ScrapeHealthSource }

// Set resolves the holder; s may be nil (metrics collection disabled).
func (h *scrapeHealthHolder) Set(s stream.ScrapeHealthSource) {
	h.slot.Store(&scrapeHealthSlot{inner: s})
}

// ScrapeTargetHealth implements stream.ScrapeHealthSource.
func (h *scrapeHealthHolder) ScrapeTargetHealth() []*agentv1.ScrapeTargetHealth {
	slot := h.slot.Load()
	if slot == nil || slot.inner == nil {
		return nil
	}
	return slot.inner.ScrapeTargetHealth()
}

// sweeperHolder is the same indirection for the capability reporter's sweep
// nudge, which the effective-config callback needs at stream.New time and
// which the collectors only produce later.
//
// A nudge that arrives before the reporter exists is DROPPED, not queued:
// the reporter runs its own sweep when it starts, from the policy that swap
// just installed, so the nudge would ask for a sweep that is already
// happening.
type sweeperHolder struct {
	slot atomic.Pointer[sweeperSlot]
}

type sweeperSlot struct{ inner sweepRequester }

// Set resolves the holder; s may be nil (no capability reporter is built
// when collect.state is disabled).
func (h *sweeperHolder) Set(s sweepRequester) {
	h.slot.Store(&sweeperSlot{inner: s})
}

// RequestSweep implements sweepRequester.
func (h *sweeperHolder) RequestSweep() {
	slot := h.slot.Load()
	if slot == nil || slot.inner == nil {
		return
	}
	slot.inner.RequestSweep()
}

// collectorsChecker keeps the readiness probe honest across the boot wait.
//
// The health server now starts BEFORE the collectors are built, so that a
// liveness probe has something to answer during the wait. Without this
// checker, readiness would go green in that window on the strength of the
// k8s, queue and stream checks alone -- reporting an agent that collects
// nothing as ready, which is this task's own failure mode wearing a
// different hat. The state-watcher checker that used to cover the gap is
// registered later, once the collector exists.
type collectorsChecker struct{ started atomic.Bool }

// Name returns the component identifier.
func (c *collectorsChecker) Name() string { return "collectors" }

// Check reports unhealthy until serve() has started the collectors.
func (c *collectorsChecker) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.started.Load() {
		return health.Unhealthy(errors.New("collectors have not started yet"))
	}
	return nil
}

// startCollectors waits briefly for the platform's first managed config set,
// builds the collectors from whatever that wait produced, and reports the
// config they were built from back to the stream manager.
//
// This ordering is the fix for C1. Collectors used to be built from the FILE
// config, while the managed set arrives at the handshake -- after it. A
// restart did not rescue it, because a restart repeats the same order, which
// is how four of the five managed blocks came to be stored, validated and
// reported as applied while changing nothing.
//
// It takes build and report as functions rather than the dozen arguments
// buildCollectors needs, because the ordering it encodes -- wait, then
// build, then report, and report ONLY after a build that succeeded -- is
// exactly what a test on serve() cannot see.
//
// The wait falls back to src.EffectiveConfig() on timeout, which is the file
// config when nothing was applied: an agent whose gateway is unreachable
// must still collect.
func startCollectors(
	ctx context.Context,
	src effectiveConfigSource,
	budget time.Duration,
	build func(*config.Config) ([]Collector, error),
	report func(*config.Config),
	log *logger.Logger,
) ([]Collector, *config.Config, error) {
	waitForManagedConfig(ctx, src.ConfigSettled(), budget, log)
	// A SIGTERM (or a server that already failed into the run group) during
	// the wait: build nothing. Starting informers and a log tailer for a
	// process that is shutting down is work that can only fail, and the
	// wrapped context error is what run() reads as a clean stop.
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	effective := src.EffectiveConfig()
	collectors, err := build(effective)
	if err != nil {
		return nil, nil, err
	}
	// Only now: with no collectors there is nothing the heartbeat could
	// honestly call "what this process is running".
	report(effective)
	return collectors, effective, nil
}

// unwindBootFailure ends a boot that could not build its collectors, with
// the servers and the stream manager already running in the run group.
//
// It exists as its own function because which of TWO errors it returns
// decides the process EXIT CODE, and serve() is not reachable from a test.
//
// The run group's error wins whenever it has one. The servers bind in
// milliseconds while the boot wait runs up to ten seconds, so a health,
// metrics, pprof or stream failure lands inside that wait essentially
// always: the errgroup records it, cancels ctx, and startCollectors comes
// back with a plain context.Canceled. Returning THAT would be read by run()
// through isContextClosed as a clean stop and exit 0 -- a port already in
// use becoming a pod that restarts forever with exit code 0 and one
// misleading "agent stopped" line. Before the collectors moved after the
// run group, the same failure surfaced through serve()'s own g.Wait as a
// named error and exit 1, and it has to keep doing so.
//
// isContextClosed guards the other direction: on a real SIGTERM during the
// wait, the group returns the same context error the cause carries, and the
// process must still exit 0.
//
// The queue is closed on every path -- it holds an open write-ahead log.
func unwindBootFailure(stop context.CancelFunc, wait func() error, closeQueue func() error, cause error) error {
	// The same cancel a SIGTERM uses, rather than returning out from under
	// the running goroutines.
	stop()
	werr := wait()
	_ = closeQueue()
	if werr != nil && !isContextClosed(werr) {
		return werr
	}
	return fmt.Errorf("collectors: %w", cause)
}

// waitForManagedConfig blocks until the agent's first handshake has been
// processed, the budget expires, or ctx ends. It never fails: every outcome
// leaves a config to build from.
func waitForManagedConfig(ctx context.Context, settled <-chan struct{}, budget time.Duration, log *logger.Logger) {
	if budget <= 0 {
		return
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()

	start := time.Now()
	select {
	case <-settled:
		log.Info("gateway config settled; building collectors from the effective config",
			logger.F("waited", time.Since(start).String()),
		)
	case <-timer.C:
		// Said at Warn, not Info: the collectors are about to be built from
		// the file config, so any managed block this cluster has is not in
		// effect until the agent restarts -- and the heartbeat will say so.
		log.Warn("no gateway config arrived within the boot wait; building collectors from the file config",
			logger.F("waited", budget.String()),
		)
	case <-ctx.Done():
	}
}

// logQueryPolicyWarnings logs the query policy's compile-time warnings, if
// any. It is called both after the boot-time compile and after every live
// swap so the two paths report identically -- see policy.NewHolder and
// policy.Holder.Swap, which both derive these warnings from the same
// compileWithWarnings function.
func logQueryPolicyWarnings(log *logger.Logger, warnings []string) {
	// A wildcard rule permits every resource, secrets among them. Paired with
	// visible Secret values it is the widest read policy the agent can hold,
	// and an operator must not have to discover that from a screen.
	if len(warnings) > 0 {
		log.Warn("live query policy permits every resource and Secret values are not redacted",
			logger.F("rules", strings.Join(warnings, ",")),
			logger.F("remedy", "set query.redact_secrets: true, or name resources explicitly"),
		)
	}
}

// newEffectiveConfigCallback builds the function serve() hands to
// SetOnEffectiveConfig. It exists as its own function -- rather than staying
// an inline closure in serve() -- for the same reason
// buildCapabilityReporterOptions does: the ordering it encodes (nudge the
// capability sweep only after a successful Swap, never on a rejected one) is
// exactly what a test on serve()'s behavior cannot see, since serve() has no
// return value that reflects it and starting a real stream manager to reach
// this closure is not practical in a unit test. Returning the callback lets
// a test invoke it directly and assert both outcomes.
//
// sweeper is a one-method interface, not *capability.Reporter, so a test can
// pass a counting fake instead of standing up a real reporter; it may be nil
// -- the capability reporter is not built when collect.state is disabled --
// in which case a successful swap has nothing to nudge.
func newEffectiveConfigCallback(holder *policy.Holder, sweeper sweepRequester, log *logger.Logger) func(*config.Config) ([]string, error) {
	return func(effective *config.Config) ([]string, error) {
		warnings, err := holder.Swap(effective)
		if err != nil {
			// ApplyBlocks validates the block schema, but a value that
			// schema allows can still be one Compile refuses (e.g.
			// "pods/": a valid k8sresource.Parse result with a zero GVR
			// field) -- reachable, not theoretical. Returning the error
			// here, rather than only logging it, makes applyManaged
			// reject the WHOLE managed-block set instead of reporting a
			// revision as applied while the agent keeps running the
			// previous one (Holder.Swap leaves it untouched on error).
			// The log still serves the cluster operator; the returned
			// error is what reaches the person who pushed the config, on
			// the heartbeat. RequestSweep is not called on this path: a
			// rejected config changed no policy, so there is nothing new
			// for the catalog to report.
			log.Error("live query policy swap rejected; keeping the previous policy",
				logger.F("error", err.Error()))
			return nil, err
		}
		logQueryPolicyWarnings(log, warnings)
		if sweeper != nil {
			// Otherwise the explorer catalog waits out the reporter's own
			// discovery tick (5m) or safety sweep (1h) before it reflects
			// this swap, even though the executor already answers the new
			// policy on its very next request.
			sweeper.RequestSweep()
		}
		return warnings, nil
	}
}

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", defaultConfigPath, "path to agent config YAML")
	devFlag := flag.Bool("dev", false, "enable local development mode")
	validateConfigFlag := flag.Bool("validate-config", false,
		"load and validate the config file through the real parser, print the result, and exit "+
			"(0 if valid) without starting the agent")
	flag.Parse()

	if *validateConfigFlag {
		return validateConfigAndExit(*configPath)
	}

	cfg, cfgWarnings, err := config.LoadWithWarnings(*configPath)
	if err != nil {
		printConfigWarnings(cfgWarnings)
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}

	devMode := *devFlag
	if devMode {
		cfg.Log.Level = "debug"
		cfg.Log.Format = "console"
	}

	level, err := logger.ParseLevel(cfg.Log.Level)
	if err != nil {
		printConfigWarnings(cfgWarnings)
		fmt.Fprintf(os.Stderr, "parse log level: %v\n", err)
		return 1
	}

	rootLog := logger.New("agent",
		logger.WithLevel(level),
		logger.WithDevelopment(cfg.Log.Format == "console"),
		logger.WithAgentID(cfg.Agent.AgentID),
		logger.WithClusterID(cfg.Agent.ClusterID),
	)

	if devMode {
		printBanner(os.Stdout)
	}
	rootLog.Info("starting kubexa-agent",
		logger.F("version", buildinfo.Version),
		logger.F("commit", buildinfo.Commit),
		logger.F("build_time", buildinfo.BuildTime),
		logger.F("dev", devMode),
		logger.F("config", *configPath),
	)

	// An unknown key is dropped, so the rule it belonged to is in force
	// WITHOUT the setting it was meant to carry -- a log rule that lost its
	// pod filter collects every pod in its namespace. That reads as an
	// over-broad rule and never as a typo, which is why it is said out loud.
	for _, w := range cfgWarnings {
		rootLog.Warn("unrecognized config key ignored; the setting it carried is NOT in force",
			logger.F("detail", w),
			logger.F("config", *configPath),
		)
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	rootCtx = logger.NewContext(rootCtx, rootLog)

	if err := serve(rootCtx, stop, cfg, devMode, rootLog); err != nil {
		if isContextClosed(err) {
			rootLog.Info("agent stopped")
			return 0
		}
		rootLog.Err(err).Error("agent exited with error")
		return 1
	}
	rootLog.Info("agent stopped")
	return 0
}

// stop is the signal.NotifyContext cancel func run() created for parentCtx:
// calling it cancels parentCtx exactly the way a real SIGINT/SIGTERM would.
// serve() also calls it when the stream manager's ShutdownRequested channel
// fires, so a gateway-requested restart runs through the identical drain
// below (g.Wait, then stopCollectors and q.Close under shutdownTimeout)
// rather than a second, untested exit path.
//
// One exception, and it is narrow enough to state exactly: a Shutdown (or
// any cancellation) that arrives BEFORE the collectors have been built
// leaves through unwindBootFailure instead -- stop, g.Wait, q.Close -- which
// skips stopCollectors and the shutdownTimeout-bounded context. What that
// bounded context bounds is stopCollectors, and on that path there are no
// collectors to stop: buildCollectors either has not run or returned an
// error, so nothing is tailing a log or holding an informer. q.Close is
// unbounded on BOTH paths already (shutdownCtx scopes stopCollectors only),
// so the drain a caller can observe is the same one. It is a second exit
// path in the code and not in behaviour, and it exists because the
// collectors are now built after the run group is running -- see
// startCollectors.
func serve(parentCtx context.Context, stop context.CancelFunc, cfg *config.Config, devMode bool, log *logger.Logger) error {
	shutdownTimeout := defaultShutdownTimeout

	mainReg := prometheus.NewRegistry()
	// A fresh registry carries no Go or process collectors, which is why a
	// v0.6.0 agent could be OOMKilled with nothing but cadvisor to reason from.
	if err := metrics.RegisterRuntimeCollectors(mainReg); err != nil {
		return fmt.Errorf("runtime collectors: %w", err)
	}

	kube, err := initKubernetes(parentCtx, cfg, devMode, log)
	if err != nil {
		return fmt.Errorf("kubernetes: %w", err)
	}
	log.Info("kubernetes client ready")

	if err := cfg.EnsureClusterID(parentCtx, kube); err != nil {
		return fmt.Errorf("cluster id: %w", err)
	}
	log = log.With("cluster_id", cfg.Agent.ClusterID)
	parentCtx = logger.NewContext(parentCtx, log)

	agentMetrics, err := metrics.New(mainReg, buildinfo.Version, cfg.Agent.ClusterID, cfg.Agent.AgentID)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	kube.EnableMetrics(agentMetrics.K8s())

	q, err := queue.New(&cfg.Buffer, logger.New("queue", logger.WithAgentID(cfg.Agent.AgentID)), agentMetrics.Queue())
	if err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	log.Info("queue initialized", logger.F("spill_dir", cfg.Buffer.SpillDir))

	// What the FILE says, which is not yet what this agent will collect: the
	// collectors are built after the first handshake, from the config the
	// platform's managed blocks produce. The second line, "collection
	// settings in effect", is the one that reports what was actually built.
	log.Info("collection settings from the config file",
		logger.F("logs_enabled", cfg.Collect.Logs.Enabled),
		logger.F("state_enabled", cfg.Collect.State.Enabled),
		logger.F("metrics_enabled", cfg.Collect.Metrics.Enabled),
	)

	// Compiled once at boot and held live thereafter: the executor enforces
	// it per request, and the capability reporter publishes its per-GVR
	// verdict to the UI. Both read the SAME holder, so a managed config
	// block pushed from the platform at runtime (stream manager's
	// onEffectiveConfig, wired below) reaches both instead of only the one
	// that happened to get a *Policy value at boot.
	queryPolicy, queryPolicyWarnings, err := policy.NewHolder(cfg)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("query policy: %w", err)
	}
	logQueryPolicyWarnings(log, queryPolicyWarnings)

	// The live-query path gets its own client pair for the same reason the
	// capability probe does: an ad-hoc user query must not drain the
	// rate-limit token bucket the informers share. Zero QPS/burst means the
	// same budget as the main client, but through a SEPARATE limiter.
	queryClients, err := k8s.NewQueryClients(&k8sconfig.Config{}, 0, 0)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("query clients: %w", err)
	}

	// Built even when query.enabled is false: the policy answers POLICY_DENIED,
	// which is a diagnosis. A nil responder would just drop the query.
	queryExecutor, err := query.New(query.Options{
		Clients:    *queryClients,
		Policy:     queryPolicy,
		Logger:     logger.New("query", logger.WithAgentID(cfg.Agent.AgentID)),
		Registerer: mainReg,
	})
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("query executor: %w", err)
	}

	// See compileMutatePolicy's doc comment for the fatal-vs-warning ruling
	// this applies. Its error is fatal exactly when mutate is enabled --
	// mirroring the query policy above -- because a compile failure while
	// mutate is disabled has already been logged and swallowed inside it.
	mutatePolicy, err := compileMutatePolicy(cfg, log)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("mutate policy: %w", err)
	}

	mutationResponder, _, err := buildMutationResponder(
		cfg,
		mutatePolicy,
		func() (*k8s.QueryClients, error) {
			// A SEPARATE client pair from queryClients above, not a reuse of
			// it -- and built here, inside the enabled path, not eagerly:
			// this closure only runs once buildMutationResponder has already
			// checked cfg.MutateEnabled(), so a disabled agent resolves no
			// REST config, builds no dynamic client, and starts no second
			// rate limiter for a responder it is about to throw away.
			//
			// k8s.QueryClients carries one rate.Limiter shared by every
			// client it hands out (see its doc comment), and that limiter
			// was carved out to protect the informer watch budget from
			// ad-hoc QUERY bursts -- it was never meant to also arbitrate
			// between reads and writes. Sharing it with mutations would mean
			// a dashboard polling live queries could throttle an operator's
			// delete, in either direction. 0/0 costs no new config key: it
			// is the same "same budget as the main client, separate
			// limiter" contract queryClients already uses.
			//
			// The trade, both halves of it: a write no longer queues behind
			// a read burst (or vice versa) -- but under full saturation the
			// agent can now issue up to twice the API calls it could with
			// one shared bucket, because two independent buckets replace
			// one.
			return k8s.NewQueryClients(&k8sconfig.Config{}, 0, 0)
		},
		logger.New("mutate", logger.WithAgentID(cfg.Agent.AgentID)),
		mainReg,
	)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("mutate executor: %w", err)
	}

	// The same fatal-vs-warning ruling, applied to exec.pod: see
	// compileExecPolicy.
	execPolicy, err := compileExecPolicy(cfg, log)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("exec policy: %w", err)
	}
	// The same ruling again, applied to exec.node: see compileNodeExecPolicy.
	nodePolicy, err := compileNodeExecPolicy(cfg, log)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("exec node policy: %w", err)
	}

	execResponder, execOpts, err := buildExecResponder(
		cfg,
		execPolicy,
		nodePolicy,
		func() (*k8s.ExecClients, error) {
			// Built only inside the enabled path, for the reason the mutate
			// factory above gives: a disabled agent resolves no REST config
			// for a console it will never open. 0/0 is the main client's
			// budget through a separate limiter.
			return k8s.NewExecClients(&k8sconfig.Config{}, 0, 0)
		},
		stream.NewExecDialer(cfg, logger.New("exec-dial", logger.WithAgentID(cfg.Agent.AgentID))),
		logger.New("exec", logger.WithAgentID(cfg.Agent.AgentID)),
		mainReg,
		func(ctx context.Context, cs kubernetes.Interface) (exec.OwnPod, error) {
			return exec.ResolveOwnPod(ctx, cs, os.Getenv("POD_NAME"), os.Getenv("POD_NAMESPACE"))
		},
	)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("exec transport: %w", err)
	}
	// capabilityNodeWiring derives what the reporter sees from execOpts (what
	// buildExecResponder actually built), not from nodePolicy unconditionally
	// -- see its own doc comment for why.
	reporterNodePolicy, helperNamespace := capabilityNodeWiring(execOpts, nodePolicy)

	// One rule store, shared. The stream manager is its only writer (it is the
	// only component that sees the gateway's messages) and the log collector
	// reads it. Created here because the collectors and the manager both need
	// the same instance and neither owns it.
	rulesStore := ingestrules.NewStore()
	// One counter set, shared the same way: the collector records truncation
	// and rate limiting, the stream manager records age drops, and the
	// heartbeat reports one number per reason.
	ruleCounters := ingestrules.NewCounters()

	// The three holders below stand in for collectors that do not exist yet.
	// The collectors are built AFTER the stream manager now -- from the
	// config its first handshake resolves -- while the manager still needs
	// what the collectors carry at construction. See reconcilerHolder for
	// the measured cycle and for what each holder answers in the window
	// before it is populated.
	reconciler := &reconcilerHolder{}
	scrapeHealth := &scrapeHealthHolder{}
	capSweeper := &sweeperHolder{}

	nodeOpsPolicy, err := compileNodeOpsPolicy(cfg, log)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("node ops policy: %w", err)
	}
	nodeJobs, err := buildNodeJobResponder(
		cfg,
		nodeOpsPolicy,
		func() (kubernetes.Interface, error) {
			// Its own limiter, like the exec and mutate pools: a drain's
			// poll loop must not queue behind the informers' bucket.
			c, err := k8s.NewExecClients(&k8sconfig.Config{}, 0, 0)
			if err != nil {
				return nil, err
			}
			return c.Clientset, nil
		},
		logger.New("nodeops", logger.WithAgentID(cfg.Agent.AgentID)),
	)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("node ops engine: %w", err)
	}

	streamMgr, err := stream.New(
		cfg,
		q,
		logger.New("stream", logger.WithAgentID(cfg.Agent.AgentID)),
		agentMetrics.Stream(),
		agentMetrics.Connection(),
		reconciler,
		queryExecutor,
		mutationResponder,
		execResponder,
		rulesStore,
		ruleCounters,
		scrapeHealth,
		nodeJobs,
	)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("stream manager: %w", err)
	}
	log.Info("stream manager initialized")

	// Wire the live query policy to the platform's managed config: every time
	// the stream manager applies a new managed-block set, swap the compiled
	// query policy to match, so the executor's per-request Decide and the
	// capability reporter's per-GVR verdicts both follow it instead of
	// staying pinned to what cfg said at boot. queryPolicy.Swap and
	// policy.NewHolder above both go through policy's compileWithWarnings,
	// so a wildcard rule arriving this way logs the same warning it would
	// have logged at boot.
	//
	// The effectiveConfigSetter assertion below is required to hold, not
	// merely attempted: if it ever fails -- a future decorator or second
	// Manager implementation that stream.New starts returning -- the swap
	// never gets wired, and the query block's whole managed-config feature
	// goes silently inert for the life of the process: the executor and the
	// capability reporter both keep running on the boot-time policy
	// forever, with no error and no log line to say so. An agent that
	// starts and runs with this feature permanently dead is worse than one
	// that refuses to start, because the first failure is invisible on a
	// customer's cluster and the second is not -- so this is a boot
	// failure, not a skipped step. See
	// TestStreamManagerSatisfiesEffectiveConfigSetter, which pins that the
	// real stream.New result satisfies this interface today.
	s, ok := streamMgr.(effectiveConfigSetter)
	if !ok {
		_ = q.Close()
		return fmt.Errorf("stream manager: %T does not expose the effective-config hook; "+
			"platform-managed config would never apply to the live query policy", streamMgr)
	}
	// Same reasoning as the effectiveConfigSetter assertion just above: a
	// failed assertion here must not silently skip the restart-on-Shutdown
	// wiring. See TestStreamManagerSatisfiesShutdownSignaler, which pins
	// that the real stream.New result satisfies this interface today.
	shutdownSig, ok := streamMgr.(shutdownSignaler)
	if !ok {
		_ = q.Close()
		return fmt.Errorf("stream manager: %T does not expose the shutdown signal; "+
			"a gateway-requested restart would never take effect", streamMgr)
	}
	// The same ruling once more, for the two interfaces boot needs to build
	// the collectors from the config the platform manages: without
	// collectorConfigSetter the heartbeat reports restart_required false for
	// every revision forever, and without effectiveConfigSource the
	// collectors go back to being built from the file config -- both of them
	// silent, and both of them the exact defect this wiring exists to close.
	// See TestStreamManagerSatisfiesCollectorConfigSetter.
	collectorCfgSink, ok := streamMgr.(collectorConfigSetter)
	if !ok {
		_ = q.Close()
		return fmt.Errorf("stream manager: %T cannot be told which config the collectors were built from; "+
			"the heartbeat would report every revision as in effect", streamMgr)
	}
	cfgSource, ok := streamMgr.(effectiveConfigSource)
	if !ok {
		_ = q.Close()
		return fmt.Errorf("stream manager: %T does not expose the effective config; "+
			"the collectors would be built from the file config and the managed blocks would apply to nothing", streamMgr)
	}

	// capSweeper is a holder, not the reporter itself: the capability
	// reporter is one of the collectors, which do not exist yet. A swap
	// landing before they do has nothing to nudge -- see sweeperHolder.
	s.SetOnEffectiveConfig(newEffectiveConfigCallback(queryPolicy, capSweeper, log))

	healthAddr := cfg.Observability.HealthAddr
	metricsAddr := cfg.Observability.MetricsAddr
	if devMode {
		healthAddr = bindLocalhost(healthAddr)
		metricsAddr = bindLocalhost(metricsAddr)
	}

	healthSrv := health.New(health.HealthConfig{Addr: healthAddr}, logger.New("health"), agentMetrics.Health())
	healthSrv.Register(health.NewK8sChecker(kube))
	healthSrv.Register(health.NewQueueChecker(q, 0))
	healthSrv.Register(health.NewStreamChecker(streamMgr))
	// Registered before the server starts and satisfied only once the
	// collectors are running: the health server now starts BEFORE the boot
	// wait, and readiness must not go green for an agent that is collecting
	// nothing yet. The state-watcher checker joins it below, once there is a
	// collector to ask.
	collectorsReady := &collectorsChecker{}
	healthSrv.Register(collectorsReady)

	metricsSrv := metrics.NewServer(metricsAddr, mainReg, logger.New("metrics"))

	// nil when observability.pprof_addr is unset, which is the default. Run() on
	// a nil server is a no-op, so the disabled path needs no branch below.
	pprofSrv := agentpprof.NewServer(cfg.Observability.PprofAddr, logger.New("pprof"))

	g, ctx := errgroup.WithContext(parentCtx)

	g.Go(func() error {
		log.Info("starting health server", logger.F("addr", healthAddr))
		if err := healthSrv.Start(ctx); err != nil && !isContextClosed(err) {
			return fmt.Errorf("health server: %w", err)
		}
		log.Info("health server stopped")
		return nil
	})

	g.Go(func() error {
		log.Info("starting metrics server", logger.F("addr", metricsAddr))
		if err := metricsSrv.Run(ctx); err != nil && !isContextClosed(err) {
			return fmt.Errorf("metrics server: %w", err)
		}
		log.Info("metrics server stopped")
		return nil
	})

	g.Go(func() error {
		if err := pprofSrv.Run(ctx); err != nil && !isContextClosed(err) {
			return fmt.Errorf("pprof server: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		log.Info("starting stream manager")
		if err := streamMgr.Run(ctx); err != nil && !isContextClosed(err) {
			return fmt.Errorf("stream manager: %w", err)
		}
		log.Info("stream manager stopped")
		return nil
	})

	// A gateway Shutdown drains the agent through the exact same path a real
	// SIGTERM does: calling stop cancels rootCtx (parentCtx here), which is
	// what ctx above is derived from, so every goroutine in this group sees
	// ctx.Done() and returns exactly as it would on a rollout's SIGTERM. The
	// log line here is the only thing that lets an operator tell a
	// platform-requested restart apart from a crash when they look at a
	// restarted pod afterward.
	g.Go(func() error {
		select {
		case <-shutdownSig.ShutdownRequested():
			log.Warn("gateway requested restart; draining",
				logger.F("reason", shutdownSig.ShutdownReason()),
			)
			stop()
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	// Only now are the collectors built, and from the config the stream
	// manager's first handshake resolved rather than from the file config --
	// see startCollectors for why that ordering is the whole fix, and
	// reconcilerHolder for why the manager had to come first.
	collectors, effectiveCfg, err := startCollectors(
		ctx,
		cfgSource,
		managedConfigBootWait,
		func(c *config.Config) ([]Collector, error) {
			return buildCollectors(c, kube, q, mainReg, log, queryPolicy, mutatePolicy, execPolicy,
				reporterNodePolicy, helperNamespace, rulesStore, ruleCounters)
		},
		collectorCfgSink.SetCollectorConfig,
		log,
	)
	if err != nil {
		return unwindBootFailure(stop, g.Wait, q.Close, err)
	}
	log.Info("collection settings in effect",
		logger.F("logs_enabled", effectiveCfg.Collect.Logs.Enabled),
		logger.F("state_enabled", effectiveCfg.Collect.State.Enabled),
		logger.F("metrics_enabled", effectiveCfg.Collect.Metrics.Enabled),
	)

	// The state collector, if enabled, also satisfies stream.WatchReconciler
	// (its Reconcile method converges demand-driven informers on the
	// gateway's watch config); the metrics collector, if enabled, satisfies
	// stream.ScrapeHealthSource; and the capability reporter -- built only
	// when collect.state is enabled -- satisfies sweepRequester. All three
	// are found by type assertion rather than by extra return values from
	// buildCollectors, so that "which collector answers which gateway
	// concern" stays a property of the collector's own type.
	//
	// Each holder is resolved EXACTLY ONCE, including to nil: an agent that
	// builds no state collector has resolved "there is no reconciler", which
	// is a different answer from "not ready yet" (see reconcilerHolder).
	var (
		foundReconciler stream.WatchReconciler
		foundScrape     stream.ScrapeHealthSource
		foundCapSweeper sweepRequester
	)
	for _, coll := range collectors {
		if foundReconciler == nil {
			if r, ok := coll.(stream.WatchReconciler); ok {
				foundReconciler = r
			}
		}
		if foundScrape == nil {
			if sh, ok := coll.(stream.ScrapeHealthSource); ok {
				foundScrape = sh
			}
		}
		if foundCapSweeper == nil {
			if sw, ok := coll.(sweepRequester); ok {
				foundCapSweeper = sw
			}
		}
		if ready, ok := coll.(health.StateWatcherReady); ok {
			healthSrv.Register(health.NewStateWatcherChecker(ready))
		}
	}
	// A watch config that arrived while the holder was unresolved is
	// replayed here -- see reconcilerHolder: nothing else re-sends it, and
	// the gateway's periodic reconciler skips a set it has already
	// delivered.
	if err := reconciler.Set(ctx, foundReconciler); err != nil {
		log.Warn("replaying the watch config that arrived before the collectors existed failed",
			logger.F("error", err.Error()),
		)
	}
	scrapeHealth.Set(foundScrape)
	capSweeper.Set(foundCapSweeper)

	for _, c := range collectors {
		c := c
		g.Go(func() error {
			log.Info("starting collector", logger.F("collector", c.Name()))
			if err := c.Start(ctx); err != nil {
				return fmt.Errorf("collector %s start: %w", c.Name(), err)
			}
			<-ctx.Done()
			return ctx.Err()
		})
	}
	collectorsReady.started.Store(true)

	log.Info("agent ready",
		logger.F("health_addr", healthAddr),
		logger.F("metrics_addr", metricsAddr),
		logger.F("gateway", cfg.Gateway.Address),
	)

	runErr := g.Wait()

	log.Info("shutting down", logger.F("timeout", shutdownTimeout.String()))
	shutdownCtx, cancel := context.WithTimeout(parentCtx, shutdownTimeout)
	defer cancel()
	if errors.Is(shutdownCtx.Err(), context.Canceled) {
		var cancelTimeout context.CancelFunc
		shutdownCtx, cancelTimeout = context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancelTimeout()
	}

	if err := stopCollectors(shutdownCtx, collectors, log); err != nil {
		log.Warn("collector shutdown", logger.F("error", err))
	}

	log.Info("closing queue")
	if err := q.Close(); err != nil && !isContextClosed(err) {
		log.Warn("queue close", logger.F("error", err))
	}

	if runErr != nil && !isContextClosed(runErr) {
		return runErr
	}
	return nil
}

// compileMutatePolicy compiles the mutate policy and applies the ruling this
// task exists to enforce.
//
// mutatepolicy.Compile validates mutate.rules UNCONDITIONALLY, even when
// mutate.enabled is false -- see its own doc comment: a disabled section's
// rule must be rejected now, not silently at the moment an operator flips
// the section on later. That makes a malformed rule a real config error even
// while the section is off.
//
// But it must not be handled the way the query policy's compile error is
// handled above in serve(): the chart deploys this agent with
// restartPolicy: Always, so treating this as fatal unconditionally would
// crash-loop the WHOLE agent -- logs, metrics, live query, everything --
// over one bad rule sitting inside a section nobody has switched on yet.
//
//   - mutate.enabled true and Compile fails: the error is returned (fatal in
//     serve, exactly like the query policy) -- the operator asked for
//     mutations and cannot have them, so say so loudly.
//   - mutate.enabled false and Compile fails: the error is logged as a
//     warning naming the offending rule and swallowed; (nil, nil) is
//     returned so the agent starts with mutation disabled, same as if the
//     section were empty.
//
// Do not "simplify" this into one uniform fatal branch matching the query
// policy above -- the two sections have different blast radii for a bad
// rule, which is the whole reason this function exists instead of inlining
// the same two lines the query policy uses.
func compileMutatePolicy(cfg *config.Config, log *logger.Logger) (*mutatepolicy.Policy, error) {
	p, err := mutatepolicy.Compile(cfg)
	if err == nil {
		return p, nil
	}
	if cfg.MutateEnabled() {
		return nil, err
	}
	log.Warn("mutate policy has an invalid rule; mutation stays disabled",
		logger.F("error", err.Error()),
	)
	return nil, nil
}

// buildMutationResponder wires cfg's mutate settings into a mutate.Executor
// and hands both the built responder AND the Options it was built from back
// to the caller.
//
// It exists as its own pure function -- rather than staying inline in
// serve() -- because the wiring itself is exactly what a test that only
// covers compileMutatePolicy's branching cannot see: passing queryPolicy
// instead of mutatePolicy, or hardcoding RedactSecrets: false, both compile
// and both leave every other test green. Returning Options alongside the
// responder lets a test assert on the wiring directly -- which policy went
// in, what RedactSecrets resolved to -- without reaching into the
// executor's unexported fields.
//
// The responder is nil when mutate.enabled is false; Options is still
// returned so a caller (or a test) can see what WOULD have been built.
//
// newClients is a FACTORY, not a value, and is called only after the
// MutateEnabled gate below -- never eagerly. Building the mutate client pair
// (its own REST config resolution, dynamic client, and rate limiter) costs
// real work, and a disabled agent must not pay it for a responder it is
// about to throw away. The caller's factory is where the "why a separate
// pool" trade is explained; this function only decides WHEN to call it.
func buildMutationResponder(
	cfg *config.Config,
	mutatePolicy *mutatepolicy.Policy,
	newClients func() (*k8s.QueryClients, error),
	log *logger.Logger,
	reg prometheus.Registerer,
) (stream.MutationResponder, mutate.Options, error) {
	opts := mutate.Options{
		Policy: mutatePolicy,
		Logger: log,
		// RedactSecrets reuses the query path's setting rather than
		// inventing a second one: both answer the same question -- does a
		// secret value leave this agent. Deleting this line, or hardcoding
		// it to false, silently hands live Secret values back on every
		// mutation regardless of what the owner configured -- exactly the
		// kind of one-line regression this function's tests exist to catch.
		RedactSecrets: cfg.QueryRedactSecrets(),
		Registerer:    reg,
	}
	if !cfg.MutateEnabled() {
		return nil, opts, nil
	}
	clients, err := newClients()
	if err != nil {
		return nil, opts, fmt.Errorf("mutate clients: %w", err)
	}
	opts.Clients = *clients
	executor, err := mutate.New(opts)
	if err != nil {
		return nil, opts, err
	}
	return executor, opts, nil
}

// compileExecPolicy applies compileMutatePolicy's ruling to exec.pod:
// execpolicy.Compile validates the rules unconditionally, so a bad rule
// under an enabled section is fatal (the operator asked for consoles and
// cannot have them) while under a disabled section it is a warning naming
// the rule and a nil policy -- every *execpolicy.Policy method is nil-safe
// and denies. See compileMutatePolicy for why the two branches must stay
// different from the query policy's single fatal one.
func compileExecPolicy(cfg *config.Config, log *logger.Logger) (*execpolicy.Policy, error) {
	p, err := execpolicy.Compile(cfg)
	if err == nil {
		return p, nil
	}
	if cfg.ExecPodEnabled() {
		return nil, err
	}
	log.Warn("exec.pod policy has an invalid rule; the pod console stays disabled",
		logger.F("error", err.Error()),
	)
	return nil, nil
}

// compileNodeExecPolicy applies compileExecPolicy's ruling to exec.node.
func compileNodeExecPolicy(cfg *config.Config, log *logger.Logger) (*execpolicy.NodePolicy, error) {
	p, err := execpolicy.CompileNode(cfg)
	if err == nil {
		return p, nil
	}
	if cfg.ExecNodeEnabled() {
		return nil, err
	}
	log.Warn("exec.node policy has an invalid pattern; the node console stays disabled", logger.F("error", err.Error()))
	return nil, nil
}

// buildExecResponder wires exec.pod and exec.node into an exec.Transport.
// Nil when BOTH sections are disabled, so an agent that answers neither
// resolves no REST config and opens no gRPC connection for a feature it
// will not answer. The policies are compiled unconditionally
// (compileExecPolicy, compileNodeExecPolicy) for the reason
// compileMutatePolicy gives. Options is returned alongside for the reason
// buildMutationResponder gives: the wiring is what a test must see.
//
// resolveOwnPod is a seam over exec.ResolveOwnPod: tests substitute a stub
// so the wiring can be exercised without a real cluster.
func buildExecResponder(
	cfg *config.Config,
	execPolicy *execpolicy.Policy,
	nodePolicy *execpolicy.NodePolicy,
	newClients func() (*k8s.ExecClients, error),
	dial exec.Dialer,
	log *logger.Logger,
	reg prometheus.Registerer,
	resolveOwnPod func(ctx context.Context, cs kubernetes.Interface) (exec.OwnPod, error),
) (stream.ExecResponder, exec.Options, error) {
	opts := exec.Options{
		Policy:     execPolicy,
		Settings:   cfg.ExecPodSettings(),
		Logger:     log,
		Registerer: reg,
	}
	if !cfg.ExecPodEnabled() && !cfg.ExecNodeEnabled() {
		return nil, opts, nil
	}
	clients, err := newClients()
	if err != nil {
		return nil, opts, fmt.Errorf("exec clients: %w", err)
	}
	opts.Clients = *clients

	// exec.node shares exec.pod's Manager (its resume window comes from
	// exec.pod.resume_window_sec and its clients are the same), so a nil
	// pod policy -- compileExecPolicy's "disabled but invalid" outcome --
	// cannot carry a node console. Config.Validate already refuses this
	// shape at load (exec.pod's rules and scalars bind while exec.node is
	// on), so this is a backstop for a config that reached here another
	// way; it names the section to fix rather than letting exec.New answer
	// "policy is required".
	if execPolicy == nil && cfg.ExecNodeEnabled() {
		_, cause := execpolicy.Compile(cfg)
		return nil, opts, fmt.Errorf("exec.node is enabled but exec.pod has an invalid rule (a node console shares exec.pod's resume_window_sec and clients); fix or remove exec.pod.rules: %w", cause)
	}

	// bootCtx bounds both the own-Pod identity Get below and the helper
	// sweep after exec.New: a stuck API server must not hold up the rest of
	// startup for either call, and resolving identity deserves no less of a
	// bound than the sweep it gates. Declared here (rather than inside the
	// ExecNodeEnabled branch) so the same context, and the same 30s budget,
	// covers both call sites -- there is exactly one boot-time deadline for
	// exec.node's setup, not one per call.
	var bootCtx context.Context
	var ownNamespace string
	if cfg.ExecNodeEnabled() {
		var cancel context.CancelFunc
		bootCtx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		ns := cfg.ExecNodeSettings()
		no := &exec.NodeOptions{Policy: nodePolicy, Settings: ns}
		own, err := resolveOwnPod(bootCtx, clients.Clientset)
		switch {
		case err != nil:
			// No identity, no node console: an ownerless helper by accident
			// is exactly what the spec forbids. exec_node stays false and
			// the operator sees why.
			log.Warn("node console disabled: the agent could not resolve its own Pod",
				logger.F("error", err.Error()))
			no = nil
		case ns.Namespace == "" || ns.Namespace == own.Namespace:
			no.Owner = &own
			no.Namespace = own.Namespace
			ownNamespace = own.Namespace
		default:
			log.Warn("exec.node.namespace differs from the agent's own namespace; helper Pods there will not be garbage-collected with the agent",
				logger.F("namespace", ns.Namespace), logger.F("agent_namespace", own.Namespace))
			no.Namespace = ns.Namespace
			ownNamespace = own.Namespace
		}
		opts.Node = no
	}

	m, err := exec.New(opts)
	if err != nil {
		return nil, opts, err
	}

	if opts.Node != nil {
		// Every helper is an orphan at boot: no session survives an agent
		// restart. Best-effort, and bounded by the same bootCtx the identity
		// resolution above used, so a stuck API server never holds up the
		// rest of startup. Both the configured namespace and the agent's own
		// are swept; the list is deduped here so the log line names each
		// namespace once (SweepHelpers dedupes again and skips the empty one).
		namespaces := []string{opts.Node.Namespace}
		if ownNamespace != opts.Node.Namespace {
			namespaces = append(namespaces, ownNamespace)
		}
		removed := exec.SweepHelpers(bootCtx, opts.Clients.Clientset, namespaces, log)
		log.Info("node shell sweep complete",
			logger.F("removed", removed), logger.F("namespaces", strings.Join(namespaces, ",")))
	}

	return exec.NewTransport(m, dial, exec.Identity{
		ClusterID: cfg.Agent.ClusterID, TenantToken: cfg.Agent.TenantToken,
	}, log), opts, nil
}

// compileNodeOpsPolicy applies compileMutatePolicy's ruling to mutate.node:
// nodeops.Compile validates the section unconditionally, so a bad rule under
// an enabled section is fatal while under a disabled one it is a warning
// naming the rule and a nil policy (every *nodeops.Policy method is
// nil-safe and denies).
func compileNodeOpsPolicy(cfg *config.Config, log *logger.Logger) (*nodeops.Policy, error) {
	p, err := nodeops.Compile(cfg)
	if err == nil {
		return p, nil
	}
	if cfg.MutateNodeEnabled() {
		return nil, err
	}
	log.Warn("mutate.node has an invalid rule; node operations stay disabled",
		logger.F("error", err.Error()))
	return nil, nil
}

// buildNodeJobResponder wires mutate.node into a nodeops.Engine. Nil when
// the section is disabled, so a disabled agent resolves no REST config for
// a feature it will not answer. newClientset is a FACTORY called only past
// the enabled gate, for the reason buildMutationResponder gives.
func buildNodeJobResponder(
	cfg *config.Config,
	policy *nodeops.Policy,
	newClientset func() (kubernetes.Interface, error),
	log *logger.Logger,
) (stream.NodeJobResponder, error) {
	if !cfg.MutateNodeEnabled() {
		return nil, nil
	}
	if policy == nil {
		return nil, errors.New("mutate.node is enabled but its policy did not compile; fix mutate.node")
	}
	cs, err := newClientset()
	if err != nil {
		return nil, fmt.Errorf("node ops clients: %w", err)
	}
	s := cfg.MutateNodeSettings()
	own := nodeops.OwnPodFromEnv()
	if own.Name == "" {
		log.Warn("POD_NAME/POD_NAMESPACE are not set; a drain of this agent's own node will evict the agent itself")
	}
	if !policy.AllowsAnyNode() {
		log.Warn("mutate.node is on but matches no node (nodes is empty)")
	}
	eng, err := nodeops.New(nodeops.Options{
		Clientset:  cs,
		Policy:     policy,
		Own:        own,
		MaxTimeout: time.Duration(s.MaxTimeoutSec) * time.Second,
		Logger:     log,
	})
	if err != nil {
		return nil, err
	}
	// Never return a typed-nil *nodeops.Engine through the interface: that
	// would make m.nodeJobs != nil true in the manager, advertising
	// node_ops=true for a responder that answers nothing.
	return eng, nil
}

// capabilityNodeWiring derives the capability reporter's node-exec inputs
// from execOpts -- the exec.Options buildExecResponder actually built --
// rather than forwarding nodePolicy unconditionally. execOpts.Node is nil
// not only when exec.node is off, but also when it is ON and the agent
// could not resolve its own Pod (see buildExecResponder's identity-failure
// branch); in that second case the handshake's exec_node capability already
// reports false (nodeConsoleReady reads exec.Manager.NodeReady, which is
// gated the same way, through opts.Node), so the capability reporter must
// see the same "not answering" fact. Forwarding nodePolicy unconditionally
// here would let the "nodes" catalog entry answer policy_exec=true and issue
// its two SSARs cluster-wide (helperNamespace empty, since it too must come
// from execOpts.Node) while the handshake says exec_node=false -- the
// catalog and the handshake would disagree about a console that is, in
// fact, down.
func capabilityNodeWiring(execOpts exec.Options, nodePolicy *execpolicy.NodePolicy) (*execpolicy.NodePolicy, string) {
	if execOpts.Node == nil {
		return nil, ""
	}
	return nodePolicy, execOpts.Node.Namespace
}

// buildCapabilityReporterOptions builds the Options capability.NewReporter is
// called with. It exists as its own pure function -- rather than staying
// inline in buildCollectors -- for the same reason buildMutationResponder
// above does: the wiring itself (dropping MutatePolicy, or handing it the
// wrong policy) is exactly what a test on buildCollectors's return value
// cannot see, since Reporter keeps its policy fields unexported. Returning
// Options lets a test assert on the wiring directly -- which policy landed
// in which field -- without reaching into the reporter itself.
func buildCapabilityReporterOptions(
	cfg *config.Config,
	probeCS kubernetes.Interface,
	q queue.Queue,
	// queryPolicy is the live Holder, not a *policy.Policy snapshot: Holder
	// satisfies capability.PolicySource directly (AllowsAnyList/AllowsAnyGet
	// delegate to Current()), so the reporter's per-GVR verdicts follow a
	// managed-config swap instead of freezing at their boot values.
	queryPolicy *policy.Holder,
	mutatePolicy *mutatepolicy.Policy,
	execPolicy *execpolicy.Policy,
	nodeExecPolicy *execpolicy.NodePolicy,
	helperNamespace string,
) capability.Options {
	return capability.Options{
		Clientset: probeCS,
		Writer:    state.NewQueueWriter(q, state.ConfigFromRoot(cfg).WriteTimeout),
		AgentMeta: &commonv1.AgentMetadata{
			ClusterId: cfg.Agent.ClusterID,
			AgentId:   cfg.Agent.AgentID,
		},
		Logger: logger.New("capability-reporter", logger.WithAgentID(cfg.Agent.AgentID)),
		Policy: queryPolicy,
		// MutatePolicy is a second, independent policy source from Policy
		// above (see capability.Options's own doc comment) -- it must be
		// wired here or can_patch/can_delete/can_create/policy_* report
		// false for every resource forever, whatever mutate.rules says.
		MutatePolicy: mutatePolicy,
		// ExecPolicy is a THIRD, independent policy source from both Policy
		// and MutatePolicy above -- it must be wired here or can_exec/
		// policy_exec report false for the pods entry forever, whatever
		// exec.pod.rules says. A disabled or invalid exec.pod section
		// compiles to a nil *execpolicy.Policy (see compileExecPolicy),
		// which assigning to this interface field yields a non-nil
		// ExecPolicySource holding a nil pointer -- not a nil interface.
		// That is harmless here: every *execpolicy.Policy method, including
		// AllowsAnyPod, is nil-safe and answers false/denied, exactly the
		// "not configured" answer this field is supposed to give. Do not add
		// a nil check on execPolicy here to "fix" this: it would just mask
		// the same harmless typed-nil shape mutatePolicy above already has.
		ExecPolicy: execPolicy,
		// NodeExecPolicy is a FOURTH, independent policy source, mirroring
		// ExecPolicy immediately above for exec.node instead of exec.pod: it
		// must be wired here or the "nodes" entry's can_exec/policy_exec
		// report false forever, whatever exec.node.nodes says. A disabled
		// but valid exec.node section compiles to a NON-nil
		// *execpolicy.NodePolicy whose AllowsAnyNode answers false (see
		// compileNodeExecPolicy); nil arrives only from the invalid+disabled
		// branch there, which is the same harmless typed-nil shape
		// ExecPolicy's own comment describes -- either way AllowsAnyNode is
		// nil-receiver-safe. The caller also passes nil here whenever
		// exec.node ended up unable to answer at all -- identity resolution
		// failed even though the section is enabled -- so this field agrees
		// with HelperNamespace below and with the handshake's exec_node
		// about whether the console is actually up, not just configured.
		NodeExecPolicy: nodeExecPolicy,
		// HelperNamespace scopes the "nodes" SSARs to where helper Pods are
		// actually created. Empty whenever exec.node is not answering --
		// disabled, invalid, or (see serve()) an own-Pod identity failure --
		// in which case probeExec never issues them (nodeExecPolicy is nil
		// in the same cases).
		HelperNamespace: helperNamespace,
	}
}

func buildCollectors(
	cfg *config.Config,
	kube k8s.Client,
	q queue.Queue,
	reg prometheus.Registerer,
	log *logger.Logger,
	queryPolicy *policy.Holder,
	mutatePolicy *mutatepolicy.Policy,
	execPolicy *execpolicy.Policy,
	nodeExecPolicy *execpolicy.NodePolicy,
	helperNamespace string,
	rules *ingestrules.Store,
	counters *ingestrules.Counters,
) ([]Collector, error) {
	var collectors []Collector

	if cfg.Collect.Logs.Enabled {
		logColl, err := logs.New(logs.Options{
			Config: logs.ConfigFromRoot(cfg),
			Kube:   kube,
			Queue:  q,
			AgentMeta: &commonv1.AgentMetadata{
				ClusterId: cfg.Agent.ClusterID,
				AgentId:   cfg.Agent.AgentID,
			},
			Logger:     logger.New("log-collector", logger.WithAgentID(cfg.Agent.AgentID)),
			Registerer: reg,
			// The stream manager is the store's only writer; the collector
			// reads whatever the gateway last pushed.
			Rules:    rules,
			Counters: counters,
		})
		if err != nil {
			return nil, err
		}
		collectors = append(collectors, logColl)
	}

	if cfg.Collect.State.Enabled {
		stateColl, err := state.New(state.Options{
			Config: state.ConfigFromRoot(cfg),
			Kube:   kube,
			Queue:  q,
			AgentMeta: &commonv1.AgentMetadata{
				ClusterId: cfg.Agent.ClusterID,
				AgentId:   cfg.Agent.AgentID,
			},
			Logger:     logger.New("state-watcher", logger.WithAgentID(cfg.Agent.AgentID)),
			Registerer: reg,
		})
		if err != nil {
			return nil, err
		}
		collectors = append(collectors, stateColl)

		// Zero QPS/burst means "take the same budget as the main client"
		// (k8sconfig.DefaultQPS/DefaultBurst) while still getting a SEPARATE
		// rate limiter — which is the whole point of a second clientset here:
		// a several-hundred-GVR sweep must not drain the token bucket the
		// informers share. Throttling the sweep itself buys nothing. An
		// earlier 5 QPS / 10 burst made a ~400-review sweep take over a
		// minute and filled the log with client-side throttling warnings.
		probeCS, err := k8s.NewProbeClientset(&k8sconfig.Config{}, 0, 0)
		if err != nil {
			return nil, err
		}
		capReporter, err := capability.NewReporter(
			buildCapabilityReporterOptions(cfg, probeCS, q, queryPolicy, mutatePolicy, execPolicy, nodeExecPolicy, helperNamespace),
		)
		if err != nil {
			return nil, err
		}
		collectors = append(collectors, capReporter)
	}

	if cfg.Collect.Metrics.Enabled {
		metricsColl, err := metricscollector.New(metricscollector.Options{
			Config: metricscollector.ConfigFromRoot(cfg),
			Kube:   kube,
			Queue:  q,
			AgentMeta: &commonv1.AgentMetadata{
				ClusterId: cfg.Agent.ClusterID,
				AgentId:   cfg.Agent.AgentID,
			},
			Logger:     logger.New("metrics-scraper", logger.WithAgentID(cfg.Agent.AgentID)),
			Registerer: reg,
		})
		if err != nil {
			return nil, err
		}
		collectors = append(collectors, metricsColl)
	}

	return collectors, nil
}

func initKubernetes(ctx context.Context, cfg *config.Config, devMode bool, log *logger.Logger) (k8s.Client, error) {
	kube, err := k8s.New(&k8sconfig.Config{}, logger.New("k8s"))
	if err != nil {
		return nil, err
	}

	inCluster := runningInCluster()
	if inCluster && !devMode {
		if err := kube.Ready(ctx); err != nil {
			return nil, fmt.Errorf("in-cluster readiness: %w", err)
		}
		return kube, nil
	}

	if err := kube.Ready(ctx); err != nil {
		if inCluster {
			return nil, fmt.Errorf("in-cluster readiness: %w", err)
		}
		log.Warn("kubernetes readiness check failed (continuing outside cluster)",
			logger.F("error", err),
			logger.F("dev", devMode),
		)
	}
	return kube, nil
}

func runningInCluster() bool {
	_, err := rest.InClusterConfig()
	return err == nil
}

func stopCollectors(ctx context.Context, collectors []Collector, log *logger.Logger) error {
	var first error
	for i := len(collectors) - 1; i >= 0; i-- {
		c := collectors[i]
		log.Info("stopping collector", logger.F("collector", c.Name()))
		if err := c.Stop(ctx); err != nil {
			if first == nil && !isContextClosed(err) {
				first = fmt.Errorf("collector %s: %w", c.Name(), err)
			}
			log.Warn("collector stop failed",
				logger.F("collector", c.Name()),
				logger.F("error", err),
			)
		}
	}
	return first
}

func bindLocalhost(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		if len(addr) > 0 && addr[0] == ':' {
			return "127.0.0.1" + addr
		}
		return "127.0.0.1:8080"
	}
	if host == "" || host == "0.0.0.0" {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return addr
}

func isContextClosed(err error) bool {
	if err == nil {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// printConfigWarnings reports unrecognized config keys on the paths that exit
// before the logger exists -- the logger is built from this same config, so
// every failure between reading it and constructing the logger would otherwise
// swallow the warnings. An unknown key is a plausible reason for such a
// failure, so it goes out ahead of the error rather than with it.
func printConfigWarnings(warnings []string) {
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "config: unrecognized key ignored: %s\n", w)
	}
}

// validateConfigAndExit runs the config file through the same parse-and-
// validate path Load uses -- not a re-implementation of it -- and reports the
// result without starting the agent. It exists so a rendered chart can be
// proven against the real loader instead of only against a YAML parser:
// rendering is not booting, and a config that is merely well-formed YAML can
// still be a duration the agent's decoder refuses or an interval/timeout pair
// its own validate() rejects.
//
// Never prints the config itself: agent.tenant_token and a custom endpoint's
// bearer_token_path name a secret's location, and LoadWithWarnings's error and
// warning strings are already scrubbed to field names, not values.
func validateConfigAndExit(configPath string) int {
	_, warnings, err := config.LoadWithWarnings(configPath)
	printConfigWarnings(warnings)
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate config: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(os.Stdout, "config is valid")
	return 0
}

func printBanner(w interface{ Write([]byte) (int, error) }) {
	_, _ = fmt.Fprintf(w, `
'##:::'##:'##::::'##:'########::'########:'##::::'##::::'###::::
 ##::'##:: ##:::: ##: ##.... ##: ##.....::. ##::'##::::'## ##:::
 ##:'##::: ##:::: ##: ##:::: ##: ##::::::::. ##'##::::'##:. ##::
 #####:::: ##:::: ##: ########:: ######:::::. ###::::'##:::. ##:
 ##. ##::: ##:::: ##: ##.... ##: ##...:::::: ## ##::: #########:
 ##:. ##:: ##:::: ##: ##:::: ##: ##:::::::: ##:. ##:: ##.... ##:
 ##::. ##:. #######:: ########:: ########: ##:::. ##: ##:::: ##:
..::::..:::.......:::........:::........::..:::::..::..:::::..::

%s (%s) %s

`, buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime)
}
