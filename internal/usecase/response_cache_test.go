package usecase

import (
	"testing"
	"time"
)

func TestResponseMapCache(t *testing.T) {
	cache := NewResponseMapCache(time.Hour, 10)
	cache.Store("", map[string]string{"_a": "a"})
	cache.Store("no-tools", nil)
	if cache.Len() != 0 {
		t.Fatal("empty ids and tool-less maps must be skipped")
	}
	wireMap := map[string]string{"_bash": "bash"}
	cache.Store("resp_1", wireMap)
	got, ok := cache.Get("resp_1")
	if !ok || got["_bash"] != "bash" {
		t.Fatalf("get = %v, %v", got, ok)
	}
	if _, ok := cache.Get("missing"); ok {
		t.Fatal("missing id must miss")
	}

	expiring := NewResponseMapCache(20*time.Millisecond, 10)
	expiring.Store("r", wireMap)
	time.Sleep(30 * time.Millisecond)
	if _, ok := expiring.Get("r"); ok {
		t.Fatal("expired entry must miss")
	}

	bounded := NewResponseMapCache(time.Hour, 4)
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		bounded.Store(id, wireMap)
	}
	if bounded.Len() > 4 {
		t.Fatalf("cache must stay bounded, len=%d", bounded.Len())
	}
}
