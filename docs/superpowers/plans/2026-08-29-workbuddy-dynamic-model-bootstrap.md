# WorkBuddy Dynamic Model Bootstrap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 WorkBuddy 0.9.0 实现按 auth 隔离的动态模型发现、models.dev metadata enrichment、跨进程 last-good cache 和 fail-closed readiness，并完成已批准的六项 management/panel 修复。

**Architecture:** `model.for_auth` 是唯一 authenticated bootstrap 入口。WorkBuddy entitlement cache 按 auth identity 持久化，models.dev metadata cache 全局持久化；每次进程首次使用时尝试刷新，刷新或持久化失败时只允许回退到对应的已校验 last-good cache。不可变 snapshot 通过 `atomic.Pointer` 发布，executor、scheduler 与 panel 只读 snapshot，不等待网络或磁盘。

**Tech Stack:** Go 1.26.5、CLIProxyAPI plugin ABI v7.2.30、Go stdlib（`encoding/json`、`net/http`、`sync`、`sync/atomic`、`os`、`crypto/sha256`）、Node.js 26 `node:test` 与 `node:vm`，不增加 dependency。

**Spec:** `docs/superpowers/specs/2026-08-29-workbuddy-dynamic-model-bootstrap-design.md`

## Global Constraints

- 所有 git 命令从 `cpa-plugin` repo root 运行；Go 命令使用 `go -C workbuddy ...`，Node 命令使用 `node --test workbuddy/panel.test.js`。
- 保留用户已有的 `LOOP.md` 修改，以及四个 2026-08-28 未跟踪 plan/spec；不得删除、覆盖、格式化或加入本任务 commit。
- 每个可变更 task 必须真实执行 RED -> GREEN -> full package regression -> commit；RED 与 GREEN 的命令及关键输出写入该 task agent 的最终报告。
- 每个 commit 只用该 task 明确列出的文件路径执行 `git add`，不得使用 `git add .`、`git add -A`、amend、push、tag 或 release。
- production 静态模型只允许 `auto` 和一个通用 default metadata template；不得保留其他特定模型 ID、固定 metadata、静态 ID mapping 或模型专属 reasoning policy。
- `model.static` 永远纯本地返回 `auto`；`model.for_auth` 初始化失败返回 `ok:true`、`Provider:"workbuddy"`、非 nil 空 `Models`，不得返回 `auto` 或 RPC error。
- 无任一来源的有效 cache 时，该来源必须 fresh fetch、完整校验并成功持久化；两个来源全部可用后 auth 才 executable。
- 有效 last-good 存在时仍刷新；失败来源使用自身 cache，auth 状态为 `stale`；成功来源替换自身 cache。
- WorkBuddy primary 只在 HTTP 404 或 405 时调用 legacy endpoint；401、403、5xx、业务错误、empty、partial、duplicate、malformed 和 schema 错误都不得 fallback。
- 所有 source tests 使用 synthetic inline fixtures、fake `modelHTTPDo` 或 `httptest.Server`，不得访问真实 WorkBuddy、models.dev 或使用真实 token。
- 不增加第三方 Go/frontend dependency、`package.json`、lockfile、后台 refresh goroutine、TTL、panel retry endpoint、request-time refresh、跨进程 file lock、第二 scheduler、账号 replay/failover 或 Enterprise custom model endpoint。
- 只格式化本任务修改的 Go 文件；不改历史 spec 和历史 CHANGELOG release 条目。

## Locked File Structure and Shared Interfaces

新增文件只按职责拆分：

- `workbuddy/model_source_workbuddy.go`：realm、WorkBuddy wire parser、primary/legacy HTTP source、完整 snapshot 校验。
- `workbuddy/model_source_workbuddy_test.go`：synthetic WorkBuddy parser、routing、fallback 与 header tests。
- `workbuddy/model_source_modelsdev.go`：models.dev parser、ETag/304、dynamic matching、metadata merge。
- `workbuddy/model_source_modelsdev_test.go`：synthetic canonical records、HTTP 与 merge tests。
- `workbuddy/model_store.go`：identity SHA-256、schema v1、primary/`.bak`、atomic write。
- `workbuddy/model_store_test.go`：store、permissions、corruption、future schema、secret absence tests。
- `workbuddy/model_readiness.go`：bootstrap、immutable snapshots、per-auth/global flights、generation gate。
- `workbuddy/model_readiness_test.go`：fresh/stale/concurrency/generation tests。
- `workbuddy/panel.test.js`：Node 26 executable panel behavior tests。
- `workbuddy/production_model_contract_test.go`：production source static-ID/reasoning regression scan。

以下名称与类型在所有 task 中固定，不得另起同义类型：

```go
type modelFacts struct {
	ID                        string   `json:"id"`
	Name                      string   `json:"name,omitempty"`
	Description               string   `json:"description,omitempty"`
	ContextLength             *int64   `json:"context_length,omitempty"`
	MaxCompletionTokens       *int64   `json:"max_completion_tokens,omitempty"`
	SupportedInputModalities  []string `json:"supported_input_modalities,omitempty"`
	SupportedOutputModalities []string `json:"supported_output_modalities,omitempty"`
}

type modelHTTPDo func(*http.Request, string) (*hostHTTPResponse, error)

type authModelRequestWire struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
```

```go
type modelReadinessState string

const (
	modelNotStarted modelReadinessState = "not_started"
	modelLoading    modelReadinessState = "loading"
	modelReady      modelReadinessState = "ready"
	modelStale      modelReadinessState = "stale"
	modelFailed     modelReadinessState = "failed"
)

type modelSnapshotSource string

const (
	modelSourceFresh modelSnapshotSource = "fresh"
	modelSourceCache modelSnapshotSource = "cache"
	modelSourceNone  modelSnapshotSource = "none"
)

func (s modelReadinessState) executable() bool {
	return s == modelReady || s == modelStale
}
```

固定 error code：

```go
type modelErrorCode string

const (
	modelErrorNone               modelErrorCode = ""
	modelErrorAuthInvalid        modelErrorCode = "auth_invalid"
	modelErrorWorkBuddyTransport modelErrorCode = "workbuddy_transport"
	modelErrorWorkBuddyHTTP      modelErrorCode = "workbuddy_http"
	modelErrorWorkBuddySchema    modelErrorCode = "workbuddy_schema"
	modelErrorModelsDevTransport modelErrorCode = "models_dev_transport"
	modelErrorModelsDevHTTP      modelErrorCode = "models_dev_http"
	modelErrorModelsDevSchema    modelErrorCode = "models_dev_schema"
	modelErrorCacheRead          modelErrorCode = "cache_read"
	modelErrorCacheWrite         modelErrorCode = "cache_write"
)
```

---

### Task 1: Capture the Baseline

**Files:**
- Read only: `workbuddy/`
- Preserve: `LOOP.md` and all pre-existing untracked 2026-08-28 docs

**Interfaces:**
- Consumes: current commit `9c45074` and approved spec.
- Produces: baseline command results in the task report; no file and no commit.

- [ ] **Step 1: Record the exact working tree before implementation**

Run:

```powershell
git status --short
git rev-parse HEAD
```

Expected: HEAD is `9c45074`; output still lists the pre-existing `LOOP.md` and four 2026-08-28 untracked documents. Save the output in the task report so later agents can distinguish user files from task files.

- [ ] **Step 2: Run the existing Go baseline**

Run:

```powershell
go -C workbuddy test ./... -count=1
go -C workbuddy test -race ./... -count=1
go -C workbuddy vet ./...
```

Expected: all commands pass before production edits. If a command fails, stop this task and report the exact existing failure; do not change code to hide a baseline failure.

- [ ] **Step 3: Confirm no frontend suite exists yet**

Run:

```powershell
node --test workbuddy/panel.test.js
```

Expected: FAIL because `workbuddy/panel.test.js` does not exist. This is not a product RED; it records the missing executable panel fixture that Task 12 adds.

- [ ] **Step 4: Do not commit**

No repository state changes are permitted in this task.

---

### Task 2: Static `auto`, Default Metadata, and Reasoning Pass-through

**Files:**
- Modify: `workbuddy/models.go:13-33,170-194`
- Modify: `workbuddy/models_test.go`
- Modify: `workbuddy/payload.go:16-49,119-135,288-320,438-458`
- Modify: `workbuddy/sanitize_test.go:83-265`
- Modify: `workbuddy/toolchoice_test.go`

**Interfaces:**
- Consumes: `pluginapi.ModelInfo`, existing alias/exclusion helpers, `prepareUpstreamBody`.
- Produces: `defaultModelInfo(id, name string) pluginapi.ModelInfo`; `wbModels()` returning only `auto`; an interim `model.for_auth` empty-success result until Task 6 wires readiness.

- [ ] **Step 1: Replace fixed-catalog tests with the static contract tests**

Replace the fixed-list assertions in `models_test.go` with these compile-ready tests:

```go
func TestWBModelsReturnsOnlyAutoDefaultMetadata(t *testing.T) {
	models := wbModels()
	if len(models) != 1 {
		t.Fatalf("len(wbModels()) = %d, want 1", len(models))
	}
	want := pluginapi.ModelInfo{
		ID:                         "auto",
		Name:                       "auto",
		OwnedBy:                    providerName,
		SupportedGenerationMethods: []string{"chat"},
	}
	if !reflect.DeepEqual(models[0], want) {
		t.Fatalf("auto metadata = %#v, want %#v", models[0], want)
	}
}

func TestDefaultModelInfoUsesSourceNameThenID(t *testing.T) {
	if got := defaultModelInfo("serve-alpha", "Alpha"); got.Name != "Alpha" || got.ID != "serve-alpha" {
		t.Fatalf("named default = %#v", got)
	}
	if got := defaultModelInfo("serve-beta", ""); got.Name != "serve-beta" || got.ID != "serve-beta" {
		t.Fatalf("unnamed default = %#v", got)
	}
}

func TestHandleModelForAuthNeverFallsBackToAuto(t *testing.T) {
	raw, err := handleModelForAuth(mustJSON(pluginapi.AuthModelRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.ModelResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Provider != providerName || resp.Models == nil || len(resp.Models) != 0 {
		t.Fatalf("failed model response = %#v", resp)
	}
}
```

Add `reflect` to imports. Add a static handler test that calls `handleModelStatic` twice and verifies both responses contain only `auto`; do not mock HTTP because the handler must have no network seam at all.

- [ ] **Step 2: Make reasoning tests express the new generic contract**

Keep `TestPrepareUpstreamBodySanitizesFingerprintsAndPreservesReasoningEffort`. Delete all `TestForceMaxThinking_*` and model-specific policy tests. Add:

```go
func TestPrepareUpstreamBodyPreservesCallerReasoningEffort(t *testing.T) {
	body := []byte(`{"model":"serve-alpha","reasoning_effort":"medium","messages":[]}`)
	out := prepareUpstreamBody(body, nil, nil, "serve-beta")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "serve-beta" || obj["reasoning_effort"] != "medium" {
		t.Fatalf("rewritten payload = %#v", obj)
	}
}
```

Change real model fixtures in `toolchoice_test.go` to `serve-alpha` and `serve-beta` without changing tool-choice assertions.

- [ ] **Step 3: Run the targeted tests and confirm RED**

Run:

```powershell
go -C workbuddy test . -run 'Test(WBModelsReturnsOnlyAutoDefaultMetadata|DefaultModelInfoUsesSourceNameThenID|HandleModelForAuthNeverFallsBackToAuto|PrepareUpstreamBodyPreservesCallerReasoningEffort)' -count=1
```

Expected: FAIL because `defaultModelInfo` is undefined, `wbModels` returns 17 entries, and `model.for_auth` still returns the fixed list.

- [ ] **Step 4: Implement the minimum static contract**

Use exactly one default constructor and one static entry:

```go
func defaultModelInfo(id, name string) pluginapi.ModelInfo {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if name == "" {
		name = id
	}
	return pluginapi.ModelInfo{
		ID:                         id,
		Name:                       name,
		OwnedBy:                    providerName,
		SupportedGenerationMethods: []string{"chat"},
	}
}

func wbModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{defaultModelInfo("auto", "")}
}
```

Until Task 6 installs the runtime, make `handleModelForAuth` decode the request, cache aliases, and return:

```go
models := []pluginapi.ModelInfo{}
return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
```

Remove both `forceMaxThinking(obj)` calls and delete `forceMaxThinking`. Update nearby comments to say system sanitization, not forced thinking. Preserve model rewrite, system sanitization, tool normalization, desensitization and system injection.

- [ ] **Step 5: Run GREEN and full Go regression**

Run:

```powershell
go -C workbuddy test . -run 'Test(WBModelsReturnsOnlyAutoDefaultMetadata|DefaultModelInfoUsesSourceNameThenID|HandleModelForAuthNeverFallsBackToAuto|PrepareUpstreamBodyPreservesCallerReasoningEffort)' -count=1
go -C workbuddy test ./... -count=1
```

