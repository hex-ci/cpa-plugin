# Qoder CPA 插件

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 Qoder Provider 插件。
一个插件承载 Qoder 全家族账号：IDE、CLI 与办公端共用同一账号体系与 Credits 余额，
CN（qoder.com.cn）与 Global（qoder.com）双区，多账号、动态模型、COSY 签名推理、积分面板、每日额度领取与 token 保活。

## 功能

| 能力 | 说明 |
|---|---|
| **双区** | CN / Global 各自独立网关与 OAuth client_id；区记录在 auth 文件的 `auth.domain` 上，导入后不随配置漂移 |
| **双登录** | ① 设备授权（PKCE 轮询，无回调端口）→ `dt-` (~30d) + `drt-` 旋转 ② PAT 导入（`pt-`）→ `jt-`/`jrt-`；两套可共存于同一 auth 文件 |
| **COSY 签名** | RSA 包 AES 会话密钥 + AES-128-CBC 身份体 + MD5 五段签名，签名覆盖**实际发送的字节**；`cosy-version` 跟官方 CLI（默认 1.1.75，可配置） |
| **设备指纹** | machine id / token / type / hostname 全部由账号 uid 纯函数派生：同账号永远同一台设备，不同账号不共用 |
| **动态模型** | COSY 拉 `/algo/api/v2/model/list`（chat scene），按账号 `enable` 只暴露可用模型；元数据驱动 context / thinking 档位；`price_factor` 以 `· x0.2` 形式写进 `display_name` |
| **思考档位** | 档位集合来自目录 `thinking_config`；请求档位按最近合法档位归一化，`none` 会同时关闭 `enable_thinking` |
| **额度与套餐** | `quota/usage`（基础 + 赠送 + 团队共享池）与 `user/plan`；上游返回退化体时保留旧值而不是写 0 |
| **每日额度** | 活动平台按 UTC+8 10:00 重置，10:00 / 21:00 定时领取（旧 sash 签到接口已废弃，返回 `DISABLED`）；进程在重置时间后才启动会补跑一次；Global 无该活动时显示「本区无签到」而不是报错 |
| **token 保活** | 22:00 定时刷新；按 `refreshToken` 前缀路由（`drt-` → deviceToken，`jrt-` → jobToken），PAT 只做最终兜底 |
| **面板** | 账号卡片（可用/已用/额度池/套餐/签到/选用/刷新）、汇总口径只统计可用账号、导入凭证、筛选与排序、模型目录状态条 |
| **屏蔽词** | 与 WorkBuddy 相同的实现：命中词首字符后插入 U+200B，只改 system/developer 提示词、带注入标记（`# AGENTS.md instructions`、`<system-reminder>` 等）的用户文本和工具 `title`/`description`；默认关闭，词表可换（默认 85 词） |
| **auth 隔离** | 文件名前缀 `qoder-<uid>.json`；更名前遗留的 `qoderwork-*` 凭证在首次 configure 时自动迁移到新身份 |

## 安装

### 从 Release（推荐）

```bash
unzip qoder_<version>_linux_amd64.zip
mkdir -p /path/to/cliproxyapi/plugins/linux/amd64
cp qoder.so /path/to/cliproxyapi/plugins/linux/amd64/qoder.so
```

### 从源码

```bash
cd qoder
make build                       # CGO_ENABLED=1 go build -buildmode=c-shared
cp qoder.so /path/to/cliproxyapi/plugins/
```

### config.yaml

```yaml
plugins:
  enabled: true
  configs:
    qoder:
      enabled: true
      # 以下全部可省略，省略即用默认值
      realm_default: cn          # cn | global，新导入账号的默认区
      cosy_version: "1.1.75"     # 跟随官方 CLI 版本
      queue_wait: true           # 10605 模型排队时按 retryAfterSeconds 等待
      queue_max_wait: 600

# 模型别名（可选）
oauth-model-alias:
  qoder:
    - name: qmodel_38max
      alias: qoder/qwen3.8-max
```

## 使用

### 方式一：设备授权（推荐）

1. CPA 管理面板 → 认证文件 → **Qoder OAuth** 卡片 → 开始登录
2. 浏览器里登录对应区（CN 用 qoder.com.cn / Global 用 qoder.com）并授权
3. 插件轮询取回 token 落盘（`dt-`/`drt-`），如需领取 Pro 包会自动处理

### 方式二：PAT 导入

1. 官网 → 设置 → Personal Access Token → 创建（`pt-` 开头）
2. 插件面板（`/v0/resource/plugins/qoder/panel`）→「导入凭证」→ 粘贴 PAT → 导入（可选 `realm`）

### 面板

`/v0/resource/plugins/qoder/panel`（需 management key）

卡片只显示额度与状态：可用 / 已用 / 额度池 / 消耗占比、套餐、区、签到状态，以及「选用 / 刷新 / 签到」。
认证信息（token、有效期、uid、文件名）一律不上卡片——那属于 CPA 的「认证文件」模块。

## 凭证家族与刷新

```
auth 文件字段（可共存）：
  accessToken:   dt- (设备授权, ~30d) 或 jt- (PAT 交换, 24h)
  refreshToken:  drt- (~1y, 旋转)     或 jrt- (48h)
  personalToken: pt- (长期兜底)
  domain:        该凭证所属区（qoder.com.cn / qoder.com）
```

