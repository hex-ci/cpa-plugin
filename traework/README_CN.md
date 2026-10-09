# TraeWork (CPA 插件)

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的 TraeWork CN 账号插件：用浏览器授权登录 TraeWork
（trae.cn）账号，并把账号令牌交给 CPA 管理。

插件跑在 CPA 宿主进程内（c-shared），不能独立运行。

## 当前版本做了什么

| 能力 | 状态 |
|---|---|
| 浏览器 OAuth 登录（面板发起，PKCE + 回环回调 / 粘贴回调 URL） | 已实现 |
| 令牌刷新（设备证明签名，与客户端同算法） | 已实现 |
| 认证文件识别与解析（`AuthParse`） | 已实现 |
| 账号面板（登录入口 + 额度 / 签到概览） | 已实现 |
| 每日签到（面板单账号 / 全部领取 + 每天 09:00、21:00 自动领取） | 已实现 |
| 模型目录（按账号拉取 `config_name` 列表） | 已实现 |
| 对话（`solo_work_lite` 通道，流式 / 非流式） | 已实现 |
| 工具调用（透传、跨分片合并、`finish_reason` 修正） | 已实现 |
| 排队通知（`request_wait_in_queue` 转为 SSE keep-alive） | 已实现 |

对话走 `solo_work_lite` 通道：请求把 OpenAI 的消息、`tools`、`tool_choice`、`temperature`、`max_tokens`
映射成该通道的形状（模型用 `config_name` 指定），响应把上游的命名事件流翻译成
`chat.completion[.chunk]`，包括 `reasoning_content`、跨分片的 `tool_calls` 合并与用量统计。
上游即使在工具调用时也把 `finish_reason` 报成 `stop`，插件会改写成 `tool_calls`。

## 安装

从插件商店安装，或手动放入 CPA 的插件目录：

```bash
unzip traework_<版本>_linux_amd64.zip
cp traework.so /path/to/cliproxyapi/plugins/
```

## 登录

登录在**插件面板**里收尾：在 CPA 认证页点「登录」拿到 TraeWork 授权链接、完成授权，再把授权后地址栏里的那条
URL 粘回面板（粘贴后点「提交」）。

1. CPA 管理端 → 插件认证 → `traework` → 点登录 → 打开返回的授权链接，按页面提示完成授权；
2. 授权后浏览器会被跳到 `http://127.0.0.1:<临时端口>/authorize?authCodeInfo=…`——**这个页面打不开是正常的**，
   那个端口只在「浏览器与 CPA 同机」时有意义。把地址栏里**这条完整 URL** 粘到插件面板的登录框，
   插件从中取出授权码；
3. 面板会自动接管 CPA 弹窗发起的那次登录（授权码绑定那次尝试的 PKCE verifier，所以必须粘给同一次尝试），
   兑换成功即落盘，账号出现在列表里。

为什么不在 CPA 自带的「回调 URL」框里粘：宿主只用 `code` + `state` 两个查询参数解析那个框，而 TraeWork 把
授权码放在回跳 URL 的 `authCodeInfo`（JSON）里、也没有 `state`，管理端对插件 provider 又不做抽取，所以那个框
对 TraeWork 结构上无法成功——登录只能在插件面板里收尾。

面板里点「开始登录」也能发起新的一次（没有进行中的登录时），完成后同样落盘。

## 配置项

| 配置键 | 类型 | 说明 |
|---|---|---|
| `management_key` | string | 可选。设置后，本插件的管理接口除宿主的鉴权外还要求这个 Bearer key（面板可用 `?key=` 传入）。也读环境变量 `TW_MANAGEMENT_KEY`。 |
| `proxy-url` | string | 可选。本插件所有上游请求走该代理（http/https/socks5/socks5h）。留空则沿用宿主的路由策略；配置非法时按失败关闭处理。 |
| `checkin_auto` | boolean | 可选，默认 `true`。每天 09:00 与 21:00（本地时间）自动领取每个账号的签到积分；晚上那趟是给早上撞上「人数过多」的账号补一次。面板上的「自动签到」开关只改本次运行，重启后以这里为准。 |
| `panel_base_url` | string | 可选。面板登录地址的前缀。**默认留空 = 用相对地址**（`/v0/resource/plugins/traework/panel?login=…`），由管理界面的实际来源解析，走反代/域名的部署也正确；填了（如 `https://cpa.example.cn`）则返回绝对地址，便于把链接复制到别处。 |

## 认证文件

登录成功后写入 `traework-<uid>.json`（宿主驱动的登录由宿主落盘，面板驱动的登录由插件经 `host.auth.save`
写入同一形状的文件），字段：

```json
{
  "type": "traework",
  "access_token": "…",
  "refresh_token": "…",
  "expires_at": 1893456000,
  "api_host": "https://api.trae.cn",
  "uid": "…",
  "region": "CN",
  "machine_id": "…",
  "device_id": "…",
  "client_id": "en1oxy7wnw8j9n"
}
```

`type` 是宿主路由认证文件给插件的依据；插件解析时保持文件路径作为记录标识（不回填 `ID`），避免同一文件
被重复登记。用户自定义字段（`weight`、`prefix`、`note` 等）在每次重写时原样保留。

## 面板

`/v0/resource/plugins/traework/panel`：每个账号一张卡片，显示**额度**——可用 / 已用 / 额度池与消耗进度条、
额度包明细（名称、已用 / 上限、到期日）、今日签到状态——外加登录入口。

额度数据来自三个只读接口：`/trae/api/v2/pay/ide_user_ent_usage`（用量与额度包）、
`/trae/api/v2/pay/ide_user_pay_status`（套餐身份、solo 通道可用性）、
`/trae/api/v2/ug/checkin_credits/status`（签到）。结果按账号缓存 60 秒，卡片上的「刷新额度」绕过缓存。

令牌有效期、区域、uid、认证文件名属于认证信息，在 CPA 管理端的「认证文件」模块里看，面板不重复展示。
签到可以直接在面板里领：每张卡片上有「签到 / 已签到」按钮，工具栏有「全部签到」和「自动签到」开关。领取本身是插件
唯一的写操作，上游对同一账号当天重复领取是幂等的；返回业务码 `9074`（当前参与用户太多）按**可重试**处理，不当失败。

自动签到默认开启，每天 09:00 和 21:00（本地时间）各跑一趟，第二趟补第一趟撞上繁忙窗口的账号；同一账号的领取用
互斥锁串起来，两个浏览器标签同时点也只会领一次。要关掉就在插件配置里设 `checkin_auto: false`（面板开关只影响本次
运行）。

## 开发

```bash
make build   # CGO_ENABLED=1 go build -buildmode=c-shared -o traework.so .
make test    # go test -race -count=1 ./... + node --test panel.test.js
make lint    # gofmt -l . + go vet + staticcheck
make clean
```

`panel.html` 通过 `go:embed` 打进二进制，改完必须重新 `make build`。

## 许可

MIT