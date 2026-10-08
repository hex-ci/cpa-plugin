# Changelog

## 0.12.2

### 修复

- **未配置 usage 上报时不再探测 CPAMP 的 `/v0/management/usage/import`**：usage 上报 URL 为空
  （或只有 key 没有 URL）时插件仍会发起探测请求；宿主对空管理密钥按失败计数，连点几次就把这个 IP
  封 30 分钟，表现为「面板/网关突然全部 401」。现在 URL 为空直接不做任何请求。
- **去掉硬编码的本机端口**：探测默认值改用官方默认端口 `8317`，README 示例里的主机名与端口
  也改成占位符。公开代码不再带任何本机私有端口值。

### 其它

- 版本号 0.12.1 → 0.12.2。

## 0.12.1

### 修复

- **面板模型目录提示条不再被网格占位样式撑高**：目录初始化时 `#modelStatus` 会带上状态类
  `loading`，而样式表里有一条给网格占位文案写的裸规则 `.loading{…padding:40px}`，于是提示条
  连内边距一起被撑到 **103px**（实为一行的文字）并变成居中；现占位类改名 `.grid-hint`，
  提示条恢复单行高度（43px，实测）。目录改成按两种客户端形态请求后初始化窗口变长，
  这个撞车才频繁可见。
- **占位类与状态类撞车的守护测试**：从 `panel.html` 抽样式表，收集所有裸单类选择器，
  断言与 `.model-status.<state>` 的状态名零交集——状态名来自后端状态枚举，不该被样式表里的
  同名布局类污染。

### 其它

- 版本号 0.12.0 → 0.12.1。

## 0.12.0

### 修复

- **模型目录按客户端形态请求**：目录端点按 `User-Agent` 区分下发形态，客户端标识（含 `WorkBuddy/`
  段）拿到的名单比 CLI 标识多出若干可调用模型（`grok-4.7`、`hy4-preview-f`、
  `deepseek-v4.1-flash-sg`），CLI 形态另有一个独有项；此前只按 CLI 标识请求，会丢掉这些模型。
  现按两种形态各请求一次并合并。同一 `WorkBuddy/` 段内版本号不影响结果（实测）。
- **企业请求补齐 `X-Tenant-Id`**：客户端鉴权标头同时下发 `X-Enterprise-Id` 与同名取值的
  `X-Tenant-Id`，只带前者的企业类请求会被上游拒绝；现两者一并下发。
- **容量排队码归入软限流**：`6020 排队中` / `6021 队列已满` / `6022 用户并发上限` 此前被判为
  服务端错误，账号会被停用与轮转；客户端把这三个码当作「等待中」状态（轮询队列状态后重发），
  现归入软限流分类，账号保持健康。边界用 `\b602[012]\b` 收口，`60201`、`6023` 不受影响。

### 新增

- **积分面板按客户端口径分组**：面板此前只出一根总进度条 + 包数量，现按客户端设置页的
  商品码→四组映射（套餐基础 / 套餐赠送 / 购买积分 / 平台奖励积分）加分组行：组名 +
  `已用 x/y` + 已用占比细进度条 + 时间文案（套餐组「下次刷新」、购买与奖励组「最近到期」）。
  未识别的商品码归入「其他」组（客户端直接丢弃，会导致分组行加不到总量）；分组行只渲染
  非空组，且**加总恒等于账号总量**（每包必落一组）。时间文案取有效期终点
  （`DeductionEndTime`），已用完的包不参与「最近到期」计算。
- **到期预警标签**：组内仍有余额时，按客户端阈值打标——≤24 小时红「24h内到期」、
  ≤3 天黄「3天内到期」；免费体验版（`freeMon`/`freeMonIntl`）属自动重置档，临近刷新不打标
  （客户端同款例外）。

### 其它

- **下线「领取专家加油包」试用入口**：客户端 5.6.2 已无该入口（`/billing/ide/trial` 不再被
  调用），插件侧的面板按钮、`/trial` 路由、`trial_claimed` 字段、相关文案与测试一并移除。
  面板副标题、插件商店描述里的对应宣传语同步清理。
- 版本号 0.11.0 → 0.12.0。

## 0.11.0

### 修复

- **模型目录口径改为账号可调用名单**：目录端点同时下发「账号可调用名单」与「全量模型明细表」，
  此前按全量明细表暴露模型，会把**该账号实际调不了**的模型（选中返回 `11102 服务信息不存在`）
  列进模型列表；现以名单为准，明细表只用于补字段。实测同一账号下，全量独有的模型有的可调用、
  有的返回 `11102`，只有名单与可用性一致。
- **双端点目录改为并集探测**：主端点与企业端点的可调用名单**各有独有项**，此前只在一个端点
  失败（404/405）时才换另一个，会丢掉各自独有的模型；现两个端点都探测并按名单合并，
  字段以前一个端点为准，单端点失败降级为另一个的结果并记录告警。
- **拦截非对话模型**：嵌入/补全/图像类模型（`nes-`/`completion-`/`codewise-` 前缀、
  输出上限 ≤256、`text-to-image` 标签）此前会进入模型列表，选中后上游返回 `11102`；
  现两条目录路径统一过滤，已下架（`disabled`）条目一并剔除。
- **工具调用历史配对修复**：会话中若存在「有调用无结果」「有结果无调用」「多个调用只有部分
  结果」等破损历史（客户端中断留下的），上游会以 400 拒绝整个会话且每轮都复现，导致会话
  彻底不可用；现出站前修复配对，丢弃无法配对的条目让会话自愈。
- **推理档位按模型真实支持范围降级**：客户端请求的思考档位若不在目标模型的档位表内，
  上游直接返回 400 丢掉请求（例如单档模型收到 `max`）；现按档位序（`off` < `minimal` < `low`
  < `medium` < `high` < `xhigh` < `max`）降级到不超过请求档的最高档，全部档位都高于请求档时
  取最低档，模型无档位元数据时保持原样。
- **`max_completion_tokens` 别名不再被静默截断**：上游只读 `max_tokens`，此前的别名请求会被
  静默套用上游默认输出上限（实测 128000 被压到 32000），现翻译为 `max_tokens` 并移除别名。
- **屏蔽词改写不再破坏普通文本**：计费标头改写所用的正则整体可选，会把正文里任何
  「冒号 + 分号」片段（如 `注意：以下；`）当成标头连同内容一起删除；现改为必须匹配标头
  本身才会删除其取值段。
