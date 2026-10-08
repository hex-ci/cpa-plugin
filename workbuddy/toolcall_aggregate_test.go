// toolcall_aggregate_test.go pins the tool_call folding rules in
// aggregateCompletion: slot assignment when the upstream omits index, keeping
// the function name intact, and dropping arguments that were cut off mid-stream.
//
// The frame shapes below mirror a real upstream capture (enterprise account,
// 2026-09-18): the opening fragment carries index/id/name, every continuation
// carries an empty name, and a truncated stream ends without [DONE].
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// sse builds a data: line the way the upstream sends them.
func sse(payload string) string { return "data: " + payload + "\n\n" }

func toolFrame(index int, id, name, args string) string {
	fn := `{"arguments":` + jsonString(args)
	if name != "" {
		fn += `,"name":` + jsonString(name)
	}
	fn += `}`
	parts := []string{`"function":` + fn}
	if id != "" {
		parts = append(parts, `"id":`+jsonString(id))
	}
	if index >= 0 {
		parts = append(parts, `"index":`+itoa(int64(index)))
	}
	parts = append(parts, `"type":"function"`)
	return sse(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{` +
		strings.Join(parts, ",") + `}]}}]}`)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func deltaFrame(content string) string {
	return sse(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":` + jsonString(content) + `}}]}`)
}

func doneFrame() string { return sse("[DONE]") }

// fold runs aggregateCompletion over the given frames.
func fold(t *testing.T, frames ...string) map[string]any {
	t.Helper()
	out, err := aggregateCompletion(strings.NewReader(strings.Join(frames, "")), "m")
	if err != nil {
		t.Fatalf("aggregateCompletion: %v", err)
	}
	var completion map[string]any
	if err := json.Unmarshal(out, &completion); err != nil {
		t.Fatalf("unmarshal completion: %v", err)
	}
	return completion
}

func toolCallsOf(t *testing.T, completion map[string]any) []map[string]any {
	t.Helper()
	choices, _ := completion["choices"].([]any)
	if len(choices) == 0 {
		t.Fatal("completion has no choices")
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	raw, _ := msg["tool_calls"].([]any)
	calls := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		call, _ := r.(map[string]any)
		calls = append(calls, call)
	}
	return calls
}

// A well-formed stream keeps its calls intact and ordered by index.
func TestAggregateToolCallsWellFormed(t *testing.T) {
	completion := fold(t,
		toolFrame(0, "call_a", "run_shell", `{"command":"ls"}`),
		toolFrame(1, "call_b", "read_file", `{"path":"sample.txt"}`),
		doneFrame(),
	)
	calls := toolCallsOf(t, completion)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if name := calls[0]["function"].(map[string]any)["name"]; name != "run_shell" {
		t.Fatalf("call0 name = %v", name)
	}
	if name := calls[1]["function"].(map[string]any)["name"]; name != "read_file" {
		t.Fatalf("call1 name = %v", name)
	}
}

// Fragments without index must not all collapse into slot 0. Two independent
// calls, neither carrying index, must stay two calls with separate arguments.
func TestAggregateToolCallsMissingIndexNotCollapsed(t *testing.T) {
	completion := fold(t,
		toolFrame(-1, "call_a", "run_shell", `{"command":"ls"}`),
		toolFrame(-1, "call_b", "read_file", `{"path":"sample.txt"}`),
		doneFrame(),
	)
	calls := toolCallsOf(t, completion)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2 (missing index must not collapse slots)", len(calls))
	}
	args0 := calls[0]["function"].(map[string]any)["arguments"]
	if args0 != `{"command":"ls"}` {
		t.Fatalf("call0 arguments = %v (concatenation of two calls?)", args0)
	}
	args1 := calls[1]["function"].(map[string]any)["arguments"]
	if args1 != `{"path":"sample.txt"}` {
		t.Fatalf("call1 arguments = %v", args1)
	}
}

// A continuation fragment that omits index but repeats the id must continue
// that call rather than open a new slot.
func TestAggregateToolCallsIDContinuesCall(t *testing.T) {
	completion := fold(t,
		toolFrame(-1, "call_a", "run_shell", `{"command":`),
		toolFrame(-1, "call_a", "", `"ls"}`),
		doneFrame(),
	)
	calls := toolCallsOf(t, completion)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1 (id must continue the same call)", len(calls))
	}
	if args := calls[0]["function"].(map[string]any)["arguments"]; args != `{"command":"ls"}` {
		t.Fatalf("arguments = %v, want the two fragments joined", args)
	}
}

