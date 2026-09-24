package policy

import (
	"sync/atomic"

	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
)

// Holder carries the live query policy. The query path is request-scoped, so
// a swap needs no coordination with anything in flight: the next request
// reads the next policy.
type Holder struct {
	current atomic.Pointer[Policy]
}

// compileWithWarnings is the ONLY place a config becomes a live query policy.
// Boot and swap both call it so the wildcard warning travels with the value
// instead of with the boot path -- a new path inherits no invariants.
func compileWithWarnings(cfg *pkgconfig.Config) (*Policy, []string, error) {
	p, err := Compile(cfg)
	if err != nil {
		return nil, nil, err
	}
	return p, p.UnredactedWildcardRuleIDs(), nil
}

// NewHolder compiles cfg into the initial policy and returns the same
// warnings a later Swap to an equivalent config would produce.
func NewHolder(cfg *pkgconfig.Config) (*Holder, []string, error) {
	p, warnings, err := compileWithWarnings(cfg)
	if err != nil {
		return nil, nil, err
	}
	h := &Holder{}
	h.current.Store(p)
	return h, warnings, nil
}

// Current returns the policy currently in effect.
func (h *Holder) Current() *Policy { return h.current.Load() }

// Swap compiles cfg and, on success, installs it as the policy every
// subsequent request sees. On failure the previously installed policy stays
// in effect.
func (h *Holder) Swap(cfg *pkgconfig.Config) ([]string, error) {
	p, warnings, err := compileWithWarnings(cfg)
	if err != nil {
		return nil, err
	}
	h.current.Store(p)
	return warnings, nil
}

// AllowsAnyList and AllowsAnyGet make Holder a capability.PolicySource, so the
// capability reporter's per-GVR verdicts follow a swap instead of freezing at
// their boot values. internal/capability stays independent of this package:
// it holds the interface, not the type.
func (h *Holder) AllowsAnyList(group, version, resource string) bool {
	return h.Current().AllowsAnyList(group, version, resource)
}

func (h *Holder) AllowsAnyGet(group, version, resource string) bool {
	return h.Current().AllowsAnyGet(group, version, resource)
}
