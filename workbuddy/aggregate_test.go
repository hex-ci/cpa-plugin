package main

import (
	"errors"
	"strings"
	"testing"
)

var errReadAfterDone = errors.New("read after [DONE]")

type doneThenErrorReader struct {
	data []byte
}

func (r *doneThenErrorReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, errReadAfterDone
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestAggregateCompletion_BasicSSE(t *testing.T) {
	sse := "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"id\":\"1\",\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":10,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"
	out, err := aggregateCompletion(strings.NewReader(sse), "test-model")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "Hello world") {
		t.Fatalf("content not merged: %s", s)
	}
	if !strings.Contains(s, "stop") {
		t.Fatalf("finish_reason missing: %s", s)
	}
}

func TestAggregateCompletion_Empty(t *testing.T) {
	_, err := aggregateCompletion(strings.NewReader(""), "test")
	if err == nil {
		t.Fatal("empty upstream stream must error")
	}
}

func TestAggregateCompletion_DoneOnly(t *testing.T) {
	_, err := aggregateCompletion(strings.NewReader("data: [DONE]\n\n"), "test")
	if err == nil {
		t.Fatal("terminator-only upstream stream must error")
	}
}

func TestAggregateCompletion_MalformedOnly(t *testing.T) {
	_, err := aggregateCompletion(strings.NewReader("data: not-json\n\n"), "test")
	if err == nil {
		t.Fatal("malformed-only upstream stream must error")
	}
}

func TestAggregateCompletion_NoDone(t *testing.T) {
	sse := "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n"
	out, _ := aggregateCompletion(strings.NewReader(sse), "m")
	if !strings.Contains(string(out), "hi") {
		t.Fatal("content missing")
	}
}

func TestAggregateCompletionStopsAtDone(t *testing.T) {
	r := &doneThenErrorReader{data: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")}
	out, err := aggregateCompletion(r, "m")
	if err != nil {
		t.Fatalf("read after terminator: %v", err)
	}
	if !strings.Contains(string(out), `"content":"ok"`) {
		t.Fatalf("content missing: %s", out)
	}
}

func TestAggregateSSEStopsAtDone(t *testing.T) {
	r := &doneThenErrorReader{data: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")}
	chunks, err := aggregateSSEWithCollector(r, false, nil)
	if err != nil {
		t.Fatalf("read after terminator: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunks=%d want 1", len(chunks))
	}
}
