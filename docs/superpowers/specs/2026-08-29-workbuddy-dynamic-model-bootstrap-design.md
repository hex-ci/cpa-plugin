# WorkBuddy Dynamic Model Bootstrap, Metadata, Persistence, and Upstream Fusion Design

**日期：** 2026-08-29

## 目标

在当前 WorkBuddy 0.9.0 插件中恢复账号级动态模型发现，并增加 models.dev metadata enrichment 与跨进程持久化，同时满足以下约束：

1. production 静态代码只保留一个 `auto` fallback model 和一个 default metadata template，不保留其他特定模型 ID、列表、metadata 或模型专属 reasoning policy。
2. 对某个 auth 第一次执行模型初始化且本地没有有效 cache 时，WorkBuddy 模型列表与 models.dev metadata 必须全部成功获取、校验并持久化，该 auth 才进入正常推理状态。
3. 本地已有两类有效 last-good cache 时，每次进程启动仍尝试刷新；刷新失败的来源继续使用自己的 last-good，该 auth 以 `stale` 状态正常工作；刷新成功的来源原子替换自己的旧 cache。
4. 缺少某类有效 cache，且该类 fresh refresh 因任意 transport、HTTP、业务、schema、校验或持久化错误失败时，该 auth 必须 fail closed。panel 保持可访问并显示脱敏错误。
5. WorkBuddy 模型列表和 models.dev metadata 都必须持久化。
6. 动态状态按 auth identity 隔离，不允许 CN、Global、Enterprise 或不同账号共享 WorkBuddy catalog。
7. 同时实现五仓审计确认值得迁移的 management/panel 修复，不重复当前插件、CPA host 或独立 `workbuddy2api` 服务已有能力。
8. 全过程使用逐 cycle RED -> GREEN -> refactor 的 TDD。

## 术语与启动语义

本文中的“插件启动”受 CLIProxyAPI v7.2.30 plugin ABI 限制，不表示 `plugin.register` 执行远程请求。

- `plugin.register` 和 `plugin.reconfigure` 没有 auth `StorageJSON`，不能正确执行账号级模型发现。
- `model.for_auth` 是第一个同时具备 auth data 和 `host_callback_id` 的 RPC，因此它是某个 auth 的 authenticated bootstrap 边界。
- `plugin.register` 必须始终保留 management capability 和 panel route。模型初始化失败不能让插件 registration 失败，否则 panel 也会消失，与错误展示要求冲突。
- “正常工作”指模型注册、scheduler 选择和三个 executor 路径。OAuth、auth import、refresh、panel、check-in、billing repair 和 keepalive 必须继续可用，以便用户修复 auth 或查看状态。

## 已确认基线

### 当前插件

- 仓库：`C:\Users\user\Downloads\cpa-wb\cpa-plugin`
- 设计基线：`2deb7537aaa55cac130e451227de7036875467af`，tag `workbuddy-v0.9.0`
- 当前 `workbuddy/models.go` 编译了 17 个特定模型；`model.static` 与 `model.for_auth` 返回同一列表。
- WorkBuddy 声明 `ExecutorModelScopeOAuth`。正常 CPA 集成会跳过 static registration，因此 `model.static` 中的 `auto` 只是静态 fallback contract，不是 fail-closed 的绕过路径。
- CPA 在 provider 解析前处理请求模型 `auto`。插件不得增加自己的 `auto` 选模算法。
- 当前 `model.for_auth` 忽略 wire 中的 `host_callback_id`，动态实现必须保留该字段并传入现有 host HTTP bridge。

### 上游与 fork

调查并固定以下来源：

