# WorkBuddy Fork Fusion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 WorkBuddy 0.8.7 加入默认关闭、可编辑持久化的屏蔽词混淆，并融合经审计确认的 OAuth、host bridge、stream error、billing、enterprise credits 和 panel 改进。

**Architecture:** 新功能沿用现有单次 payload JSON rewrite、CPA plugin config API、host callback wire、billing cache 和单文件 panel。新增配置通过一个 immutable `featureRuntimeConfig` snapshot 发布；所有 request-scoped host HTTP 调用显式携带 `host_callback_id`，异步 stream 使用 CPA 原生 error chunk。远端行为均默认关闭或严格校验，现有 personal billing 和 CLI OAuth 保持默认路径。

**Tech Stack:** Go 1.26.5、CLIProxyAPI v7.2.30 plugin ABI、`gopkg.in/yaml.v3`、Go stdlib `regexp`/`sync/atomic`/`net/http`、原生 HTML/CSS/JavaScript、Zig Linux CGO cross-compiler。

**Spec:** `docs/superpowers/specs/2026-08-29-workbuddy-fork-fusion-design.md`

## Global Constraints

- 每个行为变更必须先写测试并运行真实 RED，确认测试因缺失行为失败，再写最小 GREEN。
- 只修改 `workbuddy` 及本计划和对应 spec；不得修改或提交 `LOOP.md`、现有 2026-08-28 spec/plan、`qoderwork`。
- 不增加 Go dependency；复用已安装的 `gopkg.in/yaml.v3` 和 stdlib。
- 不加入动态模型列表或动态 model metadata。
- 不加入 plugin credential retry、account failover、session routing、raw export、账号删除、trace、usage feed 或 broad chat impersonation。
- `desensitize`、`enterprise_credits` 默认关闭；`oauth_client_mode` 默认 `cli`。
- 不 bump `workbuddy/VERSION`、`registry.json` 或 `main.version`。
- 不 push、不发布，不覆盖 `workbuddy/workbuddy_0.8.7_linux_amd64.so`。
- 新 Linux 产物固定写入 `workbuddy/workbuddy_0.8.7_fork_fusion_linux_amd64.so`。
- 每个 task 只提交其列出的文件和由该 task 产生的测试；不把用户原有未提交文件带入 commit。

## File Structure

- Create `workbuddy/desensitize.go`：默认词表、配置 snapshot、matcher 编译和文本替换。
- Create `workbuddy/desensitize_test.go`：配置、词表和 matcher 测试。
- Create `workbuddy/desensitize_payload_test.go`：message/tool payload walker 测试。
- Create `workbuddy/host_bridge_test.go`：buffered response casing 和 callback wire 测试。
- Create `workbuddy/stream_error_test.go`：native stream error 和 typed status 测试。
- Create `workbuddy/oauth_profile_test.go`：CLI/desktop OAuth request profile 测试。
- Create `workbuddy/enterprise_test.go`：strict enterprise parser、fallback 和 lifecycle freshness 测试。
- Create `workbuddy/panel_features_test.go`：settings、多文件导入、搜索和排序的静态行为 contract。
- Modify `workbuddy/usage_config.go`：解析并发布新配置 snapshot。
- Modify `workbuddy/main.go`：注册字段、RPC wrapper、poll identity、typed envelope 和 executor callback。
- Modify `workbuddy/payload.go`：在现有单次 decode/encode 中调用 walker。
- Modify `workbuddy/management.go`、`workbuddy/panel.go`、`workbuddy/panel.html`：runtime settings route 和 panel UI。
- Modify `workbuddy/host_bridge.go`：双 status casing、callback-aware buffered/stream helpers。
- Modify `workbuddy/executor_http.go`、`workbuddy/oauth.go`、`workbuddy/keepalive.go`、`workbuddy/stream.go`：callback propagation、OAuth profile 和 native stream errors。
- Modify `workbuddy/billing.go`、`workbuddy/cache.go`、`workbuddy/lifecycle.go`、`workbuddy/policy.go`、`workbuddy/credits_handler.go`、`workbuddy/checkin.go`、`workbuddy/trial.go`：billing semaphore、enterprise probe、freshness guard 和 management callback variants。

---

### Task 1: Immutable feature config and desensitize matcher

**Files:**
- Create: `workbuddy/desensitize.go`
- Create: `workbuddy/desensitize_test.go`
- Modify: `workbuddy/usage_config.go`
- Modify: `workbuddy/main.go`
- Modify: `workbuddy/registration_test.go`

**Interfaces:**
- Produces: `type featureRuntimeConfig`, `currentFeatureRuntime() *featureRuntimeConfig`, `parseFeatureRuntime([]byte) (*featureRuntimeConfig, error)`, `(*desensitizeMatcher).replace(string) string`。
- Produces config fields: `desensitize`, `desensitize_terms`, `oauth_client_mode`, `enterprise_credits`。
- Later tasks consume the immutable matcher, effective terms/source, OAuth mode and enterprise switch。

- [ ] **Step 1: Write failing config and matcher tests**

Create `workbuddy/desensitize_test.go` with tests that assert the public task contract:

```go
package main

import (
    "strings"
    "testing"
)

func TestDefaultDesensitizeTermsAreComplete(t *testing.T) {
    cfg := currentFeatureRuntime()
    if cfg == nil {
        t.Fatal("feature runtime is nil")
    }
    if got := len(cfg.desensitizeTerms); got != 85 {
        t.Fatalf("default terms = %d, want 85", got)
    }
    if cfg.desensitizeEnabled {
        t.Fatal("desensitize must default off")
    }
    if cfg.desensitizeSource != "default" {
        t.Fatalf("source = %q, want default", cfg.desensitizeSource)
    }
    if cfg.oauthClientMode != oauthClientModeCLI || cfg.enterpriseCredits {
        t.Fatalf("unsafe defaults: mode=%q enterprise=%v", cfg.oauthClientMode, cfg.enterpriseCredits)
    }
    if cfg.desensitizeTerms[83] != "Codex" || cfg.desensitizeTerms[84] != "codex" {
        t.Fatalf("Codex terms missing: %#v", cfg.desensitizeTerms[83:])
    }
}

func TestParseFeatureRuntimeDistinguishesDefaultCustomAndEmpty(t *testing.T) {
    custom, err := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms:\n  - ' exploit '\n  - exploit\n  - Codex\n"))
    if err != nil {
        t.Fatal(err)
    }
    if !custom.desensitizeEnabled || custom.desensitizeSource != "custom" {
        t.Fatalf("custom config = %#v", custom)
    }
    want := []string{"exploit", "Codex"}
    if len(custom.desensitizeTerms) != len(want) || custom.desensitizeTerms[0] != want[0] || custom.desensitizeTerms[1] != want[1] {
        t.Fatalf("custom terms = %#v, want %#v", custom.desensitizeTerms, want)
    }

    empty, err := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms: []\n"))
    if err != nil {
        t.Fatal(err)
    }
    if empty.desensitizeSource != "custom" || len(empty.desensitizeTerms) != 0 {
        t.Fatalf("empty custom terms = %#v", empty)
    }

    defaults, err := parseFeatureRuntime(nil)
    if err != nil || len(defaults.desensitizeTerms) != 85 || defaults.desensitizeSource != "default" {
        t.Fatalf("defaults = %#v err=%v", defaults, err)
    }
}

func TestParseFeatureRuntimeRejectsUnsafeTermsAndModes(t *testing.T) {
    for _, yaml := range []string{
        "desensitize_terms: [x]\n",
        "desensitize_terms: ['a\\u200bb']\n",
        "oauth_client_mode: browser\n",
    } {
        if _, err := parseFeatureRuntime([]byte(yaml)); err == nil {
            t.Fatalf("parseFeatureRuntime(%q) succeeded", yaml)
        }
    }
}

func TestDesensitizeMatcherIsLiteralCaseInsensitiveConvergentAndIdempotent(t *testing.T) {
    cfg, err := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms: [DDoS, DoS, kill, attack, noreply@anthropic.com, abc, bcd, 'a+b', Codex, codex]\n"))
    if err != nil {
        t.Fatal(err)
    }
    tests := map[string]string{
        "DDoS": "D\u200bD\u200boS",
        "EXPLOIT-free skill": "EXPLOIT-free sk\u200bill",
        "attacker": "a\u200bttacker",
        "noreply@anthropic.com": "n\u200boreply@a\u200bnthropic.com",
        "abcd": "a\u200bb\u200bcd",
        "a+b": "a\u200b+b",
        "Codex codex": "C\u200bodex c\u200bodex",
    }
    for in, want := range tests {
        got := cfg.matcher.replace(in)
        if got != want {
            t.Errorf("replace(%q) = %q, want %q", in, got, want)
        }
        if again := cfg.matcher.replace(got); again != got {
            t.Errorf("replace is not idempotent: %q -> %q", got, again)
        }
    }
    if strings.Contains(cfg.matcher.replace("DDoS"), "DoS") {
        t.Fatal("nested term still matches after convergence")
    }
}
```

Extend `registration_test.go` to require exact field types and enum values:

