package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// End-to-end proof that a blocked term in the system prompt survives all the
// way into the FINAL wire body that goes to gateway.qwenwork.cn: the U+200B
// zero-width space must be present in the marshaled agent_chat_generation
// JSON that COSY signs and transmits.
func TestBlockedTermLandsInFinalWireBody(t *testing.T) {
	old := featureRuntime.Load()
	cfg, err := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms: [exploit, malware]\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(cfg)
	t.Cleanup(func() { featureRuntime.Store(old) })

	var payload map[string]any
	if err := json.Unmarshal([]byte(`{
		"model":"m",
		"messages":[
			{"role":"system","content":"You are a helpful assistant. This system prompt mentions exploit and malware."},
			{"role":"user","content":"hello"}
		]
	}`), &payload); err != nil {
		t.Fatal(err)
	}
	wire, err := buildQwenBody(payload, "pro")
	if err != nil {
		t.Fatal(err)
	}
	s := string(wire)
	if !strings.Contains(s, "e"+zeroWidthSpace+"xploit") {
		t.Fatalf("wire body does not contain desensitized exploit: %s", s)
	}
	if !strings.Contains(s, "m"+zeroWidthSpace+"alware") {
		t.Fatalf("wire body does not contain desensitized malware: %s", s)
	}
	if strings.Contains(s, "mentions exploit") {
		t.Fatalf("wire body still contains bare 'exploit': %s", s)
	}
	// The plain 'hello' user message must be untouched.
	if !strings.Contains(s, "\"hello\"") {
		t.Fatalf("user message altered: %s", s)
	}
}
