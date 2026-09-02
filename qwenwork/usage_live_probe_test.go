package main

import (
	"fmt"
	"testing"
)

// Terminal chunk as observed live from gateway.qwenwork.cn (2026-09-02),
// inner body after unwrapping the envelope.
const liveTerminalChunk = `{"choices":[{"delta":{},"index":0}],"created":1788342700,"id":"chatcmpl-7187817f-cc5b-9ac4-a68c-201757c16b52","model":"qwen3.8-max-preview","object":"chat.completion.chunk","raw_usage":{"account_discount":0,"data":{"completion_tokens":17,"completion_tokens_details":{"reasoning_tokens":14,"text_tokens":17},"prompt_tokens":65,"prompt_tokens_details":{"cached_tokens":0,"text_tokens":65},"total_tokens":82},"model":"qwen3.8-max","ttft":1779},"sub_usages":[],"usage":{"completion_tokens":17,"completion_tokens_details":{"reasoning_tokens":14,"text_tokens":17},"prompt_tokens":65,"prompt_tokens_details":{"cached_tokens":0,"text_tokens":65},"total_tokens":82}}`

// Live outer envelope as emitted by the gateway (body as JSON string).
const liveEnvelope = `{"headers":{"Content-Type":["application/json"],"X-Model-Name":["qwen3.8-max"],"X-Provider-Name":["maas"]},"body":"{\"choices\":[{\"delta\":{},\"index\":0}],\"created\":1788342700,\"id\":\"chatcmpl-x\",\"model\":\"qwen3.8-max-preview\",\"object\":\"chat.completion.chunk\",\"usage\":{\"completion_tokens\":17,\"completion_tokens_details\":{\"reasoning_tokens\":14},\"prompt_tokens\":65,\"prompt_tokens_details\":{\"cached_tokens\":0},\"total_tokens\":82}}","statusCodeValue":200,"statusCode":"OK"}`

func TestCollectorOnLiveFrames(t *testing.T) {
	c := &sseUsageCollector{}
	body, errMsg, ok := unwrapQwenSSE(liveEnvelope)
	fmt.Println("unwrap ok=", ok, "errMsg=", errMsg)
	if !ok {
		t.Fatalf("unwrapQwenSSE rejected the live envelope")
	}
	c.feed(body)
	d := c.detail()
	fmt.Printf("detail: %+v\n", d)
	if d.InputTokens != 65 || d.OutputTokens != 17 || d.TotalTokens != 82 || d.ReasoningTokens != 14 {
		t.Fatalf("unexpected detail: %+v", d)
	}
}

func TestCollectorOnInnerChunk(t *testing.T) {
	c := &sseUsageCollector{}
	c.feed(liveTerminalChunk)
	d := c.detail()
	fmt.Printf("detail: %+v\n", d)
	if d.InputTokens != 65 || d.OutputTokens != 17 || d.TotalTokens != 82 || d.ReasoningTokens != 14 {
		t.Fatalf("unexpected detail: %+v", d)
	}
}