```go
func TestRegistrationExposesForkFusionConfig(t *testing.T) {
    got := map[string]pluginapi.ConfigField{}
    for _, field := range wbRegistration().Metadata.ConfigFields {
        got[field.Name] = field
    }
    if got["desensitize"].Type != pluginapi.ConfigFieldTypeBoolean {
        t.Fatalf("desensitize type = %q", got["desensitize"].Type)
    }
    if got["desensitize_terms"].Type != pluginapi.ConfigFieldTypeArray {
        t.Fatalf("desensitize_terms type = %q", got["desensitize_terms"].Type)
    }
    mode := got["oauth_client_mode"]
    if mode.Type != pluginapi.ConfigFieldTypeEnum || !sameStrings(mode.EnumValues, []string{"cli", "workbuddy"}) {
        t.Fatalf("oauth_client_mode = %#v", mode)
    }
    if got["enterprise_credits"].Type != pluginapi.ConfigFieldTypeBoolean {
        t.Fatalf("enterprise_credits type = %q", got["enterprise_credits"].Type)
    }
}
```

- [ ] **Step 2: Run RED**

Run from repository root:

```powershell
go test ./workbuddy -run 'Test(DefaultDesensitize|ParseFeatureRuntime|DesensitizeMatcher|RegistrationExposesForkFusion)' -count=1
```

Expected: compilation fails because `currentFeatureRuntime`, `parseFeatureRuntime`, `oauthClientModeCLI` and matcher do not exist, or registration assertions fail because fields are absent.

- [ ] **Step 3: Implement the default list, matcher and immutable snapshot**

Create `workbuddy/desensitize.go`. Use the exact list below without reordering:

```go
package main

import (
    "errors"
    "regexp"
    "sort"
    "strings"
    "sync/atomic"
    "unicode/utf8"

    "gopkg.in/yaml.v3"
)

const (
    zeroWidthSpace      = "\u200b"
    oauthClientModeCLI  = "cli"
    oauthClientModeWorkBuddy = "workbuddy"
)

var defaultDesensitizeTerms = []string{
    "DoS", "DDoS", "exploit", "credential testing", "credential stuffing",
    "supply chain compromise", "supply-chain compromise", "detection evasion",
    "C2 frameworks", "C2 framework", "command and control", "malicious purposes",
    "malicious intent", "mass targeting", "brute force", "brute-force",
    "privilege escalation", "reverse shell", "remote code execution", "SQL injection",
    "XSS", "CSRF", "phishing", "malware", "ransomware", "keylogger", "rootkit",
    "backdoor", "botnet", "zero-day", "0day", "vulnerability", "vulnerabilities",
    "red teaming", "red-teaming", "sandbox", "sandboxing", "sandboxed", "unsandboxed",
    "escalated privileges", "escalated", "escalation", "destructive action",
    "destructive command", "destructive", "attack", "attacks", "cybersecurity",
    "security review", "exploit development", "hacking", "penetration testing",
    "penetration test", "injection", "weaponize", "weaponized", "harmful", "dangerous",
    "abuse", "abusive", "illegal", "terrorist", "terrorism", "bomb", "weapon",
    "weapons", "drug", "drugs", "narcotic", "suicide", "self-harm", "murder",
    "kill", "violence", "violent", "Claude Code", "Claude Opus", "Claude Sonnet",
    "Claude Haiku", "Claude Fable", "Anthropic", "Co-Authored-By",
    "noreply@anthropic.com", "Codex", "codex",
}

type desensitizeMatcher struct {
    expression *regexp.Regexp
}

type featureRuntimeConfig struct {
    desensitizeEnabled bool
    desensitizeTerms   []string
    desensitizeSource  string
    matcher            *desensitizeMatcher
    oauthClientMode    string
    enterpriseCredits  bool
}

var featureRuntime atomic.Pointer[featureRuntimeConfig]

func init() {
    cfg, err := parseFeatureRuntime(nil)
    if err != nil {
        panic(err)
    }
    featureRuntime.Store(cfg)
}

func currentFeatureRuntime() *featureRuntimeConfig {
    return featureRuntime.Load()
}

type featureConfigYAML struct {
    Desensitize      *bool     `yaml:"desensitize"`
    DesensitizeTerms *[]string `yaml:"desensitize_terms"`
    OAuthClientMode  string    `yaml:"oauth_client_mode"`
    EnterpriseCredits *bool    `yaml:"enterprise_credits"`
}

func parseFeatureRuntime(raw []byte) (*featureRuntimeConfig, error) {
    doc := featureConfigYAML{}
    if strings.TrimSpace(string(raw)) != "" {
        if err := yaml.Unmarshal(raw, &doc); err != nil {
            return nil, errors.New("invalid config_yaml")
        }
    }
    enabled := doc.Desensitize != nil && *doc.Desensitize
    mode := strings.ToLower(strings.TrimSpace(doc.OAuthClientMode))
    if mode == "" {
        mode = oauthClientModeCLI
    }
    if mode != oauthClientModeCLI && mode != oauthClientModeWorkBuddy {
        return nil, errors.New("oauth_client_mode must be cli or workbuddy")
    }
    enterprise := doc.EnterpriseCredits != nil && *doc.EnterpriseCredits
    terms, source, err := normalizedDesensitizeTerms(doc.DesensitizeTerms)
    if err != nil {
        return nil, err
    }
    matcher, err := compileDesensitizeMatcher(terms)
    if err != nil {
        return nil, err
    }
    return &featureRuntimeConfig{
        desensitizeEnabled: enabled,
        desensitizeTerms: terms,
        desensitizeSource: source,
        matcher: matcher,
        oauthClientMode: mode,
        enterpriseCredits: enterprise,
    }, nil
}

func normalizedDesensitizeTerms(configured *[]string) ([]string, string, error) {
    source := "custom"
    input := []string(nil)
    if configured == nil {
        source = "default"
        input = defaultDesensitizeTerms
    } else {
        input = *configured
    }
    out := make([]string, 0, len(input))
    seen := make(map[string]struct{}, len(input))
    for _, raw := range input {
        term := strings.TrimSpace(raw)
        if term == "" {
            continue
        }
        if utf8.RuneCountInString(term) < 2 {
            return nil, "", errors.New("desensitize_terms entries must contain at least two Unicode runes")
        }
        if strings.Contains(term, zeroWidthSpace) {
            return nil, "", errors.New("desensitize_terms entries must not contain U+200B")
        }
        if _, exists := seen[term]; exists {
            continue
        }
        seen[term] = struct{}{}
        out = append(out, term)
    }
    return out, source, nil
}

func compileDesensitizeMatcher(terms []string) (*desensitizeMatcher, error) {
    ordered := append([]string(nil), terms...)
    sort.SliceStable(ordered, func(i, j int) bool {
        return utf8.RuneCountInString(ordered[i]) > utf8.RuneCountInString(ordered[j])
    })
    alternatives := make([]string, 0, len(ordered))
    seenFold := make(map[string]struct{}, len(ordered))
    for _, term := range ordered {
        key := strings.ToLower(term)
        if _, exists := seenFold[key]; exists {
            continue
        }
        seenFold[key] = struct{}{}
        alternatives = append(alternatives, regexp.QuoteMeta(term))
    }
    if len(alternatives) == 0 {
        return &desensitizeMatcher{}, nil
    }
    expression, err := regexp.Compile("(?i:" + strings.Join(alternatives, "|") + ")")
    if err != nil {
        return nil, err
    }
    return &desensitizeMatcher{expression: expression}, nil
}

func (m *desensitizeMatcher) replace(input string) string {
    if m == nil || m.expression == nil || input == "" {
        return input
    }
    current := input
    for {
        changed := false
        next := m.expression.ReplaceAllStringFunc(current, func(match string) string {
            _, size := utf8.DecodeRuneInString(match)
            changed = true
            return match[:size] + zeroWidthSpace + match[size:]
        })
        if !changed {
            return current
        }
        current = next
    }
}
```

If the test fixture containing `EXPLOIT-free` expects replacement but the custom list omits `exploit`, add `exploit` to that test's custom YAML. Do not weaken the assertion.

- [ ] **Step 4: Parse and publish the snapshot from `configure`**

In `usage_config.go`, decode the lifecycle wrapper once, call `parseFeatureRuntime(req.ConfigYAML)` before any state publication, and store only after `configureProxy(nextProxyURL)` succeeds:

```go
nextFeatures, err := parseFeatureRuntime(req.ConfigYAML)
if err != nil {
    return err
}
// existing proxy parse and configureProxy happen here
featureRuntime.Store(nextFeatures)
```

Preserve the existing proxy fail-closed behavior. A malformed desensitize list must not overwrite the previous `featureRuntime` pointer.

In `main.go`, add the four exact registration entries:

```go
{Name: "desensitize", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Insert U+200B into configured blocked terms in system/developer prompt text and tool title/description fields (default false)."},
{Name: "desensitize_terms", Type: pluginapi.ConfigFieldTypeArray, Description: "Editable literal term list for desensitize; missing uses the built-in 85 terms and [] means an empty custom list."},
{Name: "oauth_client_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{oauthClientModeCLI, oauthClientModeWorkBuddy}, Description: "OAuth request profile: cli (default) or explicit WorkBuddy desktop profile."},
{Name: "enterprise_credits", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Probe strict CN enterprise credits before personal resource packages (default false; Global unchanged)."},
```