- **请求级错误不再牵连账号**：`11115 提示词过长`、`11102 模型不可用` 新增独立分类，
  只做识别与日志（账号保持可用、不轮转）；此前这类请求级失败与账号级失败混在一起判定。
- **限流与余额耗尽的判定顺序**：`429` 状态此前可能被余额类措辞判成「余额耗尽」（进而按
  耗尽处理账号），现 `429` 恒为限流；`402` 保留为余额耗尽。上游已按此修正，插件与之对齐。
- **限流重置时间解析**：重置时间戳现在也识别英文表述，并移除了业务码门禁（此前只有特定
  业务码的响应才会解析重置时间）。
- **故障标记不被额度刷新覆盖**：额度定时刷新会重写 auth 文件的备注，此前会把「需重新登录」
  这类故障标记覆盖成普通额度文案，导致「因故障被禁用的账号」在额度恢复后被重新放进轮转；
  现故障标记在刷新时保留。

### 新增

- **推理档位元数据下发**：从目录解析每个模型的思考档位表、默认档与「是否允许关闭思考」，
  注册到宿主模型信息，客户端与面板据此展示真实可选项（此前不下发，客户端看不到任何档位）。
- **模型并集与过滤的端到端测试**：新增离线测试（合并规则、单端点降级、名额过滤）与
  可选的真实目录测试（设置 `WORKBUDDY_LIVE_AUTH` 后运行，默认跳过）。
- **`effort` 降级与错误分类日志**：档位降级按「字段/请求档/落档」去重记录；
  请求级错误的分类结果提升为 info 级并按「状态码/分类」去重，生产配置（`debug: false`）下可见。

### 其它

- **限流等待提示写入日志**：上游响应头里的等待时长（`Retry-After` 等三种形态）解析后记录
  到插件日志；宿主不接受插件侧的等待提示，因此只作可观测性用途。
- **工具调用流式折叠加固**：折叠时 `index` 缺失的回退顺序、流被截断时丢弃不完整参数、
  函数名取首个非空值而非拼接（当前上游逐帧发送空串，拼接不会触发，属防御性加固）。
- 版本号 0.10.1 → 0.11.0。

## 0.10.1

### 修复

- **面板耗尽状态进度条去加载动画**：修复账号额度为 0（`total_remain: 0, total_used: 0, total_size: 0`）时进度条被误判为未加载、右侧一直旋转显示“加载中…”的问题。现在当账号耗尽时，文案明确显示红色的“已耗尽”，进度条显示 100% 红色满条。
- **未加载禁用账号骨架屏优化**：未拉取额度且账号处于禁用状态时展示静态 `-`，不再显示带有加载动画的旋转小菊花。
- **懒加载允许穿透禁用账号**：异步拉取积分（`lazyLoadCredits`）不再跳过被生命周期自动禁用的耗尽账号，确保打开面板能正常获取其最新额度。

## 0.10.0

### 新增

- **活跃度上报**（`activity_auto`，默认关闭）：每日 10:00 上报对话事件，
  推动成长系统的连续天数（streak）并满足 `first_buddy` 领养前置。仅 CN 个人账号；
  企业版与 Global 跳过。单次上报条数由 `activity_report_count` 控制（默认 5，
  范围 1-50），领养门槛需要 5 次对话。
- **猫旅行循环**（`travel_auto`，默认关闭）：每日 09:00 与 21:00 执行领养 / 出发 /
  领取三步。已完成行程的领取直接产出积分。仅 CN 个人账号。
- **增长任务中心**（`growth_tasks_auto`，默认关闭）：每日 10:00 与 01:00 执行
  任务接受与领取。只处理能靠对话事件完成的任务（五次对话、GLM 对话、夜猫子对话）；
  需要桌面/网页指纹链或真实操作的任务有意不接受。其中的对话任务依赖 `first_buddy`
  前置（已领养），所以对还没有猫的账号需要同时开启 `travel_auto`，否则接受会被上游
  拒绝且不发事件。
- **夜猫子任务时点**：增长任务中心在 01:00 额外触发一次，对应夜猫子时段
  （23:00-08:00 CST）的事件上报，事件带 `mode: night`。
- **请求头对齐官方客户端**：`billing` / `oauth` / `backend` 三条路径补齐
  `X-CodeBuddy-Request: 1` 与按 realm 选择的 `Accept-Language`。
- **`stream_options.include_usage` 注入**：请求体已包含 `stream_options` 时补上
  `include_usage: true`，与官方 CLI 行为一致，确保上游在末尾 SSE 帧返回用量，
  从而使积分统计准确。

### 修复

- **消息角色归一化**：`developer` 等角色拼写被上游判为
  `11128 Illegal API invocation from an unapproved channel`。现在在请求体重写的
  第一步归一化角色拼写（`developer` → `system`），同时关闭了把非 system 家族
  角色降级为 system 的路径。
- **首条消息必须是 system**：Global 域要求 system 出现在首位，否则返回
  `first message is not system prompt`。现在在首位缺失 system 时注入一条最小
  system 消息。
- **上游限流码 `11134` 识别**：上游以 HTTP 500 + `extError.code=rate_limit_exceeded`
  返回模型级限流，此前被归类为服务端错误并触发重试。现在按业务码匹配
  （比文案匹配稳健，文案在响应体尾部可能被截断），归类为限流。
- **模型前缀防御性剥离**：宿主重试路径可能把 `prefix/` 连同模型名一起发给上游
  （如字面量 `wb/gpt-5.6-luna`），上游返回 `11102 model service info not found`。
  现在插件用**自己的模型清单**自校验后剥离前缀：只有当剥掉首段的结果确实是本插件
  注册过的模型时才接受。若完整 ID 本身就是本插件服务的模型，则绝不剥离——这保证
  「剥」只可能把非服务的名字变成服务的名字，永远不会把一个模型静默路由到另一个。
- **故障停用的凭据不再被积分流程自动恢复**：session 失效 / `11140` 封禁写入的
  fault note 意味着「需要重新登录」，此前会被「积分充足」判定顺手重新启用。
  现在 lifecycle 读取落盘的 note，只有本插件因积分耗尽做的停用才允许自动恢复。
- **错误分类重构**：上游失败按 `classifyUpstreamError` 统一分类后再决定 lifecycle
  动作，不再用「是不是 429 / 是不是积分」这类布尔判断。内容风控误报与畸形出站
  请求体归为 `content_blocked` / `bad_params` 并在此处忽略（它们不反映凭据状态），
  模型级限流（6004 / 11134）不再触发凭据生命周期动作。
