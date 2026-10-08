# AGENTS.md — cpa-plugin

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 插件集合。三个相互独立的 Go 插件，各自是独立 Go module + 独立版本号，编译为 CPA 的 c-shared 插件（CGO 必需，插件跑在宿主进程内、不能独立运行）：

| 目录 | 插件 ID | 说明 |
|---|---|---|
| `workbuddy/` | `workbuddy` | Tencent CodeBuddy OAuth provider（CN + Global） |
| `qoderwork/` | `qoderwork` | QoderWork CN provider |
| `qwenwork/` | `qwenwork` | QwenWork CN provider |
| `traework/` | `traework` | TraeWork CN（trae.cn）provider |

## 开发命令

没有根级 Makefile，一切在插件目录内执行（`cd <dir>` 后）：

```bash
make build    # CGO_ENABLED=1 go build -buildmode=c-shared -o <id>.so .
make test     # go test -race -count=1 ./...   （WorkBuddy 额外跑 node --test panel.test.js）
make lint     # gofmt -l .（必须为空，fail-fast）+ go vet + 装了才跑 staticcheck/gocritic/unparam
make clean    # 删 <id>.so / <id>.h / bin/ dist/
make release  # 交叉编 linux/amd64+arm64；无 osxcross 只有 linux 成功
```

## CI / Release

`.github/workflows/build.yml`：push/PR 出 artifact，`<id>-v*` tag 或 workflow_dispatch 触发该插件独立 Release。

**发版流程**（版本号已在 `<id>/VERSION` 与 `<id>/main.go` 的 `version` 常量中）：

```bash
# 1. 确认版本号已 bump 并推送到 main（两处：<id>/VERSION + main.go 的 var version）
git push origin main

# 2. 打 tag 并推送（触发 release）
make -C workbuddy tag          # 读 workbuddy/VERSION → workbuddy-v0.9.4
make -C qoderwork tag          # → qoderwork-v0.4.2
make -C qwenwork tag           # → qwenwork-v0.1.5
# 等价手动写法：git tag -a workbuddy-v0.9.4 -m "WorkBuddy v0.9.4" && git push origin workbuddy-v0.9.4

# 3. 观察：gh run watch（或 gh run list --repo hex-ci/cpa-plugin）
```

- tag 推上去后 CI 自动：全平台构建 → 创建 GitHub Release（7 平台 zip + checksums.txt）→ `sync-registry.py` 把该插件的 direct 安装块（URL+sha256+size）写回 `registry.json` 并 commit 回 main。
- 只想重发已有版本（不建新 tag）：用 workflow_dispatch，填 `plugin` + `version`；version 留空则取该插件的 `VERSION` 文件。
- 每插件独立版本，版本号存于 `<id>/VERSION`（tag 名由它派生，`make tag` 已自动读取）。
- `registry.json`（插件商店源）用 `python3 scripts/validate-registry.py registry.json` 校验。
- CI 的 test/build matrix 覆盖 **WorkBuddy + QoderWork + QwenWork + TraeWork** 四个插件（WorkBuddy 与 TraeWork 额外跑 `node --test panel.test.js`）。
- 四个插件共用同一仓库，插件商店无法用 `github-release` 类型：宿主按 `/releases/latest`（全仓库语义）解析，且 tag 必须能归一为纯版本号，`<id>-vX.Y.Z` 解析失败。因此 registry 走 `direct`（schema_version 2），artifacts 由 release job 的 `.github/scripts/sync-registry.py` 自动生成并 commit 回 main。

## 约定

- **显示名 vs 标识符**：面向用户的文案（面板标题、`Metadata.Name`、README、日志）一律用 CamelCase 显示名 **WorkBuddy / QoderWork / QwenWork**；标识符（插件 ID、`providerName` 常量、目录/module/文件名、`plugins.configs.<id>` 配置键、`/v0/management/plugins/<id>/*` 路由、`/v0/resource/plugins/<id>/*` 资源路由、auth 文件名前缀 `<id>-<uid>.json`、tag `<id>-vX.Y.Z`、commit scope、CI 插件键）保持小写。插件 ID 由 .so 文件名派生（版本化名 `<id>-v<version>.so` 会剥离版本段），`Metadata.Name` 只做显示，宿主不用于任何匹配。
- 每个插件目录完全自包含：自己的 `go.mod`（go 1.26）、`go.sum`、`VERSION`、`Makefile`、`panel.html`、`README.md`/`README_CN.md`、`LICENSE`；插件之间不共享依赖。
- module 路径统一为 `github.com/hex-ci/cpa-plugin/<id>`。
- **logo 一律入本仓库**：图标文件存 `<id>/assets/`，`main.go` 的 `pluginLogoURL` 与 `registry.json` 的 `logo` 均引用 `https://raw.githubusercontent.com/hex-ci/cpa-plugin/main/<id>/assets/<file>`，不直连上游 CDN（防对方 URL 变更导致图标失效）。配套 `<id>` 包内断言前缀 + 本地文件存在的单测。（现存例外：workbuddy/qoderwork 仍指向第三方 ai-icon 仓库，待逐插件迁移。）
- `panel.html` 经 `go:embed` 嵌入二进制——改完必须重新 `make build`，gofmt/vet 不碰它。
- 所有上游 HTTP 走宿主桥（`host_auth.go`/`host_bridge.go`），插件内不直接用 net/http。
- commit 风格：Conventional Commits + 插件 scope，如 `feat(qwenwork): ...`、`fix(workbuddy): ...`，信息可用中文。
- 探测/临时脚本、token/key 不进仓库；打印 token 需脱敏。

## 坑

- **验证改动别直接替换正在服务的网关插件。** 插件跑在宿主进程内，改 `.so` 后由宿主重载。先在隔离的 CPA 实例上验证新 `.so` 再上正式网关，避免打断运行中的流量。
- 裸 `go build`（不带 `-buildmode=c-shared`）会在 main 目录生成无扩展名二进制 `<id>`，已由 `.gitignore` 精确条目排除；新增插件需补对应条目。
- 从现有插件 `cp -r` 改名换皮时，四处残留要主动查：logo URL（目标仓库可能没有同名图，先抓官网真实 src）、token 前缀注释（各 provider 不同）、硬编码端口（公开代码一律用官方默认 8317，勿带任何私有端口值）、裸 build 二进制。
- `registry.json` 各插件的 `version` 与 artifacts 直链由 release job 自动同步；手工改 registry 后记得跑 validator，并确认 version 与 `<id>/VERSION` 一致。
- WorkBuddy 企业额度逻辑：fork 的 `isEnterpriseAccount`（stored `enterpriseId` 非空）与上游 `enterprise_credits: true` YAML 开关注入到同一 billing 触发条件，改动前先读 `workbuddy/billing.go` 里的融合写法，别拆坏任意一边。