- [ ] **Step 5: Run GREEN and race-focused config test**

```powershell
go test ./workbuddy -run 'Test(DefaultDesensitize|ParseFeatureRuntime|DesensitizeMatcher|RegistrationExposesForkFusion)' -count=1
go test -race ./workbuddy -run 'TestParseFeatureRuntime' -count=1
```

Expected: PASS. The race run must not report access to a mutating term slice.

- [ ] **Step 6: Commit**

```powershell
git add -- workbuddy/desensitize.go workbuddy/desensitize_test.go workbuddy/usage_config.go workbuddy/main.go workbuddy/registration_test.go
git commit -m "feat(workbuddy): add configurable desensitize matcher"
```

---

### Task 2: Scoped payload walker in the existing single-pass rewrite

**Files:**
- Create: `workbuddy/desensitize_payload_test.go`
- Modify: `workbuddy/desensitize.go`
- Modify: `workbuddy/payload.go`

**Interfaces:**
- Consumes: `currentFeatureRuntime()` and `(*desensitizeMatcher).replace` from Task 1。
- Produces: `applyDesensitizeInPlace(map[string]any, *featureRuntimeConfig) bool`。

- [ ] **Step 1: Write the failing payload scope test**

Create a test that contains positive and negative fields in one real payload:

```go
package main

import (
    "encoding/json"
    "strings"
    "testing"
)

func TestPrepareUpstreamBodyDesensitizesOnlyAllowedPromptAndToolText(t *testing.T) {
    old := currentFeatureRuntime()
    cfg, err := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms: [attack, exploit, Codex, Claude Code, Anthropic]\n"))
    if err != nil {
        t.Fatal(err)
    }
    featureRuntime.Store(cfg)
    t.Cleanup(func() { featureRuntime.Store(old) })

    input := []byte(`{
      "model":"glm-5.3",
      "messages":[
        {"role":"system","content":"Claude Code attack"},
        {"role":"developer","content":[{"type":"text","text":"Anthropic exploit"},{"type":"image_url","image_url":{"url":"attack"}}]},
        {"role":"user","content":"ordinary attack"},
        {"role":"user","content":[{"type":"text","text":"# AGENTS.md instructions"},{"type":"text","text":"attack"}]},
        {"role":"assistant","content":"attack"},
        {"role":"tool","content":"attack"}
      ],
      "tools":[{"type":"function","function":{"name":"attack","description":"attack tool","parameters":{"title":"attack schema","description":"exploit field","enum":["attack"],"default":"attack"}}}]
    }`)
    out := prepareUpstreamBody(input, nil, nil, "glm-5.3")
    var obj map[string]any
    if err := json.Unmarshal(out, &obj); err != nil {
        t.Fatal(err)
    }
    encoded := string(out)
    for _, want := range []string{"C\\u200blaude Code", "a\\u200bttack tool", "e\\u200bxploit field"} {
        if !strings.Contains(encoded, want) {
            t.Errorf("output missing %q: %s", want, encoded)
        }
    }
    if !strings.Contains(encoded, `"content":"ordinary attack"`) ||
        !strings.Contains(encoded, `"content":"attack"`) ||
        !strings.Contains(encoded, `"name":"attack"`) ||
        !strings.Contains(encoded, `"title":"attack schema"`) ||
        !strings.Contains(encoded, `"enum":["attack"]`) ||
        !strings.Contains(encoded, `"default":"attack"`) {
        t.Fatalf("negative-scope field changed: %s", encoded)
    }
}

func TestPrepareUpstreamBodyDesensitizeOffIsByteSemanticNoop(t *testing.T) {
    old := currentFeatureRuntime()
    cfg, _ := parseFeatureRuntime([]byte("desensitize: false\ndesensitize_terms: [attack]\n"))
    featureRuntime.Store(cfg)
    t.Cleanup(func() { featureRuntime.Store(old) })
    out := prepareUpstreamBody([]byte(`{"model":"m","messages":[{"role":"system","content":"attack"}]}`), nil, nil, "m")
    if strings.Contains(string(out), "a\\u200bttack") {
        t.Fatalf("disabled desensitize changed payload: %s", out)
    }
}

func TestDesensitizeUserMarkersAreExactAndCaseSensitive(t *testing.T) {
    cfg, _ := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms: [attack]\n"))
    exact := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "<system-reminder> attack"}}}
    wrong := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "<System-Reminder> attack"}}}
    applyDesensitizeInPlace(exact, cfg)
    applyDesensitizeInPlace(wrong, cfg)
    if !strings.Contains(exact["messages"].([]any)[0].(map[string]any)["content"].(string), zeroWidthSpace) {
        t.Fatal("exact marker did not enable user text")
    }
    if strings.Contains(wrong["messages"].([]any)[0].(map[string]any)["content"].(string), zeroWidthSpace) {
        t.Fatal("case-insensitive marker match changed ordinary user text")
    }
}
```

- [ ] **Step 2: Run RED**

```powershell
go test ./workbuddy -run 'TestPrepareUpstreamBodyDesensitize|TestDesensitizeUserMarkers' -count=1
```

Expected: compilation fails because `applyDesensitizeInPlace` is missing, or output lacks U+200B.

- [ ] **Step 3: Implement the walker**

Add exact markers to `desensitize.go`:

```go
var desensitizeUserMarkers = []string{
    "# AGENTS.md instructions",
    "<environment_context>",
    "<permissions instructions>",
    "<collaboration_mode>",
    "<skills_instructions>",
    "<system-reminder>",
    "# claudeMd",
}
```

Implement these helpers without a second JSON marshal round-trip:

```go
func applyDesensitizeInPlace(obj map[string]any, cfg *featureRuntimeConfig) bool
func desensitizeMessageInPlace(msg map[string]any, cfg *featureRuntimeConfig) bool
func desensitizeTextBlocksInPlace(content []any, enabled bool, cfg *featureRuntimeConfig) bool
func desensitizeToolMetadataInPlace(value any, cfg *featureRuntimeConfig) bool
```

Required logic:

```go
func applyDesensitizeInPlace(obj map[string]any, cfg *featureRuntimeConfig) bool {
    if cfg == nil || !cfg.desensitizeEnabled || cfg.matcher == nil {
        return false
    }
    changed := false
    if messages, ok := obj["messages"].([]any); ok {
        for _, raw := range messages {
            if msg, ok := raw.(map[string]any); ok && desensitizeMessageInPlace(msg, cfg) {
                changed = true
            }
        }
    }
    if tools, ok := obj["tools"]; ok && desensitizeToolMetadataInPlace(tools, cfg) {
        changed = true
    }
    return changed
}
```

For `system` and `developer`, process string content and text blocks. For `user`, concatenate only `type == "text"` block text to decide marker match, then process those text blocks; string user content is processed only when that string itself contains an exact marker. Other roles return false. Tool recursion changes a string only when its map key is exactly `description` or `title`; recursion into nested maps/arrays continues so nested `parameters.description` is reached, while `parameters.title` is also changed because the key is exactly `title`. The negative fixture above labels `title` as unchanged, so correct the fixture to expect `a\u200bttack schema`; do not violate the spec to satisfy the initial assertion.

- [ ] **Step 4: Integrate in `prepareUpstreamBody` after the existing sanitizer**

In `payload.go`:

```go
// 4. existing fixed sanitizer and reasoning policy.
rewriteSystemInPlace(obj)
// 5. configurable desensitize, still inside this JSON pass.
applyDesensitizeInPlace(obj, currentFeatureRuntime())
// 6. Global system injection.
ensureSystemMessageInPlace(obj, sa)
```

Do not move or remove `sanitizeBlockedTemplates` and do not delete tool metadata.

- [ ] **Step 5: Run GREEN and existing sanitizer regression**

```powershell
go test ./workbuddy -run 'TestPrepareUpstreamBodyDesensitize|TestDesensitizeUserMarkers|TestSanitizeBlockedTemplates|TestPrepareUpstreamBodySanitizes' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```powershell
git add -- workbuddy/desensitize.go workbuddy/desensitize_payload_test.go workbuddy/payload.go
git commit -m "feat(workbuddy): desensitize scoped prompt fields"
```

---

### Task 3: Runtime settings endpoint and panel editor

**Files:**
- Modify: `workbuddy/management.go`
- Modify: `workbuddy/panel.html`
- Create: `workbuddy/panel_features_test.go`

**Interfaces:**
- Consumes: `currentFeatureRuntime()` from Task 1。
- Produces: management `GET /plugins/workbuddy/desensitize` and panel calls to generic `GET/PATCH /v0/management/plugins/workbuddy/config`。

- [ ] **Step 1: Write failing route and HTML contract tests**

Create `panel_features_test.go`:

```go
package main

