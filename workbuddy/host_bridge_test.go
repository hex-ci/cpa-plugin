package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestDecodeBufferedHostHTTPResponseAcceptsBothStatusCasings(t *testing.T) {
	fixtures := []struct {
		name       string
		wire       string
		statusCode int
		header     string
		body       string
	}{
		{
			name:       "PascalCase",
			wire:       `{"StatusCode":201,"Headers":{"X-Test":["pascal"]},"Body":"cGM="}`,
			statusCode: http.StatusCreated,
			header:     "pascal",
			body:       "pc",
		},
		{
			name:       "snake_case",
			wire:       `{"status_code":202,"headers":{"X-Test":["snake"]},"body":"c2M="}`,
			statusCode: http.StatusAccepted,
			header:     "snake",
			body:       "sc",
		},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			got, err := decodeBufferedHostHTTPResponse([]byte(fixture.wire))
			if err != nil {
				t.Fatal(err)
			}
			if got.StatusCode != fixture.statusCode || got.Headers.Get("X-Test") != fixture.header || string(got.Body) != fixture.body {
				t.Fatalf("response = %#v, want status=%d header=%q body=%q", got, fixture.statusCode, fixture.header, fixture.body)
			}
		})
	}
}

func TestHostHTTPRequestWirePlacesCallbackAtTopLevel(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	wire := newRPCHostHTTPRequestWire(req, []byte("body"), "callback-7")
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["host_callback_id"] != "callback-7" {
		t.Fatalf("top-level callback = %#v", doc)
	}
	nested, ok := doc["request"].(map[string]any)
	if !ok {
		t.Fatalf("nested request = %#v", doc["request"])
	}
	if _, exists := nested["host_callback_id"]; exists {
		t.Fatalf("callback leaked into nested request: %#v", nested)
	}
}
