package config_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kubexa/kubexa-agent/internal/query/policy"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

func TestValidateBlockRefusesAnUnknownKey(t *testing.T) {
	// The 9 GiB/day shape: podNames instead of pod_names leaves the rule with
	// no pod filter at all, so it collects the whole namespace and reads as an
	// over-broad rule rather than a typo. The agent's file loader only warns;
	// a PUT has a human present and must refuse.
	err := config.ValidateBlock("collect.logs", []byte("rules:\n  - namespace: default\n    podNames: [web]\n"))
	if err == nil {
		t.Fatal("podNames must be refused")
	}
	if !strings.Contains(err.Error(), "podNames") {
		t.Fatalf("the error must name the key; got %v", err)
	}
}

func TestValidateBlockAcceptsTheAgentsOwnSpelling(t *testing.T) {
	if err := config.ValidateBlock("collect.logs", []byte("rules:\n  - namespace: default\n    pod_names: [web]\n")); err != nil {
		t.Fatalf("pod_names must be accepted: %v", err)
	}
}

func TestValidateBlockKeepsADurationString(t *testing.T) {
	// yaml.v3 parses "30s" into time.Duration; a JSON round trip that turned
	// it into 30 (nanoseconds) would silently resync 30ns.
	cfg, err := config.ApplyBlocks(validBase(t), map[string][]byte{
		"collect.state": []byte("enabled: true\nresync_period: 30s\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Collect.State.ResyncPeriod != 30*time.Second {
		t.Fatalf("resync = %v, want 30s", cfg.Collect.State.ResyncPeriod)
	}
}

func TestApplyBlocksLeavesUnmanagedBlocksAlone(t *testing.T) {
	base := validBase(t)
	base.Collect.Logs.TailLines = 4242
	cfg, err := config.ApplyBlocks(base, map[string][]byte{"query": []byte("enabled: true\n")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Collect.Logs.TailLines != 4242 {
		t.Fatal("a query-only overlay must not touch collect.logs")
	}
}

func TestApplyBlocksIsAllOrNothing(t *testing.T) {
	base := validBase(t)
	_, err := config.ApplyBlocks(base, map[string][]byte{
		"query":         []byte("enabled: true\n"),
		"collect.state": []byte("resync_period: not-a-duration\n"),
	})
	if err == nil {
		t.Fatal("one bad block must fail the whole overlay")
	}
}

func TestApplyBlocksKeepsFieldsYAMLDoesNotCarry(t *testing.T) {
	// Agent.ClusterID is yaml:"-": it is set at runtime from the kube-system
	// namespace UID and never written to the file. An effective config with
	// no cluster id is the kind of loss that reads as a different bug
	// entirely, several layers away.
	base := validBase(t)
	base.Agent.ClusterID = "cluster-abc"
	cfg, err := config.ApplyBlocks(base, map[string][]byte{"query": []byte("enabled: true\n")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.ClusterID != "cluster-abc" {
		t.Fatalf("cluster id = %q, want it preserved", cfg.Agent.ClusterID)
	}
}

func TestApplyBlocksKeepsUNSENTKeysInTheSameBlock(t *testing.T) {
	// A block body carries only the keys E3 edits. Saving batch_size must not
	// reset max_disk_bytes, which is deliberately NOT editable because
	// mis-setting it means an OOM and a deadlocked WAL -- both of which have
	// happened to this agent.
	base := validBase(t)
	base.Buffer.MaxDiskBytes = 7 << 30
	base.Buffer.SpillDir = "/var/lib/kubexa/spill"
	cfg, err := config.ApplyBlocks(base, map[string][]byte{
		"buffer": []byte("batch_size: 750\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Buffer.BatchSize != 750 {
		t.Fatalf("batch_size = %d, want 750", cfg.Buffer.BatchSize)
	}
	if cfg.Buffer.MaxDiskBytes != 7<<30 {
		t.Fatalf("max_disk_bytes = %d, want the operator's value kept", cfg.Buffer.MaxDiskBytes)
	}
	if cfg.Buffer.SpillDir != "/var/lib/kubexa/spill" {
		t.Fatalf("spill_dir = %q, want the operator's value kept", cfg.Buffer.SpillDir)
	}
}

func TestApplyBlocksDoesNotMutateBase(t *testing.T) {
	base := validBase(t)
	base.Buffer.BatchSize = 100
	if _, err := config.ApplyBlocks(base, map[string][]byte{"buffer": []byte("batch_size: 750\n")}); err != nil {
		t.Fatal(err)
	}
	if base.Buffer.BatchSize != 100 {
		t.Fatal("base must be left untouched; releasing a block restores it")
	}
}

func TestApplyBlocksDoesNotMutateBaseThroughASharedPointer(t *testing.T) {
	// cfg := *base is a shallow struct copy: pointer-typed fields (like
	// QueryConfig.Enabled) still point at base's own memory. yaml.v3 decodes
	// into an existing non-nil pointer by writing THROUGH it rather than
	// allocating a new one, so applying a query block with enabled: false
	// used to flip base's own Query.Enabled -- releasing the block would then
	// restore the platform's value, not the operator's file value.
	base := validBase(t)
	enabled := true
	base.Query.Enabled = &enabled

	if _, err := config.ApplyBlocks(base, map[string][]byte{"query": []byte("enabled: false\n")}); err != nil {
		t.Fatal(err)
	}
	if base.Query.Enabled == nil || !*base.Query.Enabled {
		t.Fatal("base.Query.Enabled must not be mutated through a shared pointer")
	}
}

// yamlDashFieldPaths returns the dotted field path (e.g. "Agent.ClusterID")
// of every field of t, at any depth, tagged yaml:"-". It exists so the test
// below does not have to name each such field: a second one added years from
// now is covered automatically, which a test naming only ClusterID would not
// be.
func yamlDashFieldPaths(t reflect.Type, prefix string) []string {
	var paths []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported; ApplyBlocks cannot reach it either
			continue
		}
		path := f.Name
		if prefix != "" {
			path = prefix + "." + f.Name
		}
		if f.Tag.Get("yaml") == "-" {
			paths = append(paths, path)
		}
		if f.Type.Kind() == reflect.Struct {
			paths = append(paths, yamlDashFieldPaths(f.Type, path)...)
		}
	}
	return paths
}

func fieldByPath(v reflect.Value, path string) reflect.Value {
	for _, part := range strings.Split(path, ".") {
		v = v.FieldByName(part)
	}
	return v
}

func TestApplyBlocksPreservesEveryFieldYAMLDoesNotCarry(t *testing.T) {
	paths := yamlDashFieldPaths(reflect.TypeOf(config.Config{}), "")
	if len(paths) == 0 {
		t.Fatal("expected at least one yaml:\"-\" field (Agent.ClusterID) to walk")
	}

	base := validBase(t)
	rv := reflect.ValueOf(base).Elem()
	want := make(map[string]any, len(paths))
	for _, p := range paths {
		fv := fieldByPath(rv, p)
		if !fv.CanSet() {
			t.Fatalf("field %s is not settable via reflection", p)
		}
		switch fv.Kind() {
		case reflect.String:
			fv.SetString("sentinel-" + p)
		case reflect.Bool:
			fv.SetBool(!fv.Bool())
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			fv.SetInt(987654321)
		default:
			t.Fatalf("field %s has kind %s; extend this test to set and compare it", p, fv.Kind())
		}
		want[p] = fv.Interface()
	}

	cfg, err := config.ApplyBlocks(base, map[string][]byte{"query": []byte("enabled: true\n")})
	if err != nil {
		t.Fatal(err)
	}

	rv2 := reflect.ValueOf(cfg).Elem()
	for _, p := range paths {
		got := fieldByPath(rv2, p).Interface()
		if got != want[p] {
			t.Fatalf("field %s = %v, want %v (base's value, preserved)", p, got, want[p])
		}
	}
}

func TestValidateBlockRefusesRedactSecretsInCollectState(t *testing.T) {
	// redact_secrets is the cluster owner's guarantee that Secret values do
	// not leave their cluster. It must not become settable through this
	// block just because StateCollectConfig has a field for it: the whole
	// point of the agent enforcing its own allowlist is that the platform
	// cannot open this from its own side.
	err := config.ValidateBlock("collect.state", []byte("redact_secrets: true\n"))
	if err == nil {
		t.Fatal("redact_secrets must not be editable via collect.state")
	}
	if !strings.Contains(err.Error(), "redact_secrets") {
		t.Fatalf("the error must name the key; got %v", err)
	}
}

func TestValidateBlockRefusesRedactSecretsInQuery(t *testing.T) {
	err := config.ValidateBlock("query", []byte("redact_secrets: true\n"))
	if err == nil {
		t.Fatal("redact_secrets must not be editable via query")
	}
	if !strings.Contains(err.Error(), "redact_secrets") {
		t.Fatalf("the error must name the key; got %v", err)
	}
}

func TestValidateBlockRefusesCustomEndpointsInCollectMetrics(t *testing.T) {
	err := config.ValidateBlock("collect.metrics", []byte("custom_endpoints: []\n"))
	if err == nil {
		t.Fatal("custom_endpoints must not be editable via collect.metrics")
	}
	if !strings.Contains(err.Error(), "custom_endpoints") {
		t.Fatalf("the error must name the key; got %v", err)
	}
}

func TestValidateBlockDoesNotRequireTheWholeConfig(t *testing.T) {
	// A one-block document has no agent.tenant_token and no gateway.address.
	// Validating it through the whole-config validator would refuse every
	// valid block for reasons outside the block.
	if err := config.ValidateBlock("query", []byte("enabled: true\n")); err != nil {
		t.Fatalf("a valid query block must not be refused: %v", err)
	}
}

func TestValidateBlockStillCatchesABlockLevelViolation(t *testing.T) {
	if err := config.ValidateBlock("buffer", []byte("batch_size: 0\n")); err == nil {
		t.Fatal("batch_size 0 must be refused")
	}
}

// TestValidateBlockRefusesAnExplicitlyEmptyRulesList pins the fix for the
// whole-branch review's Critical: `rules: []` used to be accepted and then
// silently widened by two different consumers -- QueryRules inherits
// collect.state's rules at len()==0, and normalize() refills an empty list
// with a permissive default for logs and state. Both are worse than what the
// operator asked for, so the block must be refused before either can run.
//
// Two YAML spellings resolve to the same nil slice and the same widening:
// `rules: []` (an empty sequence) and a bare `rules:` with no value (a null
// scalar). Only rejecting the first would leave the quieter spelling open.
func TestValidateBlockRefusesAnExplicitlyEmptyRulesList(t *testing.T) {
	blocks := []string{"collect.logs", "collect.state", "collect.metrics", "query"}
	spellings := map[string]string{
		"empty sequence": "rules: []\n",
		"null scalar":    "rules:\n",
	}
	for _, key := range blocks {
		for name, body := range spellings {
			t.Run(key+"/"+name, func(t *testing.T) {
				err := config.ValidateBlock(key, []byte(body))
				if err == nil {
					t.Fatalf("%s must be refused for %s", name, key)
				}
				if !strings.Contains(err.Error(), key) {
					t.Fatalf("error must name the block %q; got %v", key, err)
				}
				if !strings.Contains(err.Error(), "enabled: false") {
					t.Fatalf("error must state the remedy (enabled: false); got %v", err)
				}
			})
		}
	}
}

// TestValidateBlockOmittedRulesIsStillAccepted guards against an
// overcorrection: refusing an empty rules key must not start refusing a body
// that never mentions rules at all -- that spelling means "leave the current
// rules alone" under the patch rule and must keep working.
func TestValidateBlockOmittedRulesIsStillAccepted(t *testing.T) {
	if err := config.ValidateBlock("collect.state", []byte("enabled: true\n")); err != nil {
		t.Fatalf("an omitted rules key must still be accepted: %v", err)
	}
}

// TestValidateBlockEnabledFalseWithNoRulesIsAccepted pins the ruling's other
// half: enabled: false is the spelling for "collect nothing" and must keep
// working with no rules key present at all, since the error message directs
// the operator to exactly this as the remedy.
func TestValidateBlockEnabledFalseWithNoRulesIsAccepted(t *testing.T) {
	if err := config.ValidateBlock("collect.state", []byte("enabled: false\n")); err != nil {
		t.Fatalf("enabled: false with no rules must still be accepted: %v", err)
	}
}

// TestApplyBlocksOmittedRulesLeavesBaseRulesIntact is the ApplyBlocks-level
// half of the fix: a block body that edits another key in the same block
// (resync_period here) but says nothing about rules must not disturb the
// base's own rules -- that is the merge guarantee TestApplyBlocksKeepsUNSENT-
// KeysInTheSameBlock already pins for buffer, restated for the field this
// Critical was actually about.
func TestApplyBlocksOmittedRulesLeavesBaseRulesIntact(t *testing.T) {
	base := validBase(t)
	base.Collect.State.Rules = []config.StateNamespaceRule{
		{Namespace: "prod", Resources: []string{"pods"}},
	}
	cfg, err := config.ApplyBlocks(base, map[string][]byte{
		"collect.state": []byte("enabled: true\nresync_period: 45s\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Collect.State.Rules) != 1 || cfg.Collect.State.Rules[0].Namespace != "prod" {
		t.Fatalf("rules = %+v, want the base's narrowed rule kept", cfg.Collect.State.Rules)
	}
}

// TestApplyBlocksRefusesAnExplicitlyEmptyRulesList is the reported shape from
// the review, at the ApplyBlocks level rather than ValidateBlock's: a
// narrowed collect.state (secrets excluded, one namespace) must not be
// replaceable by `rules: []` and come back cluster-wide on every resource,
// including secrets that redact_secrets: false returns unredacted.
func TestApplyBlocksRefusesAnExplicitlyEmptyRulesList(t *testing.T) {
	base := validBase(t)
	base.Collect.State.Rules = []config.StateNamespaceRule{
		{Namespace: "default", Resources: []string{"pods"}},
	}
	_, err := config.ApplyBlocks(base, map[string][]byte{
		"collect.state": []byte("enabled: true\nrules: []\n"),
	})
	if err == nil {
		t.Fatal("rules: [] must be refused, not silently widen to the inherited default")
	}
}

// validBase returns a config that passes Config.Validate, which ApplyBlocks
// runs on the effective result. Built through LoadFromYAML so it cannot
// drift from what a real agent loads.
func validBase(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadFromYAML([]byte(
		"agent:\n  tenant_token: t\ngateway:\n  address: gw:443\ncollect:\n  state:\n    enabled: true\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestAPushedCollectStateChangesTheLiveQueryPolicy pins the coupling C-A
// found: QueryRules() (query.go:304) inherits collect.state.rules whenever
// query.rules is empty -- the shipped chart's default, since
// helm/kubexa-agent/values.yaml leaves query.rules commented out -- so a
// pushed collect.state block reaches the compiled query policy the moment
// applyManaged swaps the holder, with no restart. If this test ever fails
// because the inheritance was removed, the classification (effectPartial on
// collect.state) must move with it, not this test.
func TestAPushedCollectStateChangesTheLiveQueryPolicy(t *testing.T) {
	base, err := config.LoadFromYAML([]byte(
		"agent:\n  tenant_token: t\ngateway:\n  address: gw:443\n" +
			"collect:\n  state:\n    enabled: true\n    rules:\n" +
			"      - namespace: default\n        resources: [pods]\n",
	))
	if err != nil {
		t.Fatal(err)
	}

	basePolicy, err := policy.Compile(base)
	if err != nil {
		t.Fatalf("Compile(base): %v", err)
	}
	secretsRef := policy.Ref{Version: "v1", Resource: "secrets"}
	if d := basePolicy.Decide(secretsRef, policy.VerbList, "kube-system", ""); d.Allowed {
		t.Fatal("setup: the narrow base must not already permit listing secrets")
	}

	widened, err := config.ApplyBlocks(base, map[string][]byte{
		"collect.state": []byte("enabled: true\nrules:\n  - resources: [secrets]\n"),
	})
	if err != nil {
		t.Fatalf("ApplyBlocks: %v", err)
	}

	widenedPolicy, err := policy.Compile(widened)
	if err != nil {
		t.Fatalf("Compile(widened): %v", err)
	}
	if d := widenedPolicy.Decide(secretsRef, policy.VerbList, "kube-system", ""); !d.Allowed {
		t.Fatalf("a pushed collect.state must widen the live query policy with no restart; decision = %+v", d)
	}

	// The two facts together are the finding: the live query read already
	// changed, and the agent still (correctly) says a restart is pending --
	// because the informers really are still running the narrow rule.
	if got := config.RestartRequiredBlocks(base, widened); !slices.Equal(got, []string{"collect.state"}) {
		t.Fatalf("RestartRequiredBlocks = %v, want [collect.state]", got)
	}
}

func TestManagedBlockKeysIsTheClosedSet(t *testing.T) {
	want := []string{"collect.logs", "collect.state", "collect.metrics", "query", "buffer"}
	if !slices.Equal(config.ManagedBlockKeys(), want) {
		t.Fatalf("keys = %v, want %v", config.ManagedBlockKeys(), want)
	}
	if config.IsManagedBlockKey("mutate") {
		t.Fatal("mutate is never a managed block")
	}
}

// TestEveryManagedBlockIsClassifiedByWhenItTakesEffect is the literal the
// whole restart_required report rests on. It is spelled out here -- rather
// than derived from the map it checks, which would agree with any value --
// so that MOVING a block between categories cannot happen quietly: the
// person moving it has to come here and say so, and its sibling in
// internal/stream (TestHeartbeatRestartRequiredFollowsTheBlockClassification)
// fails until the heartbeat follows.
//
// Updated deliberately (Task 7e, C-A/M-B): collect.logs and collect.state
// are effectPartial, not effectRestart -- each feeds one live consumer and
// one construction-time one, and reporting either as purely "restart-only"
// was the defect a whole-branch review measured end to end.
func TestEveryManagedBlockIsClassifiedByWhenItTakesEffect(t *testing.T) {
	want := map[string]config.BlockEffect{
		"collect.logs":    config.EffectPartial,
		"collect.state":   config.EffectPartial,
		"collect.metrics": config.EffectRestart,
		"query":           config.EffectLive,
		"buffer":          config.EffectLive,
	}
	for _, key := range config.ManagedBlockKeys() {
		effect, ok := config.ManagedBlockEffect(key)
		if !ok {
			t.Errorf("managed block %q is not classified", key)
			continue
		}
		if effect != want[key] {
			t.Errorf("ManagedBlockEffect(%q) = %v, want %v -- if this block really "+
				"changed category, the heartbeat's restart_required must follow it",
				key, effect, want[key])
		}
	}
	if _, ok := config.ManagedBlockEffect("mutate"); ok {
		t.Fatal("mutate is not a managed block and must not be classified")
	}
}

// TestRestartRequiredBlocksNamesOnlyWhatChanged: the report is a COMPARISON,
// not a list of block names present in the set. A restart-only block pushed
// with the value the collectors were already built from asks for no restart
// -- the platform re-sends the whole set on every revision, so a name-based
// answer would demand a restart for every edit to any other block.
func TestRestartRequiredBlocksNamesOnlyWhatChanged(t *testing.T) {
	built := validBase(t)

	same, err := config.ApplyBlocks(built, map[string][]byte{
		"collect.state": []byte("enabled: true\n"),
	})
	if err != nil {
		t.Fatalf("ApplyBlocks: %v", err)
	}
	if got := config.RestartRequiredBlocks(built, same); len(got) != 0 {
		t.Fatalf("RestartRequiredBlocks = %v, want none: the value did not change", got)
	}

	changed, err := config.ApplyBlocks(built, map[string][]byte{
		"collect.state": []byte("enabled: true\nresync_period: 17m\n"),
	})
	if err != nil {
		t.Fatalf("ApplyBlocks: %v", err)
	}
	if got := config.RestartRequiredBlocks(built, changed); !slices.Equal(got, []string{"collect.state"}) {
		t.Fatalf("RestartRequiredBlocks = %v, want [collect.state]", got)
	}
}

// TestRestartRequiredBlocksIgnoresTheLiveOnes: a live block's change is in
// effect already, so it must never ask for a restart -- that is the false
// "waiting for agent restart" the screen would otherwise show forever for a
// query-only edit.
func TestRestartRequiredBlocksIgnoresTheLiveOnes(t *testing.T) {
	built := validBase(t)
	changed, err := config.ApplyBlocks(built, map[string][]byte{
		"buffer": []byte("batch_size: 250\n"),
		"query":  []byte("enabled: true\n"),
	})
	if err != nil {
		t.Fatalf("ApplyBlocks: %v", err)
	}
	if got := config.RestartRequiredBlocks(built, changed); len(got) != 0 {
		t.Fatalf("RestartRequiredBlocks = %v, want none for live blocks", got)
	}
}

// TestRestartRequiredBlocksWithNoBuiltConfigAnswersNothing: before boot has
// reported what the collectors were built from there is nothing to compare
// against, and nothing has been built either. An empty answer is the honest
// one; guessing "restart" would make every agent ask for a restart it does
// not need on its first heartbeat.
func TestRestartRequiredBlocksWithNoBuiltConfigAnswersNothing(t *testing.T) {
	base := validBase(t)
	if got := config.RestartRequiredBlocks(nil, base); len(got) != 0 {
		t.Fatalf("RestartRequiredBlocks(nil, cfg) = %v, want none", got)
	}
	if got := config.RestartRequiredBlocks(base, nil); len(got) != 0 {
		t.Fatalf("RestartRequiredBlocks(cfg, nil) = %v, want none", got)
	}
}