- **`workbuddy.cn` 签发的国内区凭据**：按 JWT issuer 判定 realm 的白名单此前漏了
  `workbuddy.cn` / `www.workbuddy.cn`，导致国内区凭据被误判、模型目录报
  `auth_invalid`。
- **模型目录不再因第三方元数据缺失而不可用**（见下「变更」）。

### 变更

- **不再依赖 models.dev**：插件此前会在模型目录发现之外，额外从第三方网站
  `models.dev` 拉取 `/models.json` 作为元数据补充。这条外部依赖现已彻底移除——
  模型目录的每一个字段都来自 WorkBuddy 自己的 `/v3/config`。

  背景：models.dev 实际只补充模态（`SupportedInputModalities` /
  `SupportedOutputModalities`），而 WorkBuddy 目录本身就下发 `supportsImages`
  字段（CN 域 30 个模型中 29 个有）。codebuddy 早期把上游的 `maxInputTokens` /
  `maxOutputTokens` 错读成 `maxTokens` / `contextWindow`，导致限额恒为 nil，
  才引入 models.dev 兜底；字段名修正后这条依赖没有跟着去掉。

  用户可见的行为变化：

  - **无外网环境不再导致模型列表为空**。此前 models.dev 拉不到会让整个模型目录
    被判失败（`modelFailed`）、模型列表返回空——「装了插件但模型按钮点不出任何
    模型」由此而来。现在只有 WorkBuddy 目录本身失败才算失败。
  - 模态来自上游 `supportsImages`：`true` 声明 `text` + `image`；`false` 或字段
    缺失时不声明模态（不替上游断言）。输出模态一律不声明。
  - `models` 静态配置现在完全不发网络请求、不读写缓存，直接按配置的列表返回。

### 移除

- 删除 `model_source_modelsdev.go` 及其测试（models.dev `/models.json` 解析、
  匹配与合并）。
- 删除 `model_readiness.go` 中的 metadata 机制：`metadataForAuth`、
  `selectMetadata`、`metadataCall`、`metadataCache`、`metadataResult`、
  `metadataStatus`、`metadataRetryBackoff` 及相关类型。
- 删除 `model_store.go` 中 `metadata.json` 缓存的读写与校验。**磁盘上遗留的
  `metadata.json` / `metadata.json.bak` 不再被读取，也不会被删除。**
- 删除错误码 `models_dev_transport` / `models_dev_http` / `models_dev_schema`。
- `panel.go` 的模型状态响应去掉 `metadata_source` / `metadata_fetched_at` 字段；
  `stale` 恢复单一含义（目录刷新失败，正在用缓存）。

## 0.9.8

### 新增

- **模型列表显示消耗倍率**：上游两个目录端点（`/v3/config` 的 `data.models[]`、
  `/console/enterprises/personal/models`）都为每个模型下发 `credits` 字段，值即
  该模型的扣费倍率（如 `x0.79 credits`；个别条目为无单位的 `x0.05`）。现在解析
  该字段并写进 `display_name`，形如 `Balanced · x0.59 credits`。这同时修掉了
  workbuddy 模型 `DisplayName` 恒空的问题——此前 Claude 格式 `/v1/models` 只能
  回落显示模型 ID。

  可见位置：认证文件列表的「模型」弹窗（宿主该端点只回 `id`/`display_name`/
  `type`，`display_name` 是唯一能多显示一列的字段）、Claude 格式 `/v1/models`、
  Codex 格式 `/v1/models?client_version=1`、Gemini 格式 `/v1beta/models`。

  目录未下发倍率的模型保持 `display_name` 为空（宿主回落到模型 ID），行为与改动
  前一致。倍率随目录一起缓存，无额外上游请求。

  注意：模型目录缓存 schema 版本未提升，服务中的进程要等下一次成功的目录刷新
  （或重启、`POST /v0/management/plugins/workbuddy/refresh`）才会出现倍率。

## 0.9.7

### 修复

- **模型元数据获取失败后不再反复重试**：上游 `models.dev` 拉取失败时，此前每轮
  快照重建都会重试一次，并在两次失败之间把「已就绪」结果钉死到进程结束。现在引入
  5 分钟退避窗口（`metadataRetryBackoff`）：窗口内继续提供缓存结果并复用同一个
  错误码，窗口到期后下一次调用自动重试上游。既避免打爆上游，又不会让一次瞬时失败
  永久锁住模型列表。

### 其它

- `zeroWidthSpace` 由字面量改为 `\u200b` 转义（运行时值不变，仅提升可读性）。
- `buildLoginStoredAuth` 的 `Account` 赋值改为结构体转换，去掉冗余字段枚举。

## 0.9.6

### 新增

- **国际版（WorkBuddy AI）OAuth 登录**：`oauth_client_mode: workbuddy-ai` 走
  `www.workbuddy.ai` + `platform=workbuddy-ai`，对齐官方国际版桌面客户端。
  此前只有国内登录通道，国际版账号只能手动导入凭证。
- token 轮询与账号查询改为跟随登录网关（此前硬编码在国内网关）。state、token、
  account 三个端点必须在签发 state 的那台网关上轮询——跨网关不会报错，但会静默
  换回另一个账号。
- `loginSessionId` 只在国内桌面 profile 上追加：它是国内桌面端的上报会话 ID，
  国际版登录页不认识这个参数。
- `oauth_client_mode` 枚举值由 `cli | workbuddy` 扩为
  `cli | workbuddy | workbuddy-ai`。

### 修复

- **账号卡片长用户名截断**：卡片标题行左侧的用户名过长时会把右侧徽标（使用中/已禁用/耗尽/区域/套餐）
  挤到第二行。现在用户名用省略号截断，`title` 暴露完整内容，徽标固定在同一行。
- **刷新不再丢用户配置**：auth 文件顶层的用户字段（`weight` 权重、`priority`、
  `proxy_url`、`prefix`、`headers`、`request_retry`、`websockets` 等）此前会在每轮
  重写 auth 文件时被抹掉——写文件的几条路径都从零构造 JSON，只保留
  type/provider/logo/disabled/note/auth/account。现在统一改成「读现有文件 → 只覆盖
  插件自有键」，刷新/生命周期/导入三条路径都保留用户字段。
- **`weight` 保持不变**：CPA 的 auth 文件 `weight` 字段是宿主调度权重，插件重写时
  原来会丢掉，导致每次刷新后权重回到默认值。
