# WorkBuddy Plugin Architecture

Module map and data flow for the workbuddy plugin. The plugin is a single
`package main` compiled as a c-shared `.so`, loaded by CPA at startup and
driven via the `pluginabi` RPC interface.

## Capability surface (declared in `wbRegistration`)

| Capability | Implementation file | What it does |
|---|---|---|
| `ModelProvider` | `models.go`, `model_source_*.go`, `model_store.go`, `model_readiness.go` | Static `auto` fallback, configured authoritative catalog or authenticated per-account discovery, models.dev enrichment, persistent last-good cache, readiness gates, alias reverse-resolution, `oauth-excluded-models` filter |
| `AuthProvider` | `oauth.go`, `auth_parse.go` (in `authfile.go` / `main.go`) | OAuth login flow (CN + Global), token refresh, auth file parse |
| `Executor` | `executor.go`, `stream.go`, `payload.go` | Chat completions, streaming SSE pump, request body rewriting |
| `Scheduler` | `scheduler.go`, `active_auth.go` | Optional panel-selected account routing (`scheduler_mode: credits`) |
| `ManagementAPI` | `management.go`, `panel.go`, `checkin.go`, `credits_handler.go`, `billing.go`, `usage_config.go`, `host_auth.go` | Dashboard, manual check-in, credits query, import credential, config |
| `UsagePlugin` | `usage.go` | Forward every request's usage record to CPAMP |

## File map (by responsibility)

```
main.go           C ABI exports + handleMethod dispatch + registration
registration.go   (in main.go) wbRegistration + capabilities + ConfigField
envelope.go       (in main.go) envelope/okEnvelope/errorEnvelope helpers

host_call.go      hostCall + hostBridgeUnwrap (RPC to CPA host)
host_bridge.go    hostHTTPDo/DoStream/Read/Close + hostStreamReader + inherited Direct fallbacks
proxy.go          atomic inherit/proxy/blocked snapshot + proxy transport/error redaction

executor.go       handleExecExecute / handleExecStream
stream.go         streamEmit/Close + pumpUpstreamStream + collectUpstreamStream + aggregate*
payload.go        prepareUpstreamBody + InPlace mutators (forceStream/normalizeTools/
                  rewriteSystem/ensureSystemMessage/rewriteModel) + legacy wrappers

models.go         static auto/default metadata + model.for_auth response + alias/
                  exclusion handling
model_source_workbuddy.go WorkBuddy /v3/config + 404/405-only legacy fallback + validation
model_source_modelsdev.go models.dev /models.json parser, matching, and additive enrichment
model_store.go     separated metadata/per-auth JSON caches + identity hash + atomic .bak writes
model_readiness.go per-auth bootstrap flights + global metadata flight + immutable readiness snapshots

oauth.go          handleStartLogin/PollLogin/RefreshAuth + newLoginClient + doJSON
auth_parse.go     (in authfile.go / main.go) handleParseAuth + parseStored + toAuthData

usage.go          handleUsage + publishUsage + forwardUsageToCPAMP + sseUsageCollector

management.go     managementRegistration + handleManagement + auth/ratelimit
panel.go          buildDashboardEx + summarizeCredits + servePanel + panelHTML
checkin.go        schedulerLoop + runAutoCheckin + handleManualCheckin + 
                  classifyCheckinTargets/executeCheckinBatch/summarizeCheckinResults
credits_handler.go handleImportAuth/CheckinConfig/ClaimTrial/SelectAuth/CreditsQuery
billing.go        fetchCheckinStatus/fetchUserResource/fetchPaymentType/
                  performCheckinCall/performTrialCall + JSON helpers
usage_config.go   configure + resolveUsageReport + probe* + config vars
host_auth.go      hostAuthList/Get/GetBundle (host auth-store RPC)

lifecycle.go      reconcileOneAccount/AllAccounts/AfterExecutorError/ByUID +
                  applyExhaustedPolicy + lifecycleState
policy.go         lifecycleAction decisions (pure functions) + displayNote + labelForAuth
authfile.go       authFileNameFor/sanitizeUIDForFileName/hostAuthPersist/deleteAuth +
                  path safety checks

scheduler.go      handleSchedulerPick + candidateDisabled + cachedCreditsScore
active_auth.go    activeAuthID sticky state + pickActiveAuth + clearActiveAuthIfMatch

cache.go          accountCache + accountDetailFlight singleflight + prune
redact.go         redactSecrets + 4 regex + truncateRedacted + truncate
headers.go        (in main.go / oauth.go) commonHeaders/backendHeaders/billingHeaders
stored.go         (in main.go / models.go) storedAuth/storedTokens/storedAccount
```

## Data flow

### Chat completion (streaming)