Expected: PASS. No old test may require a named production model or forced reasoning level.

- [ ] **Step 6: Commit only this cycle**

```powershell
git add workbuddy/models.go workbuddy/models_test.go workbuddy/payload.go workbuddy/sanitize_test.go workbuddy/toolchoice_test.go
git commit -m "refactor(workbuddy): reduce static model contract to auto"
```

---

### Task 3: WorkBuddy Realm, Parsers, and Strict Endpoint Fallback

**Files:**
- Create: `workbuddy/model_source_workbuddy.go`
- Create: `workbuddy/model_source_workbuddy_test.go`

**Interfaces:**
- Consumes: `storedAuth`, `backendHeaders`, `upstreamBaseCN`, `upstreamBaseGlobal`, `originReferer`, `originRefererGlobal`, `hostHTTPResponse`.
- Produces: `modelFacts`, `modelHTTPDo`, `workBuddyRealm`, `workBuddyEndpointKind`, `workBuddyCatalog`, `fetchWorkBuddyCatalog(sa *storedAuth, callbackID string, do modelHTTPDo) (workBuddyCatalog, error)`.

- [ ] **Step 1: Write pure parser and realm RED tests**

Use only synthetic IDs and these fixture shapes:

```go
func TestParseWorkBuddyV3ConfigSelectsCompleteCLIList(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"agents":[{"name":"editor","models":["ignored"]},{"name":"cli","models":["serve-alpha","serve-beta"]}]}}`)
	got, err := parseWorkBuddyV3Config(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "serve-alpha" || got[1].ID != "serve-beta" {
		t.Fatalf("models = %#v", got)
	}
}

func TestParseWorkBuddyLegacyModelsDropsDisabled(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"models":[{"id":"serve-alpha","name":"Alpha","disabled":false,"contextWindow":4096,"maxTokens":512},{"id":"serve-off","disabled":true}]}}`)
	got, err := parseWorkBuddyLegacyModels(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "serve-alpha" || got[0].ContextLength == nil || *got[0].ContextLength != 4096 {
		t.Fatalf("models = %#v", got)
	}
}
```

Add table tests for: missing `cli`, empty list, duplicate ID, whitespace-only ID, ID longer than 512 bytes, negative `contextWindow`, negative `maxTokens`, malformed JSON, non-zero business `code`, and wrong field types. Every case must fail the whole snapshot. Add JWT tests using unsigned synthetic payloads whose `iss` host is `codebuddy.cn`, `copilot.tencent.com`, or `workbuddy.ai`; malformed and unknown issuer must fail.

Keep these helpers in `model_source_workbuddy_test.go` for all later source/readiness tests:

```go
func syntheticAccessToken(t *testing.T, issuer string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(map[string]string{"iss": issuer})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func syntheticStoredAuth(t *testing.T, realm workBuddyRealm) *storedAuth {
	t.Helper()
	issuer := "https://codebuddy.cn/realms/cli"
	domain := "codebuddy.cn"
	if realm == workBuddyRealmGlobal {
		issuer = "https://workbuddy.ai/realms/cli"
		domain = "workbuddy.ai"
	}
	return &storedAuth{
		Auth: storedTokens{AccessToken: syntheticAccessToken(t, issuer), Domain: domain},
		Account: storedAccount{UID: "uid-1", EnterpriseID: "enterprise-1"},
	}
}
```

- [ ] **Step 2: Run parser tests and confirm RED**

Run:

```powershell
go -C workbuddy test . -run 'Test(WorkBuddyRealm|ParseWorkBuddy|ValidateWorkBuddy)' -count=1
```

Expected: FAIL with undefined source types/functions.

- [ ] **Step 3: Implement strict normalized parsing**

Define:

```go
const maxDiscoveredModelIDBytes = 512
const modelSourceRequestTimeout = 15 * time.Second

type workBuddyRealm string

const (
	workBuddyRealmCN     workBuddyRealm = "cn"
	workBuddyRealmGlobal workBuddyRealm = "global"
)

type workBuddyEndpointKind string

const (
	workBuddyEndpointV3Config             workBuddyEndpointKind = "v3_config"
	workBuddyEndpointLegacyPersonalModels workBuddyEndpointKind = "legacy_personal_models"
)

type workBuddyCatalog struct {
	Realm    workBuddyRealm        `json:"realm"`
	Endpoint workBuddyEndpointKind `json:"endpoint"`
	Models   []modelFacts          `json:"models"`
}

type workBuddyAgentWire struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

type workBuddyLegacyModelWire struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Disabled      bool   `json:"disabled"`
	ContextWindow *int64 `json:"contextWindow"`
	MaxTokens     *int64 `json:"maxTokens"`
}
```

Implement JWT payload decoding with `base64.RawURLEncoding`, parse `iss` as a URL, classify `workbuddy.ai` as Global and `codebuddy.cn`/`copilot.tencent.com` as CN, and reject any other host. This decodes routing facts only and must not claim signature verification.

Implement `validateModelFacts` to trim IDs/names, reject empty/oversize/duplicate IDs and negative numeric pointers, copy modality slices, and require a non-empty final list. Use normal `encoding/json` so additive unknown fields are ignored, but validate every consumed field.

- [ ] **Step 4: Write HTTP routing and fallback RED tests**

Build a fake `modelHTTPDo` that records request method, URL, headers and callback ID. Add:

```go
func TestFetchWorkBuddyCatalogFallsBackOnlyOn404Or405(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			calls := 0
			do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
				calls++
				if calls == 1 {
					return &hostHTTPResponse{StatusCode: status, Headers: make(http.Header)}, nil
				}
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"models":[{"id":"serve-alpha"}]}}`)}, nil
			}
			got, err := fetchWorkBuddyCatalog(syntheticStoredAuth(t, workBuddyRealmCN), "callback-1", do)
			if err != nil || calls != 2 || got.Endpoint != workBuddyEndpointLegacyPersonalModels {
				t.Fatalf("catalog=%#v calls=%d err=%v", got, calls, err)
			}
		})
	}
}
```

Add a table for primary 401, 403, 500, transport error, code non-zero, empty and malformed bodies; assert exactly one call. Add CN/Global tests asserting the exact base host, `/v3/config`, `Authorization`, `Accept: application/json`, realm Origin/Referer, existing client identity headers and unchanged callback ID.

- [ ] **Step 5: Run HTTP tests and confirm RED**

Run:

```powershell
go -C workbuddy test . -run 'TestFetchWorkBuddyCatalog' -count=1
```

Expected: FAIL because `fetchWorkBuddyCatalog` is undefined.

- [ ] **Step 6: Implement one callback-aware HTTP path**

Define a safe source error:

```go
type modelSourceFailureKind string

const (
	modelSourceTransportFailure modelSourceFailureKind = "transport"
	modelSourceHTTPFailure      modelSourceFailureKind = "http"
	modelSourceSchemaFailure    modelSourceFailureKind = "schema"
)

type modelSourceError struct {
	Kind       modelSourceFailureKind
	StatusCode int
	err        error
}
```

`Error()` may return only `model source transport failure`, `model source HTTP <status>`, or `model source schema failure`; `Unwrap()` returns `err`. It must not include URL, body, token or host error text.

`fetchWorkBuddyCatalog` must:

1. derive realm from JWT;
2. create a 15-second request context;
3. build `GET <realm-base>/v3/config`;
4. call `backendHeaders`, then override `Accept` to `application/json` and realm Origin/Referer;
5. invoke only the injected `do(req, callbackID)`;
6. on 200 parse a complete primary snapshot;
7. on exactly 404/405 create and invoke the legacy request;
8. reject every other status/error without legacy fallback.

Production passes `hostHTTPDoWithCallback` as `modelHTTPDo`; do not add another bridge helper, client interface or goroutine timeout.

- [ ] **Step 7: Run GREEN and package regression**

Run:

```powershell
go -C workbuddy test . -run 'Test(WorkBuddyRealm|ParseWorkBuddy|ValidateWorkBuddy|FetchWorkBuddyCatalog)' -count=1
go -C workbuddy test ./... -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit the WorkBuddy source**

```powershell
git add workbuddy/model_source_workbuddy.go workbuddy/model_source_workbuddy_test.go
git commit -m "feat(workbuddy): discover account model catalog"
```

---

### Task 4: models.dev Parser, Opaque ETag, Dynamic Matching, and Enrichment

**Files:**
- Create: `workbuddy/model_source_modelsdev.go`
- Create: `workbuddy/model_source_modelsdev_test.go`

**Interfaces:**
- Consumes: `modelFacts`, `modelHTTPDo`, `modelSourceError`, `defaultModelInfo`.
- Produces: `modelsDevFetchResult`, `fetchModelsDevMetadata(etag, callbackID string, do modelHTTPDo) (modelsDevFetchResult, error)`, `parseModelsDevMetadata`, `matchModelsDevRecord`, `modelInfoFromSources`.

- [ ] **Step 1: Write parser and matching RED tests**

Use a top-level canonical map fixture:

```go
func TestParseModelsDevMetadataAcceptsAdditiveFields(t *testing.T) {
	raw := []byte(`{
		"vendor/serve-alpha":{"id":"serve-alpha","name":"Alpha Canonical","limit":{"context":32768,"output":4096},"modalities":{"input":["text","image"],"output":["text"]},"future":{"accepted":true}},
		"vendor/serve-beta":{"id":"serve-beta","name":"Beta Canonical"}
	}`)
	got, err := parseModelsDevMetadata(raw)
	if err != nil {
		t.Fatal(err)
	}
	alpha := got["vendor/serve-alpha"]
	if alpha.ContextLength == nil || *alpha.ContextLength != 32768 || alpha.MaxCompletionTokens == nil || *alpha.MaxCompletionTokens != 4096 {
		t.Fatalf("alpha = %#v", alpha)
	}
}
```

Add failures for empty top-level map, invalid canonical key, negative limits, wrong consumed field types, duplicate/empty modalities and model IDs over 512 bytes. Missing optional limit/modalities must remain nil.

Add exact tests for:

- full key `vendor/serve-alpha` matches exactly;
- raw `serve-alpha` matches a unique final `/` segment;
- case mismatch does not match;
- two canonical keys ending in `/serve-alpha` are ambiguous and unmatched;
- no candidate is unmatched.

- [ ] **Step 2: Run parser/matcher tests and confirm RED**

Run:

```powershell
go -C workbuddy test . -run 'Test(ParseModelsDevMetadata|MatchModelsDevRecord)' -count=1
```

Expected: FAIL with undefined models.dev types/functions.

- [ ] **Step 3: Implement the consumer parser and linear matcher**

Define:

```go
const modelsDevMetadataURL = "https://models.dev/models.json"

type modelsDevLimitWire struct {
	Context *int64 `json:"context"`
	Output  *int64 `json:"output"`
}

type modelsDevModalitiesWire struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type modelsDevModelWire struct {
	ID         string                   `json:"id"`
	Name       string                   `json:"name"`
	Limit      *modelsDevLimitWire      `json:"limit"`
	Modalities *modelsDevModalitiesWire `json:"modalities"`
}

type modelsDevFetchResult struct {
	NotModified bool
	ETag        string
	Records     map[string]modelFacts
}
```

`parseModelsDevMetadata` must accept additive unknown fields, validate only consumed fields, preserve missing pointers/slices, copy slices, reject invalid records as a whole, and keep canonical map keys as `modelFacts.ID`. Do not derive price or provider-specific facts.

`matchModelsDevRecord` first performs case-sensitive map lookup by full canonical ID. Otherwise it scans records and accepts only one key whose final `/` segment equals the raw ID exactly. O(n) is deliberate because refresh is one-time and avoids a second persistent/index structure.

- [ ] **Step 4: Write ETag, 304 and merge RED tests**

Add tests that assert:

```go
func TestFetchModelsDevMetadataPreservesOpaqueETag(t *testing.T) {
	const opaque = `W/"opaque value,with punctuation"`
	var ifNoneMatch string
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		ifNoneMatch = req.Header.Get("If-None-Match")
		return &hostHTTPResponse{StatusCode: http.StatusNotModified, Headers: http.Header{"ETag": []string{opaque}}}, nil
	}
	got, err := fetchModelsDevMetadata(opaque, "callback-2", do)
	if err != nil || !got.NotModified || ifNoneMatch != opaque {
		t.Fatalf("result=%#v header=%q err=%v", got, ifNoneMatch, err)
	}
}
```

Also assert: 200 validates before returning; 304 returns no records and leaves cache validity to readiness; non-200/304 is a safe HTTP failure; callback ID is preserved; `Accept` is exact.

For merge, use serving facts with a name and canonical facts with limits/modalities. Assert serving non-missing values win, canonical only fills missing values, response ID remains the WorkBuddy raw ID, unmatched uses the default template, and mutating returned slices cannot mutate input records.

