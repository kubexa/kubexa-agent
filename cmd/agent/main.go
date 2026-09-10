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
	"github.com/kubexa/kubexa-agent/internal/health"
	"github.com/kubexa/kubexa-agent/internal/ingestrules"
	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/k8s/k8sconfig"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/internal/metrics"
	"github.com/kubexa/kubexa-agent/internal/mutate"
	mutatepolicy "github.com/kubexa/kubexa-agent/internal/mutate/policy"
	agentpprof "github.com/kubexa/kubexa-agent/internal/pprof"
	"github.com/kubexa/kubexa-agent/internal/query"
	"github.com/kubexa/kubexa-agent/internal/query/policy"
	"github.com/kubexa/kubexa-agent/internal/queue"
	"github.com/kubexa/kubexa-agent/internal/stream"
	"github.com/kubexa/kubexa-agent/pkg/buildinfo"
	"github.com/kubexa/kubexa-agent/pkg/config"
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

	if err := serve(rootCtx, cfg, devMode, rootLog); err != nil {
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

func serve(parentCtx context.Context, cfg *config.Config, devMode bool, log *logger.Logger) error {
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

	log.Info("collection settings",
		logger.F("logs_enabled", cfg.Collect.Logs.Enabled),
		logger.F("state_enabled", cfg.Collect.State.Enabled),
		logger.F("metrics_enabled", cfg.Collect.Metrics.Enabled),
	)

	// Compiled once and shared: the executor enforces it per request, and the
	// capability reporter publishes its per-GVR verdict to the UI.
	queryPolicy, err := policy.Compile(cfg)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("query policy: %w", err)
	}

	// A wildcard rule permits every resource, secrets among them. Paired with
	// visible Secret values it is the widest read policy the agent can hold,
	// and an operator must not have to discover that from a screen.
	if ids := queryPolicy.UnredactedWildcardRuleIDs(); len(ids) > 0 {
		log.Warn("live query policy permits every resource and Secret values are not redacted",
			logger.F("rules", strings.Join(ids, ",")),
			logger.F("remedy", "set query.redact_secrets: true, or name resources explicitly"),
		)
	}

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

	// One rule store, shared. The stream manager is its only writer (it is the
	// only component that sees the gateway's messages) and the log collector
	// reads it. Created here because the collectors are built before the
	// manager and both need the same instance.
	rulesStore := ingestrules.NewStore()
	// One counter set, shared the same way: the collector records truncation
	// and rate limiting, the stream manager records age drops, and the
	// heartbeat reports one number per reason.
	ruleCounters := ingestrules.NewCounters()

	collectors, err := buildCollectors(cfg, kube, q, mainReg, log, queryPolicy, mutatePolicy, rulesStore, ruleCounters)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("collectors: %w", err)
	}

	// The state collector, if enabled, also satisfies stream.WatchReconciler
	// (its Reconcile method converges demand-driven informers on the
	// gateway's watch config). Found by type assertion rather than a second
	// return value from buildCollectors so that "which collector applies
	// gateway config" stays a property of the collector's own type, not
	// something the caller has to track separately.
	var reconciler stream.WatchReconciler
	// The metrics collector, if enabled, also satisfies
	// stream.ScrapeHealthSource. Found the same way as reconciler above: an
	// agent built without metrics collection has no collector to satisfy it,
	// so scrapeHealth stays nil and the heartbeat reports no scrape_targets
	// at all rather than a fabricated empty "everything is healthy".
	var scrapeHealth stream.ScrapeHealthSource
	for _, coll := range collectors {
		if reconciler == nil {
			if r, ok := coll.(stream.WatchReconciler); ok {
				reconciler = r
			}
		}
		if scrapeHealth == nil {
			if s, ok := coll.(stream.ScrapeHealthSource); ok {
				scrapeHealth = s
			}
		}
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
		rulesStore,
		ruleCounters,
		scrapeHealth,
	)
	if err != nil {
		_ = q.Close()
		return fmt.Errorf("stream manager: %w", err)
	}
	log.Info("stream manager initialized")

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
	for _, coll := range collectors {
		if ready, ok := coll.(health.StateWatcherReady); ok {
			healthSrv.Register(health.NewStateWatcherChecker(ready))
		}
	}

	metricsSrv := metrics.NewServer(metricsAddr, mainReg, logger.New("metrics"))

	// nil when observability.pprof_addr is unset, which is the default. Run() on
	// a nil server is a no-op, so the disabled path needs no branch below.
	pprofSrv := agentpprof.NewServer(cfg.Observability.PprofAddr, logger.New("pprof"))

	g, ctx := errgroup.WithContext(parentCtx)

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
	queryPolicy *policy.Policy,
	mutatePolicy *mutatepolicy.Policy,
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
	}
}

func buildCollectors(
	cfg *config.Config,
	kube k8s.Client,
	q queue.Queue,
	reg prometheus.Registerer,
	log *logger.Logger,
	queryPolicy *policy.Policy,
	mutatePolicy *mutatepolicy.Policy,
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
			buildCapabilityReporterOptions(cfg, probeCS, q, queryPolicy, mutatePolicy),
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