```
client -> CPA -> plugin.handleExecStream
  -> read immutable per-auth readiness snapshot
      -> ready/stale: continue without waiting or refreshing
      -> not_started/loading/failed: return redacted not_ready HTTP 503
  -> parseStored(auth file)
  → resolveUpstreamModel(alias → upstream id)
  → prepareUpstreamBody (single JSON pass: forceStream + normalizeTools +
                          rewriteSystem + ensureSystemMessage + rewriteModel)
  → hostHTTPDoStream
      → proxy-url empty: inherited CPA host bridge/direct compatibility path
      → proxy-url set: immutable plugin proxy client (no fallback)
      → invalid proxy-url: blocked before network
  → pumpUpstreamStream (goroutine)
      → hostStreamReader → bufio.Scanner → SSE lines
      → cleanChunkJSON per line
      → streamEmit → CPA → client
      → sseUsageCollector collects terminal usage object
  → publishUsage → forwardUsageToCPAMP (async, via host bridge)
  → invalidateAccountCredits (async)
  → host calls UsagePlugin.HandleUsage → handleUsage → forwardUsageToCPAMP (sync)
```

### Authenticated model bootstrap

`model.static` is independent of this flow and returns only the generic `auto`
fallback. The first `model.for_auth` for each auth performs the authenticated
bootstrap:

```plaintext
model.for_auth
  -> parse credentials and realm; carry host_callback_id to source requests
  -> derive identity JSON: provider + realm + UID + EnterpriseID
      -> include AuthID only when UID is absent
      -> SHA-256 the JSON; credentials never enter the identity or file name
  -> capture config generation and the immutable `models` list together
  -> enter the per-auth flight
  -> non-empty configured `models`
      -> use ID-only serving facts in YAML order
      -> skip WorkBuddy HTTP, catalog cache reads/writes, and identity catalog flights
  -> otherwise use the WorkBuddy source
      -> GET /v3/config
      -> only HTTP 404/405: GET /console/enterprises/personal/models
      -> validate the complete entitlement/serving snapshot
      -> persist the modelCatalogCacheV1 source snapshot
  -> enter or join the process-global models.dev flight
      -> GET https://models.dev/models.json, with cached ETag when available
      -> validate the canonical metadata source snapshot
      -> persist the metadataCacheV1 source snapshot
  -> match serving IDs to canonical records and enrich missing serving fields
  -> check config generation + token SHA-256 + identity SHA-256
  -> atomically publish an immutable ready or stale snapshot
```

The dynamic data path is `source -> validate -> persist source snapshots ->
enrich -> publish`. Fresh data is never enriched or published if validation or
persistence fails. A non-empty configured `models` list replaces only the
WorkBuddy source snapshot: it is never persisted as a catalog, existing
WorkBuddy cache files remain untouched, and models.dev retains the same online,
ETag, persistence, cold-start fail-closed, and warm last-good behavior. Its
`model_source` is `config` with an empty `models_fetched_at`; fresh metadata is
`ready`, cached metadata fallback is `stale`, and no valid metadata is `failed`.
Source requests use the callback-aware host bridge, but the inherited callback
wire does not guarantee an overall request timeout.

The stores are separated under the root derived from `os.UserConfigDir()`:

```plaintext
CLIProxyAPI/workbuddy/model-catalog/
  metadata.json                    global canonical metadata
  metadata.json.bak                previous valid metadata primary
  models/<identity-sha256>.json    one WorkBuddy catalog per auth identity
  models/<identity-sha256>.json.bak
```

`metadataCacheV1` contains `schema_version`, opaque `etag`, `fetched_at`, and
canonical `records`; it has no realm or identity. It validates its own schema,
timestamp, and canonical-record content. `modelCatalogCacheV1` contains
`schema_version`, `identity_sha256`, `realm`, `fetched_at`, `endpoint`, and the
validated WorkBuddy `models`. Identity and realm validation applies only to
this per-auth model cache.

Each successful replacement first preserves the previous valid primary as
`.bak`. With no valid cache, either source failing leaves the auth `failed`;
with a valid last-good for every failed source, the auth is published `stale`.

The per-auth flight serializes WorkBuddy refreshes for one auth while allowing
different auths to initialize concurrently. Configured catalogs do not enter
the shared-identity WorkBuddy catalog flight. The global flight shares one
models.dev refresh. Reconfigure publishes the immutable feature snapshot and
increments the model config generation under the same commit lock. The
generation gate prevents a late request for an old config, token, or identity
from saving or publishing over current state.

Panel, executor, and scheduler only read immutable snapshots. They do not wait
on flights or perform network or disk work. Executor accepts `ready` and
`stale`; other states return redacted `not_ready` HTTP 503. Scheduler considers
only `ready` and `stale`, and defers to CPA when no eligible candidate exists.
There is no background refresh, panel retry endpoint, or Enterprise custom
model source.