- [ ] **Step 5: Run HTTP/merge tests and confirm RED**

Run:

```powershell
go -C workbuddy test . -run 'Test(FetchModelsDevMetadata|ModelInfoFromSources)' -count=1
```

Expected: FAIL because fetch and merge functions do not exist.

- [ ] **Step 6: Implement HTTP and source precedence**

`fetchModelsDevMetadata` creates a 15-second GET request, sets `Accept: application/json`, sets `If-None-Match` without trimming/parsing when non-empty, and calls injected `modelHTTPDo`. Status 304 returns `NotModified:true`; status 200 parses the full body before returning records and preserves the response ETag exactly; all other outcomes return classified errors.

Implement merge with this contract:

```go
func modelInfoFromSources(serving modelFacts, canonical *modelFacts) pluginapi.ModelInfo {
	merged := cloneModelFacts(serving)
	if canonical != nil {
		fillMissingModelFacts(&merged, *canonical)
	}
	info := defaultModelInfo(serving.ID, merged.Name)
	info.Description = merged.Description
	if merged.ContextLength != nil {
		info.ContextLength = *merged.ContextLength
	}
	if merged.MaxCompletionTokens != nil {
		info.MaxCompletionTokens = *merged.MaxCompletionTokens
	}
	info.SupportedInputModalities = append([]string(nil), merged.SupportedInputModalities...)
	info.SupportedOutputModalities = append([]string(nil), merged.SupportedOutputModalities...)
	return info
}
```

Do not add thinking/cost/tool/temperature fields or an explicit WorkBuddy-to-canonical mapping.

- [ ] **Step 7: Run GREEN and package regression**

Run:

```powershell
go -C workbuddy test . -run 'Test(ParseModelsDevMetadata|MatchModelsDevRecord|FetchModelsDevMetadata|ModelInfoFromSources)' -count=1
go -C workbuddy test ./... -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit the models.dev source**

```powershell
git add workbuddy/model_source_modelsdev.go workbuddy/model_source_modelsdev_test.go
git commit -m "feat(workbuddy): enrich dynamic models from models.dev"
```

---

### Task 5: Versioned Persistent Stores and Last-good Backup

**Files:**
- Create: `workbuddy/model_store.go`
- Create: `workbuddy/model_store_test.go`

**Interfaces:**
- Consumes: `modelFacts`, `workBuddyRealm`, `workBuddyEndpointKind`, `storedAuth`.
- Produces: `modelAuthIdentityFor`, `metadataCacheV1`, `modelCatalogCacheV1`, `modelStore`, load/save methods, `errFutureModelCacheSchema`.

- [ ] **Step 1: Write identity and schema RED tests**

Define tests that assert:

```go
func TestModelAuthIdentityDoesNotDependOnToken(t *testing.T) {
	left := &storedAuth{Auth: storedTokens{AccessToken: "token-one", Domain: "codebuddy.cn"}, Account: storedAccount{UID: "uid-1", EnterpriseID: "ent-1"}}
	right := &storedAuth{Auth: storedTokens{AccessToken: "token-two", Domain: "codebuddy.cn"}, Account: storedAccount{UID: "uid-1", EnterpriseID: "ent-1"}}
	li, err := modelAuthIdentityFor("auth-a", left)
	if err != nil {
		t.Fatal(err)
	}
	ri, err := modelAuthIdentityFor("auth-b", right)
	if err != nil {
		t.Fatal(err)
	}
	if li.sha256() != ri.sha256() {
		t.Fatalf("token or AuthID changed stable identity: %q != %q", li.sha256(), ri.sha256())
	}
}
```

Add tests that UID-missing identities include trimmed AuthID, realm and EnterpriseID changes alter the hash, and hash filenames contain exactly 64 lowercase hex characters. Add JSON round-trip tests for schema v1 and exact cache paths beneath a `t.TempDir()` root.

- [ ] **Step 2: Write load/save/backup RED tests**

Cover all of these independently:

- first save creates `metadata.json` or `models/<hash>.json`;
- second save moves the previous validated primary to `.bak` and installs new primary;
- corrupt primary loads valid `.bak`;
- corrupt primary does not overwrite an existing valid `.bak` during save;
- corrupt primary and backup return `found=false` plus a read error;
- identity mismatch returns no valid model cache;
- schema version 2 returns `errFutureModelCacheSchema` and save refuses to overwrite it;
- saved bytes do not contain test access token, refresh token, auth path, raw StorageJSON or alias;
- on non-Windows, directories are mode `0700` and files are mode `0600`;
- no temp file remains after a forced write/rename failure.

Inject filesystem failures with a read-only test directory where supported; skip only the permission-specific case on Windows, not schema/backup tests.

- [ ] **Step 3: Run store tests and confirm RED**

Run:

```powershell
go -C workbuddy test . -run 'Test(ModelAuthIdentity|ModelStore|WriteModelCacheAtomic)' -count=1
```

Expected: FAIL with undefined store types/functions.

- [ ] **Step 4: Implement the exact schemas and store surface**

```go
const modelCacheSchemaVersion = 1

var errFutureModelCacheSchema = errors.New("future model cache schema")

type modelAuthIdentity struct {
	Provider     string `json:"provider"`
	Realm        string `json:"realm"`
	UID          string `json:"uid,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	AuthID       string `json:"auth_id,omitempty"`
}

type metadataCacheV1 struct {
	SchemaVersion int                   `json:"schema_version"`
	ETag          string                `json:"etag,omitempty"`
	FetchedAt     time.Time             `json:"fetched_at"`
	Records       map[string]modelFacts `json:"records"`
}

type modelCatalogCacheV1 struct {
	SchemaVersion  int                   `json:"schema_version"`
	IdentitySHA256 string                `json:"identity_sha256"`
	Realm          workBuddyRealm        `json:"realm"`
	FetchedAt      time.Time             `json:"fetched_at"`
	Endpoint       workBuddyEndpointKind `json:"endpoint"`
	Models         []modelFacts          `json:"models"`
}

type modelStore struct {
	root string
}

func defaultModelStore() (*modelStore, error)
func newModelStore(root string) *modelStore
func (s *modelStore) loadMetadata() (metadataCacheV1, bool, error)
func (s *modelStore) saveMetadata(metadataCacheV1) error
func (s *modelStore) loadModels(identitySHA256 string) (modelCatalogCacheV1, bool, error)
func (s *modelStore) saveModels(modelCatalogCacheV1) error
```

`defaultModelStore` uses:

```go
configDir, err := os.UserConfigDir()
root := filepath.Join(configDir, "CLIProxyAPI", "workbuddy", "model-catalog")
```

Do not hardcode `/root`; on Linux root this naturally resolves to `/root/.config/CLIProxyAPI/workbuddy/model-catalog`.

`modelAuthIdentityFor` derives realm from the same WorkBuddy realm helper, includes provider/realm/UID/EnterpriseID, and includes AuthID only when UID is empty. `sha256()` JSON-marshals the normalized struct and returns `hex.EncodeToString(sum[:])`.

- [ ] **Step 5: Implement one atomic write primitive**

Use one internal function:

```go
func writeModelCacheAtomic(path string, data []byte, validateCurrent func([]byte) error) error
```

The function must create the parent with `0700`; create same-directory temps; `Chmod(0600)`; write all bytes; `Sync`; `Close`; validate an existing primary before copying it through another synced temp to `.bak`; replace `.bak`; replace primary; then sync the parent directory. A corrupt primary must not replace a valid backup. If either discovered file has a future schema, return the sentinel and do not write. Every temp path is removed by `defer` unless renamed.

Use `os.Rename` on Unix. On Windows, remove the destination immediately before rename when required; this reduced power-loss guarantee is the explicit platform exclusion in the spec. Do not add a lock file.

- [ ] **Step 6: Run GREEN, race and package regression**

Run:

```powershell
go -C workbuddy test . -run 'Test(ModelAuthIdentity|ModelStore|WriteModelCacheAtomic)' -count=1
go -C workbuddy test -race . -run 'Test(ModelAuthIdentity|ModelStore|WriteModelCacheAtomic)' -count=1
go -C workbuddy test ./... -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit the store**

```powershell
git add workbuddy/model_store.go workbuddy/model_store_test.go
git commit -m "feat(workbuddy): persist last-good model catalogs"
```

---

### Task 6: Fresh Bootstrap and Fail-closed Readiness

**Files:**
- Create: `workbuddy/model_readiness.go`
- Create: `workbuddy/model_readiness_test.go`

**Interfaces:**
- Consumes: both source fetchers, `modelStore`, cache schemas, `matchModelsDevRecord`, `modelInfoFromSources`.
- Produces: readiness states/sources/error codes; `newModelRuntime`, `ensureForAuth`, read-only snapshots.

- [ ] **Step 1: Write no-cache fresh bootstrap RED tests**

Build a fake `modelHTTPDo` keyed by request host/path and a real `newModelStore(t.TempDir())`. Start with this success test:

```go
func TestModelRuntimeFreshBootstrapReady(t *testing.T) {
	store := newModelStore(t.TempDir())
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if callbackID != "callback-fresh" {
			t.Fatalf("callback ID = %q", callbackID)
		}
		switch {
		case req.URL.Host == "copilot.tencent.com" && req.URL.Path == "/v3/config":
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		case req.URL.Host == "models.dev" && req.URL.Path == "/models.json":
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"ETag": []string{`"fresh-etag"`}}, Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","name":"Alpha","limit":{"context":32768,"output":4096}}}`)}, nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	}
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	runtime := newModelRuntime(store, do)
	got := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID:      "auth-fresh",
			StorageJSON: mustJSON(sa),
		},
		HostCallbackID: "callback-fresh",
	})
	if got.State != modelReady || got.ModelSource != modelSourceFresh || got.MetadataSource != modelSourceFresh {
		t.Fatalf("snapshot = %#v", got)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "serve-alpha" || got.Models[0].ContextLength != 32768 {
		t.Fatalf("models = %#v", got.Models)
	}
	identity, err := modelAuthIdentityFor("auth-fresh", sa)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.loadModels(identity.sha256()); err != nil || !found {
		t.Fatalf("model cache found=%v err=%v", found, err)
	}
	if _, found, err := store.loadMetadata(); err != nil || !found {
		t.Fatalf("metadata cache found=%v err=%v", found, err)
	}
}
```

Add a table-driven failure test with these fault positions: invalid auth, WorkBuddy transport, WorkBuddy HTTP, WorkBuddy schema, WorkBuddy save, models.dev transport, models.dev HTTP, models.dev schema and metadata save. Each table row supplies a `modelHTTPDo` and, for save faults, a store rooted under a regular file so directory creation fails. For every no-cache case assert:

```go
if got.State != modelFailed || got.executable() || got.Models == nil || len(got.Models) != 0 {
	t.Fatalf("snapshot = %#v", got)
}
```

Assert the expected fixed `modelErrorCode`, and assert no raw error, URL, token or response body exists in the snapshot.

- [ ] **Step 2: Run fresh bootstrap tests and confirm RED**

Run:

```powershell
go -C workbuddy test . -run 'TestModelRuntimeFreshBootstrap' -count=1
```

Expected: FAIL because readiness runtime is undefined.

- [ ] **Step 3: Implement immutable snapshot and runtime surfaces**

```go
type modelReadinessSnapshot struct {
	State             modelReadinessState
	ModelSource       modelSnapshotSource
	MetadataSource    modelSnapshotSource
	ModelsFetchedAt   time.Time
	MetadataFetchedAt time.Time
	ErrorCode         modelErrorCode
	Models             []pluginapi.ModelInfo
	configGeneration   uint64
	authGeneration     uint64
	identitySHA256     string
}

func (s modelReadinessSnapshot) executable() bool {
	return s.State.executable()
}

type modelMetadataStatus struct {
	Source    modelSnapshotSource
	FetchedAt time.Time
	ErrorCode modelErrorCode
}

type modelMetadataResult struct {
	cache     metadataCacheV1
	source    modelSnapshotSource
	errorCode modelErrorCode
	ok        bool
}

type modelRuntime struct {
	store              *modelStore
	do                 modelHTTPDo
	storeError         modelErrorCode
	mu                 sync.Mutex
	snapshots          map[string]modelReadinessSnapshot
	metadataCache      *metadataCacheV1
	metadataResult     *modelMetadataResult
	configGeneration   atomic.Uint64
}

var activeModelRuntime atomic.Pointer[modelRuntime]

