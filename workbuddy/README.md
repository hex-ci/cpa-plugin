# WorkBuddy Plugin for CLIProxyAPI

A [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) plugin that
provides **Tencent CodeBuddy** (`copilot.tencent.com` CN and `workbuddy.ai`
Global) as a native OAuth provider: per-account dynamic model discovery,
streaming execution, credit-aware scheduling, daily check-in automation, and a
built-in management dashboard.

[中文文档 → README_CN.md](README_CN.md)

## Features

- **OAuth login** — multi-account `workbuddy-<uid>.json` auth files via the
  host's auth store. CN and Global realms share one plugin, one config block.
- **Model catalog**: by default the plugin discovers and caches each
  authenticated account's model entitlements. An optional authoritative YAML
  list can replace WorkBuddy discovery. Both modes enrich missing metadata from
  models.dev. Host-side `oauth-model-alias` / `oauth-excluded-models` config
  still applies.
- **Executor** — OpenAI-compatible chat completions, both streaming (real SSE
  via `host.stream.emit`) and non-streaming (SSE folded into a single
  completion). `tool_choice` normalization, Claude Code template sanitization,
  and per-realm system-message injection are built in.
- **Credit lifecycle** — CN accounts auto-`disabled` when credits run out and
  re-enabled when a check-in restores them. Global accounts are deleted on
  exhaustion (one-shot trial quota). Hard credit errors from the executor
  trigger an immediate reconcile.
- **Daily check-in** — CN accounts are checked in at 09:00 and 21:00 local
  time (configurable). Manual "check in all" from the panel. Per-account
  mutex prevents duplicate claims from racing browser tabs.
- **Trial claim** — Global accounts can claim the one-time 250-credit expert
  trial pack from the panel.
- **Dashboard** — embedded panel at `/v0/resource/plugins/workbuddy/panel`
  with credits progress bars, plan badges, exhausted/disabled flags, region
  filter, and credential import.
- **Scheduler** (optional) — `scheduler_mode: credits` makes the plugin pick
  the panel-selected account; `off` (default) defers to CPA's built-in
  scheduler entirely.
- **Usage forwarding** — implements `UsagePlugin`; every request's usage
  record is forwarded to a configurable CPAMP endpoint. No record is sent
  unless a URL+key are configured.

## Quickstart

### 1. Install the plugin

Drop the compiled `workbuddy.so` into CPA's plugin directory:

```bash
cp workbuddy.so /path/to/cliproxyapi/plugins/
```

For multi-arch deployments use the platform subdirectory convention:

```
plugins/
  linux/amd64/workbuddy.so
  linux/arm64/workbuddy.so
  darwin/arm64/workbuddy.so
```

### 2. Enable in `config.yaml`

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    workbuddy:
      enabled: true
```

### 3. Sign in

Open the WorkBuddy panel from CPA's sidebar (or hit
`/v0/resource/plugins/workbuddy/panel` directly) and click **登录** to start
the OAuth flow. Repeat for each account you want to add — the plugin writes
one `workbuddy-<uid>.json` per account to the auth store.

### 4. Use it

Call the OpenAI-compatible endpoint with any alias that maps to a model in the
account's discovered catalog:

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "client-alias",
    "messages": [{"role": "user", "content": "hi"}],
    "stream": true
  }'
```

## Configuration

All fields are optional and live under `plugins.configs.workbuddy`.

```yaml
plugins:
  configs:
    workbuddy:
      enabled: true

      # Optional authoritative model ID list. Entries must be single-line YAML strings.
      # A non-empty list is the complete catalog: WorkBuddy catalog HTTP and
      # catalog cache reads/writes are bypassed, while models.dev metadata
      # fetch, ETag, and last-good cache behavior remains active.
      # Missing, null, or [] keeps dynamic WorkBuddy discovery.
      models: []

      # Optional plugin-level proxy for every HTTP request initiated by WorkBuddy.
      # Supported schemes: http, https, socks5, socks5h.
      # Empty/unset inherits existing CPA routing. Invalid settings and runtime
      # proxy failures fail closed and never fall back to CPA or a direct route.
      proxy-url: ""

      # Daily check-in automation for CN accounts (default true).
      # Runs at 09:00 and 21:00 local time.
      checkin_auto: true

      # Credit lifecycle: disable CN on exhaust, delete Global on exhaust,
      # re-enable CN after check-in restores credits (default true).
      lifecycle_auto: true

      # Scheduler behavior (default "off"):
      #   off     → defer to CPA's built-in scheduler entirely
      #   credits → plugin picks the panel-selected account (with fallback
      #             when that account is exhausted / disabled)
      scheduler_mode: "off"

      # CPAMP usage forwarding. Both must be set for any record to be sent.
      # Falls back to USAGE_REPORT_URL / USAGE_REPORT_KEY /
      # CPAMP_ADMIN_KEY env vars or docker secret files when unset here.
      usage_report_url: "http://cpa-manager-plus:18317/v0/management/usage/import"
      usage_report_key: ""

      # Plugin-layer management auth. When set, all mutating endpoints under
      # /v0/management/plugins/workbuddy/* require this Bearer token.
      # When empty (default) the host's management middleware is the only
      # guard. Also readable from WB_MANAGEMENT_KEY env var.
      management_key: ""
```

