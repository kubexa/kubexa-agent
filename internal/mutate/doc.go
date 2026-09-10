// Package mutate applies write operations requested by the gateway.
//
// Like internal/query it keeps no state between requests. Unlike
// internal/query, every operation here is irreversible from the agent's
// point of view, so each one is decided by the owner's policy first and
// carries a precondition the API server -- not this code -- enforces.
package mutate
