package main

import (
	"encoding/json"
	"regexp"
	"testing"
)

// The embedded template is a protocol reference, not a capture: it must carry no
// session identities and no environment block from the machine it was recorded
// on. Both are per-request facts that buildQoderBody supplies itself.
func TestBasePromptCarriesNoCapturedContext(t *testing.T) {
	var base map[string]any
	if err := json.Unmarshal(basepromptJSON, &base); err != nil {
		t.Fatalf("the embedded template must decode: %v", err)
	}

	for _, key := range []string{"request_id", "request_set_id", "chat_record_id", "session_id"} {
		if v, _ := base[key].(string); v != "" {
			t.Errorf("%s = %q, want it empty (each request gets its own)", key, v)
		}
	}

	msgs, _ := base["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d entries, want only the system prompt", len(msgs))
	}
	system, _ := msgs[0].(map[string]any)
	content, _ := system["content"].(string)
	if content == "" {
		t.Fatal("the system prompt must survive: it is what the upstream expects")
	}
	// Machine facts the plugin cannot know: it must not claim someone's host.
	for _, leak := range []string{"Working directory:", "OS Version:", "Platform:", "MINGW", "D:/Projects"} {
		if regexp.MustCompile(regexp.QuoteMeta(leak)).MatchString(content) {
			t.Errorf("the system prompt still carries %q", leak)
		}
	}

	if biz, ok := base["business"].(map[string]any); ok {
		if id, _ := biz["id"].(string); id != "" {
			t.Errorf("business.id = %q, want it empty", id)
		}
		if begin, _ := biz["begin_at"].(float64); begin != 0 {
			t.Errorf("business.begin_at = %v, want the 0 placeholder the request overwrites", begin)
		}
	}
}

// The request itself must supply the per-request facts the template left blank.
func TestBuildQoderBodyFillsSessionFacts(t *testing.T) {
	body, err := buildQoderBody(&openAIRequest{Messages: []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}, "lite", "personal_standard")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"request_id", "request_set_id", "chat_record_id", "session_id"} {
		if v, _ := out[key].(string); v == "" {
			t.Errorf("%s is still empty in the request body", key)
		}
	}
	biz, _ := out["business"].(map[string]any)
	if begin, _ := biz["begin_at"].(float64); begin <= 0 {
		t.Errorf("business.begin_at = %v, want the request's own start time", begin)
	}
}