import (
    "encoding/json"
    "net/http"
    "strings"
    "testing"

    "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestManagementReturnsEffectiveDesensitizeSettings(t *testing.T) {
    old := currentFeatureRuntime()
    cfg, _ := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms: [Codex]\n"))
    featureRuntime.Store(cfg)
    t.Cleanup(func() { featureRuntime.Store(old) })

    raw, err := handleManagement(mustJSON(pluginapi.ManagementRequest{
        Method: http.MethodGet,
        Path: loadedManagementBasePath() + "/plugins/workbuddy/desensitize",
    }))
    if err != nil {
        t.Fatal(err)
    }
    var env envelope
    if err := json.Unmarshal(raw, &env); err != nil {
        t.Fatal(err)
    }
    var resp pluginapi.ManagementResponse
    if err := json.Unmarshal(env.Result, &resp); err != nil {
        t.Fatal(err)
    }
    if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), `"source":"custom"`) || !strings.Contains(string(resp.Body), `"Codex"`) {
        t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
    }
}

func TestPanelEditsDesensitizeThroughGenericPluginConfigAPI(t *testing.T) {
    html := string(panelHTML)
    required := []string{
        `id="desensitizeModal"`, `id="desensitizeEnabled"`, `id="desensitizeTerms"`,
        `api("/desensitize")`, `managementAPI("/plugins/workbuddy/config"`,
        `"desensitize_terms":null`, `"desensitize":false`,
    }
    for _, want := range required {
        if !strings.Contains(html, want) {
            t.Errorf("panel missing %q", want)
        }
    }
}
```

- [ ] **Step 2: Run RED**

```powershell
go test ./workbuddy -run 'TestManagementReturnsEffectiveDesensitize|TestPanelEditsDesensitize' -count=1
```

Expected: route returns 404 and HTML strings are missing.

- [ ] **Step 3: Register and serve the read-only endpoint**

Add route registration:

```go
{Method: http.MethodGet, Path: base + "/desensitize", Description: "Get effective WorkBuddy desensitize runtime settings."},
```

Add a switch case returning cloned terms:

```go
case req.Method == http.MethodGet && path == base+"/desensitize":
    cfg := currentFeatureRuntime()
    terms := append([]string(nil), cfg.desensitizeTerms...)
    return okEnvelope(mgmtJSONResponse(http.StatusOK, map[string]any{
        "enabled": cfg.desensitizeEnabled,
        "terms": terms,
        "source": cfg.desensitizeSource,
    }))
```

- [ ] **Step 4: Add settings modal and generic management fetch**

In `panel.html`, keep existing `api(path)` for plugin-owned routes and add:

```javascript
async function managementAPI(path, opts={}){
  const k=getKey();
  if(!k){showAuth();throw new Error("需要管理密钥")}
  const r=await fetch("/v0/management"+path,{...opts,credentials:"omit",headers:{...(opts.headers||{}),...authHeaders(k)}});
  if(!r.ok){let body="";try{body=await r.text()}catch(_){}throw new Error(body.trim()||("HTTP "+r.status))}
  return r.json();
}
```

Add a toolbar button, modal, and exact operations:

```javascript
async function openDesensitizeModal(){
  const d=await api("/desensitize");
  document.getElementById("desensitizeEnabled").checked=!!d.enabled;
  document.getElementById("desensitizeTerms").value=(d.terms||[]).join("\n");
  document.getElementById("desensitizeSource").textContent=d.source==="custom"?"自定义词表":"内置默认词表";
  document.getElementById("desensitizeModal").classList.add("show");
}
async function saveDesensitize(btn){
  const terms=document.getElementById("desensitizeTerms").value.split(/\r?\n/).map(v=>v.trim()).filter(Boolean);
  await managementAPI("/plugins/workbuddy/config",{method:"PATCH",headers:{"Content-Type":"application/json"},body:JSON.stringify({desensitize:document.getElementById("desensitizeEnabled").checked,desensitize_terms:terms})});
  toast("屏蔽词设置已保存","ok");
}
async function restoreDesensitizeTerms(){
  await managementAPI("/plugins/workbuddy/config",{method:"PATCH",headers:{"Content-Type":"application/json"},body:JSON.stringify({"desensitize_terms":null})});
}
async function restoreAllDesensitizeDefaults(){
  await managementAPI("/plugins/workbuddy/config",{method:"PATCH",headers:{"Content-Type":"application/json"},body:JSON.stringify({"desensitize":false,"desensitize_terms":null})});
}
```

Use textContent for source/status. Do not embed the 85 terms in HTML.

- [ ] **Step 5: Run GREEN**

```powershell
go test ./workbuddy -run 'TestManagementReturnsEffectiveDesensitize|TestPanelEditsDesensitize' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```powershell
git add -- workbuddy/management.go workbuddy/panel.html workbuddy/panel_features_test.go
git commit -m "feat(workbuddy): edit desensitize settings in panel"
```

---

### Task 4: OAuth poll uses path-derived identity

**Files:**
- Modify: `workbuddy/oauth.go`
- Modify: `workbuddy/auth_identity_test.go`

**Interfaces:**
- Produces: `toAuthDataForLoginPoll(*storedAuth) pluginapi.AuthData`。
- Keeps: `toAuthData` behavior for import and file naming unchanged。

- [ ] **Step 1: Write failing identity tests**

Append:

```go
func TestToAuthDataForLoginPollKeepsCanonicalFileNameAndClearsID(t *testing.T) {
    sa := &storedAuth{
        Auth: storedTokens{AccessToken: "at", RefreshToken: "rt"},
        Account: storedAccount{UID: "uid-1", Nickname: "nick"},
    }
    got := toAuthDataForLoginPoll(sa)
    if got.ID != "" {
        t.Fatalf("poll ID = %q, want path-derived empty ID", got.ID)
    }
    if got.FileName != "workbuddy-uid-1.json" {
        t.Fatalf("poll filename = %q", got.FileName)
    }
    imported := toAuthData(sa)
    if imported.ID != "uid-1" || imported.FileName != "workbuddy-uid-1.json" {
        t.Fatalf("generic login/import behavior changed: %#v", imported)
    }
}
```

- [ ] **Step 2: Run RED**

```powershell
go test ./workbuddy -run 'TestToAuthDataForLoginPoll' -count=1
```

Expected: compilation fails because helper is absent.

- [ ] **Step 3: Implement and use the helper only in poll success**

```go
func toAuthDataForLoginPoll(sa *storedAuth) pluginapi.AuthData {
    ad := toAuthData(sa)
    ad.ID = ""
    return ad
}
```

Replace only `handlePollLogin` success `Auth: toAuthData(sa)` with `Auth: toAuthDataForLoginPoll(sa)`.

- [ ] **Step 4: Run GREEN and identity regressions**

```powershell
go test ./workbuddy -run 'Test(ToAuthDataForLoginPoll|ToAuthData_LoginImport|HandleParseAuth|ToAuthDataForRefresh)' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add -- workbuddy/oauth.go workbuddy/auth_identity_test.go
git commit -m "fix(workbuddy): use path-derived OAuth poll identity"
```

---

### Task 5: Buffered host response casing and callback-aware bridge primitives

**Files:**
- Modify: `workbuddy/host_bridge.go`
- Create: `workbuddy/host_bridge_test.go`

**Interfaces:**
- Produces: `hostHTTPDoWithCallback(*http.Request, string)`, `hostHTTPDoWithStateAndCallback(*proxyRoutingState, *http.Request, string)`, `hostHTTPDoStreamWithCallback(*http.Request, string)`。
- Existing `hostHTTPDo`, `hostHTTPDoWithState`, `hostHTTPDoStream` remain and delegate with empty callback ID。

- [ ] **Step 1: Write failing wire tests**

```go
package main

import (
    "encoding/json"
    "net/http"
    "testing"
)

func TestDecodeBufferedHostHTTPResponseAcceptsBothStatusCasings(t *testing.T) {
    for _, fixture := range []string{
        `{"StatusCode":201,"Headers":{"X-Test":["pc"]},"Body":"cGM="}`,
        `{"status_code":202,"headers":{"X-Test":["sc"]},"body":"c2M="}`,
    } {
        got, err := decodeBufferedHostHTTPResponse([]byte(fixture))
        if err != nil {
            t.Fatal(err)
        }
        if got.StatusCode != 201 && got.StatusCode != 202 {
            t.Fatalf("status = %d for %s", got.StatusCode, fixture)
        }
        if got.Headers.Get("X-Test") == "" || len(got.Body) != 2 {
            t.Fatalf("response = %#v", got)
        }
    }
}

func TestHostHTTPRequestWirePlacesCallbackAtTopLevel(t *testing.T) {
    req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
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
    nested := doc["request"].(map[string]any)
    if _, exists := nested["host_callback_id"]; exists {
        t.Fatalf("callback leaked into nested request: %#v", nested)
    }
}
```

- [ ] **Step 2: Run RED**

```powershell
go test ./workbuddy -run 'TestDecodeBufferedHostHTTPResponse|TestHostHTTPRequestWire' -count=1
```

Expected: compilation fails because decoder and builder are absent.

- [ ] **Step 3: Add wire field, decoder and builder**