- **国际版账号模型目录 `auth_invalid`**：`workBuddyRealmFromAccessToken` 只认裸
  `workbuddy.ai`，而国际版 JWT issuer 是 `https://www.workbuddy.ai/auth/realms/copilot`，
  落到 unknown 分支 → 模型引导判定 `auth_invalid`、面板显示「模型目录不可用」。改为按
  `isGlobalDomain` 匹配（`workbuddy.ai` 及任意子域）。
- **token 刷新改为跟随账号区域**：刷新端点此前按全局 `oauth_client_mode` 选网关，
  国际版账号在 CN 模式下会刷新到错误主机。现在按账号 `domain` 选网关（`www.workbuddy.ai`
  ↔ `copilot.tencent.com`）。

## 0.9.5

### Official-client wire alignment

- Decode the upstream model catalog with its real field names
  (`maxOutputTokens` / `maxInputTokens`). An earlier revision read
  `maxTokens` / `contextWindow`, which the service never sends, so every model's
  limits stayed empty and were silently filled from models.dev values up to 5x
  the models' actual allowance.
- Join `/v3/config`'s ordered `agents[].models` roster with its `data.models[]`
  limit metadata, so the preferred catalog path carries per-model context and
  output limits instead of bare ids. Both the personal and enterprise routes
  serve the same shape and are covered by one parser.
- Keep upstream descriptions when they arrive as `descriptionEn` /
  `descriptionZh` rather than the flat `description` field.
- Send `reasoning_summary: "auto"` and `verbosity: "high"` on requests that ask
  for thinking, mirroring the official client's thinking configuration (which is
  not part of the compatibility pipeline it skips for its own gateway). Requests
  with no thinking signal stay untouched and caller values are never overwritten.
- Add the `X-Trace-ID` and `X-Root-Request-ID` headers the official client sends
  on model requests.

## 0.9.3

- Accept the production CN JWT issuer host `www.codebuddy.cn` during per-auth model bootstrap.

- Add optional `models` YAML configuration. A non-empty single-line string list is
  the complete catalog and bypasses WorkBuddy catalog HTTP and cache access while
  retaining models.dev metadata fetch, ETag, persistence, and last-good rules.
- Discover each authenticated account's model entitlements from WorkBuddy, with a 404/405-only legacy endpoint fallback.
- Enrich missing serving metadata from models.dev without a static model mapping or metadata table.
- Persist separate global metadata and per-auth model last-good caches, and use them for fail-closed `ready` or executable `stale` startup semantics.
- Expose redacted `model_status` readiness in the panel and gate executor and scheduler access to `ready` or `stale` accounts.

### Management and panel maintenance

- Stop valid management keys from consuming the failed-authentication rate-limit bucket.
- Classify empty, non-JSON, malformed, and non-2xx panel responses without exposing response bodies or native errors.
- Sort credits as positive, real zero, then unknown while preserving the original sort cycle and source order.
- Exclude disabled and exhausted accounts from the panel's spendable remaining-credit total.
- Keep partial-import failures visible by safe filename, clear credential inputs immediately, and close the modal only when every import succeeds.
- Build panel requests from the host BasePath, store a query key in `sessionStorage`, and remove it from the URL.

## 0.9.0

### Configurable prompt desensitization

- Add an opt-in U+200B desensitizer with an editable, persistent 85-term default list.
- Restrict rewriting to system/developer prompt text, marked instruction blocks, and tool title/description fields.
- Add panel controls to save custom terms, restore the built-in list, or reset the feature to disabled defaults.

### OAuth, host bridge, and stream correctness

- Use path-derived identity for OAuth poll results and add an explicit WorkBuddy desktop OAuth profile.
- Preserve request-scoped host callback context and accept both `StatusCode` and `status_code` host responses.
- Emit CPA-native stream errors and preserve typed upstream HTTP status for synchronous failures.

### Credits and panel workflows

- Bound concurrent billing fetches to four and add an opt-in, CN-only strict enterprise credits probe.
- Prevent lifecycle actions from using stale credits after a refresh error and recognize `额度已用尽`.
- Add sequential multi-file credential import, account search, and remaining-credit sorting to the panel.

## 0.8.7

### Client identity headers

- Send the CodeBuddy-compatible `X-IDE-*` and `X-Agent-Intent` headers so upstream usage records identify CLI requests.
- Generate independent 32-character hexadecimal request, conversation, conversation-request, and message IDs for every upstream request.

## 0.8.6

### Proxy routing, models, request policy, and panel diagnostics

- Add plugin-level explicit proxy routing while preserving inherited CPA routing and fail-closed behavior.
- Add `hy3-x`, `hy4-preview`, `hy4-preview-x`, and `glm-5.3-flash` to the fixed model list.
- Pin canonical-model `reasoning_effort`: Hy3 and Hy4 preview models use `high`, `glm-5.3` uses `xhigh`, and `glm-5.3-flash` uses `max`.
- Sanitize blocked outbound billing and `cc_*` fingerprint fields without changing ordinary prompt text.
- Add the authenticated `/egress-ip` management endpoint and non-blocking current egress IP display in the panel.

## 0.8.2

### Concurrency + lifecycle hardening

- `lifecycle.go` — P0-2: `reconcileOneAccount` now routes credits fetch
  through `cachedAccountDetails(force=true)` so singleflight serializes
  concurrent writers, eliminating a Load→Store race that could clobber
  newer plan/checkin values.
- `lifecycle.go` — P1-4: Global `lifecycleDelete` now requires a second
  `fetchUserResource` confirmation before deleting. Prevents transient 402
  from irreversibly removing an account.
- `checkin.go` — P1-5: after a successful checkin the credits cache is
  refreshed immediately (was only updating the checkin field). Panel now
  shows updated balance without waiting for the async reconcile pass.
- `cache.go` — P1-1 documented trade-off: force=true callers still join
  singleflight (skipping would re-introduce P0-2).
- `main.go` — P0-5: `scheduler_mode` ConfigField description now warns that
  `off + lifecycle_auto=false` leaves exhausted accounts routable.

## 0.8.1

### Bug fixes + compliance polish

- `keepalive.go` (new) — daily 22:00 access-token refresh to prevent Keycloak
  offline-session expiry; reuses `schedulerLoop`, routes via `host.http.do`,
  uses CPA native `disabled` field for session-dead auths.
- `models.go` — fix `filterExcludedModels` slice aliasing that corrupted
  `dynamicModelsCache` (P0).