// A repeated name must not be concatenated (the BashBashBash shape).
func TestAggregateToolCallsNameNotConcatenated(t *testing.T) {
	completion := fold(t,
		toolFrame(0, "call_a", "run_shell", `{"command":`),
		toolFrame(0, "", "run_shell", `"ls"}`),
		toolFrame(0, "", "run_shell", ``),
		doneFrame(),
	)
	calls := toolCallsOf(t, completion)
	name := calls[0]["function"].(map[string]any)["name"]
	if name != "run_shell" {
		t.Fatalf("name = %q, want run_shell (not repeated per fragment)", name)
	}
}

// Empty-name continuations (the real upstream shape) must also leave the name
// intact.
func TestAggregateToolCallsEmptyNameContinuations(t *testing.T) {
	completion := fold(t,
		toolFrame(0, "call_a", "run_shell", `{"comm`),
		toolFrame(0, "", "", `and":`),
		toolFrame(0, "", "", `"ls"}`),
		doneFrame(),
	)
	calls := toolCallsOf(t, completion)
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "run_shell" {
		t.Fatalf("name = %v", fn["name"])
	}
	if fn["arguments"] != `{"command":"ls"}` {
		t.Fatalf("arguments = %v", fn["arguments"])
	}
}

// finish_reason=length with half-written arguments: the broken call is dropped,
// not passed through and not invented as "{}".
func TestAggregateToolCallsTruncatedByLengthDropped(t *testing.T) {
	completion := fold(t,
		toolFrame(0, "call_a", "run_shell", `{"command":"ls"`),
		sse(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`),
	)
	choices, _ := completion["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if _, present := msg["tool_calls"]; present {
		t.Fatalf("truncated call survived: %v", msg["tool_calls"])
	}
}

// EOF without [DONE] is the other truncation source.
func TestAggregateToolCallsTruncatedByEOF(t *testing.T) {
	completion := fold(t,
		deltaFrame("thinking"),
		toolFrame(0, "call_a", "run_shell", `{"command":`),
	)
	choices, _ := completion["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if _, present := msg["tool_calls"]; present {
		t.Fatalf("truncated call survived EOF: %v", msg["tool_calls"])
	}
}

// An empty arguments string is a valid no-argument tool call and must be kept
// even on a truncated stream.
func TestAggregateToolCallsEmptyArgsKept(t *testing.T) {
	completion := fold(t,
		toolFrame(0, "call_a", "get_time", ``),
		sse(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`),
	)
	calls := toolCallsOf(t, completion)
	if len(calls) != 1 {
		t.Fatalf("empty-args call must be kept, got %d calls", len(calls))
	}
}

// Arguments that parse as JSON are never dropped, even with a wrong value type:
// that is model output, and the client's schema check owns it.
func TestAggregateToolCallsParseableArgsKept(t *testing.T) {
	completion := fold(t,
		toolFrame(0, "call_a", "run_shell", `"just a string"`),
		sse(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`),
	)
	calls := toolCallsOf(t, completion)
	if len(calls) != 1 {
		t.Fatalf("parseable args must be kept, got %d calls", len(calls))
	}
}

// A normal stop with complete arguments keeps the call (no over-eager dropping).
func TestAggregateToolCallsCompleteOnStopKept(t *testing.T) {
	completion := fold(t,
		toolFrame(0, "call_a", "run_shell", `{"command":"ls"}`),
		sse(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		doneFrame(),
	)
	if calls := toolCallsOf(t, completion); len(calls) != 1 {
		t.Fatalf("complete call must survive, got %d", len(calls))
	}
}

func TestIsTruncatedArguments(t *testing.T) {
	cases := []struct {
		name string
		call map[string]any
		want bool
	}{
		{"no function", map[string]any{}, false},
		{"empty args", map[string]any{"function": map[string]any{"arguments": ""}}, false},
		{"whitespace args", map[string]any{"function": map[string]any{"arguments": "  "}}, false},
		{"valid object", map[string]any{"function": map[string]any{"arguments": `{"a":1}`}}, false},
		{"valid scalar", map[string]any{"function": map[string]any{"arguments": `42`}}, false},
		{"valid null", map[string]any{"function": map[string]any{"arguments": `null`}}, false},
		{"half object", map[string]any{"function": map[string]any{"arguments": `{"a":`}}, true},
		{"unclosed string", map[string]any{"function": map[string]any{"arguments": `{"a":"x`}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTruncatedArguments(tc.call); got != tc.want {
				t.Fatalf("isTruncatedArguments = %v, want %v", got, tc.want)
			}
		})
	}
}