```go
type rpcHostHTTPRequestWire struct {
    HostCallbackID string            `json:"host_callback_id,omitempty"`
    Request        *rpcHostHTTPInner `json:"request,omitempty"`
}

type rpcHostHTTPBufferedResponseWire struct {
    StatusCodeSnake  int                 `json:"status_code"`
    StatusCodePascal int                 `json:"StatusCode"`
    Headers          map[string][]string `json:"headers,omitempty"`
    Body             []byte              `json:"body,omitempty"`
}

func decodeBufferedHostHTTPResponse(result []byte) (*hostHTTPResponse, error) {
    var wire rpcHostHTTPBufferedResponseWire
    if err := json.Unmarshal(result, &wire); err != nil {
        return nil, fmt.Errorf("decode host.http.do response: %w", err)
    }
    status := wire.StatusCodeSnake
    if status == 0 {
        status = wire.StatusCodePascal
    }
    return &hostHTTPResponse{StatusCode: status, Headers: http.Header(wire.Headers), Body: wire.Body}, nil
}

func newRPCHostHTTPRequestWire(req *http.Request, body []byte, callbackID string) rpcHostHTTPRequestWire {
    return rpcHostHTTPRequestWire{
        HostCallbackID: strings.TrimSpace(callbackID),
        Request: &rpcHostHTTPInner{Method: req.Method, URL: req.URL.String(), Headers: map[string][]string(req.Header), Body: body},
    }
}
```

- [ ] **Step 4: Add callback-aware helpers without changing old callers**

```go
func hostHTTPDo(req *http.Request) (*hostHTTPResponse, error) {
    return hostHTTPDoWithCallback(req, "")
}
func hostHTTPDoWithCallback(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
    return hostHTTPDoWithStateAndCallback(currentProxyState(), req, callbackID)
}
func hostHTTPDoWithState(state *proxyRoutingState, req *http.Request) (*hostHTTPResponse, error) {
    return hostHTTPDoWithStateAndCallback(state, req, "")
}
func hostHTTPDoWithStateAndCallback(state *proxyRoutingState, req *http.Request, callbackID string) (*hostHTTPResponse, error) {
    // preserve current blocked, explicit-proxy, Windows and direct behavior;
    // only inherited host bridge wire uses callbackID.
}
func hostHTTPDoStream(req *http.Request) (*hostHTTPStream, int, http.Header, error) {
    return hostHTTPDoStreamWithCallback(req, "")
}
func hostHTTPDoStreamWithCallback(req *http.Request, callbackID string) (*hostHTTPStream, int, http.Header, error) {
    // preserve current behavior and use newRPCHostHTTPRequestWire for inherited host bridge.
}
```

Use `decodeBufferedHostHTTPResponse(result)` in the buffered path.

- [ ] **Step 5: Run GREEN and proxy regressions**

```powershell
go test ./workbuddy -run 'TestDecodeBufferedHostHTTPResponse|TestHostHTTPRequestWire|TestHostHTTPHelpersRouteByProxyState|TestHTTPProxyActuallyRoutes' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```powershell
git add -- workbuddy/host_bridge.go workbuddy/host_bridge_test.go
git commit -m "fix(workbuddy): preserve host callback and status casing"
```

---

### Task 6: Propagate inbound callback IDs through executor, refresh and management

**Files:**
- Modify: `workbuddy/main.go`
- Modify: `workbuddy/executor_http.go`
- Modify: `workbuddy/oauth.go`
- Modify: `workbuddy/keepalive.go`
- Modify: `workbuddy/management.go`
- Modify: `workbuddy/panel.go`
- Modify: `workbuddy/cache.go`
- Modify: `workbuddy/billing.go`
- Modify: `workbuddy/checkin.go`
- Modify: `workbuddy/credits_handler.go`
- Modify: `workbuddy/trial.go`
- Modify: `workbuddy/stream.go`
- Modify: `workbuddy/executor_http_test.go`
- Modify: `workbuddy/host_bridge_test.go`

**Interfaces:**
- Consumes callback-aware helpers from Task 5。
- Produces local wrappers matching CPA v7.2.30: `executorRequestWire`, `executorHTTPRequestWire`, `authRefreshRequestWire`, `managementRequestWire`。
- Produces callback variants while preserving existing empty-ID wrappers for scheduler and detached callers。

- [ ] **Step 1: Write failing wrapper and call-graph tests**

Add tests for JSON wrapper decoding:

```go
func TestInboundRPCWrappersPreserveHostCallbackID(t *testing.T) {
    fixtures := []struct {
        raw []byte
        decode func([]byte) string
    }{
        {mustJSON(map[string]any{"host_callback_id":"exec-1"}), func(raw []byte) string { var v executorRequestWire; _ = json.Unmarshal(raw, &v); return v.HostCallbackID }},
        {mustJSON(map[string]any{"host_callback_id":"http-2"}), func(raw []byte) string { var v executorHTTPRequestWire; _ = json.Unmarshal(raw, &v); return v.HostCallbackID }},
        {mustJSON(map[string]any{"host_callback_id":"refresh-3"}), func(raw []byte) string { var v authRefreshRequestWire; _ = json.Unmarshal(raw, &v); return v.HostCallbackID }},
        {mustJSON(map[string]any{"host_callback_id":"management-4"}), func(raw []byte) string { var v managementRequestWire; _ = json.Unmarshal(raw, &v); return v.HostCallbackID }},
    }
    want := []string{"exec-1", "http-2", "refresh-3", "management-4"}
    for i, fixture := range fixtures {
        if got := fixture.decode(fixture.raw); got != want[i] {
            t.Fatalf("wrapper %d callback = %q, want %q", i, got, want[i])
        }
    }
}
```

Add a source-contract test that reads the named handler bodies and requires use of `HostCallbackID` plus callback-aware helpers. This repository already uses source-contract tests in `panel_ip_test.go`; keep the assertions restricted to exact handler bodies and signatures rather than general text counts.

- [ ] **Step 2: Run RED**

```powershell
go test ./workbuddy -run 'TestInboundRPCWrappersPreserveHostCallbackID|TestInboundHandlersForwardHostCallbackID' -count=1
```

Expected: missing wrapper types or source contract failure.

- [ ] **Step 3: Add exact inbound wrappers and executor propagation**

```go
type executorRequestWire struct {
    pluginapi.ExecutorRequest
    HostCallbackID string `json:"host_callback_id,omitempty"`
}
type executorHTTPRequestWire struct {
    pluginapi.ExecutorHTTPRequest
    HostCallbackID string `json:"host_callback_id,omitempty"`
}
type authRefreshRequestWire struct {
    pluginapi.AuthRefreshRequest
    HostCallbackID string `json:"host_callback_id,omitempty"`
}
type managementRequestWire struct {
    pluginapi.ManagementRequest
    HostCallbackID string `json:"host_callback_id,omitempty"`
}
```

`handleExecExecute` decodes `executorRequestWire` and calls `hostHTTPDoStreamWithCallback(httpReq, req.HostCallbackID)`.

`handleExecStream` already has the field. Pass it to both:

```go
collectUpstreamStream(body, sa, sseFramed, collector, req.HostCallbackID)
pumpUpstreamStream(httpReq, cancel, req.StreamID, sseFramed, req.Model, upstreamModel, authUID, started, req.AuthID, req.HostCallbackID)
```

`handleExecHTTPRequest` decodes `executorHTTPRequestWire` and calls `hostHTTPDoWithCallback`.

- [ ] **Step 4: Add refresh and management callback variants**

Preserve old no-ID functions for scheduler use:

```go
func refreshCall(sa *storedAuth) (json.RawMessage, []byte, int, error) {
    return refreshCallWithCallback(sa, "")
}
func refreshCallWithCallback(sa *storedAuth, callbackID string) (json.RawMessage, []byte, int, error) {
    // existing body, hostHTTPDoWithCallback(req, callbackID)
}
```

`handleRefreshAuth` decodes `authRefreshRequestWire` and uses `refreshCallWithCallback`.

For management, decode `managementRequestWire`. Add callback variants only where a management request can synchronously issue HTTP:

```go
func fetchEgressIPWithCallback(callbackID string) (string, error)
func buildDashboardExWithCallback(force, fetchCredits bool, callbackID string) map[string]any
func cachedAccountDetailsWithCallback(authID string, sa *storedAuth, force bool, callbackID string) (...)
func fetchUserResourceWithCallback(sa *storedAuth, callbackID string) (*creditsSummary, error)
func fetchCheckinStatusWithCallback(sa *storedAuth, callbackID string) (*checkinSummary, error)
func fetchPaymentTypeWithCallback(sa *storedAuth, callbackID string) string
func billingCallWithCallback(sa *storedAuth, path string, body any, callbackID string) (json.RawMessage, error)
```

Existing public package helpers delegate with `""`. Thread the management ID through joined goroutines only. Detached reconcile, scheduler, usage and janitor paths must call the old empty-ID wrappers.

Apply the same pattern to manual checkin, credits query, trial and manual keepalive handlers. Do not place the ID in a package global.

- [ ] **Step 5: Run GREEN and race tests**

```powershell
go test ./workbuddy -run 'TestInboundRPCWrappersPreserveHostCallbackID|TestInboundHandlersForwardHostCallbackID|TestExecutorHTTPRequest|TestManagement' -count=1
go test -race ./workbuddy -run 'TestInboundRPCWrappersPreserveHostCallbackID|TestManagement' -count=1
```

Expected: PASS with no race.

- [ ] **Step 6: Commit**

```powershell
git add -- workbuddy/main.go workbuddy/executor_http.go workbuddy/oauth.go workbuddy/keepalive.go workbuddy/management.go workbuddy/panel.go workbuddy/cache.go workbuddy/billing.go workbuddy/checkin.go workbuddy/credits_handler.go workbuddy/trial.go workbuddy/stream.go workbuddy/executor_http_test.go workbuddy/host_bridge_test.go
git commit -m "fix(workbuddy): propagate host callback context"
```

---

### Task 7: Native stream errors and typed synchronous HTTP status

**Files:**
- Modify: `workbuddy/main.go`
- Modify: `workbuddy/stream.go`
- Create: `workbuddy/stream_error_test.go`

**Interfaces:**
- Produces: `errorEnvelopeWithStatus(code, message string, status int) []byte`。
- Produces native stream error wire `{"stream_id":...,"error":...}`。
- Produces `upstreamStatusError` for sync stream collection status detection。

- [ ] **Step 1: Write failing error-wire tests**

```go
package main

