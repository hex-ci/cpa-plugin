# CPA 插件仓库

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 插件集合。当前提供 **WorkBuddy / CodeBuddy**、**QoderWork (CN)**、**QwenWork (CN)** 与 **TraeWork (CN)** 四个 OAuth Provider。

## 插件

| ID | 说明 | 源码 |
|---|---|---|
| `workbuddy` | Tencent CodeBuddy OAuth、动态模型、executor、CN 每日签到、Global 专家包、积分面板、可选积分调度 | [workbuddy/](workbuddy/) |
| `qoderwork` | QoderWork CN（qoder.com.cn）：OAuth 设备授权 + PAT 双登录（可共存）、COSY 签名推理、动态模型、每日签到、积分面板、token 保活 | [qoderwork/](qoderwork/) |
| `qwenwork` | QwenWork CN（gateway.qwenwork.cn）：OAuth 设备授权登录、COSY 签名推理、动态模型、积分/套餐面板、token 保活（无签到/无 PAT） | [qwenwork/](qwenwork/) |
| `traework` | TraeWork CN（trae.cn）：浏览器 OAuth 登录（PKCE + 设备证明刷新）、认证文件识别解析、动态模型目录、`solo_work_lite` 通道流式对话与工具调用、额度卡片面板、每日签到（手动 + 每天两次自动） | [traework/](traework/) |

## 多架构 Release

每个插件独立版本发 Release（tag `<id>-v*`），产物为 CPA 插件商店标准格式：

```text
<id>_<version>_linux_amd64.zip      # zip 根目录: <id>.so
<id>_<version>_linux_arm64.zip
<id>_<version>_darwin_amd64.zip     # <id>.dylib
<id>_<version>_darwin_arm64.zip
<id>_<version>_windows_amd64.zip    # <id>.dll
<id>_<version>_windows_arm64.zip
<id>_<version>_freebsd_amd64.zip
checksums.txt
```

命名规则与官方一致：`ArchiveName(id, version, goos, goarch) = {id}_{version}_{goos}_{goarch}.zip`
（见 CLIProxyAPI `internal/pluginstore`）。

CI：push / PR 全量构建（只出 artifacts）；tag `<id>-v*`（如 `traework-v0.1.0`）或 dispatch 触发**该插件独立版本**的 Release。

## 安装（linux/amd64 示例）

```bash
# 从 Release 下载
unzip qoderwork_<version>_linux_amd64.zip
# 扁平 plugins 目录（常见 docker 挂载）
cp qoderwork.so /path/to/cliproxyapi/plugins/qoderwork.so
# 或平台子目录布局
# mkdir -p plugins/linux/amd64 && cp qoderwork.so plugins/linux/amd64/
```

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    workbuddy:
      enabled: true
    qoderwork:
      enabled: true
    qwenwork:
      enabled: true
    traework:
      enabled: true
```

## 通过插件商店安装 / 更新

本仓库的 `registry.json` 可直接作为 CPA 插件商店的自定义源使用。四个插件以
`direct`（schema_version 2）方式声明各平台产物直链与 sha256——每次 Release 由
CI 自动刷新，用户在商店 UI 即可一键安装/更新，无需手工下载。

```text
https://raw.githubusercontent.com/hex-ci/cpa-plugin/main/registry.json
```

添加后在商店 UI 安装/更新 **WorkBuddy**、**QoderWork**、**QwenWork** 和 **TraeWork**。