- 刷新路由按 `refreshToken` 前缀：`drt-` → `deviceToken/refresh`；`jrt-` → `jobToken/refresh`，失败回落 PAT 重新交换
- `personalToken` 不覆盖活跃 token，只做最终兜底
- 宿主 15 分钟 auto-refresh 与插件 22:00 keepalive 共用这套规则

## 协议要点（排查用）

- 推理：`POST {gateway}/algo/api/v2/service/pro/sse/agent_chat_generation?...&Encode=1`，body 先做自定义 base64 编码再签名
- 目录：`GET {gateway}/algo/api/v2/model/list?Encode=1`，body 形状由 `model_list_body` 决定（`kwm` = 编码后的 `{}`，与当前 CLI 一致；`empty` = 不带 body）
- 响应：双层 SSE 信封，`data: {"body":"<内层 chunk>","statusCodeValue":200}`；错误可能藏在 HTTP 200 里
- 额度单位是 credits（`usage.credits`，复数字段），`price_factor` 是倍率且会漂移，不要写死
- **不能带 `custom_model`**：空对象 `{}` 会被上游当成 BYOK 请求（声明了自定义模型但没给 endpoint），模型解析因此失败，所有模型统一返回 400 `Unsupported model format`。要带就带完整配置，否则整个字段省略
- **`business.version` 是业务协议版本（模板值 `0.1.43`），不是 `cosy-version`**：写成客户端版本会走到另一条路由
- `chat_context.extra.modelConfig` 要与顶层 `model_config` 保持一致，模板自带的 `key: "lite"` 必须覆盖成实际模型
- **每日积分走活动平台，不是旧的 sash 签到**：旧接口 `/sash/api/v1/me/daily-check-in/*` 现在对账号返回 `DISABLED`；真正的每日 100 在 `GET /sash/api/v1/me/campaigns`（找 `claimStatus=CLAIMABLE` 且 `benefit.kind=CREDITS`），领取是 `POST .../campaigns/{id}/claim`，幂等
- **活动接口必须用桌面身份**：`User-Agent: Qoder` + `cosy-clienttype: 10` + `cosy-version: <桌面版本>`（推理面才是 5）。缺这三个会静默返回空活动列表
- **活动接口不能带派生机器头**：`cosy-machine*` 六头全套是派生值时，上游判定为非官方客户端并把 CLAIMABLE 活动整条过滤掉；所以活动请求只发身份三件套，不带机器头
- 每日额度按 UTC+8 10:00 重置，自动签到因此排在 10 点后；启动晚于 10:00 时 `catchUpCheckin` 会补跑（领取接口幂等，重复调用只会返回已领）
- **一次性 Pro 升级包活动已下线**：`/sash/api/v1/me/pro-upgrade/claim` 现在返回 `404 NotFound`，相关按钮/接口/配置项已移除
- **调度线程里的 HTTP 不能走宿主桥**：桥在没有宿主请求上下文时回 `status 0`，定时任务会静默失败；`hostHTTPDoFallback` 先试桥、遇到 0 或错误就直连，签到/保活/额度这些元数据调用都改走它
- **模型目录按凭证缓存，不按区域**：上游的 `enable` 是账号级权利（免费/耗尽账号可能只认两个免费模型），按区域缓存会让「先刷新的那个账号」决定同区所有账号能看到什么。现在缓存 key 是 `区域:uid`，对外发布的模型列表是同区多账号的并集（任何账号 enable 就保留，全部 disable 才隐藏），生命周期与面板计数也按凭证算
- **模型倍率只能写在 `display_name`**：宿主的 `ModelInfo` 没有自定义字段，模型选择器、`/v1/models`（OpenAI/Claude/Codex/Gemini 各格式）能多显示的就只有它，这与 WorkBuddy 插件的做法一致（`Balanced · x0.59 credits`）。本插件的格式是 `Qwen3.8-Max · x0.2`，免费模型是 `Qwen3.8-Flash · x0.0`（CN 个人号的 Qwen3.8-Flash / DeepSeek-Flash 就是 0 倍率 + `is_free`）；目录没下发倍率时保持原名
- **屏蔽词作用域**：只改提示词与工具元数据，assistant/tool 消息、工具 `name`、`parameters` 一律不动——改这些会破坏多轮 tool-call 或让模型解析不了参数。用户自己的话（无注入标记）也不动，只处理 harness 塞进来的 AGENTS.md / system-reminder 块
- 目录里的 `promotion` 块（`discount_factor`、`before_promotion_price_factor`、`window_start/end`、`badge.zh` 如「错峰2折」）是官方客户端错峰角标的来源，目前只解析不使用：它和 `price_factor` 的口径不完全一致（`auto` 的 `2.0 × 0.2 = 0.4` 与下发的 `0.5` 对不上），不要自己相乘推「当前生效倍率」

## 已知边界

- **不实现活动平台（campaign）签到**：那条链路要求官方 UMID 原生组件生成机器身份，派生身份会被静默过滤。每日签到走 `/sash/api/v1/me/daily-check-in/*`。
- Global 区可用模型由服务端下发，`enable=false` 的模型不会出现在 `/v1/models`。

## License

MIT（based on WorkBuddy by lovingfish，见 [LICENSE](LICENSE)）