import (
    "encoding/json"
    "strings"
    "testing"
)

func TestNativeStreamErrorWireUsesErrorNotPayload(t *testing.T) {
    raw := streamErrorWire("stream-1", "Bearer secret-token failed")
    var doc map[string]any
    if err := json.Unmarshal(raw, &doc); err != nil {
        t.Fatal(err)
    }
    if _, exists := doc["payload"]; exists {
        t.Fatalf("error encoded as payload: %#v", doc)
    }
    message, _ := doc["error"].(string)
    if message == "" || strings.Contains(message, "secret-token") {
        t.Fatalf("unredacted native error: %#v", doc)
    }
}

func TestTypedErrorEnvelopeCarriesHTTPStatus(t *testing.T) {
    raw := errorEnvelopeWithStatus("http_error", "upstream 429", 429)
    var env envelope
    if err := json.Unmarshal(raw, &env); err != nil {
        t.Fatal(err)
    }
    if env.OK || env.Error == nil || env.Error.HTTPStatus != 429 || env.Error.Code != "http_error" {
        t.Fatalf("envelope = %#v", env)
    }
}

func TestStreamErrorAndCloseWireAreSeparate(t *testing.T) {
    errDoc := string(streamErrorWire("s", "failed"))
    closeDoc := string(streamCloseWire("s"))
    if !strings.Contains(errDoc, `"error":"failed"`) || strings.Contains(closeDoc, `"error"`) {
        t.Fatalf("error=%s close=%s", errDoc, closeDoc)
    }
}
```

Add tests around `aggregateSSEWithCollector` and the pump test seam to confirm malformed-only/empty streams and mid-read errors return native errors, while emit failure does not attempt a second error emit. Use a small injectable function variable only around `hostCall` in `stream.go` if the C host API cannot be replaced; restore it with `t.Cleanup` and do not expose it outside the package.

- [ ] **Step 2: Run RED**

```powershell
go test ./workbuddy -run 'TestNativeStreamErrorWire|TestTypedErrorEnvelope|TestStreamErrorAndCloseWire|TestPumpUpstream' -count=1
```

Expected: missing helpers or assertions show payload-form errors.

- [ ] **Step 3: Implement native wire and typed envelope**

In `main.go`:

```go
type envelopeError struct {
    Code       string `json:"code"`
    Message    string `json:"message"`
    HTTPStatus int    `json:"http_status,omitempty"`
}
func errorEnvelopeWithStatus(code, message string, status int) []byte {
    raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: status}})
    return raw
}
```

In `stream.go`:

```go
func streamErrorWire(streamID, message string) []byte {
    raw, _ := json.Marshal(map[string]any{"stream_id": streamID, "error": redactSecrets(message)})
    return raw
}
func streamCloseWire(streamID string) []byte {
    raw, _ := json.Marshal(map[string]any{"stream_id": streamID})
    return raw
}
func streamEmitError(streamID, message string) {
    if streamID == "" { return }
    _, _ = hostCall(pluginabi.MethodHostStreamEmit, streamErrorWire(streamID, message))
}
```

Keep close exactly once. On `streamEmit` failure, return without calling `streamEmitError` because the client stream is already gone.

Define:

```go
type upstreamStatusError struct { status int; message string }
func (e *upstreamStatusError) Error() string { return e.message }
```

Return this from `collectUpstreamStream` for status >= 400. In `handleExecStream` no-async branch, use `errors.As` and return `errorEnvelopeWithStatus("http_error", redactSecrets(err.Error()), status), nil`. In `handleExecExecute`, directly return the typed envelope for status >= 400. Transport/parser errors continue through the existing Go error path without invented status.

- [ ] **Step 4: Run GREEN and SSE regressions**

```powershell
go test ./workbuddy -run 'TestNativeStreamErrorWire|TestTypedErrorEnvelope|TestStreamErrorAndCloseWire|TestPumpUpstream|TestAggregateSSE|TestCleanChunk' -count=1
```

Expected: PASS. Existing `[DONE]`, `json.Valid` and empty-stream checks remain.

- [ ] **Step 5: Commit**

```powershell
git add -- workbuddy/main.go workbuddy/stream.go workbuddy/stream_error_test.go
git commit -m "fix(workbuddy): emit native stream errors"
```

---

### Task 8: Hard-credit marker and bounded billing concurrency

**Files:**
- Modify: `workbuddy/policy.go`
- Modify: `workbuddy/lifecycle_test.go`
- Modify: `workbuddy/billing.go`
- Create: `workbuddy/billing_concurrency_test.go`

**Interfaces:**
- Produces: process-wide capacity-4 resource-fetch semaphore covering enterprise/personal pagination and retries。

- [ ] **Step 1: Write failing marker and concurrency tests**

```go
func TestHardCreditErrorRecognizesQuotaAlreadyExhausted(t *testing.T) {
    if !isHardCreditError(429, `{"message":"额度已用尽"}`) {
        t.Fatal("额度已用尽 not recognized")
    }
    if isHardCreditError(429, `{"message":"too many requests"}`) {
        t.Fatal("pure 429 classified as hard credit")
    }
}
```

For concurrency, run 10, 20 and 50 concurrent `fetchUserResource` calls against an `httptest.Server`. Increment an atomic in-flight counter in the handler, record max, block each request on a release channel, then release in waves. Configure `billingBase` to the server and explicit/direct test routing as existing billing tests do. Assert `max <= 4` and every call completes.

- [ ] **Step 2: Run RED**

```powershell
go test ./workbuddy -run 'TestHardCreditErrorRecognizesQuotaAlreadyExhausted|TestFetchUserResourceConcurrencyLimit' -count=1
```

Expected: marker assertion fails and max in-flight exceeds 4.

- [ ] **Step 3: Add the marker and semaphore at the shared boundary**

Add only:

```go
"额度已用尽",
```

Add:

```go
const userResourceConcurrency = 4
var userResourceSlots = make(chan struct{}, userResourceConcurrency)

func acquireUserResourceSlot() func() {
    userResourceSlots <- struct{}{}
    return func() { <-userResourceSlots }
}
```

Acquire at the start of `fetchUserResourceWithCallback`, `defer` release, and hold the slot through all pages and retry delays. Do not add a second semaphore in panel or cache.

- [ ] **Step 4: Run GREEN and race**

```powershell
go test ./workbuddy -run 'TestHardCreditErrorRecognizesQuotaAlreadyExhausted|TestFetchUserResourceConcurrencyLimit' -count=1
go test -race ./workbuddy -run 'TestFetchUserResourceConcurrencyLimit' -count=1
```

Expected: PASS, observed max is between 1 and 4.

- [ ] **Step 5: Commit**

```powershell
git add -- workbuddy/policy.go workbuddy/lifecycle_test.go workbuddy/billing.go workbuddy/billing_concurrency_test.go
git commit -m "fix(workbuddy): bound billing concurrency"
```

---

### Task 9: Explicit WorkBuddy desktop OAuth profile

**Files:**
- Modify: `workbuddy/oauth.go`
- Modify: `workbuddy/main.go`
- Modify: `workbuddy/keepalive.go`
- Create: `workbuddy/oauth_profile_test.go`

**Interfaces:**
- Consumes: `currentFeatureRuntime().oauthClientMode`。
- Produces: immutable `oauthRequestProfile`, profile stored in `loginCtx`, request builders for state/poll/account/refresh。

- [ ] **Step 1: Write failing request-shape tests**

```go
package main

import (
    "net/http"
    "net/url"
    "testing"
)

func TestCLIProfilePreservesCurrentOAuthRequest(t *testing.T) {
    p := oauthProfileForMode(oauthClientModeCLI)
    req, err := buildAuthStateRequest(p)
    if err != nil { t.Fatal(err) }
    if req.Method != http.MethodPost || req.URL.String() != endpointAuthState || req.Header.Get("User-Agent") != clientUA {
        t.Fatalf("CLI state request changed: %s %s %#v", req.Method, req.URL, req.Header)
    }
}