- `billing.go` — route all billing API calls through `hostHTTPDo` (was missed
  in v0.7.0); improve "parse failed" error to include a redacted body snippet.
- `checkin.go` — avoid double `fetchCheckinStatus` in classify already-branch.
- `billing.go` — `performCheckinCall` now sets `success=true` as bool to avoid
  downstream type-mismatch when upstream returns a string.
- `host_auth.go` — fresh slice in `hostAuthList` to avoid aliasing RPC response.
- `oauth.go` — route `handleRefreshAuth` via `hostHTTPDo` (last path still on
  `sharedHTTPClient()`); make OAuth error messages actionable.

## 0.8.0

### Refactor — community-grade file layout

完成 v0.7.0 合规改造后的代码组织大重构，把两个超大主档拆成单一职责的
小文件，对齐 CPA 原生 plugin 案例的"一个能力一个文件"原则。

**File splits (main.go 2940 → 809, management.go 2263 → 349, lifecycle.go 980 → 535)：**

- `redact.go` (49) — redactSecrets + 4 个 regex + truncate
- `usage.go` (242) — handleUsage + publishUsage + forwardUsageToCPAMP + sseUsageCollector
- `payload.go` (469) — prepareUpstreamBody + 4 个 InPlace mutator + 4 个 legacy 包装
- `stream.go` (452) — streamEmit/Close + pumpUpstreamStream + collectUpstreamStream + aggregate*
- `models.go` (443) — callModelsAPI + fetchDynamicModels + resolveUpstreamModel + alias 反解
- `oauth.go` (240) — handleStartLogin/PollLogin/RefreshAuth + newLoginClient + doJSON
- `host_bridge.go` (388) — hostHTTPDo/DoStream/Read/Close + hostStreamReader + Direct fallbacks
- `billing.go` (486) — billing API + fetch* + perform* + JSON helpers
- `cache.go` (183) — accountCache + accountDetailFlight singleflight + prune
- `host_auth.go` (73) — hostAuthList/Get/GetBundle (host auth-store RPC)
- `usage_config.go` (202) — configure + resolveUsageReport + probe* + config vars
- `checkin.go` (515) — handleManualCheckin + runAutoCheckin + schedulerLoop + classify/execute/summarize
- `credits_handler.go` (285) — handleImportAuth/CheckinConfig/ClaimTrial/SelectAuth/CreditsQuery
- `panel.go` (266) — buildDashboardEx + summarizeCredits + servePanel + panelHTML
- `policy.go` (188) — lifecycleAction decisions + displayNote + labelForAuth
- `authfile.go` (299) — authFileNameFor/sanitizeUIDForFileName/hostAuthPersist/deleteAuth + path safety

**保留的小文件**：`scheduler.go` (138)、`active_auth.go` (158) — 本来就够小。

**文档：**

- `README.md` — 英文版，Features / Quickstart / Configuration / Lifecycle / Development / License
- `README_CN.md` — 中文版
- `LICENSE` — MIT
- `Makefile` — build / test / lint / clean / release / tag 目标
- `.gitignore` — 忽略 `*.so` / `*.h` / `bin/` / `dist/`
- `docs/architecture.md` — 模块图 + 数据流 + 关键设计决策 + 与 CPA 的集成点
- `docs/development.md` — 本地构建 / 测试 / 调试 / 发布流程
- `docs/definition-of-done.md` — v0.8.0 验收标准（量化可测）

### Lint / style

- `gofmt -l .` → 0 files
- `go vet ./...` → 0 issues
- `gocritic check ./...` → 0 issues（修复 policy.go 的 ifElseChain）
- `staticcheck` 真实代码问题 0（工具链版本噪音已过滤）

### Bug Fixes (carried over from v0.6.31 / v0.7.0)

本次重构完整保留了之前所有 bug 修复：
- UID 路径穿越白名单（authfile.go sanitizeUIDForFileName）
- refresh_token 不再泄露到 chat 上游（main.go backendHeaders）
- invalidateAccountCredits 数据竞争修复（值拷贝）
- handleManualCheckin early-already merge（不丢 credits/plan）
- configure 嵌套锁修复（parse-then-lock）
- scheduler_mode off 接通（handleSchedulerPick 读取配置）
- deleteAuth 调 clearActiveAuthIfMatch
- runAutoCheckin 串行改并发（sem=4）
- cachedAccountDetails singleflight
- panel.html XSS 修复（addEventListener + dataset）
- panel.html CSRF（fetch credentials:omit）
- redactSecrets 裸 JWT 兜底
- pumpUpstreamStream context cancel
- out[:0] 共享底层数组改新 slice
- 热路径 4 次 JSON 序列化合并为 1 次
- 冒泡排序改 sort.Slice
- usageReportConfigured/buildDashboard 死代码删除
- handleManualCheckin 三段拆分（classify/execute/summarize）
- management BasePath 缓存（register 时读取宿主注入）

### Tests

- 115/115 tests pass (`go test -race`)
- 新增 `TestSchedulerPick_OffMode_Defers` 覆盖 scheduler_mode=off 行为

## 0.7.0

### Compliance — CPA native patterns
本次大版本把「自建通道」全部替换为 CPA 官方提供的 RPC / 能力接口，
对齐 `sdk/pluginapi` 的设计意图。生产路径 100% 走宿主桥接，插件不再
绕过宿主审计 / request-log / transport policy。

- **所有上游 HTTP 调用走 `host.http.do` / `host.http.do_stream`**：
  - `models API`、`billing API`、`usage 上报`、`chat completions`（流式 + 非流式）
    全部从 `sharedHTTPClient().Do` 切到 `hostHTTPDo` / `hostHTTPDoStream`。
  - 宿主 request-log 现在能捕获插件的出站请求和原始响应（之前完全看不到）。
  - 宿主 transport policy（proxy、超时、连接池）对插件上游调用生效。
  - `sharedHTTPClient` 降级为 fallback 专用：仅当宿主桥不可用（单元测试 /
    老版本 CPA）时使用。新代码直接调用 `sharedHTTPClient` 视为合规 bug。
- **`hostStreamReader` 适配层**：把宿主桥的 32KB 任意字节块适配为 `io.Reader`，
  `bufio.Scanner` 的 SSE 行切分逻辑不变，pump / collect / aggregate 全部透明迁移。
