package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestCountTokensReturnsUnsupported(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodExecutorCountTokens, nil)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK {
		t.Fatal("count_tokens must not return a fabricated token count")
	}
	if env.Error == nil || env.Error.Code != "unsupported_method" {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
}