func TestWorkBuddyProfileBuildsDesktopStateRequest(t *testing.T) {
    p := oauthProfileForMode(oauthClientModeWorkBuddy)
    req, err := buildAuthStateRequest(p)
    if err != nil { t.Fatal(err) }
    if req.URL.String() != upstreamBaseCN+"/v2/plugin/auth/state?platform=workbuddy" {
        t.Fatalf("state URL = %s", req.URL)
    }
    wants := map[string]string{
        "User-Agent":"WorkBuddy/5.3.14 WorkBuddy/5.3.14 CLI/2.115.0",
        "Origin":"https://www.workbuddy.cn",
        "Referer":"https://www.workbuddy.cn/",
        "X-No-Authorization":"true",
        "X-No-User-Id":"true",
        "X-No-Enterprise-Id":"true",
        "X-No-Department-Info":"true",
    }
    for key, want := range wants {
        if got := req.Header.Get(key); got != want { t.Errorf("%s=%q want %q", key, got, want) }
    }
}

func TestWorkBuddyProfileAddsBrowserQueryWithoutDroppingExistingQuery(t *testing.T) {
    got, err := decorateDesktopAuthURL("https://example.test/login?state=s", "0123456789abcdef0123456789abcdef")
    if err != nil { t.Fatal(err) }
    u, _ := url.Parse(got)
    if u.Query().Get("state") != "s" || u.Query().Get("version") != "5.3.14" || u.Query().Get("loginSessionId") != "0123456789abcdef0123456789abcdef" {
        t.Fatalf("query = %v", u.Query())
    }
}
```

Add a cookie-continuity test that stores a desktop profile in `loginCtx`, changes global config to CLI, then asserts poll request builders still use the stored desktop profile.

- [ ] **Step 2: Run RED**

```powershell
go test ./workbuddy -run 'TestCLIProfile|TestWorkBuddyProfile' -count=1
```

Expected: missing types/functions.

- [ ] **Step 3: Implement endpoint-specific profiles**

```go
type oauthRequestProfile struct {
    mode      string
    stateURL  string
    userAgent string
    origin    string
    version   string
}

func oauthProfileForMode(mode string) oauthRequestProfile {
    if mode == oauthClientModeWorkBuddy {
        return oauthRequestProfile{
            mode: mode,
            stateURL: upstreamBaseCN + "/v2/plugin/auth/state?platform=workbuddy",
            userAgent: "WorkBuddy/5.3.14 WorkBuddy/5.3.14 CLI/2.115.0",
            origin: "https://www.workbuddy.cn",
            version: "5.3.14",
        }
    }
    return oauthRequestProfile{mode: oauthClientModeCLI, stateURL: endpointAuthState, userAgent: clientUA, origin: originReferer}
}
```

Create endpoint-specific header functions. Do not replace `commonHeaders` or `backendHeaders` globally.

Extend `loginCtx`:

```go
type loginCtx struct {
    client         *http.Client
    expires        time.Time
    profile        oauthRequestProfile
    loginSessionID string
}
```

At start, snapshot mode, generate `randomHex(16)` only for desktop, build state request, decorate browser URL, and store profile/session ID. Poll/account use `lc.profile`, not current global mode.

`refreshCallWithCallback` uses the current configured OAuth profile only for client-identifying headers, while keeping `X-Refresh-Token` and `X-Auth-Refresh-Source: plugin`. Chat headers remain unchanged.

- [ ] **Step 4: Run GREEN and OAuth regressions**

```powershell
go test ./workbuddy -run 'TestCLIProfile|TestWorkBuddyProfile|TestHandlePollLogin|TestBuildLoginAuth|TestBuildRefreshedAuth' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add -- workbuddy/oauth.go workbuddy/main.go workbuddy/keepalive.go workbuddy/oauth_profile_test.go
git commit -m "feat(workbuddy): add desktop OAuth profile"
```

---

### Task 10: Strict CN enterprise credits probe and lifecycle freshness guard

**Files:**
- Modify: `workbuddy/billing.go`
- Modify: `workbuddy/cache.go`
- Modify: `workbuddy/lifecycle.go`
- Create: `workbuddy/enterprise_test.go`

**Interfaces:**
- Consumes: `currentFeatureRuntime().enterpriseCredits`, callback-aware billing and capacity-4 semaphore。
- Produces: `fetchEnterpriseCreditsCN(*storedAuth, string) (*creditsSummary, error)` and sentinel `errEnterpriseCreditsUnsupported` used only for HTTP 404 fallback。

- [ ] **Step 1: Write failing strict parser and selection tests**

```go
package main

import (
    "errors"
    "testing"
)

func TestParseEnterpriseCreditsStrict(t *testing.T) {
    got, err := parseEnterpriseCredits([]byte(`{"code":0,"data":{"credit":12.6,"limitNum":100,"cycleStartTime":"a","cycleEndTime":"b"}}`))
    if err != nil { t.Fatal(err) }
    if got.TotalUsed != 13 || got.TotalSize != 100 || got.TotalRemain != 87 || got.PackCount != 1 {
        t.Fatalf("credits = %#v", got)
    }
    bad := []string{
        `{"code":0,"data":{"credit":1}}`,
        `{"code":0,"data":{"credit":-1,"limitNum":100}}`,
        `{"code":0,"data":{"credit":"NaN","limitNum":100}}`,
        `{"code":0,"data":{"credit":1,"limitNum":0}}`,
        `{"code":7,"msg":"denied","data":{"credit":1,"limitNum":100}}`,
    }
    for _, raw := range bad {
        if _, err := parseEnterpriseCredits([]byte(raw)); err == nil {
            t.Errorf("accepted invalid enterprise response %s", raw)
        }
    }
}

func TestEnterpriseCreditsFallbackOnlyOn404(t *testing.T) {
    if !errors.Is(classifyEnterpriseHTTPStatus(404), errEnterpriseCreditsUnsupported) {
        t.Fatal("404 must allow personal fallback")
    }
    for _, status := range []int{401, 403, 429, 500} {
        if errors.Is(classifyEnterpriseHTTPStatus(status), errEnterpriseCreditsUnsupported) {
            t.Fatalf("status %d incorrectly allows fallback", status)
        }
    }
}

func TestReconcileSkipsLifecycleWhenCreditsRefreshFailed(t *testing.T) {
    if !creditsErrorsBlockLifecycle([]string{"checkin: x", "credits: upstream 500"}) {
        t.Fatal("credits error did not block lifecycle")
    }
    if creditsErrorsBlockLifecycle([]string{"checkin: x"}) {
        t.Fatal("unrelated error blocked lifecycle")
    }
}
```

Add httptest integration cases for enabled CN enterprise, disabled CN personal, Global personal, and 404 fallback. Assert enterprise and personal values are never added together.

- [ ] **Step 2: Run RED**

```powershell
go test ./workbuddy -run 'TestParseEnterpriseCredits|TestEnterpriseCreditsFallback|TestReconcileSkipsLifecycle|TestFetchUserResourceEnterprise' -count=1
```

Expected: missing helpers or personal endpoint called in enterprise case.

- [ ] **Step 3: Split personal fetch and add strict enterprise fetch**

Keep the semaphore in the outer `fetchUserResourceWithCallback`. Move current pagination body unchanged into:

```go
func fetchPersonalUserResourceWithCallback(sa *storedAuth, callbackID string) (*creditsSummary, error)
```

Add:

```go
var errEnterpriseCreditsUnsupported = errors.New("enterprise credits unsupported")

func fetchUserResourceWithCallback(sa *storedAuth, callbackID string) (*creditsSummary, error) {
    release := acquireUserResourceSlot()
    defer release()
    cfg := currentFeatureRuntime()
    if cfg != nil && cfg.enterpriseCredits && accountRegion(sa) == "cn" {
        credits, err := fetchEnterpriseCreditsCN(sa, callbackID)
        if err == nil { return credits, nil }
        if !errors.Is(err, errEnterpriseCreditsUnsupported) { return nil, err }
    }
    return fetchPersonalUserResourceWithCallback(sa, callbackID)
}
```

Build `POST billingBaseFor(sa)+"/billing/meter/get-enterprise-user-usage"`, body `{}`, strict 2xx status, and headers from the spec. `X-Tenant-Id` repeats non-empty enterprise ID. Parse `apiEnvelope.Data` as `map[string]json.RawMessage`, require `credit` and `limitNum`, accept JSON number or numeric string, reject NaN/Inf/negative, use `math.Round`, require rounded or raw limit greater than zero, and clamp remain at zero.

Return a one-entry package named `Enterprise` with cycle fields if present.

- [ ] **Step 4: Add lifecycle error guard**

```go
func creditsErrorsBlockLifecycle(errs []string) bool {
    for _, message := range errs {
        if strings.HasPrefix(message, "credits:") {
            return true
        }
    }
    return false
}
```

In `reconcileOneAccount`, capture `errs` from `cachedAccountDetails`; when `creditsErrorsBlockLifecycle(errs)` is true, return `lifecycleNone, nil` before any disable/delete/reenable decision. Panel still receives stale values plus error text.

- [ ] **Step 5: Run GREEN and billing regressions**

```powershell
go test ./workbuddy -run 'TestParseEnterpriseCredits|TestEnterpriseCreditsFallback|TestReconcileSkipsLifecycle|TestFetchUserResourceEnterprise|TestPackageRemainUsed|TestIsCreditsExhausted' -count=1
go test -race ./workbuddy -run 'TestFetchUserResourceEnterprise|TestFetchUserResourceConcurrencyLimit' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```powershell
git add -- workbuddy/billing.go workbuddy/cache.go workbuddy/lifecycle.go workbuddy/enterprise_test.go
git commit -m "feat(workbuddy): probe strict CN enterprise credits"
```

