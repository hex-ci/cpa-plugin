package main

import (
	"testing"
)

// (hostHTTPResponseFromWire lives in host_bridge.go; these tests pin its
// wire-compat behavior — PascalCase from the current host, snake_case legacy.)

func TestHostHTTPResponseFromWire_PascalCase(t *testing.T) {
	// The current host serializes pluginapi.HTTPResponse without field tags:
	// keys are "StatusCode"/"Headers"/"Body" (PascalCase). This is the shape
	// that was previously decoded with zero StatusCode (field tag mismatch).
	raw := []byte(`{"StatusCode":200,"Headers":{"Content-Type":["application/json"]},"Body":"eyJmb28iOiJiYXIifQ=="}`)
	resp, err := hostHTTPResponseFromWire(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	// Body is base64-encoded by encoding/json; this fixture decodes to 13 bytes.
	if len(resp.Body) != 13 {
		t.Fatalf("body len = %d, want 13", len(resp.Body))
	}
}

func TestHostHTTPResponseFromWire_SnakeCase(t *testing.T) {
	raw := []byte(`{"status_code":404,"headers":{"X-Req":["abc"]},"body":"bW9kZWxz"}`)
	resp, err := hostHTTPResponseFromWire(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("StatusCode = %d, want 404", resp.StatusCode)
	}
	if x := resp.Headers.Get("X-Req"); x != "abc" {
		t.Fatalf("X-Req = %q, want abc", x)
	}
	if string(resp.Body) != "models" {
		t.Fatalf("body = %q, want models", resp.Body)
	}
}

func TestHostHTTPResponseFromWire_EmptyBody(t *testing.T) {
	// Some hosts may emit no Body key at all for empty responses.
	raw := []byte(`{"StatusCode":204,"Headers":{},"Body":null}`)
	resp, err := hostHTTPResponseFromWire(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.StatusCode != 204 {
		t.Fatalf("StatusCode = %d, want 204", resp.StatusCode)
	}
	if len(resp.Body) != 0 {
		t.Fatalf("body len = %d, want 0", len(resp.Body))
	}
}
