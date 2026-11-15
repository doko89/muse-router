// Package translator converts between client wire formats and the upstream
// Muse Responses contract, including Muse's tool quirks.
package translator

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// DecodeToolName maps a wire name back to the caller name. Names missing
// from the map (uncached GETs, undeclared upstream calls) fall back to
// stripping one prefix: Encode only prefixes names that stay within the
// limit, so longer names pass through untouched.
func DecodeToolName(wire string, wireMap map[string]string) string {
	if orig, ok := wireMap[wire]; ok {
		return orig
	}
	if len(wire) > 1 && len(wire) <= ToolNameMaxLength && strings.HasPrefix(wire, ToolNamePrefix) {
		return wire[len(ToolNamePrefix):]
	}
	return wire
}

// Tool choice modes shared by the chat and responses paths. Upstream accepts
// only auto: none is honored by withholding tools, forced (required or a
// specific function) is rejected with a 400 instead of silently becoming
// auto.
const (
	ToolChoiceAuto   = "auto"
	ToolChoiceNone   = "none"
	ToolChoiceForced = "forced"
)

// ClassifyToolChoice maps a raw tool_choice value to its mode. Missing or
// null means auto. detail describes a forced choice for the error message
// (`"required"`, `for function "x"`, `of type "y"`). where prefixes errors
// ("chat" or "responses").
func ClassifyToolChoice(where string, raw json.RawMessage) (mode, detail string, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ToolChoiceAuto, "", nil
	}
	var asString string
	if json.Unmarshal(trimmed, &asString) == nil {
		switch asString {
		case ToolChoiceAuto:
			return ToolChoiceAuto, "", nil
		case ToolChoiceNone:
			return ToolChoiceNone, "", nil
		case "required":
			return ToolChoiceForced, `"required"`, nil
		default:
			return "", "", domain.BadRequest("%s: invalid tool_choice %q (use \"auto\" or \"none\")", where, asString)
		}
	}
	var asObject struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(trimmed, &asObject) != nil || asObject.Type == "" {
		return "", "", domain.BadRequest("%s: invalid tool_choice (use \"auto\" or \"none\")", where)
	}
	switch asObject.Type {
	case "function":
		name := asObject.Function.Name
		if name == "" {
			name = asObject.Name
		}
		return ToolChoiceForced, fmt.Sprintf("for function %q", name), nil
	case "allowed_tools":
		return ToolChoiceForced, `of type "allowed_tools"`, nil
	default:
		return "", "", domain.BadRequest("%s: invalid tool_choice type %q (use \"auto\" or \"none\")", where, asObject.Type)
	}
}

// ForcedToolChoiceError rejects a required/specific tool_choice the upstream
// cannot honor. detail comes from ClassifyToolChoice.
func ForcedToolChoiceError(where, detail string) error {
	return domain.BadRequest("%s: tool_choice %s is not supported by upstream (only \"auto\" and \"none\")", where, detail)
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
