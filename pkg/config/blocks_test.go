package config_test

import (
	"slices"
	"strings"
	"testing"
	"time"

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

func TestManagedBlockKeysIsTheClosedSet(t *testing.T) {
	want := []string{"collect.logs", "collect.state", "collect.metrics", "query", "buffer"}
	if !slices.Equal(config.ManagedBlockKeys(), want) {
		t.Fatalf("keys = %v, want %v", config.ManagedBlockKeys(), want)
	}
	if config.IsManagedBlockKey("mutate") {
		t.Fatal("mutate is never a managed block")
	}
}
