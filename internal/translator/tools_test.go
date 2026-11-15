package translator

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeToolName(t *testing.T) {
	if got := EncodeToolName("bash"); got != "_bash" {
		t.Fatalf("EncodeToolName(bash) = %q, want %q", got, "_bash")
	}
	long63 := strings.Repeat("a", 63)
	if got := EncodeToolName(long63); got != "_"+long63 {
		t.Fatalf("63-char name must still prefix, got len %d", len(got))
	}
	long64 := strings.Repeat("a", 64)
	if got := EncodeToolName(long64); got != long64 {
		t.Fatalf("64-char name must go out unprefixed, got %q", got)
	}
}

func TestBuildWireMapCollision(t *testing.T) {
	long63 := strings.Repeat("a", 63)
	// "_"+63 chars collides with the unprefixable 64-char twin.
	if _, err := BuildWireMap([]string{"_" + long63, long63}); err == nil {
		t.Fatal("expected collision error, got nil")
	} else if !strings.Contains(err.Error(), "both encode as") {
		t.Fatalf("unexpected error: %v", err)
	}
	wireMap, err := BuildWireMap([]string{"bash", "web_search"})
	if err != nil {
		t.Fatalf("BuildWireMap: %v", err)
	}
	if wireMap["_bash"] != "bash" || DecodeToolName("_web_search", wireMap) != "web_search" {
		t.Fatalf("wire map roundtrip broken: %v", wireMap)
	}
	if got := DecodeToolName("_unknown", wireMap); got != "unknown" {
		t.Fatalf("unknown wire name must fall back to stripped name, got %q", got)
	}
	if got := DecodeToolName("plain", wireMap); got != "plain" {
		t.Fatalf("unprefixed name must pass through, got %q", got)
	}
	longUnprefixed := "_" + strings.Repeat("b", 64)
	if got := DecodeToolName(longUnprefixed, wireMap); got != longUnprefixed {
		t.Fatalf("over-long wire name cannot be a prefix, got %q", got)
	}
}

func TestClassifyToolChoice(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		mode   string
		detail string
	}{
		{"missing", ``, ToolChoiceAuto, ""},
		{"null", `null`, ToolChoiceAuto, ""},
		{"auto", `"auto"`, ToolChoiceAuto, ""},
		{"none", `"none"`, ToolChoiceNone, ""},
		{"required", `"required"`, ToolChoiceForced, `"required"`},
		{"chat named", `{"type":"function","function":{"name":"bash"}}`, ToolChoiceForced, `for function "bash"`},
		{"responses named", `{"type":"function","name":"bash"}`, ToolChoiceForced, `for function "bash"`},
		{"allowed tools", `{"type":"allowed_tools","mode":"auto"}`, ToolChoiceForced, `of type "allowed_tools"`},
	}
	for _, tc := range cases {
		mode, detail, err := ClassifyToolChoice("chat", json.RawMessage(tc.raw))
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if mode != tc.mode || detail != tc.detail {
			t.Fatalf("%s: got (%q,%q), want (%q,%q)", tc.name, mode, detail, tc.mode, tc.detail)
		}
	}
	for _, raw := range []string{`"sometimes"`, `{"type":"bogus"}`, `{"function":{}}`, `42`, `[1]`, `true`} {
		if _, _, err := ClassifyToolChoice("chat", json.RawMessage(raw)); err == nil {
			t.Fatalf("raw %s must be rejected", raw)
		} else if !strings.Contains(err.Error(), "tool_choice") {
			t.Fatalf("raw %s error must mention tool_choice: %v", raw, err)
		}
	}
	if _, _, err := ClassifyToolChoice("responses", json.RawMessage(`"sometimes"`)); err == nil ||
		!strings.Contains(err.Error(), "responses:") {
		t.Fatalf("error must carry the caller prefix: %v", err)
	}
}

