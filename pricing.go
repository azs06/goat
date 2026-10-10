package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// modelPrice is a model's price in US dollars per million tokens.
type modelPrice struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// builtinPrices are Anthropic's list prices (2026-10) for the models the picker offers.
// Cache writes are the 5-minute rate, 1.25x input. Other models have no built-in price;
// their costs can be set in ~/.config/goat/prices.json:
//
//	{"gpt-6-luna": {"input": 1.25, "output": 10, "cache_read": 0.125}}
var builtinPrices = map[string]modelPrice{
	"claude-fable-5-1":  {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.20, CacheWrite: 5},
	"claude-sonnet-5-5": {Input: 2, Output: 10, CacheRead: 0.20, CacheWrite: 2.5},
	"claude-haiku-5-5":  {Input: 0.10, Output: 0.50, CacheRead: 0.01, CacheWrite: 0.125},
}

var userPrices struct {
	once   sync.Once
	prices map[string]modelPrice
}

func userPricesPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "goat", "prices.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "goat", "prices.json")
}

// priceFor returns the price of model, or false when it is unknown. Prices in the
// user's prices.json win over the built-in ones; a dated model ID matches its base.
func priceFor(model string) (modelPrice, bool) {
	userPrices.once.Do(func() {
		if data, err := os.ReadFile(userPricesPath()); err == nil {
			if err := json.Unmarshal(data, &userPrices.prices); err != nil {
				warnf("ignoring %s: %v", userPricesPath(), err)
			}
		}
	})
	for _, table := range []map[string]modelPrice{userPrices.prices, builtinPrices} {
		if price, ok := table[model]; ok {
			return price, true
		}
		for id, price := range table {
			if strings.HasPrefix(model, id+"-") {
				return price, true
			}
		}
	}
	return modelPrice{}, false
}

// usageCost sums the cost of the responses in entries. known is false when a response
// came from a model without a price, in which case cost covers only the priced ones.
func usageCost(entries []historyEntry) (cost float64, responses int, known bool) {
	known = true
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.PromptTokens == 0 || entry.ResponseID == "" || seen[entry.ResponseID] {
			continue
		}
		seen[entry.ResponseID] = true
		responses++
		price, ok := priceFor(entry.Model)
		if !ok {
			known = false
			continue
		}
		uncached := entry.PromptTokens - entry.CacheReadTokens - entry.CacheWriteTokens
		output := max(entry.ContextTokens-entry.PromptTokens, 0)
		cost += (float64(uncached)*price.Input + float64(output)*price.Output +
			float64(entry.CacheReadTokens)*price.CacheRead + float64(entry.CacheWriteTokens)*price.CacheWrite) / 1e6
	}
	return cost, responses, known
}

// formatCost renders a dollar amount with enough digits to be meaningful.
func formatCost(cost float64) string {
	switch {
	case cost == 0:
		return "$0"
	case cost < 0.01:
		return fmt.Sprintf("$%.4f", cost)
	case cost < 1:
		return fmt.Sprintf("$%.3f", cost)
	default:
		return fmt.Sprintf("$%.2f", cost)
	}
}

// turnCostText is the cost of the responses recorded after before, the history as it
// was when the turn started, or "" when nothing priced was used.
func turnCostText(before []historyEntry) string {
	after, err := loadConversationHistoryEntries()
	if err != nil || len(after) <= len(before) {
		return ""
	}
	cost, responses, known := usageCost(after[len(before):])
	if responses == 0 || (!known && cost == 0) {
		return ""
	}
	text := formatCost(cost)
	if !known {
		text += "+"
	}
	return text
}
