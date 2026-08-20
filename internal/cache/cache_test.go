package cache

import (
	"context"
	"testing"
	"time"
)

func TestMemoryCacheHitAndMiss(t *testing.T) {
	c := NewMemoryCache()
	t.Cleanup(c.Close)
	ctx := context.Background()

	if _, ok := c.Get(ctx, "missing"); ok {
		t.Fatal("expected miss for key that was never set")
	}

	if err := c.Set(ctx, "k1", []byte("hello"), time.Minute); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	val, ok := c.Get(ctx, "k1")
	if !ok {
		t.Fatal("expected hit after Set")
	}
	if string(val) != "hello" {
		t.Fatalf("got %q, want %q", val, "hello")
	}
}

func TestMemoryCacheExpiry(t *testing.T) {
	c := NewMemoryCache()
	t.Cleanup(c.Close)
	ctx := context.Background()

	if err := c.Set(ctx, "k1", []byte("hello"), 10*time.Millisecond); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	if _, ok := c.Get(ctx, "k1"); !ok {
		t.Fatal("expected hit immediately after Set")
	}

	time.Sleep(30 * time.Millisecond)

	if _, ok := c.Get(ctx, "k1"); ok {
		t.Fatal("expected miss after TTL expired")
	}
}

func TestKeyIsStableAndDistinguishesModelAndPayload(t *testing.T) {
	payload := map[string]any{"messages": []map[string]string{{"role": "user", "content": "hi"}}}

	k1 := Key("gpt-4o", payload)
	k2 := Key("gpt-4o", payload)
	if k1 != k2 {
		t.Fatal("expected identical model+payload to produce the same key")
	}

	k3 := Key("gpt-4o-mini", payload)
	if k1 == k3 {
		t.Fatal("expected different models to produce different keys")
	}

	otherPayload := map[string]any{"messages": []map[string]string{{"role": "user", "content": "bye"}}}
	k4 := Key("gpt-4o", otherPayload)
	if k1 == k4 {
		t.Fatal("expected different payloads to produce different keys")
	}
}