func newModelRuntime(store *modelStore, do modelHTTPDo) *modelRuntime
func currentModelRuntime() *modelRuntime
func (r *modelRuntime) ensureForAuth(req authModelRequestWire) modelReadinessSnapshot
func (r *modelRuntime) snapshotForAuthID(authID string) modelReadinessSnapshot
func (r *modelRuntime) metadataStatus() modelMetadataStatus
func (r *modelRuntime) advanceConfigGeneration() uint64
func (r *modelRuntime) markAuthNotStarted(authID string)
```

`currentModelRuntime` lazily constructs one runtime from `defaultModelStore()` and `hostHTTPDoWithCallback`, then installs it with `activeModelRuntime.CompareAndSwap(nil, candidate)`. During construction it calls `store.loadMetadata()` once to seed `metadataCache` and read-only `metadataStatus`; this never settles the refresh flight, so the first auth still attempts models.dev. Invalid/missing cache leaves source `none`; a cache read/future-schema error records only `modelErrorCacheRead`. If `defaultModelStore` fails, install a runtime with `storeError:modelErrorCacheRead`; registration remains successful, but auth bootstrap fails closed. Tests replace `activeModelRuntime` with `Swap` and restore it in `t.Cleanup`.

Add `cloneModelInfo`, `cloneModelInfos` and `cloneModelReadinessSnapshot`; copy `SupportedGenerationMethods`, input/output modalities, `Thinking` and `Thinking.Levels`. Published snapshots are never mutated.

- [ ] **Step 4: Implement the fresh bootstrap transaction**

For one auth request:

1. parse `StorageJSON` through existing `parseStored`;
2. derive identity and token hash;
3. publish `loading`;
4. load both caches, retaining valid values only;
5. fetch WorkBuddy and validate;
6. persist WorkBuddy before treating it as fresh;
7. fetch models.dev and validate;
8. persist models.dev before treating it as fresh; a 304 without valid metadata cache is a schema failure;
9. enrich every WorkBuddy model by exact/unique-last-segment matching;
10. publish `ready` only when both selected sources are fresh;
11. publish `failed` with a non-nil empty model slice when any missing source cannot become fresh.

Map source error kinds to fixed source-specific codes. Cache read/write failures map only to `cache_read`/`cache_write`. Keep raw errors local to the function and do not store them.

At this stage a single mutex implementation is acceptable so tests become green; Tasks 8 and 9 replace it with keyed flights and generation commit checks. Do not add `time.Sleep` to tests.

- [ ] **Step 5: Run GREEN and full regression**

Run:

```powershell
go -C workbuddy test . -run 'TestModelRuntimeFreshBootstrap' -count=1
go -C workbuddy test ./... -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit fresh readiness**

```powershell
git add workbuddy/model_readiness.go workbuddy/model_readiness_test.go
git commit -m "feat(workbuddy): fail closed during fresh model bootstrap"
```

---

### Task 7: Stale Bootstrap and Independent Last-good Fallback

**Files:**
- Modify: `workbuddy/model_readiness.go`
- Modify: `workbuddy/model_readiness_test.go`

**Interfaces:**
- Consumes: Task 6 runtime and both valid cache schemas.
- Produces: correct `ready`/`stale` composition, 304 success and retryable no-cache metadata failure.

- [ ] **Step 1: Write stale matrix RED tests**

Preseed valid model and metadata caches with fetched times and records distinct from fresh fixtures. Add table cases:

| WorkBuddy refresh | models.dev refresh | Expected state | Model source | Metadata source |
|---|---|---|---|---|
| success + save | success + save | `ready` | `fresh` | `fresh` |
| failure | success + save | `stale` | `cache` | `fresh` |
| success + save | failure | `stale` | `fresh` | `cache` |
| failure | failure | `stale` | `cache` | `cache` |
| success + save | 304 with cache | `ready` | `fresh` | `fresh` |

For every stale case assert execution remains allowed and the selected record actually comes from the correct old/fresh source. Add persistence-failure cases proving old primary remains selected and state is stale. Add partial/corrupt fresh body cases proving it never replaces last-good.

Add a no-cache metadata failure retry test: first auth attempt fails and closes its global flight; a later auth attempt with a now-successful fake source makes a second metadata HTTP call and can become ready.

- [ ] **Step 2: Run stale tests and confirm RED**

Run:

```powershell
go -C workbuddy test . -run 'TestModelRuntime(Stale|NotModified|RetriesMetadataWithoutCache)' -count=1
```

Expected: FAIL because Task 6 currently treats refresh failure as failed or does not compose sources independently.

- [ ] **Step 3: Implement source-by-source fallback**

Represent each source result with concrete selection helpers, not a generic interface:

```go
type modelCatalogSelection struct {
	cache     modelCatalogCacheV1
	source    modelSnapshotSource
	errorCode modelErrorCode
	ok        bool
}

func selectModelCatalog(
	fresh modelCatalogCacheV1,
	freshOK bool,
	cached modelCatalogCacheV1,
	cacheOK bool,
	failure modelErrorCode,
) modelCatalogSelection {
	if freshOK {
		return modelCatalogSelection{cache: fresh, source: modelSourceFresh, ok: true}
	}
	if cacheOK {
		return modelCatalogSelection{cache: cached, source: modelSourceCache, errorCode: failure, ok: true}
	}
	return modelCatalogSelection{source: modelSourceNone, errorCode: failure}
}

type metadataSelection struct {
	cache     metadataCacheV1
	source    modelSnapshotSource
	errorCode modelErrorCode
	ok        bool
}

func selectMetadata(
	fresh metadataCacheV1,
	freshOK bool,
	cached metadataCacheV1,
	cacheOK bool,
	failure modelErrorCode,
) metadataSelection {
	if freshOK {
		return metadataSelection{cache: fresh, source: modelSourceFresh, ok: true}
	}
	if cacheOK {
		return metadataSelection{cache: cached, source: modelSourceCache, errorCode: failure, ok: true}
	}
	return metadataSelection{source: modelSourceNone, errorCode: failure}
}
```

A 304 is fresh success only when the loaded metadata cache is valid; pass that cache as both `fresh` and `cached` with `freshOK=true`, keep its existing records/ETag/fetched time, and do not rewrite the file. Final state is `ready` only when both selection sources are fresh; otherwise `stale`. If either selection has `ok=false`, publish failed. If both cache fallbacks carry an error, snapshot `ErrorCode` uses the WorkBuddy failure first, then metadata; ready clears the code. If a fresh save fails, pass `freshOK=false` and never select the in-memory fresh response.

A metadata refresh failure with cache settles metadata for the process as cache/stale. A failure without cache must not behave like `sync.Once`; close the current call and leave metadata unsettled so a later auth can retry.

- [ ] **Step 4: Run GREEN and race regression**

Run:

```powershell
go -C workbuddy test . -run 'TestModelRuntime(Stale|NotModified|RetriesMetadataWithoutCache)' -count=1
go -C workbuddy test -race . -run 'TestModelRuntime(Fresh|Stale|NotModified|RetriesMetadataWithoutCache)' -count=1
go -C workbuddy test ./... -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit stale fallback**

```powershell
git add workbuddy/model_readiness.go workbuddy/model_readiness_test.go
git commit -m "feat(workbuddy): start from validated stale model caches"
```

---

### Task 8: Per-auth Isolation, Singleflight, and Immutable Results

**Files:**
- Modify: `workbuddy/model_readiness.go`
- Modify: `workbuddy/model_readiness_test.go`

**Interfaces:**
- Consumes: Task 7 bootstrap transaction.
- Produces: per-auth slots with `atomic.Pointer`, per-auth keyed flights, one global metadata flight and lock-free reads.

- [ ] **Step 1: Write deterministic concurrency RED tests**

Use channels/barriers, never `time.Sleep`. The same-auth test must use this synchronization shape:

```go
func TestModelRuntimeSameAuthSingleflight(t *testing.T) {
	var workBuddyCalls atomic.Int32
	var metadataCalls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		switch req.URL.Host {
		case "copilot.tencent.com":
			if workBuddyCalls.Add(1) == 1 {
				close(started)
			}
			<-release
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		case "models.dev":
			metadataCalls.Add(1)
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha"}}`)}, nil
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return nil, nil
		}
	}
	runtime := newModelRuntime(newModelStore(t.TempDir()), do)
	req := authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-one", StorageJSON: mustJSON(syntheticStoredAuth(t, workBuddyRealmCN))}}
	results := make(chan modelReadinessSnapshot, 32)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- runtime.ensureForAuth(req)
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(results)
	for result := range results {
		if result.State != modelReady {
			t.Fatalf("result = %#v", result)
		}
	}
	if workBuddyCalls.Load() != 1 || metadataCalls.Load() != 1 {
		t.Fatalf("calls: workbuddy=%d metadata=%d", workBuddyCalls.Load(), metadataCalls.Load())
	}
}
```

Add these separate channel-based cases:

- launch two auths with different UID/EnterpriseID/realm and block each HTTP call independently; assert both calls are in flight concurrently and resulting IDs never cross;
- launch two auths simultaneously; assert one global models.dev HTTP call;
- mutate every slice and nested `Thinking.Levels` returned by one caller; assert the next snapshot is unchanged;
- call `snapshotForAuthID` from 32 goroutines while bootstrap publishes; race detector must remain clean;
- assert alias and exclusion config are absent from cache and shared snapshot.

- [ ] **Step 2: Run concurrency tests and confirm RED**

Run:

```powershell
go -C workbuddy test -race . -run 'TestModelRuntime(SameAuthSingleflight|DifferentAuthIsolation|MetadataSingleflight|SnapshotImmutable|ConcurrentReaders)' -count=1
```

Expected: FAIL from duplicate calls, serialization across auths, mutable returned slices or a race.

- [ ] **Step 3: Implement focused flights without a dependency**

Use:

```go
type modelGenerationKey struct {
	TokenSHA256    [sha256.Size]byte
	IdentitySHA256 string
}

type modelAuthSlot struct {
	mu       sync.Mutex
	current  atomic.Pointer[modelReadinessSnapshot]
	calls    map[uint64]*modelAuthCall
	nextAuth uint64
	key      modelGenerationKey
}

type modelAuthCall struct {
	done chan struct{}
}

type metadataCall struct {
	done   chan struct{}
	result modelMetadataResult
}

type modelRuntime struct {
	store            *modelStore
	do               modelHTTPDo
	storeError       modelErrorCode
	configGeneration atomic.Uint64
	authSlots        sync.Map
	metadataMu       sync.Mutex
	metadataCall     *metadataCall
	metadataCache    *metadataCacheV1
	metadataResult   *modelMetadataResult
}

func (r *modelRuntime) authSlot(authID string) *modelAuthSlot
```

Store auth slots in `sync.Map` keyed by host AuthID, with identity hash inside the generation key. The leader publishes `loading`, runs network without holding the slot mutex, publishes an immutable result, closes `done`, and removes only the call pointer it still owns. Waiters block on `done`, then read and clone `atomic.Pointer` state rather than a mutable call result.

Use a separate short mutex for one global `metadataCall`. Success, 304 or valid-cache fallback settles metadata for the process. Failure without cache closes and clears the call so a later caller may lead a new attempt. Do not use `sync.Once` or `x/sync/singleflight`.

`model.for_auth` is allowed to wait. `snapshotForAuthID` and `metadataStatus` perform only atomic/short read operations and never HTTP, disk access or flight waiting.

- [ ] **Step 4: Run GREEN under race and package regression**

Run:

```powershell
go -C workbuddy test -race . -run 'TestModelRuntime(SameAuthSingleflight|DifferentAuthIsolation|MetadataSingleflight|SnapshotImmutable|ConcurrentReaders)' -count=1
go -C workbuddy test -race ./... -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit isolation/concurrency**

```powershell
git add workbuddy/model_readiness.go workbuddy/model_readiness_test.go
git commit -m "feat(workbuddy): isolate concurrent model bootstrap by auth"
```

---

### Task 9: Config and Token Generation Race Gate

**Files:**
- Modify: `workbuddy/model_readiness.go`
- Modify: `workbuddy/model_readiness_test.go`
- Modify: `workbuddy/usage_config.go:60-162`

**Interfaces:**
- Consumes: Task 8 slots/flights; successful `configure` lifecycle.
- Produces: `modelGenerationKey`, stale commit rejection, config-generation invalidation.

- [ ] **Step 1: Write old-generation-late RED tests**

Use two token fixtures with the same UID/identity. The core race test must follow this order:

```go
func TestModelRuntimeOldGenerationCannotCommit(t *testing.T) {
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if req.URL.Host == "models.dev" {
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha"},"vendor/serve-beta":{"id":"serve-beta"}}`)}, nil
		}
		token := req.Header.Get("Authorization")
		if strings.HasSuffix(token, "signature-a") {
			close(oldStarted)
			<-releaseOld
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		}
		return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-beta"]}]}}`)}, nil
	}
	store := newModelStore(t.TempDir())
	runtime := newModelRuntime(store, do)
	saOld := syntheticStoredAuth(t, workBuddyRealmCN)
	parts := strings.Split(saOld.Auth.AccessToken, ".")
	saOld.Auth.AccessToken = parts[0] + "." + parts[1] + ".signature-a"
	saNew := *saOld
	saNew.Auth.AccessToken = parts[0] + "." + parts[1] + ".signature-b"
	oldDone := make(chan modelReadinessSnapshot, 1)
	go func() {
		oldDone <- runtime.ensureForAuth(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-race", StorageJSON: mustJSON(saOld)}})
	}()
	<-oldStarted
	newResult := runtime.ensureForAuth(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-race", StorageJSON: mustJSON(&saNew)}})
	if newResult.State != modelReady || len(newResult.Models) != 1 || newResult.Models[0].ID != "serve-beta" {
		t.Fatalf("new result = %#v", newResult)
	}
	close(releaseOld)
	<-oldDone
	current := runtime.snapshotForAuthID("auth-race")
	if len(current.Models) != 1 || current.Models[0].ID != "serve-beta" {
		t.Fatalf("old generation overwrote current = %#v", current)
	}
	identity, err := modelAuthIdentityFor("auth-race", &saNew)
	if err != nil {
		t.Fatal(err)
	}
	cached, found, err := store.loadModels(identity.sha256())
	if err != nil || !found || len(cached.Models) != 1 || cached.Models[0].ID != "serve-beta" {
		t.Fatalf("cache=%#v found=%v err=%v", cached, found, err)
	}
}
```

Also assert A does not replace `.bak` or publish an error. Add separate tests that old config snapshots read as `not_started` and are not executable, failed `configure` does not advance generation, and successful `configure` advances exactly once.

- [ ] **Step 2: Run generation tests and confirm RED**

Run:

```powershell
go -C workbuddy test -race . -run 'TestModelRuntime(OldGenerationCannotCommit|ConfigGenerationInvalidatesSnapshot|FailedConfigureKeepsGeneration)' -count=1
```

Expected: FAIL because Task 8 has no final generation comparison.

- [ ] **Step 3: Implement generation keys and commit checks**

```go
type modelGenerationKey struct {
	Config         uint64
	TokenSHA256    [sha256.Size]byte
	IdentitySHA256 string
}
```

Each slot increments `authGeneration` when this key changes and immediately publishes `loading`. Network calls from different generations may overlap. Immediately before each model-cache save, metadata-cache save and `atomic.Pointer.Store`, acquire only the relevant short mutex and compare current identity, auth generation and config generation. On mismatch, discard the result without persistence or publication.

`advanceConfigGeneration` uses an atomic counter. `snapshotForAuthID` returns a cloned `not_started` snapshot when the published snapshot generation differs from current config.

At the successful end of `configure`, call:

```go
currentModelRuntime().advanceConfigGeneration()
```

Do not call it on parse/proxy/feature failure paths. Registration must still avoid auth listing and HTTP.

- [ ] **Step 4: Run GREEN, race and package regression**

Run:

```powershell
go -C workbuddy test -race . -run 'TestModelRuntime(OldGenerationCannotCommit|ConfigGenerationInvalidatesSnapshot|FailedConfigureKeepsGeneration)' -count=1
go -C workbuddy test -race ./... -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the generation gate**

```powershell
git add workbuddy/model_readiness.go workbuddy/model_readiness_test.go workbuddy/usage_config.go
git commit -m "fix(workbuddy): reject stale model bootstrap generations"
```

---

### Task 10: RPC Bootstrap, Callback Propagation, Executor Guard, and Scheduler Gate

**Files:**
- Modify: `workbuddy/models.go:170-194`
- Modify: `workbuddy/models_test.go`
- Modify: `workbuddy/main.go:240-293,681-804`
- Modify: `workbuddy/executor_http.go:14-57`
- Modify: `workbuddy/executor_http_test.go`
- Modify: `workbuddy/scheduler.go:71-104`
- Modify: `workbuddy/scheduler_test.go`
- Modify: `workbuddy/oauth.go:363+` only to mark a successfully refreshed auth not-started
- Test: `workbuddy/model_readiness_test.go`

**Interfaces:**
- Consumes: `currentModelRuntime`, immutable snapshots, `authModelRequestWire`.
- Produces: final `model.for_auth`, `guardExecutorReadiness(authID string) []byte`, scheduler filtering, refresh invalidation.

- [ ] **Step 1: Write RPC integration RED tests**

Extend `TestInboundRPCWrappersPreserveHostCallbackID` with `model.for_auth` decoding through `authModelRequestWire`. Extend `TestInboundHandlersForwardHostCallbackID` with `models.go` requirements that pass `req.HostCallbackID` into `ensureForAuth` via the full wire.

Add handler tests for:

- ready/stale returns cloned dynamic models, applies alias cache/exclusion only to the response;
- failed/not-started returns `ok:true`, provider `workbuddy`, non-nil empty models, no `auto`;
- callback ID reaches both fake source calls;
- `model.static` remains `auto` and never observes runtime/cache;
- `plugin.register` and `plugin.reconfigure` perform zero authenticated HTTP calls.

- [ ] **Step 2: Write executor/scheduler gate RED tests**

For each of `handleExecExecute`, `handleExecStream` and `handleExecHTTPRequest`, install a failed/not-started runtime and pass deliberately invalid `StorageJSON`. Assert an `ok:false` envelope with:

```json
{"code":"not_ready","message":"WorkBuddy model catalog is not ready","http_status":503}
```

Because storage is invalid, this also proves the guard runs before credential parsing. Use a fake HTTP client and assert zero calls.

Add ready and stale pass-through tests. Add scheduler candidates in all five states and assert only ready/stale are considered; all blocked returns `Handled:false`. Because existing scheduler and executor HTTP tests predate readiness, add this test-only helper in `model_readiness_test.go` and call it with every auth ID expected to execute:

```go
func installModelStatesForTest(t *testing.T, states map[string]modelReadinessState) *modelRuntime {
	t.Helper()
	runtime := newModelRuntime(newModelStore(t.TempDir()), func(*http.Request, string) (*hostHTTPResponse, error) {
		t.Fatal("unexpected model bootstrap HTTP")
		return nil, nil
	})
	generation := runtime.configGeneration.Load()
	for authID, state := range states {
		slot := runtime.authSlot(authID)
		snapshot := modelReadinessSnapshot{
			State:            state,
			ModelSource:      modelSourceFresh,
			MetadataSource:   modelSourceFresh,
			Models:           []pluginapi.ModelInfo{},
			configGeneration: generation,
		}
		slot.current.Store(&snapshot)
	}
	old := activeModelRuntime.Swap(runtime)
	t.Cleanup(func() { activeModelRuntime.Store(old) })
	return runtime
}
```

Task 8 must therefore expose the internal `authSlot(authID string) *modelAuthSlot` helper used by production `ensureForAuth`; this is not a test-only production seam. Update the two pre-existing executor HTTP tests with an explicit `AuthID` and a ready snapshot. Update each pre-existing scheduler test that expects `Handled:true` with ready snapshots for its candidate IDs; tests for off mode, non-WorkBuddy, disabled or deferred paths do not need a ready state.

Add scope tests proving `management.handle`, `management.register`, `auth.parse`, `auth.refresh`, import, check-in, billing, keepalive and panel resources do not call the executor guard. A successful `auth.refresh` may mark the corresponding auth `not_started`, but it must return its normal refresh response first.

- [ ] **Step 3: Run integration tests and confirm RED**

Run:

```powershell
go -C workbuddy test . -run 'Test(ModelForAuth|InboundRPCWrappersPreserveHostCallbackID|InboundHandlersForwardHostCallbackID|ExecutorReadiness|SchedulerPick_.*Readiness|ReadinessGateScope)' -count=1
```

Expected: FAIL because final wiring and gates are absent.

- [ ] **Step 4: Wire `model.for_auth` without an error fallback**

Decode the complete wire:

```go
func handleModelForAuth(raw []byte) ([]byte, error) {
	var req authModelRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cacheModelAliases(req.Host)
	snapshot := currentModelRuntime().ensureForAuth(req)
	models := []pluginapi.ModelInfo{}
	if snapshot.State.executable() {
		models = cloneModelInfos(snapshot.Models)
		models = filterExcludedModels(models, req.Host)
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}
```

Keep aliases and exclusions out of persistent and shared snapshots. Do not return a Go/RPC error for bootstrap failure.

- [ ] **Step 5: Add one shared executor guard at the three concrete entries**

```go
func guardExecutorReadiness(authID string) []byte {
	if currentModelRuntime().snapshotForAuthID(authID).State.executable() {
		return nil
	}
	return errorEnvelopeWithStatus(
		"not_ready",
		"WorkBuddy model catalog is not ready",
		http.StatusServiceUnavailable,
	)
}
```

Immediately after each executor wire is decoded, before `parseStored`, URL validation or HTTP:

```go
if blocked := guardExecutorReadiness(req.AuthID); blocked != nil {
	return blocked, nil
}
```

Do not place the guard in `hostHTTPDo*`, shared payload helpers, management, OAuth or `count_tokens`.

- [ ] **Step 6: Filter scheduler candidates by readiness**

In the existing WorkBuddy candidate loop, after provider/disabled checks:

```go
if !currentModelRuntime().snapshotForAuthID(c.ID).State.executable() {
	continue
}
```

Do not bootstrap, wait, add replay or change active-auth/credits behavior. If no executable candidate remains, return `Handled:false`.

On successful plugin token refresh, call `currentModelRuntime().markAuthNotStarted(req.AuthID)` after the normal refresh result is complete; this invalidates execution but performs no catalog fetch. The next `model.for_auth` observes the new token generation and bootstraps.

- [ ] **Step 7: Run GREEN and full race regression**

Run:

```powershell
go -C workbuddy test . -run 'Test(ModelForAuth|InboundRPCWrappersPreserveHostCallbackID|InboundHandlersForwardHostCallbackID|ExecutorReadiness|SchedulerPick_.*Readiness|ReadinessGateScope)' -count=1
go -C workbuddy test -race ./... -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit host integration**

```powershell
git add workbuddy/models.go workbuddy/models_test.go workbuddy/main.go workbuddy/executor_http.go workbuddy/executor_http_test.go workbuddy/scheduler.go workbuddy/scheduler_test.go workbuddy/oauth.go workbuddy/model_readiness.go workbuddy/model_readiness_test.go
git commit -m "feat(workbuddy): gate execution on dynamic model readiness"
```

---

### Task 11: Safe Panel `model_status` Backend

**Files:**
- Modify: `workbuddy/panel.go:20-206`
- Create: `workbuddy/model_status_test.go`

**Interfaces:**
- Consumes: `snapshotForAuthID`, `metadataStatus`, `pluginapi.HostAuthFileEntry`.
- Produces: `modelStatus`, `modelAuthStatus`, `buildModelStatus(files []pluginapi.HostAuthFileEntry) modelStatus`; `/accounts` and `/refresh` response field `model_status`.

- [ ] **Step 1: Write status projection and aggregation RED tests**

Install synthetic runtime snapshots and use safe auth indices. Start with:

```go
func TestBuildModelStatusUsesFixedPriorityAndSafeAuthIndex(t *testing.T) {
	installModelStatesForTest(t, map[string]modelReadinessState{
		"internal-ready": modelReady,
		"internal-stale": modelStale,
		"internal-failed": modelFailed,
	})
	got := buildModelStatus([]pluginapi.HostAuthFileEntry{
		{ID: "internal-ready", AuthIndex: "account-1"},
		{ID: "internal-stale", AuthIndex: "account-2"},
		{ID: "internal-failed", AuthIndex: "account-3"},
	})
	if got.State != modelFailed || len(got.Auths) != 3 {
		t.Fatalf("status = %#v", got)
	}
	if got.Auths[0].AuthIndex != "account-1" || got.Auths[1].State != modelStale || got.Auths[2].State != modelFailed {
		t.Fatalf("auth statuses = %#v", got.Auths)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("internal-")) || bytes.Contains(raw, []byte("token")) {
		t.Fatalf("unsafe status JSON = %s", raw)
	}
}
```

Assert the exact JSON shape, non-nil empty auth list, UTC RFC3339 times and no `auth_id`, token, endpoint, path, body, query or raw error.

Test aggregation priority:

```go
var modelStatePriority = map[modelReadinessState]int{
	modelReady: 1,
	modelStale: 2,
	modelNotStarted: 3,
	modelLoading: 4,
	modelFailed: 5,
}
```

Zero auth must be `not_started`; mixed auth must use `failed > loading > not_started > stale > ready`. Add tests that two auth rows retain independent state and map by host file ID while exposing only `AuthIndex`.

Add dashboard tests proving normal and early `hostAuthList` error responses both contain `model_status`. The model status message must be fixed and must not include the dashboard error.

- [ ] **Step 2: Run status tests and confirm RED**

Run:

```powershell
go -C workbuddy test . -run 'Test(BuildModelStatus|DashboardIncludesModelStatus)' -count=1
```

Expected: FAIL because status structs/function are undefined.

- [ ] **Step 3: Implement exact status DTOs and fixed messages**

```go
type modelStatus struct {
	State             modelReadinessState `json:"state"`
	Message           string              `json:"message"`
	MetadataSource    modelSnapshotSource `json:"metadata_source"`
	MetadataFetchedAt string              `json:"metadata_fetched_at"`
	Auths             []modelAuthStatus   `json:"auths"`
}

type modelAuthStatus struct {
	AuthIndex       string              `json:"auth_index"`
	State           modelReadinessState `json:"state"`
	ModelSource     modelSnapshotSource `json:"model_source"`
	ModelsFetchedAt string              `json:"models_fetched_at"`
	ErrorCode       modelErrorCode      `json:"error_code"`
}
```

Fixed messages:

```go
var modelStatusMessages = map[modelReadinessState]string{
	modelReady:      "模型目录已就绪",
	modelStale:      "模型目录刷新失败，正在使用上次有效缓存",
	modelFailed:     "模型目录不可用",
	modelLoading:    "模型目录正在初始化",
	modelNotStarted: "模型目录尚未初始化",
}
```

`buildModelStatus` allocates `Auths` with `make`, reads snapshots only, formats non-zero times with `UTC().Format(time.RFC3339)`, and never loads disk/network.

- [ ] **Step 4: Add status to both dashboard paths**

On `hostAuthList` failure return:

```go
return map[string]any{
	"error":        err.Error(),
	"model_status": buildModelStatus(nil),
}
```

After the first successful list, track `statusFiles := files`. If force lifecycle obtains `files2`, set `statusFiles = files2` so deleted auths are absent. Add to normal response:

```go
"model_status": buildModelStatus(statusFiles),
```

Do not trigger bootstrap or replace existing account grid/top-level dashboard semantics.

- [ ] **Step 5: Run GREEN and package regression**

Run:

```powershell
go -C workbuddy test . -run 'Test(BuildModelStatus|DashboardIncludesModelStatus)' -count=1
go -C workbuddy test ./... -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit backend status**

```powershell
git add workbuddy/panel.go workbuddy/model_status_test.go
git commit -m "feat(workbuddy): expose model readiness in panel data"
```

---

### Task 12: Executable Node Fixture and Persistent Model-status Banner

**Files:**
- Create: `workbuddy/panel.test.js`
- Modify: `workbuddy/panel.html:46-128,175-205,656-718`
- Modify: `workbuddy/Makefile:1-14`

**Interfaces:**
- Consumes: `model_status` JSON from Task 11.
- Produces: Node `loadPanel()` test fixture, `updateModelStatus(status)`, accessible persistent banner, explicit frontend test command.

- [ ] **Step 1: Create a minimal executable panel fixture**

Use only Node stdlib. The helper must read the final application `<script>`, remove the terminal `loadInitial();` call for deterministic unit tests, and execute in `vm` with a minimal fake DOM:

```js
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

function fakeElement() {
  const queries = new Map();
  const element = {
    hidden: false,
    className: "",
    textContent: "",
    innerHTML: "",
    value: "",
    files: [],
    style: {},
    dataset: {},
    children: [],
    parentNode: null,
    classList: {
      values: new Set(),
      add(value) { this.values.add(value); },
      remove(value) { this.values.delete(value); },
      contains(value) { return this.values.has(value); },
    },
    appendChild(child) {
      child.parentNode = this;
      this.children.push(child);
      return child;
    },
    querySelector(selector) {
      if (!queries.has(selector)) queries.set(selector, fakeElement());
      return queries.get(selector);
    },
    querySelectorAll() { return []; },
    addEventListener() {},
    focus() {},
    remove() {
      if (!this.parentNode) return;
      const index = this.parentNode.children.indexOf(this);
      if (index >= 0) this.parentNode.children.splice(index, 1);
      this.parentNode = null;
    },
  };
  Object.defineProperty(element, "firstChild", {
    get() { return element.children[0] || null; },
  });
  return element;
}

function loadPanel(overrides = {}) {
  const html = fs.readFileSync(path.join(__dirname, "panel.html"), "utf8");
  const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)];
  const source = scripts.at(-1)[1].replace(/\nloadInitial\(\);\s*$/, "");
  const elements = new Map();
  const storage = new Map();
  const document = {
    documentElement: fakeElement(),
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, fakeElement());
      return elements.get(id);
    },
    createElement() { return fakeElement(); },
    querySelectorAll() { return []; },
    addEventListener() {},
  };
  const sessionStorage = {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, String(value)); },
    removeItem(key) { storage.delete(key); },
  };
  const context = {
    console,
    document,
    sessionStorage,
    localStorage: overrides.localStorage || { getItem() { return null; }, setItem() { throw new Error("localStorage write"); } },
    location: overrides.location || { href: "http://localhost/panel", search: "", pathname: "/panel", hash: "", host: "localhost" },
    history: overrides.history || { replaceState() {} },
    navigator: { userAgent: "node-test" },
    URL,
    URLSearchParams,
    TextEncoder,
    TextDecoder,
    Uint8Array,
    btoa(value) { return Buffer.from(value, "binary").toString("base64"); },
    atob(value) { return Buffer.from(value, "base64").toString("binary"); },
    requestAnimationFrame(fn) { fn(); },
    setTimeout(fn) { fn(); return 1; },
    clearTimeout() {},
    fetch: overrides.fetch || (async () => { throw new Error("unexpected fetch"); }),
  };
  context.window = context;
  context.self = context;
  context.top = context;
  vm.createContext(context);
  vm.runInContext(source, context, { filename: "panel.html" });
  return { context, document, elements, storage };
}
```

The fixture above is sufficient for banner, parser, sorting, summary, toast and import tests in Tasks 12-18; do not introduce jsdom or npm files.

- [ ] **Step 2: Write model banner RED tests**

Test ready hides/clears the banner; stale and failed show fixed backend message; loading/not_started show persistent status; a malicious message renders through `textContent`, never `innerHTML`; and `load()` updates the banner before handling a top-level dashboard error. Use these executable tests as the starting point:

```js
test("model status banner hides ready and persists failed", () => {
  const { context, elements } = loadPanel();
  context.updateModelStatus({ state: "failed", message: "模型目录不可用" });
  const banner = elements.get("modelStatus");
  assert.equal(banner.hidden, false);
  assert.equal(banner.className, "model-status failed");
  assert.equal(banner.textContent, "模型目录不可用");
  context.updateModelStatus({ state: "ready", message: "ignored" });
  assert.equal(banner.hidden, true);
  assert.equal(banner.textContent, "");
});

test("load updates model status before dashboard error", async () => {
  const { context, elements, storage } = loadPanel();
  storage.set("workbuddy-mgmt-key", "test-key");
  let toastCalls = 0;
  context.toast = () => { toastCalls += 1; };
  context.api = async () => ({
    model_status: { state: "failed", message: "模型目录不可用" },
    error: "dashboard unavailable",
  });
  await context.load(false);
  assert.equal(elements.get("modelStatus").textContent, "模型目录不可用");
  assert.equal(toastCalls, 0);
});
```

Add a third malicious-message case that passes `<img src=x onerror=1>` and asserts it appears only in `textContent` while `innerHTML` remains empty.

- [ ] **Step 3: Run Node test and confirm RED**

Run:

```powershell
node --test workbuddy/panel.test.js
```

Expected: FAIL because `modelStatus` element and `updateModelStatus` do not exist.

- [ ] **Step 4: Add accessible banner and update function**

Add after the subtitle:

```html
<div id="modelStatus" class="model-status" role="status" aria-live="polite" aria-atomic="true" hidden></div>
```

Add CSS using existing tokens for neutral/warn/error states. Implement:

```js
function updateModelStatus(status){
  const el=document.getElementById("modelStatus");
  const state=status&&status.state?status.state:"not_started";
  if(state==="ready"){
    el.hidden=true;
    el.className="model-status";
    el.textContent="";
    return;
  }
  el.hidden=false;
  el.className="model-status "+state;
  el.textContent=(status&&status.message)||"模型目录尚未初始化";
}
```

In `load()`, call `updateModelStatus(d.model_status)` immediately after the API result and before `if(d.error)`. Do not toast or replace account cards for model state.

- [ ] **Step 5: Wire the explicit Node test target**

Add:

```make
NODE ?= node
```

and after Go race test in `test`:

```make
	$(NODE) --test panel.test.js
```

The filename must be explicit; a missing test file must not pass as zero tests.

- [ ] **Step 6: Run GREEN and regression**

Run:

```powershell
node --test workbuddy/panel.test.js
go -C workbuddy test ./... -count=1
```

Expected: Node reports at least one passing test; Go tests pass.

- [ ] **Step 7: Commit fixture and banner**

```powershell
git add workbuddy/panel.test.js workbuddy/panel.html workbuddy/Makefile
git commit -m "feat(workbuddy): show persistent model readiness status"
```

---

### Task 13: Management Limiter Counts Only Failed Authentication

**Files:**
- Modify: `workbuddy/management_auth_test.go`
- Modify: `workbuddy/management.go:188-219`

**Interfaces:**
- Consumes: existing `checkManagementAuth`, `allowManagementRequest`, capacity/refill.
- Produces: correct-key requests bypass failure bucket; wrong-key requests consume it.

- [ ] **Step 1: Write exact limiter RED tests**

Reset `mgmtRateLimit` under its mutex and configure key `secret`. Use this table shape against an unknown POST route so a correctly authenticated request reaches the normal 404 path:

```go
func TestManagementRateLimitDoesNotChargeValidKey(t *testing.T) {
	managementAPIKeyMu.Lock()
	oldKey := managementAPIKey
	managementAPIKey = "secret"
	managementAPIKeyMu.Unlock()
	mgmtRateLimitMu.Lock()
	oldBuckets := mgmtRateLimit
	mgmtRateLimit = map[string]*mgmtRateEntry{}
	mgmtRateLimitMu.Unlock()
	t.Cleanup(func() {
		managementAPIKeyMu.Lock()
		managementAPIKey = oldKey
		managementAPIKeyMu.Unlock()
		mgmtRateLimitMu.Lock()
		mgmtRateLimit = oldBuckets
		mgmtRateLimitMu.Unlock()
	})
	base := loadedManagementBasePath() + "/plugins/" + providerName + "/not-found"
	for i := 0; i < 6; i++ {
		resp := managementResponseForTest(t, pluginapi.ManagementRequest{
			Method: http.MethodPost,
			Path:   base,
			Headers: http.Header{
				"Authorization": []string{"Bearer secret"},
				"X-Real-Ip":     []string{"192.0.2.10"},
			},
		})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("request %d status=%d, want 404", i+1, resp.StatusCode)
		}
	}
}
```

Add a sibling `TestManagementRateLimitChargesFailedKey` with the same setup and `Bearer wrong`; requests 1-5 must be 403 and request 6 must be 429. Do not use clock sleeps or change capacity/refill constants.

- [ ] **Step 2: Run limiter tests and confirm RED**

Run:

```powershell
go -C workbuddy test . -run 'TestManagementRateLimit(DoesNotChargeValidKey|ChargesFailedKey)' -count=1
```

Expected: valid-key sixth request is currently 429.

- [ ] **Step 3: Move limiter after failed authentication only**

Use this ordering after the public resource early return:

```go
mutating := req.Method == http.MethodPost || mutatingManagementPath(path)
if loadedManagementKey() != "" || mutating {
	if status, msg := checkManagementAuth(req.ManagementRequest); status != 0 {
		ip := managementClientIP(req.ManagementRequest)
		if !allowManagementRequest(ip) {
			return okEnvelope(mgmtJSONResponse(http.StatusTooManyRequests, map[string]any{
				"error": "rate limit exceeded, try again later",
			}))
		}
		return okEnvelope(mgmtJSONResponse(status, map[string]any{"error": msg}))
	}
}
```

Correct key and disabled plugin key consume no token. Wrong/missing key retains the existing bucket behavior. Do not add panel POST retry.

- [ ] **Step 4: Run GREEN and regression**

Run:

```powershell
go -C workbuddy test . -run 'TestManagement' -count=1
go -C workbuddy test ./... -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit limiter root fix**

```powershell
git add workbuddy/management.go workbuddy/management_auth_test.go
git commit -m "fix(workbuddy): rate limit failed management auth only"
```

---

### Task 14: Safe Panel Response Trust Boundary

**Files:**
- Modify: `workbuddy/panel.test.js`
- Modify: `workbuddy/panel.html:603-655,656-718`

**Interfaces:**
- Consumes: Node fixture from Task 12.
- Produces: `readPanelResponse(response)`, recursive error-field redaction, fixed network/auth/parser errors.

- [ ] **Step 1: Write executable safe-response RED tests**

Queue fake responses for empty, HTML, malformed JSON, non-2xx and valid JSON with raw error fields. Add this helper and test:

```js
function fakeResponse(status, contentType, body) {
  return {
    status,
    ok: status >= 200 && status < 300,
    headers: { get(name) { return name.toLowerCase() === "content-type" ? contentType : null; } },
    async text() { return body; },
  };
}

test("panel response parser never exposes response bodies", async () => {
  const { context } = loadPanel();
  const cases = [
    [fakeResponse(200, "application/json", ""), "响应为空"],
    [fakeResponse(200, "text/html; charset=utf-8", "<html>secret-token</html>"), "响应格式无效"],
    [fakeResponse(200, "application/json", "{secret-token"), "响应 JSON 无效"],
    [fakeResponse(503, "application/json", `{"error":"secret-token"}`), "请求失败"],
  ];
  for (const [response, category] of cases) {
    await assert.rejects(
      context.readPanelResponse(response),
      error => error.message.includes(category) &&
        error.message.includes("HTTP ") &&
        !error.message.includes("secret-token"),
    );
  }
  const sanitized = await context.readPanelResponse(
    fakeResponse(200, "application/json", `{"error":"secret-token","nested":{"error":"credential-value"}}`),
  );
  assert.equal(sanitized.error, "请求失败");
  assert.equal(sanitized.nested.error, "请求失败");
});

test("panel transport errors use a fixed message", async () => {
  const { context, storage } = loadPanel({
    fetch: async () => { throw new Error("https://host.invalid/?key=secret-token"); },
  });
  storage.set("workbuddy-mgmt-key", "test-key");
  await assert.rejects(context.api("/accounts"), error => error.message === "网络请求失败");
});
```

Also assert no grid, toast or returned object contains body text, credential, token, original `d.error` or native fetch `e.message`.

- [ ] **Step 2: Run trust-boundary tests and confirm RED**

Run:

```powershell
node --test --test-name-pattern='panel response' workbuddy/panel.test.js
```

Expected: FAIL because current code calls `r.json()`, displays response bodies and forwards raw errors.

- [ ] **Step 3: Implement one safe response parser**

```js
function panelContentType(response){
  const raw=response.headers&&response.headers.get?response.headers.get("content-type")||"":"";
  return (raw.split(";",1)[0].trim().toLowerCase()||"unknown");
}
function panelHTTPError(kind,response){
  return new Error(kind+"（HTTP "+response.status+"，"+panelContentType(response)+"）");
}
function sanitizePanelErrors(value){
  if(Array.isArray(value)) return value.map(sanitizePanelErrors);
  if(!value||typeof value!=="object") return value;
  for(const key of Object.keys(value)){
    if(key==="error"&&value[key]) value[key]="请求失败";
    else value[key]=sanitizePanelErrors(value[key]);
  }
  return value;
}
async function readPanelResponse(response){
  let text="";
  try{text=await response.text()}catch(_){throw panelHTTPError("响应读取失败",response)}
  if(!text) throw panelHTTPError("响应为空",response);
  const contentType=panelContentType(response);
  if(contentType!=="application/json"&&!contentType.endsWith("+json")){
    throw panelHTTPError("响应格式无效",response);
  }
  let data;
  try{data=JSON.parse(text)}catch(_){throw panelHTTPError("响应 JSON 无效",response)}
  if(!response.ok) throw panelHTTPError("请求失败",response);
  return sanitizePanelErrors(data);
}
```

Wrap `fetch` in both `api` and `managementAPI`; any rejection becomes `new Error("网络请求失败")`. For 401/403, clear the session key and use fixed messages and a fixed local cooldown; do not read or regex-match the body. All successful responses pass through `readPanelResponse`.

`load()` must retain its Task 12 `updateModelStatus` ordering, but show a fixed dashboard failure rather than the original `d.error`. Existing callers may keep reading `.error` because `sanitizePanelErrors` has replaced every error field with `请求失败`.

- [ ] **Step 4: Run GREEN and all panel tests**

Run:

```powershell
node --test workbuddy/panel.test.js
go -C workbuddy test ./... -count=1
```

Expected: PASS, with non-zero Node test count.

- [ ] **Step 5: Commit the response boundary**

```powershell
git add workbuddy/panel.html workbuddy/panel.test.js
git commit -m "fix(workbuddy): classify panel responses without body leaks"
```

---

### Task 15: Credits Sorting Keeps Real Zero Above Unknown

**Files:**
- Modify: `workbuddy/panel.test.js`
- Modify: `workbuddy/panel.html:519-547`

**Interfaces:**
- Consumes: `creditOf`, `accountsForView`, sort cycle.
- Produces: original -> desc -> asc -> original without mutating `lastAccounts`; known/unknown ordering.

- [ ] **Step 1: Write sorting RED tests**

Use accounts in original order with positive credits, a real zero credits object, and missing credits:

```js
test("credits sort keeps real zero above unknown without mutation", () => {
  const { context } = loadPanel();
  const accounts = [
    { auth_index: "unknown" },
    { auth_index: "zero", credits: { total_remain: 0, total_used: 0, total_size: 0 } },
    { auth_index: "positive", credits: { total_remain: 10, total_used: 0, total_size: 10 } },
  ];
  const original = structuredClone(accounts);
  const ids = list => Array.from(list, account => account.auth_index);
  assert.deepEqual(ids(context.accountsForView(accounts)), ["unknown", "zero", "positive"]);
  const button = { textContent: "" };
  context.cycleRemainSort(button);
  assert.deepEqual(ids(context.accountsForView(accounts)), ["positive", "zero", "unknown"]);
  context.cycleRemainSort(button);
  assert.deepEqual(ids(context.accountsForView(accounts)), ["zero", "positive", "unknown"]);
  context.cycleRemainSort(button);
  assert.deepEqual(ids(context.accountsForView(accounts)), ["unknown", "zero", "positive"]);
  assert.deepEqual(accounts, original);
});
```

This proves original -> desc -> asc -> original, descending positive > real zero > unknown, ascending real zero < positive < unknown, and no mutation of `lastAccounts` inputs.

- [ ] **Step 2: Run sorting tests and confirm RED**

Run:

```powershell
node --test --test-name-pattern='credits sort' workbuddy/panel.test.js
```

Expected: FAIL because current `creditOf` treats all-zero as unknown and comparator treats unknown as zero.

- [ ] **Step 3: Implement explicit known ordering**

A non-null credits object is a real reading, including zero:

```js
function creditOf(a){
  const cr=a&&a.credits;
  if(!cr) return {remain:0,used:0,size:0,known:false};
  return {
    remain:Number(cr.total_remain)||0,
    used:Number(cr.total_used)||0,
    size:Number(cr.total_size)||0,
    known:true
  };
}
```

In `accountsForView`, always copy first. Unknown is last in both directions; known values compare numerically with stable ties:

```js
return view.slice().sort((a,b)=>{
  const left=creditOf(a),right=creditOf(b);
  if(left.known!==right.known) return left.known?-1:1;
  if(!left.known) return 0;
  const diff=left.remain-right.remain;
  return sortDirection==="desc"?-diff:diff;
});
```

- [ ] **Step 4: Run GREEN and panel regression**

Run:

```powershell
node --test workbuddy/panel.test.js
go -C workbuddy test ./... -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit sorting**

```powershell
git add workbuddy/panel.html workbuddy/panel.test.js
git commit -m "fix(workbuddy): sort real zero credits above unknown"
```

---

### Task 16: Panel Available Balance Excludes Disabled and Exhausted Accounts

**Files:**
- Modify: `workbuddy/panel.test.js`
- Modify: `workbuddy/panel.html:548-602`
- Test only: `workbuddy/summary_test.go` remains unchanged

**Interfaces:**
- Consumes: `creditOf`, `renderSummary`.
- Produces: frontend-only spendable remain totals; backend `summarizeCredits.total_remain` unchanged.

- [ ] **Step 1: Write available-balance RED tests**

Render a summary with one active account at 100, one disabled account at 50 and one exhausted account at 25:

```js
test("available balance excludes disabled and exhausted positive credits", () => {
  const { context, elements } = loadPanel();
  context.renderSummary([
    { region: "cn", credits: { total_remain: 100, total_used: 1, total_size: 101 } },
    { region: "cn", disabled: true, credits: { total_remain: 50, total_used: 2, total_size: 52 } },
    { region: "cn", exhausted: true, credits: { total_remain: 25, total_used: 3, total_size: 28 } },
  ]);
  const html = elements.get("summaryBox").innerHTML;
  assert.match(html, /剩余\(可用\)<\/div><div class="v ok">100<\/div>/);
  assert.match(html, /已用\(消耗\)<\/div><div class="v [^"]*">6<\/div>/);
  assert.match(html, /CN 可用 100 \/ 已用 6/);
  assert.doesNotMatch(html, /剩余\(可用\)<\/div><div class="v ok">175<\/div>/);
});
```

Also run existing `TestSummarizeCredits` and assert backend total semantics remain unchanged.

- [ ] **Step 2: Run tests and confirm RED**

Run:

```powershell
node --test --test-name-pattern='available balance' workbuddy/panel.test.js
go -C workbuddy test . -run TestSummarizeCredits -count=1
```

Expected: Node test FAIL with 175; Go backend test PASS.

- [ ] **Step 3: Change only frontend remain accumulation**

Inside both scoped and all-account loops, compute:

```js
const spendable=!a.disabled&&!a.exhausted;
if(c.known){
  if(spendable) remain+=c.remain;
  used+=c.used;
  size+=c.size;
  knownN++;
}
```

For regional remain, add only when spendable; continue adding regional used for known accounts. Do not modify `panel.go:summarizeCredits` or its compatibility field.

- [ ] **Step 4: Run GREEN and regressions**

Run:

```powershell
node --test workbuddy/panel.test.js
go -C workbuddy test . -run TestSummarizeCredits -count=1
go -C workbuddy test ./... -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit available-balance fix**

```powershell
git add workbuddy/panel.html workbuddy/panel.test.js
git commit -m "fix(workbuddy): exclude unusable credits from panel balance"
```

---

### Task 17: Partial Import Keeps Modal Open and Clears Credentials

**Files:**
- Modify: `workbuddy/panel.test.js`
- Modify: `workbuddy/panel.html:956-1003`

**Interfaces:**
- Consumes: existing bounded sequential import and safe `api`.
- Produces: `safeImportName`, mixed-result lifecycle, immediate credential clearing.

- [ ] **Step 1: Write partial-import RED tests**

Exercise `importAuth` with two synthetic files where one succeeds and one fails:

```js
test("partial import clears credentials and keeps modal open", async () => {
  const { context, elements } = loadPanel();
  const modal = elements.get("importModal") || context.document.getElementById("importModal");
  modal.classList.add("show");
  const raw = context.document.getElementById("importRaw");
  raw.value = "raw-secret-credential";
  const files = context.document.getElementById("importFiles");
  files.value = "selected";
  files.files = [
    { name: "ok.json", size: 10, async text() { return "credential-one"; } },
    { name: "bad.json", size: 10, async text() { return "credential-two"; } },
  ];
  context.importOneCredential = async value => ({ success: value !== "credential-two" });
  let toastDetail = "";
  context.toast = (title, kind, detail) => { toastDetail = title + " " + kind + " " + detail; };
  let reloads = 0;
  context.load = async () => { reloads += 1; return true; };
  await context.importAuth({ dataset: {}, innerHTML: "导入", disabled: false });
  assert.equal(modal.classList.contains("show"), true);
  assert.equal(raw.value, "");
  assert.equal(files.value, "");
  assert.equal(reloads, 1);
  assert.match(toastDetail, /ok\.json：成功/);
  assert.match(toastDetail, /bad\.json：导入失败/);
  assert.doesNotMatch(toastDetail, /raw-secret|credential-one|credential-two/);
});
```

Add an all-success case asserting modal closes, and an all-failure case asserting it stays open. Use a filename containing `\u0000`, `\u001f`, `\u007f` and more than 120 characters; assert displayed name is stripped and truncated.

- [ ] **Step 2: Run import tests and confirm RED**

Run:

```powershell
node --test --test-name-pattern='partial import' workbuddy/panel.test.js
```

Expected: mixed case FAIL because current code closes whenever `ok > 0`.

- [ ] **Step 3: Implement safe result names and lifecycle**

```js
function safeImportName(name){
  return String(name||"文件").replace(/[\u0000-\u001f\u007f]/g,"").trim().slice(0,120)||"文件";
}
```

Store only the safe display name in `imports/results`. Immediately after the sequential batch and before toast/reload:

```js
document.getElementById("importRaw").value="";
files.value="";
```

Close only when `fail===0`. If `ok>0`, reload dashboard whether or not failures exist; mixed failures keep modal open. All error labels remain the fixed local categories already used: `超过 2 MiB`, `读取失败`, `导入失败`, `请求失败`.

- [ ] **Step 4: Run GREEN and panel regression**

Run:

```powershell
node --test workbuddy/panel.test.js
go -C workbuddy test ./... -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit import lifecycle**

```powershell
git add workbuddy/panel.html workbuddy/panel.test.js
git commit -m "fix(workbuddy): preserve partial import results safely"
```

---

### Task 18: Inject Actual Management BasePath and Consume Query Key Once

**Files:**
- Modify: `workbuddy/panel.go:306-314`
- Modify: `workbuddy/panel.html:246-294,603-609`
- Modify: `workbuddy/management_basepath_test.go`
- Modify: `workbuddy/panel_features_test.go:47-65`
- Modify: `workbuddy/panel.test.js`

**Interfaces:**
- Consumes: `loadedManagementBasePath`, public panel resource handler, session storage.
- Produces: JSON-safe BasePath injection and one-shot `?key=` capture/removal.

- [ ] **Step 1: Write BasePath and URL-key RED tests**

Extend `TestManagementUsesRegisteredResourceBasePath` after decoding the served response:

```go
html := string(resp.Body)
if !strings.Contains(html, `const MANAGEMENT_BASE_PATH="/custom/manage";`) {
	t.Fatalf("panel does not contain registered BasePath: %s", html)
}
if strings.Contains(html, `fetch("/v0/management/plugins/workbuddy`) {
	t.Fatal("panel still hardcodes the historical management BasePath")
}
```

In `panel.test.js:loadPanel`, initialize storage with `const storage = new Map(overrides.sessionEntries || []);`, then add:

```js
test("query key replaces session key once and is removed from URL", () => {
  let replaced = "";
  let localWrites = 0;
  const location = {
    href: "http://localhost/panel?key=secret-value&view=all#accounts",
    search: "?key=secret-value&view=all",
    pathname: "/panel",
    hash: "#accounts",
    host: "localhost",
  };
  const { storage } = loadPanel({
    sessionEntries: [["workbuddy-mgmt-key", "old-value"]],
    location,
    history: { replaceState(state, title, url) { replaced = url; } },
    localStorage: {
      getItem() { return null; },
      setItem() { localWrites += 1; },
    },
  });
  assert.equal(storage.get("workbuddy-mgmt-key"), "secret-value");
  assert.equal(replaced, "/panel?view=all#accounts");
  assert.equal(localWrites, 0);
});
```

This proves the explicit query key is consumed before a pre-existing session key, retains unrelated query/hash state, and never writes `localStorage`. In `TestPanelEditsDesensitizeThroughGenericPluginConfigAPI`, replace the old `fetch("/v0/management"+path` expected fragment with both `const MANAGEMENT_BASE_PATH=__WB_MANAGEMENT_BASE_PATH_JSON__` and `fetch(MANAGEMENT_BASE_PATH+path`.

- [ ] **Step 2: Run tests and confirm RED**

Run:

```powershell
go -C workbuddy test . -run TestManagementUsesRegisteredResourceBasePath -count=1
node --test --test-name-pattern='BasePath|query key' workbuddy/panel.test.js
```

Expected: FAIL because panel API constants remain hardcoded and query capture happens after an existing session key lookup.

- [ ] **Step 3: Inject BasePath as a JSON literal**

In HTML use an unquoted build token:

```js
const MANAGEMENT_BASE_PATH=__WB_MANAGEMENT_BASE_PATH_JSON__;
const API=MANAGEMENT_BASE_PATH+"/plugins/workbuddy";
```

Update `managementAPI` to fetch `MANAGEMENT_BASE_PATH+path`.

In `servePanel`, marshal the loaded path and replace only the token:

```go
func servePanel(sub string) []byte {
	if sub != "" && sub != "/" && sub != "/panel" && sub != "/panel.html" {
		return []byte("<h1>404</h1>")
	}
	base, _ := json.Marshal(loadedManagementBasePath())
	return bytes.ReplaceAll(panelHTML, []byte("__WB_MANAGEMENT_BASE_PATH_JSON__"), base)
}
```

Add `bytes` import. This injects no key and safely escapes any path string. In `panel.test.js:loadPanel`, replace the raw token before `vm.runInContext` so direct fixture execution remains valid:

```js
const source = scripts.at(-1)[1]
  .replace("__WB_MANAGEMENT_BASE_PATH_JSON__", JSON.stringify("/v0/management"))
  .replace(/\nloadInitial\(\);\s*$/, "");
```

- [ ] **Step 4: Capture and remove query key before all other lookup**

```js
function captureUrlKey(){
  const url=new URL(window.location.href);
  const key=url.searchParams.get("key");
  if(!key) return;
  sessionStorage.setItem(SS_KEY,key);
  url.searchParams.delete("key");
  history.replaceState(null,"",url.pathname+url.search+url.hash);
}
captureUrlKey();
function getKey(){
  return sessionStorage.getItem(SS_KEY)||readPanelKey();
}
```

The Node fixture must provide `window.location.href`. Remove `readUrlKey`. Do not write management keys to `localStorage`, HTML text, console or logs. Reading CPA main-panel storage through existing `readPanelKey` remains allowed.

- [ ] **Step 5: Run GREEN and regressions**

Run:

```powershell
go -C workbuddy test . -run TestManagementUsesRegisteredResourceBasePath -count=1
node --test workbuddy/panel.test.js
go -C workbuddy test ./... -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit path/key handling**

```powershell
git add workbuddy/panel.go workbuddy/panel.html workbuddy/management_basepath_test.go workbuddy/panel_features_test.go workbuddy/panel.test.js
git commit -m "fix(workbuddy): honor panel BasePath and consume query key"
```

---

### Task 19: Production Deletion Gate, Documentation, and Final Verification

**Files:**
- Create: `workbuddy/production_model_contract_test.go`
- Modify: `workbuddy/main.go:381-392,692-697`
- Verify without further edits: `workbuddy/models_test.go`, `workbuddy/sanitize_test.go`, `workbuddy/toolchoice_test.go` already use synthetic fixtures after Task 2
- Modify: `workbuddy/README.md`
- Modify: `workbuddy/README_CN.md`
- Modify: `workbuddy/docs/architecture.md`
- Modify: `workbuddy/CHANGELOG.md`
- Verify: `workbuddy/Makefile`

**Interfaces:**
- Consumes: all prior tasks and approved spec.
- Produces: executable deletion regression, user-facing cache/startup docs and final validated change set.

- [ ] **Step 1: Write the production-source deletion RED test**

Read every `workbuddy/*.go` file except `_test.go`. Scan raw bytes so comments and string literals count. Implement the test with this exact structure; the banned IDs live only in the test file, which is excluded from its own production scan:

```go
func TestProductionModelSourceHasNoFixedIDs(t *testing.T) {
	banned := []string{
		"glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5v-turbo",
		"kimi-k2.6", "kimi-k3-1", "kimi-k2.7", "minimax-m3",
		"hy3", "hy3-x", "hy3-preview", "hy3-preview-agent",
		"hy4-preview", "hy4-preview-x", "deepseek-v4-pro", "deepseek-v4-flash",
		"forceMaxThinking",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range banned {
			if bytes.Contains(raw, []byte(value)) {
				t.Errorf("production file %s contains banned model contract %q", name, value)
			}
		}
	}
}

func TestProductionModelInfoLiteralsOnlyNameAuto(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			selector, ok := literal.Type.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "ModelInfo" {
				return true
			}
			for _, element := range literal.Elts {
				pair, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := pair.Key.(*ast.Ident)
				value, literalID := pair.Value.(*ast.BasicLit)
				if !ok || key.Name != "ID" || !literalID || value.Kind != token.STRING {
					continue
				}
				id, err := strconv.Unquote(value.Value)
				if err != nil || id != "auto" {
					t.Errorf("production file %s has static ModelInfo ID %s", name, value.Value)
				}
			}
			return true
		})
	}
}

func TestProductionMetadataHasOnlyDefaultTemplate(t *testing.T) {
	got := modelInfoFromSources(modelFacts{ID: "serve-alpha"}, nil)
	want := defaultModelInfo("serve-alpha", "")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unmatched dynamic metadata = %#v, want %#v", got, want)
	}
}
```

Use imports `bytes`, `go/ast`, `go/parser`, `go/token`, `os`, `reflect`, `strconv`, `strings`, `testing`. The AST test permits dynamic `ID: variable` and the single static `auto` fallback; the raw scan rejects all removed static IDs and reasoning function names.

- [ ] **Step 2: Run the deletion test and confirm RED when stale references remain**

Run:

```powershell
go -C workbuddy test . -run 'TestProduction(ModelSourceHasNoFixedIDs|MetadataHasOnlyDefaultTemplate)' -count=1
```

Expected: FAIL on remaining production comments/examples or literals. If Task 2 already removed every occurrence, temporarily prove the test by inserting one banned ID into a local copy of a production comment, run RED, then revert that temporary line before GREEN; never commit the proof mutation.

- [ ] **Step 3: Remove only stale production references and syntheticize tests**

Change the two alias comments/examples in `workbuddy/main.go` to generic `client-alias` -> `upstream-model` wording, without placing any synthetic or real model ID in production source. Verify `models_test.go`, `sanitize_test.go` and `toolchoice_test.go` contain only the synthetic IDs established in Task 2. Do not remove alias resolution, tool-choice or payload behavior.

Run:

```powershell
go -C workbuddy test . -run 'TestProduction(ModelSourceHasNoFixedIDs|MetadataHasOnlyDefaultTemplate)' -count=1
```

Expected: PASS.

- [ ] **Step 4: Update README and architecture with exact operational contract**

Document in both README languages:

- `model.static` only returns `auto`;
- first `model.for_auth` performs authenticated bootstrap;
- WorkBuddy `/v3/config`, 404/405-only legacy fallback and models.dev `/models.json` roles;
- cache path from `os.UserConfigDir`, including Linux root example `/root/.config/CLIProxyAPI/workbuddy/model-catalog/`;
- first no-cache failure is fail-closed;
- subsequent refresh failure with valid last-good starts stale;
- `ready`/`stale` executable, other states blocked with 503;
- panel `model_status` and fixed error categories;
- no background refresh, panel retry endpoint or Enterprise custom model support.

Update `docs/architecture.md` with source -> validate -> persist -> publish flow, separated caches, identity hash fields, `.bak`, per-auth/global flights, generation gate and executor/scheduler read-only gate. Do not describe inherited host callback as having a guaranteed overall timeout.

- [ ] **Step 5: Add only an `Unreleased` CHANGELOG entry**

At the top of `CHANGELOG.md`, add an `Unreleased` section listing dynamic discovery, models.dev enrichment, last-good persistence/readiness/panel status and the six maintenance fixes. Do not edit historical release entries or version/tag files.

- [ ] **Step 6: Format only changed Go files and run the complete acceptance suite**

Get the task-owned changed Go list from `git diff --name-only 9c45074 -- 'workbuddy/*.go'`, then pass only those files to `gofmt -w`. Do not run `gofmt -w .`.

Run exactly:

```powershell
go -C workbuddy test ./... -count=1
go -C workbuddy test -race ./... -count=1
go -C workbuddy vet ./...
go -C workbuddy build ./...
node --test workbuddy/panel.test.js
node --test
git diff --check
git status --short
```

Expected:

- every Go command passes;
- both Node commands report a non-zero test count and pass;
- `git diff --check` is empty;
- `git status --short` contains only task files plus the same pre-existing `LOOP.md` and four 2026-08-28 untracked docs;
- no network test reaches a live endpoint;
- no new module/frontend dependency appears.

- [ ] **Step 7: Inspect the final diff against explicit exclusions**

Run:

```powershell
git diff 9c45074 --stat
git diff 9c45074 -- workbuddy/go.mod workbuddy/go.sum
git log --oneline 9c45074..HEAD
```

Expected: no dependency changes unless `go.sum` was already changed before this task; no Pool, weighted routing, state.json, logger, retry/replay, second scheduler, Enterprise endpoint, background refresh or cross-process lock implementation.

- [ ] **Step 8: Commit deletion test and documentation**

```powershell
git add workbuddy/production_model_contract_test.go workbuddy/main.go workbuddy/README.md workbuddy/README_CN.md workbuddy/docs/architecture.md workbuddy/CHANGELOG.md
git commit -m "docs(workbuddy): document dynamic model bootstrap"
```

No test fixture file should change in this task; Task 2 already converted `models_test.go`, `sanitize_test.go` and `toolchoice_test.go`. If verification finds a real-model fixture in another test file, stop Task 19 and report the exact path rather than broad-staging an unplanned file.

- [ ] **Step 9: Final commit-scope verification**

Run:

```powershell
git status --short
git show --stat --oneline HEAD
```

Expected: the documentation commit contains only the deletion regression, changed synthetic fixtures and four current docs. User-owned files remain unstaged and unchanged.
