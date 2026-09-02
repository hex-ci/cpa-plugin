# WorkBuddy Fork Fusion Design

## 目标

在当前 WorkBuddy 0.8.7 基线上，加入可编辑、持久化且默认关闭的屏蔽词混淆，并迁移四个指定 fork 中已确认与 WorkBuddy 直接相关、当前尚未实现、不会重复 CPA host 能力的功能和修复。

本次只修改 `workbuddy`，不引入动态模型列表或动态 model metadata，不修改 `qoderwork`，不推送、不发布，也不覆盖已验证的 `workbuddy/workbuddy_0.8.7_linux_amd64.so`。

## 调查结论

已审计以下仓库的 branches、tags、releases、历史 commits、源码、tests 和 workflows：

- [luode0320/cpa-workbuddy-plugin](https://github.com/luode0320/cpa-workbuddy-plugin)
- [AllenReder/cpa-plugin](https://github.com/AllenReder/cpa-plugin)
- [hijakke/cpa-plugin](https://github.com/hijakke/cpa-plugin)
- [hurleychin/cpa-plugin](https://github.com/hurleychin/cpa-plugin)

四个 fork 中没有发现当前插件尚未实现的新签到 endpoint、活动状态 endpoint 或 trial claim endpoint。当前插件已有 CN `checkin-activity-status` 到 `checkin-status` fallback、每日签到、Global trial、个人资源包分页、生命周期处理、per-account lock、proxy-aware OAuth、redirect 拒绝、严格 account lookup、静态模型、固定 sanitizer 和基础客户端 headers。

可迁移或重写的内容如下：

| 来源或问题 | 采用方式 |
|---|---|
| `ShouZhuo0413/codebuddy2api/desensitize.py` | 重写为可配置、幂等的 U+200B 混淆器，保留其完整 83 项词表并加入 `Codex`、`codex` |
| OAuth poll 返回 UID `AuthData.ID` | poll success 定点返回空 ID，让 CPA 使用 path-derived identity |
| buffered `host.http.do` 的 `StatusCode` / `status_code` wire 差异 | 同时兼容 PascalCase 和 snake_case |
| inbound `host_callback_id` 被本地 struct 丢弃 | 在 executor、refresh、management wrapper 中保留并传给 host HTTP wire |
| stream error 被当作普通 payload | 使用 CPA 原生 `error` wire 生成 `ExecutorStreamChunk.Err` |
| sync executor 上游状态丢失 | 返回带 `http_status` 的 typed plugin envelope error |
| 中文硬额度提示不完整 | 加入 `额度已用尽` |
| 多账号 billing 峰值无界 | 在共享 `fetchUserResource` 边界设置容量 4 的 semaphore |
| WorkBuddy desktop OAuth request profile | 作为显式 opt-in，默认继续使用当前 CLI profile |
| 历史 enterprise credits endpoint | 作为默认关闭、CN-only、严格校验的 capability probe |
| panel 多文件导入、搜索和积分排序 | 复用现有 `/import` 和前端状态，不增加 credential backend |

## 明确排除

以下内容不实现：

- 动态模型列表、动态 model metadata。
- plugin 内 credential retry、account failover、anomaly pool。
- 持久 `disabled:true`、`rate_limit_until` 或账号级限流停放。
- plugin session routing。CPA 已有 `routing.session-affinity`。
- preserve watchdog、私有 counters、session trace JSONL、usage feed NDJSON。
- raw credential export。
- fork 中的账号删除实现。它可能误删 UID 不一致的 legacy `workbuddy.json`。
- broad chat impersonation headers。没有可信 chat capture 支持替换当前 headers。
- Stainless Linux/node 伪装。
- tool-call 损坏后自动重放 POST 或 fake SSE。
- prompt compact、runtime block pruning、tool metadata 删除。
- content-filter 自动重试。
- Enterprise API 整体替换 personal billing。
- Global enterprise endpoint。当前没有可信 fixture。
- async first-verdict。CPA v7.2.30 会让异步 executor 的 callback context 存活到 stream 结束；native stream error 已能让 bootstrap 识别失败。精确 async 429 status 需要 host ABI 支持，不在 plugin 内引入 Windows 上不稳的同步等待。

## 1. 可配置屏蔽词混淆

### 配置字段

注册以下字段：

- `desensitize`：boolean，默认 `false`。
- `desensitize_terms`：array。配置缺失或 JSON PATCH 值为 `null` 时使用内置默认词表；显式 `[]` 表示合法空词表。
- `oauth_client_mode`：enum，`cli` 或 `workbuddy`，默认 `cli`。
- `enterprise_credits`：boolean，默认 `false`。

`configure` 使用 `gopkg.in/yaml.v3` 读取 `desensitize_terms`，不能按行解析数组。配置解析、词表校验和 matcher 编译全部成功后，才一次发布新的 immutable runtime snapshot。屏蔽词配置失败时保留上一个可用 snapshot；现有非法 `proxy-url` 的 fail-closed 语义不变。

词表处理规则：

- 去掉每项首尾空白，丢弃空项。
- exact duplicate 只保留第一次出现的位置。
- effective terms 保留大小写不同的项，因此默认列表仍显示 `Codex` 和 `codex`。
- matcher 编译时按 case-insensitive key 去重。
- 少于 2 个 Unicode rune 的词返回配置错误。
- 含 U+200B 的自定义词返回配置错误，避免用户输入制造不收敛匹配。
- 配置 snapshot 对外返回副本，调用者不能修改内部 slice。

### 默认词表

默认词表共 85 项：

```plaintext
DoS
DDoS
exploit
credential testing
credential stuffing
supply chain compromise
supply-chain compromise
detection evasion
C2 frameworks
C2 framework
command and control
malicious purposes
malicious intent
mass targeting
brute force
brute-force
privilege escalation
reverse shell
remote code execution
SQL injection
XSS
CSRF
phishing
malware
ransomware
keylogger
rootkit
backdoor
botnet
zero-day
0day
vulnerability
vulnerabilities
red teaming
red-teaming
sandbox
sandboxing
sandboxed
unsandboxed
escalated privileges
escalated
escalation
destructive action
destructive command
destructive
attack
attacks
cybersecurity
security review
exploit development
hacking
penetration testing
penetration test
injection
weaponize
weaponized
harmful
dangerous
abuse
abusive
illegal
terrorist
terrorism
bomb
weapon
weapons
drug
drugs
narcotic
suicide
self-harm
murder
kill
violence
violent
Claude Code
Claude Opus
Claude Sonnet
Claude Haiku
Claude Fable
Anthropic
Co-Authored-By
noreply@anthropic.com
Codex
codex
```

### Matcher

匹配语义：

- literal、case-insensitive substring，不使用 word boundary。
- `regexp.QuoteMeta` 处理自定义词中的正则字符。
- 先按 Unicode rune 长度降序排列，同长度保持配置中的原顺序。
- 每个 match 在实际匹配文本的第一个 Unicode rune 后插入 U+200B ZERO WIDTH SPACE，保留原始大小写。
- 在一次调用内重复匹配，直到没有连续命中。
- 不做 Unicode normalization，不删除已有 U+200B。
- 输出必须幂等，再次调用结果不变。

固定 fixture：

```plaintext
EXPLOIT -> E​XPLOIT
skill -> sk​ill
attacker -> a​ttacker
DDoS -> D​D​oS
noreply@anthropic.com -> n​oreply@a​nthropic.com
abcd with terms abc,bcd -> a​b​cd
Codex -> C​odex
codex -> c​odex
```

### Payload 范围

混淆继续复用 `prepareUpstreamBody` 的一次 JSON decode/encode。执行顺序：

```plaintext
force stream
normalize tools
rewrite canonical model
run existing fixed sanitizer and reasoning policy
run configurable desensitize walker
ensure Global system message
marshal
```

现有 `sanitizeBlockedTemplates` 必须先执行，避免通用 matcher 先改写 `Claude Code` 或 `Anthropic` 后破坏已有 exact rewrite。

只处理以下内容：

- `system` 和 `developer` message 的 string content。
- `content` array 中 `type == "text"` 的 `text`。
- 普通 `user` message 仅在同一 message 的 text blocks 拼接后包含下列精确、区分大小写的 marker 时处理：

```plaintext
# AGENTS.md instructions
<environment_context>
<permissions instructions>
<collaboration_mode>
<skills_instructions>
<system-reminder>
# claudeMd
```

- top-level `tools` 子树中，递归处理 key 精确为 `description` 或 `title` 的 string。

不处理普通 user content、assistant content、tool result、tool name、arguments、schema key、enum、default、example、image 或 audio。保留所有 tool metadata。

## 2. 配置和 panel

### Runtime 状态 API

增加 plugin management `GET /plugins/workbuddy/desensitize`，只返回：

```json
{
  "enabled": false,
  "terms": ["DoS"],
  "source": "default"
}
```

`source` 只能是 `default` 或 `custom`。该 endpoint 返回 effective runtime state，不写配置。

panel 保存设置时调用 CPA 已有 generic config API：

- `GET /v0/management/plugins/workbuddy/config`
- `PATCH /v0/management/plugins/workbuddy/config`

不直接编辑 `config.yaml`，不增加自定义 config write endpoint。

保存语义：

- 普通保存：PATCH `desensitize` 和 `desensitize_terms`。
- 恢复默认词表：PATCH `{"desensitize_terms":null}`。
- 恢复全部默认：PATCH `{"desensitize":false,"desensitize_terms":null}`。
- textarea 一行一个词。

### Panel 便利功能

在现有单文件 HTML 中加入：

- 屏蔽词设置 modal：checkbox、词表 textarea、保存、恢复默认词表、恢复全部默认、default/custom 来源显示。
- 原生 `<input type="file" multiple accept=".json,application/json">`。
- 单文件最大 2 MiB，浏览器内存中逐个读取并串行调用现有 `/import`。
- textarea 和文件都为空时拒绝提交；每个文件独立记录成功或失败，全部结束后显示汇总。
- 不把 credential 写入 `localStorage`、URL、DOM data attribute 或日志。
- 搜索 nickname、label、name/file name 和 UID，大小写不敏感。
- 搜索与现有 CN、Global、耗尽 filter 组合。
- remain 排序三态：原始顺序 -> 降序 -> 升序 -> 原始顺序。
- 排序只作用于当前过滤结果，不修改 `lastAccounts`。

## 3. OAuth identity 和 desktop profile

### Path-derived identity

只修改 `handlePollLogin` 的 success response：

- `FileName` 仍是 canonical `workbuddy-<uid>.json`。
- `AuthData.ID` 设为空，让 host 用保存路径生成唯一 identity。

不修改 generic `toAuthData`。import 仍需要 UID ID 和 canonical `FileName`；refresh 已继续返回空 ID 和空 `FileName`。

### `oauth_client_mode=workbuddy`

默认 `cli` profile 保持当前行为。显式选择 `workbuddy` 时，仅 OAuth state、token poll、account lookup 和 token refresh 使用 desktop profile，不替换 chat headers。

Desktop profile：

```plaintext
POST https://copilot.tencent.com/v2/plugin/auth/state?platform=workbuddy
Body: {}
User-Agent: WorkBuddy/5.3.14 WorkBuddy/5.3.14 CLI/2.115.0
Origin: https://www.workbuddy.cn
Referer: https://www.workbuddy.cn/
X-No-Authorization: true
X-No-User-Id: true
X-No-Enterprise-Id: true
X-No-Department-Info: true
```

返回的 browser URL 保留原 query，并增加：

```plaintext
version=5.3.14
loginSessionId=<32 lowercase hex>
```

`loginCtx` 保存启动时的 profile 和 `loginSessionId`，poll 不能读取可能已变化的全局配置。token 成功后仍严格执行 account lookup，仍要求 UID，仍复用隔离 cookie jar、proxy snapshot、redirect 拒绝和 5 分钟 TTL。

Refresh endpoint 不变：

```plaintext
POST {realm}/v2/plugin/auth/token/refresh
X-Refresh-Token: <refreshToken>
X-Auth-Refresh-Source: plugin
```

Desktop mode 只改变该请求的 client-identifying headers。refresh 仍通过 host HTTP bridge，不能泄漏 refresh token 到 chat。

## 4. Host HTTP bridge 和 callback context

### Buffered response casing

`host.http.do` 当前 host 直接 marshal `pluginapi.HTTPResponse`，因此 v7.2.30 的 status key 是 `StatusCode`；streaming wire 使用 `status_code`。本地 decoder 同时接受：

```json
{"StatusCode":201,"Headers":{},"Body":"..."}
```

和：

```json
{"status_code":201,"headers":{},"body":"..."}
```

只增加双 status 字段兼容，不复制多余的 PascalCase headers/body 字段，因为 Go JSON 已对纯大小写差异做不区分匹配。

### Callback propagation

本地 host HTTP wire 顶层增加：

```go
HostCallbackID string `json:"host_callback_id,omitempty"`
```

保留无 callback 的现有 helper，并增加显式 callback 入口；不能使用 package global callback ID，因为 plugin RPC 可并发。

保留 wrapper 的入口：

- `executor.execute`
- `executor.execute_stream`
- `executor.http_request`
- `auth.refresh`
- `management.handle`

这些同步 call graph 中产生的 host HTTP 请求传入 wrapper 的 callback ID。scheduler、detached usage、reconcile、keepalive scheduler 等没有有效 inbound callback scope，继续传空 ID。

CPA v7.2.30 在 async `ExecuteStream` 中把 callback context 保留到 stream channel 结束。因此 async handler 可以在 goroutine 中用该 ID 打开 host HTTP stream，并继承原 inbound cancellation、deadline 和 request-scoped logging context。

限制：显式 plugin proxy 直接使用 plugin `http.Client`，opaque callback ID 只能由 host registry 解析。此次不修改 CPA host/SDK，因此显式 proxy 仍只能依赖 `http.Request` 自身 context 和现有 client timeout；不能声称具备 host callback 恢复能力。Windows inherited buffered call 仍保留当前 direct fallback，避免 nested callback stack pointer 问题。

## 5. Native stream errors 和 typed sync errors

### Native stream errors

`streamEmitError` 改为调用 `host.stream.emit` 的 native error wire：

```json
{"stream_id":"...","error":"redacted message"}
```

不再发送普通 payload：

```json
{"error":{"message":"..."}}
```

所有 message 先经过 `redactSecrets`。错误分支随后关闭 stream，host 收到的 chunk 必须满足 `ExecutorStreamChunk.Err != nil`。

行为约束：

- transport、HTTP、empty stream、read error 在输出前后都用 native Err。
- 首个正常 chunk 只有在 `streamEmit` 成功后才算成功。
- 首个 chunk 后 read error 发送 native Err，host 标记失败但不重放已开始的 POST。
- emit 失败说明 client stream 已关闭，不再尝试第二次 error emit。
- stream close exactly once。
- 保留 `[DONE]` break、`json.Valid` guard 和 empty valid-event 检查。

### Sync typed status

`envelopeError` 增加：

```go
HTTPStatus int `json:"http_status,omitempty"`
```

增加 status-aware error envelope helper。同步 `executor.execute` 和无 async `StreamID` 的 `executor.execute_stream` 在上游 HTTP status >= 400 时返回 code `http_error`、redacted message 和精确 `http_status`。transport 或 parser error 不伪造 status。

不依赖 `Retryable`。CPA v7.2.30 的当前调用路径不使用该字段作 credential decision。

## 6. Credits 正确性和并发

### 中文 hard-credit marker

在现有 `hardCreditMarkers` 中只增加：

```plaintext
额度已用尽
```

不把所有 429 或 business code 14018 无条件判为 hard credit。现有 lifecycle 仍需重新读取 billing，Global 仍执行二次确认。

### Billing 并发上限

在共享 `fetchUserResource` 边界使用容量 4 的 process-wide semaphore。slot 覆盖整个分页和 transient retry 周期，并保证所有 return 路径释放。

该限制覆盖 panel lazy credits、force refresh、reconcile 和其他直接调用者。保留现有 per-auth `accountDetailFlight`，不新增 refresh runner 或第二套 singleflight。

### CN enterprise credits probe

仅当 `enterprise_credits=true` 且账号为 CN 时，先请求：

```plaintext
POST https://www.codebuddy.cn/billing/meter/get-enterprise-user-usage
Body: {}
```

请求保留当前 Authorization 和 account headers，并增加或确认：

```plaintext
Accept: application/json, text/plain, */*
Content-Type: application/json
X-Client-Platform: web
X-User-Id: <uid>                    optional
X-Enterprise-Id: <enterprise>       optional
X-Tenant-Id: <enterprise>           optional
X-Domain: <domain>                  optional
Origin: https://www.codebuddy.cn
Referer: https://www.codebuddy.cn/
```

只有以下条件全部满足时才接受 enterprise snapshot：

- HTTP 2xx。
- business `code == 0`。
- `credit` 和 `limitNum` 均明确存在。
- 两者可解析为有限、非负数字。
- 首版要求 `limitNum > 0`。

映射：

```plaintext
used = round(credit)
size = round(limitNum)
remain = max(size - used, 0)
```

有效 enterprise snapshot 优先，不能与 personal packages 相加。只在明确 HTTP 404 或 fixture 覆盖的 unsupported business code 时 fallback 到现有 personal `get-user-resource`。401、403、5xx、transport error、unknown business error、字段缺失或非法值返回 error，由现有 stale-while-error cache 继续向 panel 展示旧值。

`reconcileOneAccount` 必须检查 `cachedAccountDetails` 返回的 `credits:` error。只要本轮 credits fetch 失败，即使 stale cache 中有旧 snapshot，也返回 `lifecycleNone`，不能 disable、delete 或 re-enable。Global 永远继续 personal billing。

## 7. 测试和交付

每项严格按 RED -> GREEN -> refactor 执行。生产代码前必须运行对应新测试并确认因缺失行为失败。

最低覆盖：

- 默认 85 项词表、缺失/custom/empty/null 配置语义、非法短词、并发 reconfigure。
- literal special chars、case-insensitive、overlap、nested term、已有 U+200B 和幂等性。
- payload message/tool walker 正负范围。
- panel generic config calls、设置恢复、多文件 2 MiB 限制、串行导入、搜索和排序。
- OAuth path-derived ID、CLI profile不变、desktop request method/URL/headers/query/cookie continuity。
- buffered PascalCase 和 snake_case status fixtures。
- callback ID 位于 host HTTP wire 顶层，并从每个 inbound wrapper 传播。
- native stream error wire、secret redaction、emit failure、mid-stream read error、empty stream、close once。
- sync typed `http_status`。
- `额度已用尽`。
- 10、20、50 个账号场景的 billing in-flight 峰值不超过 4，并通过 race detector。
- enterprise parser、strict failure、fallback allowlist、Global bypass 和 stale lifecycle guard。

最终命令：

```powershell
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

Linux amd64 CGO 编译使用已安装的 Zig toolchain，输出不同文件名，例如：

```plaintext
workbuddy/workbuddy_0.8.7_fork_fusion_linux_amd64.so
```

验证：

- ELF64。
- x86-64。
- DYN shared object。
- CPA ABI exports。
- SHA-256。

不 bump `VERSION` 或 `registry.json`，不 push，不发布。
