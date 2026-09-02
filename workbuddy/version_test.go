package main

import (
	"os"
	"strings"
	"testing"
)

func TestDefaultVersionMatchesVersionFile(t *testing.T) {
	raw, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSpace(string(raw)); version != want {
		t.Fatalf("default version %q does not match VERSION %q", version, want)
	}
}
