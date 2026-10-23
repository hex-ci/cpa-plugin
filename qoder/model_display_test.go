package main

import (
	"strings"
	"testing"
)

// The host's model pickers and every /v1/models flavour only carry id and
// display_name, so the charge rate rides on display_name — same approach as the
// WorkBuddy plugin.
func TestModelDisplayNameCarriesPriceFactor(t *testing.T) {
	cases := []struct {
		name   string
		factor float64
		want   string
	}{
		{"Qwen3.8-Max", 0.2, "Qwen3.8-Max · x0.2"},
		{"Sonus", 8, "Sonus · x8.0"},
		{"Kimi-K3", 1.4, "Kimi-K3 · x1.4"},
		{"Qwen3.8-Flash", 0.1, "Qwen3.8-Flash · x0.1"},
		// Zero rate: say so when the catalogue marks the model free, otherwise
		// stay quiet (no rate was reported at all).
		{"Qwen3.8-Flash", 0, "Qwen3.8-Flash"},
		{"Auto", 0, "Auto"},
		{"  Auto  ", 0, "Auto"},
	}
	for _, c := range cases {
		if got := modelDisplayName(c.name, c.factor, false); got != c.want {
			t.Fatalf("modelDisplayName(%q, %v) = %q, want %q", c.name, c.factor, got, c.want)
		}
	}
	if got := modelDisplayName("Qwen3.8-Flash", 0, true); got != "Qwen3.8-Flash · x0.0" {
		t.Fatalf("free model = %q", got)
	}
	if got := modelDisplayName("Efficient", 0, true); got != "Efficient · x0.0" {
		t.Fatalf("free model with no rate = %q", got)
	}
}

func TestModelInfoUsesRateInDisplayName(t *testing.T) {
	entry := catalogEntry{Key: "qmodel_38max", DisplayName: "Qwen3.8-Max", Enable: true, PriceFactor: 0.2}
	info := entry.modelInfo()
	if info.DisplayName != "Qwen3.8-Max · x0.2" {
		t.Fatalf("DisplayName = %q", info.DisplayName)
	}
	// Name (provider-native id surface) stays clean so routing never depends on
	// a human label.
	if strings.Contains(info.Name, "x0.2") {
		t.Fatalf("Name must stay the bare model name, got %q", info.Name)
	}
}