- Sliverkiss origin 的动态实现：`f2f1b77d123086ffd5be751a3cea88ab5314e5d3`
- `luode0320/cpa-workbuddy-plugin`：`694734ed36df14cf57e93b32fda9b6ea45bb84d4`
- `AllenReder/cpa-plugin`：`12ea05bb1cd218a06475d4bf69fbec70f37fbc60`
- `hurleychin/cpa-plugin`：`5adf18b14e66f4c4e57fb4d8ac5b9a69325943ab`
- `hijakke/cpa-plugin`：当前 GitHub 404，只能保留 2026-08-28 历史快照 `27785d180ef3c902674da854d07ef4d1c317fdb3`，不能据此推断后续变化。
- Sliverkiss PR #5：`65170a4678834feca6d9666900885bee7cc97a52`
- `workbuddy2api`：已从 `c1a14cb` fast-forward 到 `44d2245`
- 持久化参考：[cpa-plugin-codex-quota-scheduler commit ebf4071](https://github.com/JefferyZhang2019/cpa-plugin-codex-quota-scheduler/commit/ebf40711099fb193757c3f70d9e4f0d1387a3133)
- models.dev schema 调查基线：[models.dev commit ef75469](https://github.com/anomalyco/models.dev/tree/ef75469d66e46854f8b86ea13d19fb0ffe0f73f0)

三个可读 fork 与 PR #5 的 dynamic cache 都是 process-global，且在解析当前 auth 前读取，会跨账号、realm 或 enterprise 复用首个 catalog。不得移植该结构。

`workbuddy2api c1a14cb..44d2245` 的 pool、单请求内部换号、`state.json` flush、04:00 cooldown、运维 endpoints 和 stdout table logger 都属于独立 HTTP service。当前插件已有 host-managed auth、sticky routing、lifecycle、usage 和 request logging，且新增 logger 存在截断 SSE 错误被吞、日志注入和 TTFB 误计问题。本任务不移植这些内容。

PR #5 作为整体 adoption unit 被拒绝：Windows workaround 已存在；Kimi ID 缺少 authenticated WorkBuddy fixture；双区域 OAuth 让 region 在 poll 阶段再次由客户端提供，并让 token 经 browser 往返；global cache 会跨账号污染。

## 方案选择

### 采用：per-auth authenticated bootstrap

`plugin.register` 只注册 capability、配置和 panel。每个 auth 的首个 `model.for_auth` 负责加载 cache、尝试 refresh、发布 readiness 和返回模型。

该方案符合当前 ABI，不修改 CPA host，并允许模型失败时继续显示 panel。

### 不采用：registration 阶段全局门控

registration wire 没有 auth 或 callback context。失败会让 management capability 一起消失，多账号扫描还会重现最坏串行等待和 global cache 污染。

### 不采用：修改 Host ABI

Host startup hook、deadline wire、plugin state dir 和 registry resync callback 能提供更完整的进程级启动语义，但会把任务扩大到 CLIProxyAPI host、SDK、ABI、插件和部署版本协调。当前 plugin-only 任务不需要该范围。

## 组件与职责

### 1. Static fallback

production 只保留：

- 一个 `auto` `pluginapi.ModelInfo`。
- 一个 default metadata template。

`model.static` 返回 `auto`；正常 OAuth scope 下 host 不使用该 response。`model.for_auth` 绝不在初始化失败时返回 `auto`，否则会绕过 fail-closed。

Default metadata 只提供当前 plugin contract 能确定的通用字段：

- 动态 model ID。
- name，优先使用上游值，没有时使用 ID。
- `OwnedBy: "workbuddy"`。
- `SupportedGenerationMethods: ["chat"]`。

Default metadata 不猜测 context、input/output limit、modalities、reasoning、tool support、structured output、temperature 或 cost。

### 2. WorkBuddy catalog source

WorkBuddy 是账号 entitlement 和 serving model 字段的权威来源。

#### Endpoint 顺序

1. Realm 对应 base URL 的 `GET /v3/config`。
2. 只有 primary 明确返回 HTTP 404 或 405 时，回退 `GET /console/enterprises/personal/models`。

401、403、5xx、业务错误、empty、malformed JSON 或 schema 不符合不触发 legacy fallback。这些情况必须进入 cache fallback 或 failed，不能被另一个 endpoint 掩盖。

CN 与 Global 使用现有 JWT realm 判断、base URL、Origin、Referer、User-Agent 和 client headers。请求发送：

- `Authorization: Bearer <accessToken>`
- `Accept: application/json`
- realm 对应的 Origin 和 Referer
- 当前最小 WorkBuddy client identity headers

请求通过 `model.for_auth` wire 的 `host_callback_id` 使用现有 callback-aware HTTP helper，保持 CPA proxy route。Windows 或 explicit proxy 的 direct client继续使用已有 route policy。

Inherited host bridge wire 不传 request deadline。实现可以给 direct request 设置 timeout，但不得宣称插件能在所有 inherited route 上保证严格 overall timeout，也不得用泄漏 goroutine的 `select` 伪造 timeout。

#### Snapshot 校验

只发布名为 `cli` 的 agent 成员列表。支持两种已确认的 wire shape：

- `/v3/config` 的 string model IDs。
- legacy personal models 的 model object 列表和 `disabled` 标记。

有效 snapshot 必须：

- HTTP 与业务状态成功。
- `cli` agent存在。
- 结果非空。
- 每个 ID 非空、长度受限、唯一。
- disabled model不进入结果。
- 数值字段类型正确且非负；缺失保持 absent，不转换为猜测值。
- 整份 response 被视为 complete snapshot。partial、duplicate 或 invalid entry使本次 refresh失败，不覆盖 last-good。

Enterprise custom models endpoint目前只有 fork代码，没有 authenticated fixture。本任务不接入该 endpoint。后续拿到 CN/Global Enterprise完整 fixture后再设计，不从 fork guessed metadata直接移植。

### 3. models.dev metadata source

使用 [models.dev canonical model API](https://models.dev/models.json)。本任务不下载 4 MB 以上的 `api.json` 或 `catalog.json`，不导入其他 serving provider 的 price 或 limits。

请求规则：

- `Accept: application/json`
- 本地有 opaque ETag 时发送 `If-None-Match`
- `304 Not Modified` 且本地 metadata cache有效时算 refresh成功
- `200` 必须先完成 schema与内容校验，再替换 cache

Consumer parser只验证使用的字段并接受 additive unknown fields。可选值保持 absent，不把 missing转换为 false或 0。

#### 动态匹配

代码中不维护 WorkBuddy ID到 models.dev canonical ID 的显式映射表。

对每个 WorkBuddy raw ID：

1. 在 models.dev records中做完整 canonical ID、大小写敏感的精确匹配。
2. 没有完整匹配时，查找 canonical ID末段与 raw ID大小写敏感且完全相等的记录。
3. 末段匹配必须唯一；多候选或没有候选即视为 unmatched。
4. unmatched model使用 default metadata，不阻止整个 catalog发布。

WorkBuddy 明确返回的 serving字段优先于 models.dev。models.dev只补充缺失的 canonical facts，不决定账号 entitlement，不从另一个 provider借用 price、context或 output cap。

若 models.dev后续公开稳定的 provider-to-canonical join key，可以在不增加静态映射的前提下使用该动态字段；当前不预实现尚未合并的 `base_model` contract。

### 4. Persistent stores

两类 cache分离：

```plaintext
<os.UserConfigDir()>/CLIProxyAPI/workbuddy/model-catalog/
  metadata.json
  metadata.json.bak
  models/
    <identity-sha256>.json
    <identity-sha256>.json.bak
```

Linux root 默认路径是：

```plaintext
/root/.config/CLIProxyAPI/workbuddy/model-catalog/
```

不硬编码 Linux、Windows 或 macOS 路径。

#### Auth identity

Per-auth cache identity至少包含：

- provider。
- realm。
- UID。
- EnterpriseID。

字段不足时加入 `AuthID`。identity序列化后做 SHA-256，文件名只使用 hash。access token、refresh token和完整 `StorageJSON`不得进入 key、文件名、状态或日志。

#### `metadata.json` schema v1

保存：

- `schema_version`
- opaque `etag`
- `fetched_at`
- 归一化 canonical records

#### Per-auth models schema v1

保存：

- `schema_version`
- `identity_sha256`
- `realm`
- `fetched_at`
- 成功使用的 endpoint kind
- 校验后的 WorkBuddy models

不保存 alias、`oauth-excluded-models`、token、auth path、原始 response body或原始错误。

#### 写入语义

1. 在目标目录创建随机 temp。
2. 写入完整 JSON。
3. `Sync` temp file。
4. `Close` temp file。
5. 把当前 primary保留为 `.bak` last-good。
6. replace primary。
7. `Sync` parent directory。
8. 所有失败路径清理 temp。

Unix 新建目录和文件使用 `0700` 与 `0600`。这不是 Windows ACL保证。第一版明确只支持一个 CPA进程写该目录，不增加跨进程 file lock。

Primary损坏时尝试 `.bak`。两者均损坏、identity不匹配或 future schema时，该来源视为没有有效 cache；future schema不得被旧代码覆盖。

#### Refresh 与替换

- 每个进程只在第一次需要 models.dev metadata时尝试一次全局 refresh。失败后，如果没有有效 metadata cache，后续 auth可以再次尝试；不能用 `sync.Once` 永久锁死失败。
- 每个 auth在本进程第一次 `model.for_auth` 时尝试一次 WorkBuddy refresh。auth identity、token generation或 plugin config generation变化后可以重新尝试。
- Fresh response只有在校验与持久化全部成功后才成为该来源的 fresh snapshot。
- Fresh持久化失败时，有旧 cache就继续旧 cache并标记 `stale`；没有旧 cache就 `failed`。
- 不增加后台 refresh goroutine、五分钟 TTL、panel retry endpoint或 request-time catalog refresh。

## Readiness 状态机

Registration状态与 auth readiness分开。

### Registration

```plaintext
unregistered -> registered
```

模型 fetch、cache或持久化错误不得把 registered变回 unregistered。

### Per-auth readiness

```plaintext
not_started -> loading
loading -> ready    两项来源都使用本进程 fresh snapshot
loading -> stale    至少一项 refresh失败，但每个失败来源都有有效 last-good
loading -> failed   至少一项既无 fresh也无有效 last-good
```

状态 snapshot不可变并原子发布。panel、executor和scheduler只读 snapshot，不等待 init mutex、HTTP或磁盘。

同一 auth只有一个初始化请求；不同 auth可以并行。models.dev refresh全局 singleflight。所有返回的 slice、map及 `ModelInfo`子 slice必须复制或保持 immutable。

Config/auth generation与 identity都匹配时才允许发布结果。旧 generation请求晚到时，不能覆盖新 snapshot或错误。

## RPC 与执行门控

### `plugin.register` / `plugin.reconfigure`

- 解析 config。
- 安装 proxy与 feature runtime。
- 初始化本地 store路径与只读 cache状态。
- 不扫描 auth，不发认证网络请求。
- 始终声明 `ManagementAPI`、`ModelProvider`、`AuthProvider` 和 executors。

### `model.static`

- 返回 `auto` 与 default metadata。
- 不读取 per-auth cache。
- 不发网络请求。

### `model.for_auth`

1. 解码包含 `host_callback_id` 的完整 wire。
2. 解析并验证 auth identity与 access token。
3. 执行或等待该 auth bootstrap。
4. `ready` 或 `stale`：把 WorkBuddy list与 metadata enrichment合并成 `ModelInfo`。
5. 在每次响应时应用 alias和 `oauth-excluded-models`，这些配置结果不进入 persistent cache。
6. `failed`：返回 `ok:true`、`Provider:"workbuddy"`、空 `Models`，不返回 RPC error，也不回退静态特定模型。

空成功 response用于让 CPA在热重载时注销旧 registry client。冷启动无模型时，客户端可能先得到 CPA的 unknown-provider response；不能承诺所有失败都由插件返回503。

### Executor

以下三个入口在解析凭据和上游 HTTP前检查 auth readiness：

- `executor.execute`
- `executor.execute_stream`
- `executor.http_request`

`ready`、`stale`允许执行；`not_started`、`loading`、`failed`返回固定、脱敏、带 `http_status:503` 的 `not_ready` error envelope。该 guard处理热重载留下的旧路由与并发窗口。

### Scheduler

`scheduler.pick`只选择 `ready` 或 `stale` auth。没有候选时返回 `Handled:false`，把决定交还CPA host。不得在plugin内增加第二套账号轮转、weighted pool或request replay。

## Panel 状态

复用受management鉴权保护的 `/accounts` 与 `/refresh` response，增加一个统一的 `model_status`：

```json
{
  "state": "ready|stale|failed|loading|not_started",
  "message": "固定脱敏文案",
  "metadata_source": "fresh|cache|none",
  "metadata_fetched_at": "RFC3339 or empty",
  "auths": [
    {
      "auth_index": "panel已有安全标识",
      "state": "ready|stale|failed|loading|not_started",
      "model_source": "fresh|cache|none",
      "models_fetched_at": "RFC3339 or empty",
      "error_code": "固定枚举或空"
    }
  ]
}
```

固定 error code只表示分类，例如：

- `workbuddy_transport`
- `workbuddy_http`
- `workbuddy_schema`
- `models_dev_transport`
- `models_dev_http`
- `models_dev_schema`
- `cache_read`
- `cache_write`

不得返回 endpoint、query、access token、response body、host error原文或系统路径。

Panel增加默认 `hidden` 的常驻 banner：

- `role="status"`
- `aria-live="polite"`
- `aria-atomic="true"`
- 只用 `textContent`

`load()` 在处理 dashboard顶层 `d.error` 前更新 banner。模型失败不替换账号 grid，不使用短toast。现有顶层 `error`继续只表示 dashboard获取失败。

## 同批实现的 management 与 panel 修复

### 1. Management limiter root fix

当前plugin在验证key前让所有mutating request消耗burst=5 bucket，导致第六个合法import得到429。

修改为：

- 正确management key不消耗失败bucket。
- 错误key仍返回403，并按现有burst与refill规则进入429。
- 不改变bucket容量或refill。
- panel不增加7秒POST retry，不重放mutating request。

### 2. Panel response trust boundary

`api()`必须安全分类：

- empty body
- HTML/non-JSON
- malformed JSON
- non-2xx

用户可见错误只含安全的HTTP status与content-type分类。不得显示body preview、token、credential、`d.error`原文或`e.message`。

### 3. Credits排序

Descending严格为：

```plaintext
positive > real zero > unknown
```

保留：

```plaintext
original -> desc -> asc -> original
```

不得修改`lastAccounts`原顺序。

### 4. 「剩余（可用）」口径

Disabled或exhausted账号的正余额不计入panel的「剩余（可用）」。本任务不修改backend `summary.total_remain`兼容字段，不引入preserve/anomaly分类。

### 5. Partial import生命周期

- 1成功+1失败时modal保持打开。
- 保留安全filename级结果与generic分类。
- Batch结束后立即清空textarea与file input中的credential。
- 只有零失败时自动关闭。
- 不保留或展示raw failed credential、server body或内部错误。

### 6. BasePath与query key

- Served panel使用host注入的实际management BasePath，不写死`/v0/management`。
- `?key=`第一次读取后写入`sessionStorage`，再从URL移除。
- Key不得写入HTML正文、日志、`localStorage`或其他长期storage。

## 明确不实现

- 任何特定模型列表、metadata表或ID映射表。
- 模型专属`reasoning_effort`强制策略。
- WorkBuddy enterprise custom model endpoint，直到取得authenticated fixtures。
- PR #5的双区域panel OAuth、global model cache与Kimi static entry。
- `workbuddy2api` Pool、Top5 weighted routing、100ms LRU、04:00 cooldown、`state.json`、内部multi-account retry、`/healthz`、`/status`与stdout logger。
- Fork中的persistent `disabled:true + rate_limit_until`、sweeper、第二scheduler、session routing、failover/replay、anomaly/preserve pool、raw export、account delete、advanced impersonation headers、session IDs与JSONL trace。
- 后台model refresh、panel refresh endpoint或plugin -> host registry resync。
- 新第三方Go或frontend dependency。
- 多进程共享cache目录的file lock。
- Windows断电级atomic replacement或ACL保证。

## TDD顺序

每个编号是独立RED -> GREEN -> refactor cycle。先只添加当前cycle测试并运行，确认失败原因是目标行为缺失，再写最少production code。

1. **Baseline**：记录当前`go test ./...`、`go test -race ./...`、`go vet ./...`结果和工作树。
2. **Static contract**：测试production只含`auto`与default metadata；删除17项fixed models和模型特定reasoning policy。
3. **WorkBuddy parsers**：`/v3/config`、legacy objects、disabled、empty、duplicate、malformed、业务错误、404/405 fallback与非fallback状态。
4. **models.dev parser与matching**：additive fields、optional values、opaque ETag、完整ID、唯一末段、歧义、unmatched与default metadata。
5. **Persistent stores**：schema v1、identity hash、permissions、temp/Sync/replace、`.bak`、corrupt primary、future schema、secret absence和成功替换。
6. **Fresh bootstrap**：无cache时两项fetch、validate与persist全部成功才ready；任一步失败都failed。
7. **Stale bootstrap**：已有cache时仍refresh；单项失败使用该项last-good；成功项替换；组合状态为stale。
8. **Isolation与concurrency**：同auth 32个goroutine只发一次WorkBuddy请求；models.dev全局singleflight；不同auth/realm/enterprise不串数据；返回值不能修改snapshot；race通过。
9. **Generation race**：旧generation晚到不能覆盖新snapshot。
10. **Host integration**：callback ID、空成功Models、三个executor 503 gate、scheduler排除failed，以及management/OAuth/import/check-in不受gate影响。
11. **Panel status JSON**：正常与dashboard early error response都包含脱敏`model_status`，多auth状态不互相覆盖。
12. **Executable panel fixture**：Node 26 stdlib `node:test`、`vm`与最小DOM/fetch/fake timer，不增加npm dependency。
13. **六项maintenance slices**：limiter、safe response、credits sort、available balance、partial import、BasePath/query-key各自独立RED/GREEN。
14. **Deletion与docs**：production Go文件扫描不得包含特定模型ID、静态映射或模型专属reasoning；更新当前README、architecture与CHANGELOG，不改历史spec。

网络测试全部使用本地fixture，不使用真实凭据或实时endpoint。

## 验收标准

- Production静态模型只有`auto`，静态metadata只有一个default template。
- Production代码没有任何其他特定模型ID、metadata或映射表。
- WorkBuddy list按auth identity隔离；不同realm与enterprise不串cache。
- models.dev metadata全局持久化；WorkBuddy models按auth持久化。
- 无有效cache时，两项来源全部fresh成功并持久化后才ready。
- 有效cache存在时仍尝试refresh；失败来源使用last-good，状态为stale且允许执行。
- Empty、partial、duplicate、malformed或future schema不覆盖last-good。
- Fresh持久化失败时，无cache则failed；有cache则使用old cache并stale。
- `model.for_auth`失败返回空成功Models；三个executor仍有独立503 guard。
- OAuth、import、refresh、panel、check-in与keepalive在模型failed时仍可用。
- Panel错误常驻、脱敏、可访问，不阻断账号grid。
- 六项management/panel修复通过可执行行为测试。
- PR #5、fork和`workbuddy2api`的重复或不安全能力没有进入diff。
- 不增加第三方dependency。
- 最终运行：

```powershell
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./...
node --test
git diff --check
```

只格式化本次修改文件，不顺带格式化当前仓库已有的其他文件。
