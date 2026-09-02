package main

import (
	"encoding/json"
	"testing"
)

// Terminal usage frame shape observed live from gateway.qwenwork.cn:
// a chunk whose delta is empty and which carries NO finish_reason, so the
// empty-delta drop in cleanChunkJSON must NOT swallow it.
func TestCleanChunkJSONKeepsUsageFrame(t *testing.T) {
	frame := `{"choices":[{"delta":{},"index":0}],"created":1788342700,"id":"chatcmpl-x","model":"qwen3.8-max-preview","object":"chat.completion.chunk","usage":{"completion_tokens":17,"prompt_tokens":65,"total_tokens":82}}`
	out := cleanChunkJSON(frame)
	if out == "" {
		t.Fatal("usage frame was dropped by cleanChunkJSON")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("unmarshal cleaned output: %v", err)
	}
	u, ok := obj["usage"].(map[string]any)
	if !ok || len(u) == 0 {
		t.Fatalf("usage block lost: %s", out)
	}
	if u["total_tokens"] != float64(82) {
		t.Fatalf("total_tokens mangled: %v", u["total_tokens"])
	}
}

// Upstream pads the terminal usage frame with private telemetry siblings
// (raw_usage/sub_usages) that leak internal routing metadata. They must be
// stripped while the standard usage block survives.
func TestCleanChunkJSONStripsUsageTelemetry(t *testing.T) {
	frame := `{"choices":[{"delta":{},"index":0}],"model":"m","object":"chat.completion.chunk","raw_usage":{"model_account":"maas-tnt-x","target":"maas-y","ttft":1779},"sub_usages":[],"usage":{"completion_tokens":17,"prompt_tokens":65,"total_tokens":82}}`
	out := cleanChunkJSON(frame)
	if out == "" {
		t.Fatal("usage frame was dropped by cleanChunkJSON")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("unmarshal cleaned output: %v", err)
	}
	if _, present := obj["raw_usage"]; present {
		t.Error("raw_usage telemetry not stripped")
	}
	if _, present := obj["sub_usages"]; present {
		t.Error("sub_usages telemetry not stripped")
	}
	if _, ok := obj["usage"].(map[string]any); !ok {
		t.Error("usage block lost during telemetry strip")
	}
}

// The original empty-delta drop still applies to non-usage noise frames.
func TestCleanChunkJSONStillDropsEmptyNoiseFrame(t *testing.T) {
	frame := `{"choices":[{"delta":{"function_call":null},"index":0}],"model":"m","object":"chat.completion.chunk"}`
	if out := cleanChunkJSON(frame); out != "" {
		t.Fatalf("expected empty noise frame to be dropped, got %s", out)
	}
}
