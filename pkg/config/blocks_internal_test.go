package config

import "testing"

// TestManagedBlockAllowedKeysCoversEveryManagedKey guards against a sixth
// managed block landing in managedBlockKeys without a matching entry in
// managedBlockAllowedKeys, which is only reachable from inside the package.
// Without this, a missing entry would reach checkAllowedKeys as a nil map --
// today that fails safe by refusing every key, but the failure would be
// silent (no test breaks) and the block would ship completely uneditable
// instead of what its author intended. A stray entry for a key that is not
// managed would be equally silent. Both must fail loudly, here, rather than
// waiting for a security review to notice the coming block or the leftover
// one.
func TestManagedBlockAllowedKeysCoversEveryManagedKey(t *testing.T) {
	for _, key := range managedBlockKeys {
		if _, ok := managedBlockAllowedKeys[key]; !ok {
			t.Errorf("managedBlockAllowedKeys has no entry for managed block %q", key)
		}
	}
	for key := range managedBlockAllowedKeys {
		if !IsManagedBlockKey(key) {
			t.Errorf("managedBlockAllowedKeys has an entry for %q, which is not a managed block", key)
		}
	}
}

// TestEveryManagedBlockHasAPrototype guards the same gap as the test above,
// for ManagedBlockPrototype: schemagen (tools/schemagen, Task 7) reflects
// over a block's struct through it instead of carrying its own key->type
// switch. A sixth block must fail here rather than silently generating a
// schema that is missing it.
func TestEveryManagedBlockHasAPrototype(t *testing.T) {
	for _, key := range managedBlockKeys {
		if _, ok := ManagedBlockPrototype(key); !ok {
			t.Errorf("ManagedBlockPrototype has no case for managed block %q", key)
		}
	}
}

// TestManagedBlockAppliesLiveCoversEveryManagedKey guards the same gap as
// the two tests above, for the live/restart-only classification: a sixth
// block landing without an entry would read as restart-only by map default
// (the zero value is false) and silently ask for a restart nobody needs, or
// -- if the default ever flipped -- report a block as live that nothing
// applies. Both are the "reported as working" failure restart_required
// exists to close, so a missing entry must fail here.
func TestManagedBlockAppliesLiveCoversEveryManagedKey(t *testing.T) {
	for _, key := range managedBlockKeys {
		if _, ok := managedBlockAppliesLive[key]; !ok {
			t.Errorf("managedBlockAppliesLive has no entry for managed block %q", key)
		}
	}
	for key := range managedBlockAppliesLive {
		if !IsManagedBlockKey(key) {
			t.Errorf("managedBlockAppliesLive has an entry for %q, which is not a managed block", key)
		}
	}
}

// TestManagedBlockYAMLRendersEveryManagedKey pins that the comparison
// RestartRequiredBlocks makes actually looks at each block: a key that fell
// out of managedBlockYAML's switch renders nil, nil equals nil, and the
// block would silently never ask for a restart again.
func TestManagedBlockYAMLRendersEveryManagedKey(t *testing.T) {
	cfg := Default()
	for _, key := range managedBlockKeys {
		if len(managedBlockYAML(cfg, key)) == 0 {
			t.Errorf("managedBlockYAML(%q) rendered nothing; RestartRequiredBlocks would never see it change", key)
		}
	}
}