---

### Task 11: Panel multi-file import, search and remain sorting

**Files:**
- Modify: `workbuddy/panel.html`
- Modify: `workbuddy/panel_features_test.go`

**Interfaces:**
- Consumes: existing `api("/import")`, `lastAccounts`, region filter and card renderer。
- Produces: browser-only sequential multi-file import, combined search/filter and three-state remain sort。

- [ ] **Step 1: Write failing panel contract tests**

Append:

```go
func TestPanelSupportsBoundedSequentialFileImportSearchAndSort(t *testing.T) {
    html := string(panelHTML)
    required := []string{
        `type="file"`, `multiple`, `accept=".json,application/json"`,
        `const MAX_IMPORT_FILE_BYTES=2*1024*1024`,
        `for(const item of imports)`, `await importOneCredential(item.raw)`,
        `id="accountSearch"`, `function accountsForView(`,
        `nickname`, `label`, `name`, `uid`,
        `sortDirection`, `original`, `desc`, `asc`,
    }
    for _, want := range required {
        if !strings.Contains(html, want) { t.Errorf("panel missing %q", want) }
    }
    if strings.Contains(html, "localStorage.setItem(\"workbuddy-import")") {
        t.Fatal("credential import persisted in localStorage")
    }
}
```

- [ ] **Step 2: Run RED**

```powershell
go test ./workbuddy -run 'TestPanelSupportsBoundedSequentialFileImportSearchAndSort' -count=1
```

Expected: required UI/functions absent.

- [ ] **Step 3: Add native file input and serial importer**

Add:

```html
<input id="importFiles" type="file" multiple accept=".json,application/json">
```

Use:

```javascript
const MAX_IMPORT_FILE_BYTES=2*1024*1024;
async function importOneCredential(raw){
  let payload={raw};
  try{payload={json:JSON.parse(raw)}}catch(_){}
  return api("/import",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(payload)});
}
async function importAuth(btn){
  const imports=[];
  const text=(document.getElementById("importRaw").value||"").trim();
  if(text)imports.push({name:"粘贴内容",raw:text});
  for(const file of Array.from(document.getElementById("importFiles").files||[])){
    if(file.size>MAX_IMPORT_FILE_BYTES){imports.push({name:file.name,error:"超过 2 MiB"});continue}
    imports.push({name:file.name,raw:await file.text()});
  }
  if(!imports.length){toast("请选择文件或粘贴凭证 JSON","warn");return}
  let ok=0,fail=0;
  for(const item of imports){
    if(item.error){fail++;continue}
    try{const result=await importOneCredential(item.raw);if(result.success)ok++;else fail++}catch(_){fail++}
  }
  document.getElementById("importRaw").value="";
  document.getElementById("importFiles").value="";
  toast("导入完成",fail?"warn":"ok",ok+" 成功 / "+fail+" 失败");
  if(ok)await load(true);
}
```

Do not place `raw`, parsed credential or file contents in toast, DOM attributes, URL or storage.

- [ ] **Step 4: Add combined search and sorting**

Add states:

```javascript
let accountSearch="";
let sortDirection="original";
```

`accountsForView(accounts)` must first apply current region/exhausted filter, then case-insensitive search over `[nickname,label,name,uid]`, then sort a copy when direction is `desc` or `asc` using `(account.credits&&account.credits.total_remain)||0`. Preserve original order with `accounts.slice()` when state is `original`.

The sort button cycles:

```javascript
sortDirection=sortDirection==="original"?"desc":(sortDirection==="desc"?"asc":"original");
```

Every path that re-renders cards uses `accountsForView(lastAccounts)`. Summary totals continue to use the region-filtered account set, not the search result, unless the current implementation already ties summary to card filter; do not silently change credit totals.

- [ ] **Step 5: Run GREEN**

```powershell
go test ./workbuddy -run 'TestPanelSupportsBoundedSequentialFileImportSearchAndSort|TestPanelEditsDesensitize|TestPanelShowsAndRefreshesEgressIP' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```powershell
git add -- workbuddy/panel.html workbuddy/panel_features_test.go
git commit -m "feat(workbuddy): improve account panel workflows"
```

---

### Task 12: Full verification and Linux amd64 build

**Files:**
- Modify only if verification finds a regression caused by Tasks 1 through 11: the directly responsible task files and their tests。
- Create build artifact: `workbuddy/workbuddy_0.8.7_fork_fusion_linux_amd64.so`。

**Interfaces:**
- Consumes all preceding tasks。
- Produces a tested Windows development tree and verified Linux amd64 CPA shared object。

- [ ] **Step 1: Check protected working-tree state before verification**

```powershell
git status --short
```

Expected: `LOOP.md` and the pre-existing 2026-08-28 docs may remain modified/untracked exactly as before; no implementation commit contains them. The new 2026-08-29 spec/plan and task commits are tracked.

- [ ] **Step 2: Run focused feature suite**

```powershell
go test ./workbuddy -run 'Test(DefaultDesensitize|ParseFeatureRuntime|DesensitizeMatcher|PrepareUpstreamBodyDesensitize|ManagementReturnsEffectiveDesensitize|ToAuthDataForLoginPoll|DecodeBufferedHostHTTPResponse|InboundRPCWrappers|NativeStreamError|TypedErrorEnvelope|HardCreditErrorRecognizes|FetchUserResourceConcurrency|WorkBuddyProfile|ParseEnterpriseCredits|PanelSupports)' -count=1
```

Expected: PASS.

- [ ] **Step 3: Run all tests**

```powershell
go test ./... -count=1
```

Expected: PASS with no package failures.

- [ ] **Step 4: Run race detector**

```powershell
go test -race ./... -count=1
```

Expected: PASS with no race report.

- [ ] **Step 5: Run vet**

```powershell
go vet ./...
```

Expected: exit code 0 and no diagnostics.

- [ ] **Step 6: Build without overwriting the validated 0.8.7 artifact**

From repository root, verify the existing target before writing the new path:

```powershell
Test-Path "workbuddy/workbuddy_0.8.7_linux_amd64.so"
Test-Path "workbuddy/workbuddy_0.8.7_fork_fusion_linux_amd64.so"
```

Expected: the first is `True`. If the second already exists and was not created in this plan, stop rather than overwrite it. Otherwise build with the installed Zig command confirmed by `Get-Command zig`:

```powershell
$env:GOOS="linux"
$env:GOARCH="amd64"
$env:CGO_ENABLED="1"
$env:CC="zig cc -target x86_64-linux-gnu"
$env:CXX="zig c++ -target x86_64-linux-gnu"
go build -buildmode=c-shared -ldflags "-s -w -X main.version=0.8.7" -o "workbuddy/workbuddy_0.8.7_fork_fusion_linux_amd64.so" ./workbuddy
```

Expected: exit code 0 and the new file exists; the old file timestamp and hash remain unchanged.

- [ ] **Step 7: Verify binary format, exports and hash**

Use available local tools:

```powershell
& "C:\Program Files\Git\usr\bin\file.exe" "workbuddy/workbuddy_0.8.7_fork_fusion_linux_amd64.so"
zig objdump -p "workbuddy/workbuddy_0.8.7_fork_fusion_linux_amd64.so"
zig nm -D "workbuddy/workbuddy_0.8.7_fork_fusion_linux_amd64.so" | Select-String "cliproxy_plugin_init|cliproxyPluginCall|cliproxyPluginFree|cliproxyPluginShutdown"
Get-FileHash -Algorithm SHA256 "workbuddy/workbuddy_0.8.7_fork_fusion_linux_amd64.so"
Get-FileHash -Algorithm SHA256 "workbuddy/workbuddy_0.8.7_linux_amd64.so"
```

Expected: ELF 64-bit LSB shared object, x86-64, DYN; all four CPA ABI symbols appear; hashes are printed and differ because the binaries contain different code.

- [ ] **Step 8: Commit only verification fixes, never the build artifact unless already tracked by repository policy**

If no fixes were needed, do not create an empty commit. If fixes were needed, first add a failing regression test, rerun RED/GREEN, then commit only the responsible source and test files:

```powershell
git add -- workbuddy/<responsible-file>.go workbuddy/<responsible-test>_test.go
git commit -m "fix(workbuddy): resolve fusion verification regression"
```

Do not push or publish.
