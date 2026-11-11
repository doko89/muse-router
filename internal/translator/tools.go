// Package translator converts between client wire formats and the upstream
// Muse Responses contract, including Muse's tool quirks.
package translator

import (
	"strings"

	"muse-router/internal/domain"
)

// ToolNamePrefix is prepended to every function tool declared to Muse:
// subscription keys answer 403 client_attestation_denied for exact reserved
// names (bash, web_search, ...); prefixed names are accepted.
const ToolNamePrefix = "_"

// ToolNameMaxLength is the longest tool name api.meta.ai accepts.
const ToolNameMaxLength = 64

// EncodeToolName maps a caller tool name to its wire name. Names the prefix
// would push past the limit go out unprefixed.
func EncodeToolName(name string) string {
	wire := ToolNamePrefix + name
	if len(wire) > ToolNameMaxLength {
		return name
	}
	return wire
}

// BuildWireMap returns wire->caller names, rejecting collisions where two
// tools would share one wire name.
func BuildWireMap(names []string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	for _, name := range names {
		wire := EncodeToolName(name)
		if prev, ok := out[wire]; ok && prev != name {
			return nil, domain.BadRequest("Tool names %q and %q both encode as %q", prev, name, wire)
		}
		out[wire] = name
	}
	return out, nil
}

// DecodeToolName maps a wire name back to the caller name.
func DecodeToolName(wire string, wireMap map[string]string) string {
	if orig, ok := wireMap[wire]; ok {
		return orig
	}
	return wire
}

// ValidEfforts are the reasoning effort tiers Muse serves.
var ValidEfforts = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// NormalizeEffort validates a requested effort tier.
func NormalizeEffort(effort string) (string, error) {
	if effort == "" {
		return "", nil
	}
	lowered := strings.ToLower(strings.TrimSpace(effort))
	for _, valid := range ValidEfforts {
		if lowered == valid {
			return lowered, nil
		}
	}
	return "", domain.BadRequest("Unsupported reasoning effort %q; use one of: %s", effort, strings.Join(ValidEfforts, ", "))
}

// ClampEffort maps a normalized effort onto a model ladder. Unknown ladders
// (nil/empty) and empty requests pass through; names outside the known tiers
// also pass through so NormalizeEffort can reject them. Otherwise the nearest
// tier wins, ties toward the lower (cheaper) tier.
func ClampEffort(ladder []string, effort string) string {
	if effort == "" || len(ladder) == 0 {
		return effort
	}
	want := -1
	for i, tier := range ValidEfforts {
		if tier == effort {
			want = i
			break
		}
	}
	if want < 0 {
		return effort
	}
	best := ""
	bestDist := len(ValidEfforts) + 1
	for _, tier := range ladder {
		for i, known := range ValidEfforts {
			if known != strings.ToLower(strings.TrimSpace(tier)) {
				continue
			}
			dist := want - i
			if dist < 0 {
				dist = -dist
			}
			if dist < bestDist || (dist == bestDist && i < tierIndex(best)) {
				bestDist = dist
				best = known
			}
			break
		}
	}
	if best == "" {
		return effort
	}
	return best
}

func tierIndex(tier string) int {
	for i, known := range ValidEfforts {
		if known == tier {
			return i
		}
	}
	return len(ValidEfforts)
}

// StripProviderPrefix removes a "provider/" qualifier, returning the bare id.
func StripProviderPrefix(id string) string {
	if i := strings.Index(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}
