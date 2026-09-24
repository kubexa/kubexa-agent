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