func TestNormalizeEffort(t *testing.T) {
	for _, effort := range ValidEfforts {
		if got, err := NormalizeEffort(effort); err != nil || got != effort {
			t.Fatalf("NormalizeEffort(%q) = %q, %v", effort, got, err)
		}
	}
	if got, err := NormalizeEffort(" HIGH "); err != nil || got != "high" {
		t.Fatalf("NormalizeEffort trims/cases: %q, %v", got, err)
	}
	if _, err := NormalizeEffort("ultra"); err == nil {
		t.Fatal("expected error for unknown effort")
	}
	if got, err := NormalizeEffort(""); err != nil || got != "" {
		t.Fatalf("empty effort must pass through: %q, %v", got, err)
	}
}

func TestClampEffort(t *testing.T) {
	ladder5 := []string{"minimal", "low", "medium", "high", "xhigh"}
	cases := []struct {
		ladder []string
		effort string
		want   string
	}{
		{nil, "max", "max"},
		{[]string{}, "max", "max"},
		{ladder5, "", ""},
		{ladder5, "ultra", "ultra"},
		{ladder5, "low", "low"},
		{ladder5, "max", "xhigh"},
		{ValidEfforts, "max", "max"},
		{[]string{"low", "high"}, "medium", "low"},
		{[]string{"low", "high"}, "max", "high"},
	}
	for _, tc := range cases {
		if got := ClampEffort(tc.ladder, tc.effort); got != tc.want {
			t.Fatalf("ClampEffort(%v, %q) = %q, want %q", tc.ladder, tc.effort, got, tc.want)
		}
	}
}

func TestStripProviderPrefix(t *testing.T) {
	if got := StripProviderPrefix("muse-code/muse-spark-1.3"); got != "muse-spark-1.3" {
		t.Fatalf("got %q", got)
	}
	if got := StripProviderPrefix("muse-spark-1.3"); got != "muse-spark-1.3" {
		t.Fatalf("got %q", got)
	}
}

func TestDecodeDotAlias(t *testing.T) {
	wireMap, err := BuildWireMap([]string{"mcp__nex_search__search_web"})
	if err != nil {
		t.Fatalf("BuildWireMap: %v", err)
	}
	// Models sometimes emit the MCP namespace separator as a dot instead
	// of the declared double underscore; every spelling must resolve to
	// the registered tool or the client router rejects the call.
	for _, candidate := range []string{
		"_mcp__nex_search__search_web",
		"mcp__nex_search__search_web",
		"mcp__nex_search.search_web",
		"_mcp__nex_search.search_web",
		"mcp.nex_search.search_web",
	} {
		if got := DecodeToolName(candidate, wireMap); got != "mcp__nex_search__search_web" {
			t.Fatalf("DecodeToolName(%q) = %q, want registered name", candidate, got)
		}
	}
	// A dotted name that IS the declared tool is never remapped.
	dotted, err := BuildWireMap([]string{"my.tool"})
	if err != nil {
		t.Fatalf("BuildWireMap: %v", err)
	}
	if got := DecodeToolName("my.tool", dotted); got != "my.tool" {
		t.Fatalf("declared dotted name remapped to %q", got)
	}
	// Unknown dotted names keep the old passthrough behavior.
	if got := DecodeToolName("other.thing", wireMap); got != "other.thing" {
		t.Fatalf("unknown dotted name changed to %q", got)
	}
	if got := DecodeToolName("_other.thing", wireMap); got != "other.thing" {
		t.Fatalf("unknown prefixed dotted name = %q, want stripped", got)
	}
	if got := DecodeToolName("a.b", nil); got != "a.b" {
		t.Fatalf("nil map must pass through, got %q", got)
	}
}

func TestJoinNamespaceName(t *testing.T) {
	cases := []struct{ ns, bare, want string }{
		{"mcp__nex_search", "search_web", "mcp__nex_search__search_web"},
		{"mcp__nex_search__", "search_web", "mcp__nex_search__search_web"},
		{"mcp__nex_search", "_search_web", "mcp__nex_search__search_web"},
		{"multi_agent_v1", "spawn_agent", "multi_agent_v1__spawn_agent"},
		{"ns", "tool", "ns__tool"},
	}
	for _, tc := range cases {
		if got := JoinNamespaceName(tc.ns, tc.bare); got != tc.want {
			t.Fatalf("JoinNamespaceName(%q,%q) = %q, want %q", tc.ns, tc.bare, got, tc.want)
		}
	}
}