### Daily check-in (CN, 09:00 / 21:00)

```
schedulerLoop → runAutoCheckin (sem=4 concurrent)
  → processAutoCheckinAccount per account
      → fetchCheckinStatus → performCheckinCall if needed
      → update accountCache (merge, not wipe)
      → reconcileOneAccount → applyExhaustedPolicy
          → policy.go decides: disable (CN) / delete (Global) / reenable (CN)
          → authfile.go applies: hostAuthPersist / deleteAuth
```

### Dashboard load

```
panel.html → /v0/management/plugins/workbuddy/accounts
  → handleManagement (auth + ratelimit)
  → buildDashboardEx (concurrent cachedAccountDetails per account, sem=4)
      → accountDetailFlight singleflight dedups concurrent fetches
      → accountCache hit → return cached
      → miss → 3 concurrent billing API calls (plan/checkin/credits)
  → summarizeCredits
```

## Key design decisions

1. **Plugin proxy is an explicit transport boundary.** With `proxy-url`
   empty, `hostHTTPDo` / `hostHTTPDoStream` preserve existing routing: bridged
   requests use CPA's global proxy, request-log and transport policy, while
   established OAuth, usage-probe, Windows and old-host direct paths remain
   compatible. With a valid `http`, `https`, `socks5` or `socks5h` URL, an
   immutable plugin-owned client handles every plugin-initiated HTTP request,
   including OAuth and usage probing. Invalid configuration and runtime proxy
   failures fail closed without host/direct fallback or POST replay. Native
   plugin host HTTP in CLIProxyAPI v7.2.30 has no per-request proxy override,
   so explicit plugin-proxy traffic cannot appear in CPA's request-log. OAuth
   start creates an isolated cookie jar. Before both token and account requests,
   polling shallow-copies that client and applies the current explicit proxy;
   inherit preserves the flow transport. A blocked configuration deletes the
   flow before the next request can send.

2. **Single-flight per account for billing API.** `cachedAccountDetails`
   uses a `sync.Map` of in-flight calls so concurrent dashboard refreshes
   and reconcile ticks for the same account share one upstream fetch
   instead of stampeding the billing API.

3. **Cache merge, never wipe.** All cache writes merge with the previous
   entry (credits + plan + checkin) instead of replacing it. The "early
   already checked in" fast path used to wipe credits/plan; v0.6.31 fixed
   that by always merging.

4. **UID whitelist for auth file names.** `sanitizeUIDForFileName` strips
   any character outside `[a-zA-Z0-9_-]` and caps length at 64, preventing
   path traversal when importing credentials with attacker-controlled UIDs.

5. **Plugin-layer management auth is opt-in.** When `management_key` is
   unset the plugin defers entirely to CPA's management middleware
   (historical default). When set, mutating endpoints require a constant-time
   Bearer match plus a per-IP token bucket.

6. **Scheduler defers by default.** `scheduler_mode: off` (default) makes
   `handleSchedulerPick` always return `Handled: false` so CPA's built-in
   scheduler picks accounts. The plugin only routes when the operator
   explicitly opts in with `scheduler_mode: credits`.

7. **No goroutine leaks across hot-reload.** The scheduler loop uses a
   `schedulerStop` channel and is idempotent. The plugin's `Shutdown` is a
   deliberate no-op because c-shared runtime teardown races with Go sync
   primitives (SIGSEGV) — `dlclose` cleans up the whole runtime anyway.

## Integration points with CPA

- **Auth store**: `host.auth.list` / `host.auth.get` / `host.auth.save` —
  plugin never writes auth files directly to disk, always via host RPC.
- **Model registration**: `model.static` returns only `auto`; the first
  `model.for_auth` uses the configured authoritative catalog or performs
  authenticated discovery and returns dynamic models. Host `oauth-model-alias`
  / `oauth-excluded-models` is applied to each response.
- **Streaming**: `host.stream.emit` / `host.stream.close` — async SSE
  chunks pushed to the client without blocking the executor return.
- **Usage**: `usage.handle` RPC — host calls `UsagePlugin.HandleUsage`
  after every request with a canonical `pluginapi.UsageRecord`.
- **Management**: `management.register` returns routes under
  `/v0/management/plugins/workbuddy/*` and a panel resource under
  `/v0/resource/plugins/workbuddy/panel`.
- **Scheduler**: `scheduler.pick` RPC reads readiness and returns `Handled: true`
  with an `AuthID` only when `scheduler_mode: credits` and a `ready` or `stale`
  candidate exists; otherwise it defers.