- **`UsagePlugin` 能力声明 + `handleUsage` RPC handler**：
  - 注册能力 `usage_plugin: true`，宿主每次请求完成后会把规范化的
    `pluginapi.UsageRecord` 推送给插件。
  - 插件在 `handleUsage` 里把 record 转发到 CPAMP，与宿主 `DefaultManager`
    的记录并行，不再重复也不遗漏。
  - 旧路径 `publishUsage` 保留向后兼容（老版本 CPA 没接 UsagePlugin 时仍可
    上报），新路径 `handleUsage` 同步触发，CPAMP 侧基于 (timestamp + auth +
    model + total_tokens) 幂等去重。
- **`reportUsageToCPAMP` 重命名为 `forwardUsageToCPAMP` 并走 host.http.do**：
  CPAMP 上报自身也走宿主桥，宿主能看到插件的运维流量。

### Architecture notes
- `hostBridgeAvailable()` 检查 `hostAPI.call` 是否为 nil，统一决定是否
  fallback。生产环境永远为 true，单元测试永远为 false（无宿主）。
- 所有 `*Direct` 函数仅服务测试；生产路径不经过。
- 宿主侧 `sanitizePluginRequest` 会把 `ExecutorRequest.HTTPClient` 置 nil
  （跨 c-shared 边界接口无法传输），所以**插件不可能用宿主注入的
  HTTPClient**——`host.http.*` RPC 是 c-shared 插件访问宿主 transport 的
  唯一合规方式，本版本全部采用。

## 0.6.31

### Security
- **UID 路径穿越修复**：`authFileNameFor` 新增 `sanitizeUIDForFileName` 白名单
  （`[^a-zA-Z0-9_-]+` → `_`、长度 ≤64、拒绝 `.`/`..`），导入凭证的
  `workbuddy-<uid>.json` 不再可能被 `../` 注入到任意路径。
- **refresh_token 停止泄露到 chat 上游**：`backendHeaders` 移除
  `X-Refresh-Token`。refresh_token 是长期凭证，只在 refresh 端点用；之前每次
  chat completion 都附带它，上游日志一旦记录请求头即等同账号被盗。
- **插件层 management 鉴权 + 限流**：`handleManagement` 入口对所有 POST /
  写端点新增插件层防护：constant-time Bearer 比对（`crypto/subtle`），
  per-IP token-bucket 限流（容量 5、每 6s 1 个）。配置方式：
  `config_yaml management_key:` 或 env `WB_MANAGEMENT_KEY`。空则保持
  历史行为（仅依赖宿主鉴权）。
- **panel.html XSS 修复**：4 处 `onclick="...('${esc(auth_index)}',this)"`
  改为 `data-action` + `data-auth-index` + `addEventListener`。`esc()` 只
  转义 HTML 不防 JS 字符串上下文注入。
- **panel.html CSRF 缓解**：`fetch` 显式 `credentials:'omit'`，面板纯靠
  Authorization Bearer，不再隐式带 cookie。
- **redactSecrets 兜底裸 JWT**：新增 `redactREJWTLoose` 正则，匹配不带
  `Bearer` 前缀、`access_token` key 的 `eyJ…` 两段/三段 JWT。

### Bug Fixes
- `invalidateAccountCredits` 数据竞争：直接改 sync.Map 共享 entry 的字段
  （`e.credits = nil`），并发 dashboard / reconcile / chat 后置 invalidate
  会拿到撕裂状态。改为 `fresh := *e; Store(&fresh)` 值拷贝，与其他 4 处
  写法一致。
- `handleManualCheckin` "early already" 路径丢 credits/plan：直接构造
  `accountCacheEntry{checkin: ci}` 覆盖整个 entry，签到后面板积分消失。
  改为 merge prev 的 credits/plan。
- `configure` 嵌套锁：在 `checkinAutoMu` 内嵌套获取 `lifecycleAutoMu` /
  `schedulerModeMu`，未来加反向获取路径即死锁。改为两阶段：无锁解析到
  局部变量，再分别单锁写入。
- `scheduler_mode: off` 配置断链：configure 解析但 `handleSchedulerPick`
  从不读取，"off" 实际表现为 "credits"。现在 off 正确 defer 给内置 scheduler。
- 删除 Global 账号后 `activeAuthID` 残留指向已删 ID：`deleteAuth` 两个成功
  路径现在都调 `clearActiveAuthIfMatch(authID)`。
- `runAutoCheckin` 重复 `fetchCheckinStatus` + 变量 shadow：原代码内层
  `ci` shadow 外层，且第二次调用与第一次状态可能不一致。改为单次调用，
  签到成功才 refresh。
- `out[:0]` 共享底层数组：`filtered := out[:0]` 复用底层数组在 range 中
  写入，改为 `make([]wbAccount, 0, len(out))`。
- `pumpUpstreamStream` 无 context：`http.NewRequest` 无 context，客户端
  断开后 goroutine 一直读到 120s 超时。改为 `NewRequestWithContext` +
  cancel 传入 pump，所有退出路径释放。

### Performance
- **热路径 4 次 JSON 序列化合并为 1 次**：新增 `prepareUpstreamBody` 统一
  `forceStreamBody` + `normalizeToolsForUpstream` + `rewriteSystemForUpstream`
  + `ensureSystemMessage` + `rewriteModelInBody`，单次 unmarshal + 单次
  marshal。每次 chat completion 省 4-5 个 JSON 往返。
- **`runAutoCheckin` 串行改并发**：抽出 `processAutoCheckinAccount`，主循环
  `sem=4` 并发。N 账号从 3N 串行 HTTP 降到并发 4 路。
- **`cachedAccountDetails` 加 singleflight**：per-authID `sync.Map` + done
  channel。并发 dashboard / reconcile 对同一账号只跑 1 次上游 fetch，
  其他 goroutine 等结果，消除 6x upstream QPS + last-writer-wins。
- **冒泡排序改 sort.Slice**：`pruneAccountCacheSoftCap` 从 O(n²) 降到 O(n log n)。

### Refactor
- **handleManualCheckin 273 行拆分**：`classifyCheckinTargets` /
  `executeCheckinBatch` / `summarizeCheckinResults` 三段独立函数，各自
  单一职责，便于单测。
- **management BasePath 不再硬编码**：register 时缓存宿主注入的 BasePath，
  handleManagement 用 cached 值。宿主未来版本化路径不会失效。
- 死代码清理：删 `upstreamBase` legacy 常量、`usageReportConfigured` 无人
  调用、`buildDashboard` 包装函数。

### Tests
- 新增 `TestSchedulerPick_OffMode_Defers` 覆盖 scheduler_mode=off 行为。
- 全套 115 tests + `-race` 通过。

