package main

import (
	"os"
	"strings"
	"testing"
)

// The release pipeline reads VERSION (CI uses it for the artifact name and tag),
// while the binary carries main.version (injected by -ldflags from the same
// file by the Makefile). Nothing else ties the two together: if they drift, the
// release publishes a build whose reported version disagrees with its own
// filename, and every "is the new build actually loaded?" check reads the wrong
// answer.
func TestDefaultVersionMatchesVersionFile(t *testing.T) {
	raw, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSpace(string(raw)); version != want {
		t.Fatalf("default version %q does not match VERSION %q", version, want)
	}
}
