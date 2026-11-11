package translator

import (
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
	if got := DecodeToolName("_unknown", wireMap); got != "_unknown" {
		t.Fatalf("unknown wire name must pass through, got %q", got)
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
