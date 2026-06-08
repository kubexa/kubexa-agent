// Package protoversion defines the agent/gateway protobuf API version contract
// and handshake negotiation helpers.
//
// Backward compatibility rules (protobuf wire format):
//   - Never reuse or change field numbers; reserve removed fields.
//   - Add new fields and oneof arms only; do not remove or rename existing ones.
//   - Mark superseded fields/messages with [deprecated = true] but keep them on the wire.
//   - Enum numeric values are append-only; never renumber or delete values.
package protoversion

import (
	"fmt"
	"slices"
	"strings"
)

const (
	// V1 is the initial stable agent/gateway API version.
	V1 = "v1"

	// Current is the preferred API version for new agents and gateways.
	Current = V1
)

// Supported lists API versions implemented by this build, newest first.
var Supported = []string{Current}

// Normalize returns a canonical lowercase version label.
func Normalize(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// Known reports whether version is a recognized API label.
func Known(version string) bool {
	version = Normalize(version)
	if version == "" {
		return false
	}
	return slices.Contains(Supported, version)
}

// Validate reports whether version is supported by this build.
func Validate(version string) error {
	version = Normalize(version)
	if version == "" {
		return fmt.Errorf("proto version is required")
	}
	if !Known(version) {
		return fmt.Errorf("unsupported proto version %q (supported: %s)", version, strings.Join(Supported, ", "))
	}
	return nil
}

// AgentHandshake returns the preferred version and the full supported set for handshake.
func AgentHandshake() (preferred string, supported []string) {
	return Current, slices.Clone(Supported)
}

// Negotiate picks the highest mutually supported version.
// preferred is the peer's stated preference; remoteSupported is the peer's advertised set.
// localSupported is this side's supported set (typically Supported).
func Negotiate(preferred string, remoteSupported, localSupported []string) (string, error) {
	preferred = Normalize(preferred)
	local := normalizedSet(localSupported)
	if len(local) == 0 {
		return "", fmt.Errorf("no local proto versions configured")
	}

	remote := normalizedOrdered(remoteSupported)
	if len(remote) == 0 && preferred != "" {
		remote = []string{preferred}
	}
	if len(remote) == 0 {
		// Legacy peers only sent proto_version on the request; assume v1.
		if local[V1] {
			return V1, nil
		}
		return "", fmt.Errorf("peer did not advertise proto versions")
	}

	for _, version := range remote {
		if local[version] {
			return version, nil
		}
	}
	return "", fmt.Errorf(
		"no compatible proto version (peer=%s local=%s)",
		strings.Join(remote, ", "),
		strings.Join(sortedKeys(local), ", "),
	)
}

// ValidateAgentRequest checks an incoming agent handshake version declaration.
func ValidateAgentRequest(preferred string, supported []string) (string, error) {
	return Negotiate(preferred, supported, Supported)
}

// ValidateGatewayResponse checks the negotiated version returned by the gateway.
// Empty negotiatedVersion is accepted for legacy gateways and treated as v1.
func ValidateGatewayResponse(negotiatedVersion string, gatewaySupported []string) error {
	negotiatedVersion = Normalize(negotiatedVersion)
	if negotiatedVersion == "" {
		negotiatedVersion = V1
	}
	if err := Validate(negotiatedVersion); err != nil {
		return err
	}
	if len(gatewaySupported) == 0 {
		return nil
	}
	if _, err := Negotiate(negotiatedVersion, gatewaySupported, Supported); err != nil {
		return fmt.Errorf("gateway proto negotiation: %w", err)
	}
	return nil
}

func normalizedSet(versions []string) map[string]bool {
	out := make(map[string]bool, len(versions))
	for _, version := range versions {
		version = Normalize(version)
		if version == "" {
			continue
		}
		out[version] = true
	}
	return out
}

func normalizedOrdered(versions []string) []string {
	seen := make(map[string]struct{}, len(versions))
	out := make([]string, 0, len(versions))
	for _, version := range versions {
		version = Normalize(version)
		if version == "" {
			continue
		}
		if _, ok := seen[version]; ok {
			continue
		}
		seen[version] = struct{}{}
		out = append(out, version)
	}
	return out
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for version := range set {
		out = append(out, version)
	}
	slices.Sort(out)
	return out
}