Model aliases and exclusions are handled natively by CPA's
`oauth-model-alias` and `oauth-excluded-models` config — no plugin-side
duplication needed.

When `proxy-url` is set, chat, billing/check-in/trial, token refresh,
`executor.http_request`, usage forwarding, OAuth state/token/account calls,
and usage endpoint probes all use that proxy. Because CLIProxyAPI v7.2.30's
native-plugin host HTTP API has no per-request proxy override, explicit plugin
proxy traffic is sent by the plugin and does not appear in CPA's request-log.
The OAuth URL opened by the browser is not fetched by the plugin; the browser
needs its own network route.

## Model catalog

`model.static` is an offline fallback contract. It returns only `auto` with the
generic default metadata template and never reads account caches or performs a
network request.

When `models` is a non-empty YAML sequence of single-line strings, it is the complete model
list in the configured order. `model.for_auth` validates the account as usual,
but does not request WorkBuddy catalog HTTP, read or write the per-auth WorkBuddy
catalog cache, or delete an existing cache. models.dev metadata still uses the
same online fetch, ETag, persistence, and last-good fallback. Fresh metadata
publishes `ready`; a valid cached metadata fallback publishes `stale`; no valid
metadata publishes `failed` with an empty model response. Missing `models`,
`models: null`, and `models: []` restore dynamic discovery and can reuse the
existing WorkBuddy catalog cache.

In dynamic mode, the first `model.for_auth` call for an account is the
authenticated bootstrap boundary:

1. WorkBuddy `GET /v3/config` supplies the account's entitled model IDs and
   serving fields. Only an HTTP 404 or 405 falls back to the legacy
   `GET /console/enterprises/personal/models` endpoint; other failures do not.
2. [models.dev `/models.json`](https://models.dev/models.json) supplies
   canonical metadata for fields that WorkBuddy omitted. It never determines
   account entitlement or overrides WorkBuddy serving fields.
3. Source responses are validated before they replace the persistent cache and
   an immutable per-account catalog is published.

The cache root comes from `os.UserConfigDir()` rather than a hard-coded
platform path:

```plaintext
<user-config-dir>/CLIProxyAPI/workbuddy/model-catalog/
  metadata.json
  metadata.json.bak
  models/
    <identity-sha256>.json
    <identity-sha256>.json.bak
```

For example, the default path for Linux root is
`/root/.config/CLIProxyAPI/workbuddy/model-catalog/`.

A first bootstrap with no valid cache is fail-closed: both sources must fetch,
validate, and persist successfully before the account becomes `ready`.
`model.for_auth` returns an empty successful model response if bootstrap fails.
On later process starts, the first call still attempts both refreshes. If a
refresh fails but that source has a valid last-good cache, the account starts
`stale` and uses that cache. A source without either a fresh result or valid
last-good cache leaves the account `failed`.

Only `ready` and `stale` are executable states. `not_started`, `loading`, and
`failed` are blocked at all executor entry points with a fixed, redacted
`not_ready` response and HTTP 503; the scheduler also excludes them. The panel
keeps loading and exposes `model_status` with per-account source and timestamp
fields. Its fixed error categories are `auth_invalid`,
`workbuddy_transport`, `workbuddy_http`, `workbuddy_schema`,
`models_dev_transport`, `models_dev_http`, `models_dev_schema`, `cache_read`,
and `cache_write`; raw upstream errors, response bodies, credentials, and cache
paths are not exposed.

There is no background or request-time refresh, panel retry endpoint, or
Enterprise custom-model source. A new process start or an auth, token, or
plugin-config generation change supplies the next bootstrap opportunity.

## Lifecycle

| State | CN account | Global account |
|---|---|---|
| Credits > 0 | active | active |
| Credits = 0 | `disabled: true` (auth file kept) | auth file **deleted** |
| Check-in restores credits | re-enabled | n/a (already deleted) |
| Trial available | n/a | claimable once per account |
| Unknown credits | untouched (never mis-kill) | untouched |

Hard credit errors from the executor (status 402, "insufficient credits",
"积分不足", etc.) trigger an immediate reconcile of the failing account.

## Development

Requires Go 1.26+ (matches CPA).

```bash
# Build the plugin
go build -buildmode=c-shared -o workbuddy.so .

# Run tests
go test -race ./...

# Lint
gofmt -l .
go vet ./...
```

With `proxy-url` empty, the shared request helpers preserve their existing
routing: host-bridged requests use CPA's request-log and transport policy,
while the established OAuth, usage-probe, Windows, and old-host direct paths
remain unchanged. With `proxy-url` set, every plugin-initiated HTTP request
uses the plugin proxy instead and never falls back.

See [docs/development.md](docs/development.md) for the full workflow and
[docs/architecture.md](docs/architecture.md) for the module map.

## License

MIT — see [LICENSE](LICENSE).
