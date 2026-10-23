//go:build live

// Opt-in live probe for the chat request shape. Compares the encoded body the
// plugin sends today with a plain-JSON body, so a realm-specific envelope
// mismatch can be settled with evidence instead of guesswork.
//
//	QODER_LIVE_AUTH=/path/qoder-<uid>.json QODER_LIVE_MODEL=efficient \
//	  go test -tags live -run TestLiveChatBodyModes -v .

package main

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLiveChatBodyModes(t *testing.T) {
	path := os.Getenv("QODER_LIVE_AUTH")
	if path == "" {
		t.Skip("QODER_LIVE_AUTH not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth: %v", err)
	}
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	model := strings.TrimSpace(os.Getenv("QODER_LIVE_MODEL"))
	if model == "" {
		model = "efficient"
	}
	req := &openAIRequest{
		Model:     model,
		MaxTokens: 16,
		Messages:  []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	body, err := buildQoderBody(req, sa, model, accountUserType(sa))
	if err != nil {
		t.Fatalf("build body: %v", err)
	}

	r := realmOf(sa)
	for _, mode := range []string{"encoded", "plain"} {
		payload := string(body)
		if mode == "encoded" {
			payload = qoderEncode(body)
		}
		httpReq, rerr := http.NewRequest(http.MethodPost, r.chatURL(), strings.NewReader(payload))
		if rerr != nil {
			t.Fatalf("[%s] request: %v", mode, rerr)
		}
		if aerr := applyCosyHeaders(httpReq, sa, payload, r.chatURL(), model, true); aerr != nil {
			t.Fatalf("[%s] cosy: %v", mode, aerr)
		}
		resp, herr := hostHTTPDo(httpReq)
		if herr != nil {
			t.Logf("[%s] transport: %v", mode, herr)
			continue
		}
		head := string(resp.Body)
		if len(head) > 320 {
			head = head[:320]
		}
		t.Logf("[%s] status=%d body=%s", mode, resp.StatusCode, head)
	}
}

// TestLiveChatBodyVariants mutates one body field at a time against the real
// gateway. A rejected shape reports the same generic message, so the only way
// to find which field the router needs is to change them one by one.
func TestLiveChatBodyVariants(t *testing.T) {
	path := os.Getenv("QODER_LIVE_AUTH")
	if path == "" {
		t.Skip("QODER_LIVE_AUTH not set")
	}
	raw, _ := os.ReadFile(path)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	model := strings.TrimSpace(os.Getenv("QODER_LIVE_MODEL"))
	if model == "" {
		model = "efficient"
	}
	r := realmOf(sa)

	base := func() map[string]any {
		req := &openAIRequest{Model: model, MaxTokens: 16,
			Messages: []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
		body, berr := buildQoderBody(req, sa, model, accountUserType(sa))
		if berr != nil {
			t.Fatalf("build: %v", berr)
		}
		var doc map[string]any
		if uerr := json.Unmarshal(body, &doc); uerr != nil {
			t.Fatalf("decode: %v", uerr)
		}
		return doc
	}

	type variant struct {
		name string
		edit func(map[string]any)
	}
	variants := []variant{
		{"baseline", func(map[string]any) {}},
		{"no-extra-modelConfig", func(d map[string]any) {
			if cc, ok := d["chat_context"].(map[string]any); ok {
				if extra, ok := cc["extra"].(map[string]any); ok {
					delete(extra, "modelConfig")
				}
			}
		}},
		{"minimal-model_config", func(d map[string]any) {
			d["model_config"] = map[string]any{"key": model, "format": "openai", "source": "system"}
		}},
		{"no-is_vl", func(d map[string]any) {
			if mc, ok := d["model_config"].(map[string]any); ok {
				delete(mc, "is_vl")
				delete(mc, "is_reasoning")
			}
		}},
		{"no-business", func(d map[string]any) { delete(d, "business") }},
		{"session_type-qoder", func(d map[string]any) { d["session_type"] = "qoder" }},
		{"user_type-empty", func(d map[string]any) { delete(d, "aliyun_user_type") }},
		{"no-parameters-max_tokens", func(d map[string]any) { d["parameters"] = map[string]any{} }},
		{"no-tools", func(d map[string]any) { d["tools"] = []any{} }},
		{"no-tools-no-extra-mc", func(d map[string]any) {
			d["tools"] = []any{}
			if cc, ok := d["chat_context"].(map[string]any); ok {
				if extra, ok := cc["extra"].(map[string]any); ok {
					delete(extra, "modelConfig")
				}
			}
		}},
		{"one-user-message-only", func(d map[string]any) {
			d["tools"] = []any{}
			d["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
			d["chat_task"] = "FREE_INPUT"
		}},
	}
	for _, v := range variants {
		doc := base()
		v.edit(doc)
		plain, _ := json.Marshal(doc)
		encoded := qoderEncode(plain)
		httpReq, rerr := http.NewRequest(http.MethodPost, r.chatURL(), strings.NewReader(encoded))
		if rerr != nil {
			t.Fatalf("[%s] request: %v", v.name, rerr)
		}
		if aerr := applyCosyHeaders(httpReq, sa, encoded, r.chatURL(), model, true); aerr != nil {
			t.Fatalf("[%s] cosy: %v", v.name, aerr)
		}
		resp, herr := hostHTTPDo(httpReq)
		if herr != nil {
			t.Logf("[%s] transport: %v", v.name, herr)
			continue
		}
		head := string(resp.Body)
		if len(head) > 200 {
			head = head[:200]
		}
		t.Logf("[%s] %s", v.name, strings.ReplaceAll(head, "\n", " "))
	}
}

// TestLiveChatURLVariants settles which query string the inference endpoint
// accepts. The query we send was captured from an older CN client; the Global
// gateway may route by it and pick a node that cannot serve the model.
func TestLiveChatURLVariants(t *testing.T) {
	path := os.Getenv("QODER_LIVE_AUTH")
	if path == "" {
		t.Skip("QODER_LIVE_AUTH not set")
	}
	raw, _ := os.ReadFile(path)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	model := strings.TrimSpace(os.Getenv("QODER_LIVE_MODEL"))
	if model == "" {
		model = "efficient"
	}
	r := realmOf(sa)
	req := &openAIRequest{Model: model, MaxTokens: 16,
		Messages: []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	body, berr := buildQoderBody(req, sa, model, accountUserType(sa))
	if berr != nil {
		t.Fatalf("build: %v", berr)
	}
	encoded := qoderEncode(body)
	basePath := r.Gateway + "/algo/api/v2/service/pro/sse/agent_chat_generation"
	for _, u := range []string{
		basePath,
		basePath + "?Encode=1",
		basePath + "?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
		basePath + "?AgentId=agent_common&Encode=1",
	} {
		httpReq, rerr := http.NewRequest(http.MethodPost, u, strings.NewReader(encoded))
		if rerr != nil {
			t.Fatalf("request: %v", rerr)
		}
		if aerr := applyCosyHeaders(httpReq, sa, encoded, u, model, true); aerr != nil {
			t.Fatalf("cosy: %v", aerr)
		}
		resp, herr := hostHTTPDo(httpReq)
		if herr != nil {
			t.Logf("[%s] transport: %v", u, herr)
			continue
		}
		head := string(resp.Body)
		if len(head) > 180 {
			head = head[:180]
		}
		t.Logf("[%s] %s", strings.TrimPrefix(u, r.Gateway), head)
	}
}

// TestLiveChatHostVariants checks which inference host serves chat for this
// realm. The catalog answers on api3.qoder.sh; the chat plane may live on a
// different host, and a wrong host returns a generic model error rather than
// a 404.
func TestLiveChatHostVariants(t *testing.T) {
	path := os.Getenv("QODER_LIVE_AUTH")
	if path == "" {
		t.Skip("QODER_LIVE_AUTH not set")
	}
	raw, _ := os.ReadFile(path)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	model := strings.TrimSpace(os.Getenv("QODER_LIVE_MODEL"))
	if model == "" {
		model = "efficient"
	}
	req := &openAIRequest{Model: model, MaxTokens: 16,
		Messages: []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	body, berr := buildQoderBody(req, sa, model, accountUserType(sa))
	if berr != nil {
		t.Fatalf("build: %v", berr)
	}
	encoded := qoderEncode(body)
	path2 := "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	for _, host := range []string{
		"https://api3.qoder.sh",
		"https://api1.qoder.sh",
		"https://api2.qoder.sh",
		"https://center.qoder.sh",
		"https://qts2.qoder.sh",
	} {
		u := host + path2
		httpReq, rerr := http.NewRequest(http.MethodPost, u, strings.NewReader(encoded))
		if rerr != nil {
			continue
		}
		if aerr := applyCosyHeaders(httpReq, sa, encoded, u, model, true); aerr != nil {
			t.Fatalf("cosy: %v", aerr)
		}
		if os.Getenv("QODER_LIVE_DUMP_HEADERS") != "" {
			for k, v := range httpReq.Header {
				t.Logf("  hdr %s: %s", k, strings.Join(v, ","))
			}
		}
		resp, herr := hostHTTPDo(httpReq)
		if herr != nil {
			t.Logf("[%s] transport: %v", host, herr)
			continue
		}
		head := string(resp.Body)
		if len(head) > 160 {
			head = head[:160]
		}
		t.Logf("[%s] %s", host, head)
	}
}

// TestLiveChatFormatVariants: the node chosen for this org rejects "Unsupported
// model format", so the declared format/source pair is the remaining suspect.
func TestLiveChatFormatVariants(t *testing.T) {
	path := os.Getenv("QODER_LIVE_AUTH")
	if path == "" {
		t.Skip("QODER_LIVE_AUTH not set")
	}
	raw, _ := os.ReadFile(path)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	model := strings.TrimSpace(os.Getenv("QODER_LIVE_MODEL"))
	if model == "" {
		model = "efficient"
	}
	r := realmOf(sa)
	req := &openAIRequest{Model: model, MaxTokens: 16,
		Messages: []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	body, berr := buildQoderBody(req, sa, model, accountUserType(sa))
	if berr != nil {
		t.Fatalf("build: %v", berr)
	}
	var doc map[string]any
	if uerr := json.Unmarshal(body, &doc); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	for _, v := range []struct{ format, source string }{
		{"openai", "system"},
		{"dashscope", "system"},
		{"qwen", "system"},
		{"openai", "user"},
		{"", "system"},
	} {
		d2 := deepCopyMap(doc)
		mc := deepCopyMap(d2["model_config"].(map[string]any))
		mc["format"] = v.format
		mc["source"] = v.source
		d2["model_config"] = mc
		plain, _ := json.Marshal(d2)
		encoded := qoderEncode(plain)
		u := r.chatURL()
		httpReq, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(encoded))
		if aerr := applyCosyHeaders(httpReq, sa, encoded, u, model, true); aerr != nil {
			t.Fatalf("cosy: %v", aerr)
		}
		resp, herr := hostHTTPDo(httpReq)
		if herr != nil {
			t.Logf("[fmt=%q src=%q] transport: %v", v.format, v.source, herr)
			continue
		}
		head := string(resp.Body)
		if len(head) > 150 {
			head = head[:150]
		}
		t.Logf("[fmt=%q src=%q] %s", v.format, v.source, head)
	}
}

// TestLiveChatEndpointVariants probes alternative inference paths. The CLI
// bundle references /model/v1/chat/completions, which suggests the SSE "pro"
// path is not the only door and may not be the one this account class uses.
func TestLiveChatEndpointVariants(t *testing.T) {
	path := os.Getenv("QODER_LIVE_AUTH")
	if path == "" {
		t.Skip("QODER_LIVE_AUTH not set")
	}
	raw, _ := os.ReadFile(path)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	model := strings.TrimSpace(os.Getenv("QODER_LIVE_MODEL"))
	if model == "" {
		model = "efficient"
	}
	r := realmOf(sa)
	req := &openAIRequest{Model: model, MaxTokens: 16, Stream: false,
		Messages: []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	body, berr := buildQoderBody(req, sa, model, accountUserType(sa))
	if berr != nil {
		t.Fatalf("build: %v", berr)
	}
	plainOpenAI, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 16, "stream": false,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	cases := []struct {
		path   string
		encode bool
	}{
		{"/algo/model/v1/chat/completions", false},
		{"/model/v1/chat/completions", false},
		{"/algo/model/v1/chat/completions", true},
		{"/algo/api/v3/service/pro/sse/agent_chat_generation?Encode=1", true},
		{"/algo/api/v2/service/sse/agent_chat_generation?Encode=1", true},
	}
	for _, c := range cases {
		u := r.Gateway + c.path
		payload := string(body)
		if !strings.Contains(c.path, "chat/completions") {
			payload = qoderEncode(body)
		} else if c.encode {
			payload = qoderEncode(plainOpenAI)
		} else {
			payload = string(plainOpenAI)
		}
		httpReq, rerr := http.NewRequest(http.MethodPost, u, strings.NewReader(payload))
		if rerr != nil {
			continue
		}
		if aerr := applyCosyHeaders(httpReq, sa, payload, u, model, true); aerr != nil {
			t.Fatalf("cosy: %v", aerr)
		}
		resp, herr := hostHTTPDo(httpReq)
		if herr != nil {
			t.Logf("[%s enc=%t] transport: %v", c.path, c.encode, herr)
			continue
		}
		head := string(resp.Body)
		if len(head) > 200 {
			head = head[:200]
		}
		t.Logf("[%s enc=%t status=%d] %s", c.path, c.encode, resp.StatusCode, head)
	}
}

// TestLiveChatModelHeaderVariants: routing follows the x-model-key header, so a
// key the server cannot map makes it fall back to the account node and answer
// with the node's own model in the error. Trying node-model keys shows whether
// an upstream id is required instead of the catalog alias.
func TestLiveChatModelHeaderVariants(t *testing.T) {
	path := os.Getenv("QODER_LIVE_AUTH")
	if path == "" {
		t.Skip("QODER_LIVE_AUTH not set")
	}
	raw, _ := os.ReadFile(path)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := realmOf(sa)
	for _, model := range []string{"auto", "efficient", "qwen-plus-2025-04-28", "qwen3-max", "qmodel_38max"} {
		req := &openAIRequest{Model: model, MaxTokens: 16,
			Messages: []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
		body, berr := buildQoderBody(req, sa, model, accountUserType(sa))
		if berr != nil {
			t.Logf("[%s] build: %v", model, berr)
			continue
		}
		encoded := qoderEncode(body)
		u := r.chatURL()
		httpReq, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(encoded))
		if aerr := applyCosyHeaders(httpReq, sa, encoded, u, model, true); aerr != nil {
			t.Fatalf("cosy: %v", aerr)
		}
		resp, herr := hostHTTPDo(httpReq)
		if herr != nil {
			t.Logf("[%s] transport: %v", model, herr)
			continue
		}
		head := string(resp.Body)
		if len(head) > 220 {
			head = head[:220]
		}
		t.Logf("[%s] %s", model, head)
	}
}

// TestLiveChatHeaderVariants: the node is fixed regardless of body or model
// key, so the envelope headers (product/scene/version) are what remains.
func TestLiveChatHeaderVariants(t *testing.T) {
	path := os.Getenv("QODER_LIVE_AUTH")
	if path == "" {
		t.Skip("QODER_LIVE_AUTH not set")
	}
	raw, _ := os.ReadFile(path)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := realmOf(sa)
	model := "efficient"
	req := &openAIRequest{Model: model, MaxTokens: 16,
		Messages: []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	body, _ := buildQoderBody(req, sa, model, accountUserType(sa))
	encoded := qoderEncode(body)
	for _, hv := range []struct{ k, v string }{
		{"cosy-business-product", "qoder"},
		{"cosy-business-product", "qoderwork"},
		{"cosy-scene", "chat"},
		{"cosy-version", "1.0.0"},
		{"x-model-source", "user"},
	} {
		u := r.chatURL()
		httpReq, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(encoded))
		if aerr := applyCosyHeaders(httpReq, sa, encoded, u, model, true); aerr != nil {
			t.Fatalf("cosy: %v", aerr)
		}
		httpReq.Header.Set(hv.k, hv.v)
		resp, herr := hostHTTPDo(httpReq)
		if herr != nil {
			t.Logf("[%s=%s] transport: %v", hv.k, hv.v, herr)
			continue
		}
		head := string(resp.Body)
		if len(head) > 170 {
			head = head[:170]
		}
		t.Logf("[%s=%s] %s", hv.k, hv.v, head)
	}
}

// TestLiveChatBearerVariants: the remaining envelope knob is the COSY bearer
// payload itself (protocol version / ideVersion), which the server can use to
// pick a compatible node.
func TestLiveChatBearerVariants(t *testing.T) {
	path := os.Getenv("QODER_LIVE_AUTH")
	if path == "" {
		t.Skip("QODER_LIVE_AUTH not set")
	}
	raw, _ := os.ReadFile(path)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := realmOf(sa)
	model := "efficient"
	req := &openAIRequest{Model: model, MaxTokens: 16,
		Messages: []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	body, _ := buildQoderBody(req, sa, model, accountUserType(sa))
	encoded := qoderEncode(body)
	u, perr := url.Parse(r.chatURL())
	if perr != nil {
		t.Fatalf("url: %v", perr)
	}
	sess, serr := cosySessionFor(sa)
	if serr != nil {
		t.Fatalf("session: %v", serr)
	}
	for _, v := range []struct{ ver, ide string }{
		{"v1", ""},
		{"v2", ""},
		{"v1", "1.1.75"},
		{"v2", "1.1.75"},
	} {
		payload := map[string]string{
			"cosyVersion": cosyVersion(),
			"ideVersion":  v.ide,
			"info":        sess.Info,
			"requestId":   uuid.NewString(),
			"version":     v.ver,
		}
		b64 := base64.StdEncoding.EncodeToString(jsonSortedCompact(payload))
		date := strconv.FormatInt(time.Now().Unix(), 10)
		pathSig := strings.TrimPrefix(u.Path, "/algo")
		sum := md5.Sum([]byte(b64 + "\n" + sess.CosyKey + "\n" + date + "\n" + encoded + "\n" + pathSig))
		bearer := "Bearer COSY." + b64 + "." + hex.EncodeToString(sum[:])

		hdr, herr := sess.headers(sa.Account.UID, encoded, r.chatURL(), "text/event-stream", true)
		if herr != nil {
			t.Fatalf("headers: %v", herr)
		}
		httpReq, _ := http.NewRequest(http.MethodPost, r.chatURL(), strings.NewReader(encoded))
		for k, val := range hdr {
			httpReq.Header.Set(k, val)
		}
		httpReq.Header.Set("authorization", bearer)
		httpReq.Header.Set("cosy-date", date)
		httpReq.Header.Set("x-model-key", model)
		httpReq.Header.Set("x-model-source", "system")
		resp, rerr := hostHTTPDo(httpReq)
		if rerr != nil {
			t.Logf("[ver=%s ide=%s] transport: %v", v.ver, v.ide, rerr)
			continue
		}
		head := string(resp.Body)
		if len(head) > 170 {
			head = head[:170]
		}
		t.Logf("[ver=%s ide=%s] %s", v.ver, v.ide, head)
	}
}

// TestLiveChatQueryVariants: routing may also read the model from the query
// string, which the plugin never sends.
func TestLiveChatQueryVariants(t *testing.T) {
	path := os.Getenv("QODER_LIVE_AUTH")
	if path == "" {
		t.Skip("QODER_LIVE_AUTH not set")
	}
	raw, _ := os.ReadFile(path)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := realmOf(sa)
	model := "efficient"
	req := &openAIRequest{Model: model, MaxTokens: 16,
		Messages: []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	body, _ := buildQoderBody(req, sa, model, accountUserType(sa))
	encoded := qoderEncode(body)
	base := r.Gateway + "/algo/api/v2/service/pro/sse/agent_chat_generation"
	for _, q := range []string{
		"",
		"?Encode=1&model=" + model,
		"?Encode=1&ModelKey=" + model,
		"?Encode=1&modelKey=" + model,
		"?Encode=1&model_key=" + model,
		"?" + model,
	} {
		u := base + q
		httpReq, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(encoded))
		if aerr := applyCosyHeaders(httpReq, sa, encoded, u, model, true); aerr != nil {
			t.Fatalf("cosy: %v", aerr)
		}
		resp, rerr := hostHTTPDo(httpReq)
		if rerr != nil {
			t.Logf("[%q] transport: %v", q, rerr)
			continue
		}
		head := string(resp.Body)
		if len(head) > 190 {
			head = head[:190]
		}
		t.Logf("[%q status=%d] %s", q, resp.StatusCode, head)
	}
}

// TestLiveReplayReferenceBody replays a body captured from a working
// implementation. If it succeeds, the remaining failure is inside our body
// construction and can be bisected field by field.
func TestLiveReplayReferenceBody(t *testing.T) {
	path := os.Getenv("QODER_LIVE_AUTH")
	capPath := os.Getenv("QODER_REPLAY_BODY")
	if path == "" || capPath == "" {
		t.Skip("QODER_LIVE_AUTH and QODER_REPLAY_BODY required")
	}
	raw, _ := os.ReadFile(path)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	capRaw, err := os.ReadFile(capPath)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	var entry struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(capRaw, &entry); err != nil {
		t.Fatalf("capture decode: %v", err)
	}
	r := realmOf(sa)
	model := "efficient"
	encoded := qoderEncode([]byte(entry.Body))
	u := r.chatURL()
	httpReq, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(encoded))
	if aerr := applyCosyHeaders(httpReq, sa, encoded, u, model, true); aerr != nil {
		t.Fatalf("cosy: %v", aerr)
	}
	resp, rerr := hostHTTPDo(httpReq)
	if rerr != nil {
		t.Fatalf("transport: %v", rerr)
	}
	head := string(resp.Body)
	if len(head) > 400 {
		head = head[:400]
	}
	t.Logf("replay status=%d body=%s", resp.StatusCode, head)

	// Follow-up: same body but with our session/request ids refreshed is not
	// needed; the point is whether the server accepts this exact body.
}

// TestLiveBisectBody swaps one section at a time from our builder into a body
// captured from a working implementation. The section that flips 200 -> 400 is
// the field group the gateway rejects.
func TestLiveBisectBody(t *testing.T) {
	path := os.Getenv("QODER_LIVE_AUTH")
	capPath := os.Getenv("QODER_REPLAY_BODY")
	if path == "" || capPath == "" {
		t.Skip("QODER_LIVE_AUTH and QODER_REPLAY_BODY required")
	}
	raw, _ := os.ReadFile(path)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	capRaw, _ := os.ReadFile(capPath)
	var entry struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(capRaw, &entry); err != nil {
		t.Fatalf("capture decode: %v", err)
	}
	var theirs map[string]any
	if err := json.Unmarshal([]byte(entry.Body), &theirs); err != nil {
		t.Fatalf("theirs decode: %v", err)
	}

	model := "efficient"
	req := &openAIRequest{Model: model, MaxTokens: 16,
		Messages: []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	ourRaw, err := buildQoderBody(req, sa, model, accountUserType(sa))
	if err != nil {
		t.Fatalf("our body: %v", err)
	}
	var ours map[string]any
	if err := json.Unmarshal(ourRaw, &ours); err != nil {
		t.Fatalf("ours decode: %v", err)
	}

	r := realmOf(sa)
	send := func(name string, doc map[string]any) {
		plain, _ := json.Marshal(doc)
		encoded := qoderEncode(plain)
		u := r.chatURL()
		httpReq, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(encoded))
		if aerr := applyCosyHeaders(httpReq, sa, encoded, u, model, true); aerr != nil {
			t.Logf("[%s] cosy: %v", name, aerr)
			return
		}
		resp, rerr := hostHTTPDo(httpReq)
		if rerr != nil {
			t.Logf("[%s] transport: %v", name, rerr)
			return
		}
		head := string(resp.Body)
		ok := strings.Contains(head, "chat.completion")
		if len(head) > 200 {
			head = head[:200]
		}
		t.Logf("[%s] ok=%t %s", name, ok, head)
	}

	send("theirs-control", theirs)
	for _, field := range []string{"tools", "business", "messages", "model_config", "chat_context", "parameters", "chat_task", "session_type", "aliyun_user_type"} {
		doc := deepCopyJSON(theirs)
		doc[field] = ours[field]
		send("theirs+"+field, doc)
	}
	doc := deepCopyJSON(theirs)
	for k, v := range ours {
		doc[k] = v
	}
	send("theirs+all-ours", doc)
}

// deepCopyJSON clones a decoded JSON document.
func deepCopyJSON(src map[string]any) map[string]any {
	b, _ := json.Marshal(src)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// TestLiveBisectCumulative applies our values onto the reference body one key
// at a time and reports the first application that flips the answer to 400.
func TestLiveBisectCumulative(t *testing.T) {
	path := os.Getenv("QODER_LIVE_AUTH")
	capPath := os.Getenv("QODER_REPLAY_BODY")
	if path == "" || capPath == "" {
		t.Skip("QODER_LIVE_AUTH and QODER_REPLAY_BODY required")
	}
	raw, _ := os.ReadFile(path)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	capRaw, _ := os.ReadFile(capPath)
	var entry struct {
		Body string `json:"body"`
	}
	_ = json.Unmarshal(capRaw, &entry)
	var theirs map[string]any
	_ = json.Unmarshal([]byte(entry.Body), &theirs)

	model := "efficient"
	req := &openAIRequest{Model: model, MaxTokens: 16,
		Messages: []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	ourRaw, _ := buildQoderBody(req, sa, model, accountUserType(sa))
	var ours map[string]any
	_ = json.Unmarshal(ourRaw, &ours)

	r := realmOf(sa)
	send := func(doc map[string]any) bool {
		plain, _ := json.Marshal(doc)
		encoded := qoderEncode(plain)
		u := r.chatURL()
		httpReq, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(encoded))
		if aerr := applyCosyHeaders(httpReq, sa, encoded, u, model, true); aerr != nil {
			return false
		}
		resp, rerr := hostHTTPDo(httpReq)
		if rerr != nil {
			return false
		}
		return strings.Contains(string(resp.Body), "chat.completion")
	}

	keys := make([]string, 0, len(ours))
	for k := range ours {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	doc := deepCopyJSON(theirs)
	for _, k := range keys {
		doc[k] = ours[k]
		if !send(doc) {
			t.Logf("BREAKS at key %q (value=%v)", k, truncateAny(ours[k], 120))
			return
		}
	}
	t.Logf("no single key broke it")
}

func truncateAny(v any, n int) string {
	b, _ := json.Marshal(v)
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
