package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestUsageCost(t *testing.T) {
	entries := []historyEntry{
		// 1M uncached input, 1M cache reads and 1M output on Opus 5.5: $4 + $0.20 + $20.
		{Type: "tool_call", ResponseID: "r1", Model: "claude-opus-5-5", PromptTokens: 2_000_000, CacheReadTokens: 1_000_000, ContextTokens: 3_000_000},
		{Type: "tool_call", ResponseID: "r1", Model: "claude-opus-5-5", PromptTokens: 2_000_000, CacheReadTokens: 1_000_000, ContextTokens: 3_000_000},
		{Type: "assistant", ResponseID: "r2", Model: "claude-haiku-5-5-20261001", PromptTokens: 1_000_000, ContextTokens: 1_000_000},
	}
	cost, responses, known := usageCost(entries)
	if responses != 2 || !known || formatCost(cost) != "$24.30" {
		t.Fatalf("got %s over %d responses (known %v)", formatCost(cost), responses, known)
	}
	if _, _, known := usageCost([]historyEntry{{ResponseID: "x", Model: "mystery", PromptTokens: 5}}); known {
		t.Fatal("expected an unknown model to be reported")
	}
	for cost, want := range map[float64]string{0: "$0", 0.0042: "$0.0042", 0.31: "$0.310", 12.345: "$12.35"} {
		if got := formatCost(cost); got != want {
			t.Errorf("formatCost(%v) = %q, want %q", cost, got, want)
		}
	}
}

func TestUserPrices(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	os.MkdirAll(filepath.Join(dir, "goat"), 0o755)
	os.WriteFile(filepath.Join(dir, "goat", "prices.json"), []byte(`{"gpt-6-luna": {"input": 1, "output": 8, "cache_read": 0.1}}`), 0o644)
	userPrices.once = sync.Once{}
	userPrices.prices = nil

	t.Cleanup(func() { userPrices.once = sync.Once{}; userPrices.prices = nil })
	if price, ok := priceFor("gpt-6-luna"); !ok || price.Output != 8 {
		t.Fatalf("got %+v %v", price, ok)
	}
}
