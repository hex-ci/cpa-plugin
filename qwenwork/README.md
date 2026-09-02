# QwenWork (千问办公) CPA 插件

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 **QwenWork（千问办公，gateway.qwenwork.cn）** Provider 插件：设备授权登录、COSY 签名推理、动态模型、积分/套餐面板、token 自动保活。

千问办公（QwenWork）与 QoderWork 同属阿里 Qoder 底层（RSA 公钥相同、COSY 签名同构、device flow 同构），但差异明显：单网关域名、明文 JSON body（无 Encode=1）、cosyVersion 1.1.18、**无 PAT/jobToken 路径**。

## 功能

| 能力 | 说明 |
|---|---|
| **登录** | OAuth 设备授权（PKCE），浏览器授权，JWT access + `ory_rt_` refresh token |
| **COSY 推理** | RSA 包 AES 会话密钥 + MD5 请求签名，对接 gateway.qwenwork.cn SSE 流式 |
| **动态模型** | 静态回退（pro/flash/qwen3.8-max-preview）+ `/algo/api/v2/model/list` 动态刷新 |
| **token 保活** | 22:00 定时刷新；`POST /api/v1/deviceToken/refresh`（body `{refresh_token, target:"c"}`） |
| **积分/套餐** | `GET /api/v1/adapter/user/account-context?include=user,plan,quota`（一次拿到 user+plan+quota；企业号有固定额度，个人免费号只有 `remaining`、无固定总额） |
| **面板** | 积分、套餐、账号管理（`/v0/resource/plugins/qwenwork/panel`） |

## 登录流程

1. CPA 插件面板 / OAuth 入口触发设备授权 → 插件生成 PKCE（challenge/verifier）+ nonce + machine_id，拼出授权页 URL：

   ```
   https://gateway.qwenwork.cn/device/selectAccounts?challenge=…&challenge_method=S256&nonce=…&machine_id=…&client_id=e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb&redirect_uri=qwenwork-cn%3A%2F%2F
   ```

2. 浏览器打开授权页 → 登录（阿里云 SSO / 钉钉企业登录）→ 授权。
3. 插件轮询 `GET /api/v1/deviceToken/poll?nonce=…&verifier=…&challenge_method=S256` 直至拿到 JWT（access）+ `ory_rt_`（refresh）落盘。

## 构建 / 测试

```bash
make build   # CGO_ENABLED=1 go build -buildmode=c-shared -o qwenwork.so .
make test    # go test -race -count=1 ./...
make lint    # gofmt + go vet
```

## 安装

```yaml
plugins:
  enabled: true
  configs:
    qwenwork:
      enabled: true
```

## 端点一览（gateway.qwenwork.cn 单网关）

| 用途 | 端点 |
|---|---|
| 授权页 | `GET /device/selectAccounts` |
| 轮询 grant | `GET /api/v1/deviceToken/poll` |
| 刷新 token | `POST /api/v1/deviceToken/refresh` |
| 用户信息 | `GET /api/v1/userinfo` |
| 推理 | `POST /algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common` |
| 模型列表 | `GET /algo/api/v2/model/list` |
| 积分/套餐 | `GET /api/v1/adapter/user/account-context?include=user,plan,quota` |