## 0.6.29

### Fixed
- 修复签到后按钮不变"已签到"、套餐标记丢失的问题
  根因：handleManualCheckin/runAutoCheckin/handleClaimTrial 在签到/领取成功后
  accountCache.Delete(f.ID) 把 cache 清了，light load 时 checkin/plan 是 nil。
  handleCreditsQuery 的 cache merge 逻辑从 prev.plan（空）取值而不是用刚获取的
  fetchPaymentType(sa) 结果，导致 plan 在 light load 后丢失。
  修复：签到/领取成功后把 checkinSummary 存回 cache 而不是删除；
  handleCreditsQuery cache merge 用刚获取的 plan；runAutoCheckin/handleClaimTrial
  改为 invalidate credits（置 nil）而不是删除整个 cache entry。

## 0.6.28

### Fixed
- 修复面板选中卡片与实际路由账号不一致的根本问题
  根因：activeAuthID 存的是 auth.Index（运行时 SHA256 hash），但 scheduler
  的 SchedulerAuthCandidate.ID 是 auth.ID（持久化 UUID），两者永远不匹配，
  导致 pickActiveAuth 永远走 fallback 选第一个，面板显示选中第一个但实际
  路由到别的账号。同时 cachedCreditsScore 用 auth.ID 查 accountCache（key
  是 auth.Index）也查不到，exhausted 判断也坏了。
  修复：全链路统一用 auth.ID — activeAuthID、accountCache key、
  lifecycleState key、面板 selected 判断、/select API 返回值全部改用
  auth.ID。lifecycle 函数（reconcileOneAccount/disableAuth/reenableAuth/
  deleteAuth/syncAuthNote）加 authID 参数，resolveAuthIndex 改为
  resolveAuthIndexAndID 同时返回 index+ID。
- 修复首次加载面板时选中耗尽账号的问题
  首次 GET /accounts 不拉 credits（fetchCredits=false），所有卡片
  Exhausted=false，ensureDefaultActiveAuth 选第一个。lazyLoadCredits
  异步获取积分后发现第一个已耗尽，但选中状态不会更新。
  修复：lazyLoadCredits 全部完成后前端静默再拉一次 /accounts（此时
  cache 已有 credits，light load 能拿到正确 exhausted 和 selected），
  重新渲染卡片。

## 0.6.27

### Fixed
- ensureDefaultActiveAuth 也检查 Exhausted：面板刷新时选中账号已耗尽会同步切换
  修复 scheduler.pick 切了但面板 ensureDefaultActiveAuth 又选回去的 race
  现在 pickActiveAuth 和 ensureDefaultActiveAuth 用同一套规则，选中状态不会漂移

## 0.6.26

### Fixed
- 选中账号积分耗尽时自动切换到第一个可用账号，并同步更新选中状态
  全部耗尽时留在当前账号不 flip-flop
  修复 v0.6.25 过度 sticky 导致耗尽后一直报错的问题

## 0.6.25

### Fixed
- 选中账号 sticky：scheduler 不会因缓存过期/积分耗尽自动切换到别的账号
  只有 host 把选中账号从候选列表移除（disabled/deleted）才切换
  修复面板显示选中A但实际路由到B、静默消耗积分的问题

## 0.6.24

### Fixed
- model.static / model.for_auth 现在尊重 CPA 的 oauth-excluded-models 配置
  在 config.yaml 的 oauth-excluded-models.workbuddy 里列出的模型不再出现在 /models

## 0.6.23

### Fixed
- usage import URL 自动探测：先试 127.0.0.1:8317（裸机/Docker host），再试 Docker 服务名 cpa-manager-plus:8317
  不再写死 Docker 服务名，裸机安装也能自动找到 CPAMP

## 0.6.22

### Fixed
- ExecutorModelScope 改为 OAuth：插件只处理 WorkBuddy auth 绑定的模型
  不再拦截其他 openai-compatible 供应商的同名裸模型（如 deepseek-v4-flash、glm-5.2）
  修复启用 WorkBuddy 后自定义供应商模型请求不进监控的问题

## 0.6.21

### Fixed
- 积分懒加载改为并发：所有卡片同时请求，不再逐个排队

## 0.6.20

### Fixed
- 懒加载积分时同时拉取 plan（套餐类型），修复 plan 徽章显示「-」不更新

## 0.6.19

### Added
- 每张卡片新增「刷新」按钮：单独查询积分并即时更新该卡

## 0.6.18

### Added
- 积分懒加载：进页面先渲染骨架卡（加载中…），逐卡异步拉积分，失败自动重试一次
- 后端 `/accounts` 默认不再并发拉所有账号 credits（避免上游 500）
- `/credits?auth_index=` 单账号查询返回完整字段（region/exhausted/trial_claimed）

### Fixed
- 缓存有效时仍返回缓存的 credits，不再触发上游请求

## 0.6.17

### Fixed
- 流式路径也强制 `stream:true`：WorkBuddy API 现仅支持 stream 模式，`stream:false` 会报 "Non-stream chat request is currently not supported"

## 0.6.16

### Fixed
- 夜间模式：用量汇总卡与账号卡统一 `--card` 底色；内部指标格改用 `--surface`，避免汇总卡看起来更深/发黑

## 0.6.15

### Added
- 面板「选用」账号：默认第一张可用卡；选中卡决定 CN/Global 路由（读 domain，不解码 JWT）
- 选中账号耗尽/禁用/消失时随机切换下一张可用卡并记住

### Changed
- scheduler.pick 改为始终跟随 active 选中账号（不再依赖 credits 排行模式）

## 0.6.14

### Fixed
- Global 账号聊天 401/400 修复：JWT iss=workbuddy.ai 必须走 www.workbuddy.ai 端点（copilot.tencent.com 会对 Global token 返回 401）
- Global 请求自动注入 system message（www.workbuddy.ai 对 user-only 请求返回 code 11101）
- token 刷新和 models 发现也走域名感知端点

## 0.6.13

### Changed
- 请求监控 key 自动探测：config → env（CPAMP_ADMIN_KEY/USAGE_REPORT_KEY）→ docker secret `/run/secrets/cpamp_admin_key`，无需手写 usage_report_key


## 0.6.12

### Changed
- 删除无效 `usage.PublishRecord` 路径，请求监控仅走 CPAMP `/v0/management/usage/import`


## 0.6.11

