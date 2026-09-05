# AGENTS.md — cpa-plugin

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 插件集合，fork 自上游 `Sliverkiss/cpa-plugin`。三个相互独立的 Go 插件，各自是独立 Go module + 独立版本号，编译为 CPA 的 c-shared 插件（CGO 必需，插件跑在宿主进程内、不能独立运行）：

| 目录 | 插件 ID | 说明 |
|---|---|---|
| `workbuddy/` | workbuddy | Tencent CodeBuddy OAuth provider（CN + Global） |
| `qoderwork/` | qoderwork | QoderWork CN provider |
| `qwenwork/` | qwenwork | QwenWork CN provider |

## 开发命令

没有根级 Makefile，一切在插件目录内执行（`cd <dir>` 后）：

```bash
make build    # CGO_ENABLED=1 go build -buildmode=c-shared -o <id>.so .
make test     # go test -race -count=1 ./...   （workbuddy 额外跑 node --test panel.test.js）
make lint     # gofmt -l .（必须为空，fail-fast）+ go vet + 装了才跑 staticcheck/gocritic/unparam
make clean    # 删 <id>.so / <id>.h / bin/ dist/
make release  # 交叉编 linux/amd64+arm64；无 osxcross 只有 linux 成功
```

## CI / Release

`.github/workflows/build.yml`：push/PR 出 artifact，`<id>-v*` tag 或 workflow_dispatch 触发该插件独立 Release。

- Release tag 必须是 `<id>-vX.Y.Z`（如 `qoderwork-v0.4.1`）。`make tag` 生成的是 `vX.Y.Z`，不触发 release——手动 `git tag <id>-vX.Y.Z`。
- 每插件独立版本，版本号存于 `<id>/VERSION`。
- `registry.json`（插件商店源）用 `python3 scripts/validate-registry.py registry.json` 校验。
- ⚠️ CI 的 test/build matrix 硬编码只覆盖 **workbuddy + qoderwork**，qwenwork 未接入 CI——改 qwenwork 必须本地 `make test` 验证。

## 约定

- 每个插件目录完全自包含：自己的 `go.mod`（go 1.26）、`go.sum`、`VERSION`、`Makefile`、`panel.html`、`README.md`/`README_CN.md`、`LICENSE`；插件之间不共享依赖。
- module 路径：qoderwork/qwenwork 是 `github.com/Sliverkiss/cpa-plugin/<id>`；**workbuddy 例外**，为 `github.com/sliverkiss/workbuddy-plus`（历史遗留，勿"修正"）。
- `panel.html` 经 `go:embed` 嵌入二进制——改完必须重新 `make build`，gofmt/vet 不碰它。
- 所有上游 HTTP 走宿主桥（`host_auth.go`/`host_bridge.go`），插件内不直接用 net/http。
- commit 风格：Conventional Commits + 插件 scope，如 `feat(qwenwork): ...`、`fix(workbuddy): ...`，信息可用中文。
- 探测/临时脚本、token/key 不进仓库；打印 token 需脱敏。

## 坑

- **验证改动别直接替换正在服务的网关插件。** 插件跑在宿主进程内，改 `.so` 后由宿主重载。先在隔离的 CPA 实例上验证新 `.so` 再上正式网关，避免打断运行中的流量。
- 裸 `go build`（不带 `-buildmode=c-shared`）会在 main 目录生成无扩展名二进制 `<id>`，已由 `.gitignore` 精确条目排除；新增插件需补对应条目。
- 从现有插件 `cp -r` 改名换皮时，四处残留要主动查：logo URL（目标仓库可能没有同名图，先抓官网真实 src）、token 前缀注释（各 provider 不同）、硬编码端口（公开代码一律用官方默认 8317，勿带任何私有端口值）、裸 build 二进制。
- `registry.json` 的 version 会与 `<id>/VERSION` 不同步（fork 与 upstream 分叉，实测 qoderwork/qwenwork 的 registry 值滞后）——release 前核对。
- workbuddy 企业额度逻辑：fork 的 `isEnterpriseAccount`（stored `enterpriseId` 非空）与上游 `enterprise_credits: true` YAML 开关注入到同一 billing 触发条件，改动前先读 `workbuddy/billing.go` 里的融合写法，别拆坏任意一边。