### Fixed
- **请求监控**：c-shared 隔离导致 `usage.PublishRecord` 进不了宿主 redisqueue；改为异步 POST CPA-Manager-Plus `/v0/management/usage/import`（`usage_report_url`/`usage_report_key`）
- 补全 ExecutorType/AuthType/Source；配置字段暴露于管理面板


## 0.6.10

### Fixed
- **批量签到先过滤再操作**：Global 不参与；今日已签跳过；仅对 CN 未签账号调用 daily-checkin
- 返回 `summary{success,already,skipped_global,fail,eligible}`，面板文案不再把 Global/已签当失败
- 分类/签到并发（限流），降低「全部签到」卡到 502 context canceled

## 0.6.9

### Changed
- **Panel theme adaptive**: CSS variables now default to light (paper) theme; `[data-theme="white"]` and `[data-theme="dark"]` overrides align with CPA management panel tokens. Embedded iframe mirrors parent `data-theme` via MutationObserver; standalone page follows `prefers-color-scheme`. All hardcoded dark colors (toast, modal, input, buttons) replaced with theme-aware CSS variables.

## 0.6.3

### Fixed
- Auth identity: parse/refresh leave ID empty; regression tests (A-01)
- Stream pump: emit failure is failed usage; defer streamClose (A-06)
- No dual-write after host.auth.save (A-15)
- Scheduler skips host-disabled candidates (A-04)
- Global delete reconstructs path via peer auth dir (A-07)
- Panel IP ban wait parses upstream window (A-08)
- accountCache concurrent errs race + soft cap (A-02)
- Dashboard single host.auth.get per row (A-05)
- Instant check-in/trial button state (panel)


## 0.6.2

### Fixed
- **Credits look frozen after chat**: cache TTL 5m→45s; invalidate cache after successful chat (stream + non-stream)
- **Spend math**: package used = cycle size−remain; account total_size from package sizes; TotalDosage treated as capacity pool (not consumption)
- **Check-in packs inflate "available"**: UI labels 可用/已用/额度池 so grant vs spend is visible; note shows 余/已用/池

## 0.6.1

### Added
- WorkBuddy panel **用量汇总**：筛选范围内 剩余/已用/总量/占比 + 进度条；全部视图附 CN/Global 分项
- Dashboard API `summary` 字段：`total_remain` / `total_used` / 分区域统计

### Notes
- CPAMP Auth 页进度条仅支持内置 `codex/claude/kimi/xai/antigravity`（`QUOTA_PROVIDER_TYPES` 白名单）；WorkBuddy 无法靠 `note` 注入进度条，完整用量看插件面板

## 0.6.0

### Added
- **Credit lifecycle** (plugin-only, no CPA/CPAMP source changes):
  - CN exhausted → write auth file `disabled:true` (host skips scheduling)
  - Global exhausted → **delete** auth file (`os.Remove` on path from `host.auth.get`)
  - CN disabled + credits return (after check-in / refresh) → `disabled:false`
  - Executor hard credit errors → async reconcile; pure 429 does not delete Global
  - Unknown credits → no-op (safe default)
- Auth file **note** / **label** enrichment: `CN · 余 x · …` / `Global · …` / 已禁用
- Panel: CN/Global filter tags + counts; disabled badge; lifecycle toast on refresh
- Panel: management-key discipline to avoid CPA IP ban (no request without key; 401/403 backoff)
- Config field `lifecycle_auto` (default true)

### Changed
- Scheduled tick **no longer auto-claims Global trial** (one-shot; manual `/trial` / panel only)
- Tick = CN check-in (if `checkin_auto`) + lifecycle reconcile for all regions
- Import/save writes top-level `type`/`logo`/`note`/`disabled` with nested auth/account
- Force dashboard refresh runs lifecycle and may drop deleted Global rows

### Notes (CPAMP Auth page)
- Filter letter **「W」** / brand typeBadge colors cannot be fixed from the plugin (frontend static icon table)
- Plugin sets `Metadata.logo` + registration Logo; Auth cards show **note** for region/credits summary
- Full UX: WorkBuddy side panel

## 0.5.0

### Added
- International (Global) WorkBuddy account support (`www.workbuddy.ai` domain)
- Domain-aware billing API routing: CN accounts → `codebuddy.cn`, Global → `workbuddy.ai`
- Expert trial pack claim API: `POST /plugins/workbuddy/trial` (Global only, one-time 250 credits / 14 days)
- Panel region badges: light green `CN` (daily checkin) + light orange `Global` (expert trial)
- "全部领取" batch claim button for Global accounts
- Auto-scheduler region branch: CN → daily checkin, Global → claim expert trial if unclaimed
- `wbAccount.region` and `wbAccount.trial_claimed` fields in accounts API response
- `hasTrialPack()` helper detects trial pack from `get-user-resource` packages

### Changed
- `billingBase` selection is now domain-driven via `billingBaseFor(sa)`
- `backendHeaders` Origin/Referer dynamically set per account domain via `originRefererFor(sa)`
- Panel card buttons: CN → 签到, Global → 领取专家加油包 / 已领取
- "全部签到" button only triggers CN accounts (Global accounts are skipped with a message)
- `runAutoCheckin` branches by region: CN daily checkin, Global trial claim

## 0.4.3

### Changed
- Panel import modal: white surface + dark text for readable contrast (was dark-on-dark)

## 0.4.2

### Changed
- Panel: credential import is a toolbar button (left of 刷新数据) opening a modal, instead of an always-visible card

## 0.4.1

### Added
- Panel **耗尽** badge + `exhausted` field on accounts API (shared with scheduler)
- Credential **import** API `POST /plugins/workbuddy/import` + panel paste UI
- Per-account check-in lock (multi-tab safe)
- `executor.count_tokens` stub (`input_tokens:0` — upstream has no API)
- LICENSE (MIT), VERSION file, GitHub Actions multi-arch release workflow

### Changed
- SSE cleanChunk strips empty `extra_fields` / `refusal` / `reasoning_content`
- Scheduler credits mode prefers non-exhausted accounts first

## 0.4.0

### Added
- CPA **Scheduler** capability with `scheduler_mode`: `off` (default) | `credits`
- Credits-aware multi-account pick using panel credit cache

## 0.3.18

### Fixed
- ConfigFields use SDK `ConfigFieldType*` constants

## 0.3.17

### Fixed
- `FrontendAuthProvider` set false; remove dead frontend-auth handlers

## 0.3.16

### Fixed
- Panel refresh toast + busy feedback

## 0.3.15

### Fixed
- Normalize OpenAI object `tool_choice` for CodeBuddy upstream
