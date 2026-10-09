# TraeWork (CPA plugin)

TraeWork CN account provider for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI): sign in to a
TraeWork (trae.cn) account through the browser and hand the credentials to CPA.

The plugin runs inside the CPA host process (c-shared); it cannot run standalone.

## What this version does

| Capability | State |
|---|---|
| Browser OAuth login (panel-driven, PKCE + loopback callback / pasted callback URL) | implemented |
| Token refresh (device-proof signature, same algorithm as the client) | implemented |
| Auth file recognition and parsing (`AuthParse`) | implemented |
| Account panel (login entry + quota / check-in overview) | implemented |
| Daily bonus (panel claim for one or all + automatic claim at 09:00 and 21:00) | implemented |
| Model catalogue (per-account `config_name` list) | implemented |
| Chat (the `solo_work_lite` channel, streaming and non-streaming) | implemented |
| Tool calls (passthrough, cross-fragment merging, finish_reason fix-up) | implemented |
| Queue notices (`request_wait_in_queue` turned into an SSE keep-alive) | implemented |

Chat runs on the `solo_work_lite` channel: the request maps OpenAI messages, `tools`, `tool_choice`,
`temperature` and `max_tokens` onto that channel's shape (the model is selected by `config_name`), and the
response translates the upstream's named event stream into `chat.completion[.chunk]` frames, including
`reasoning_content`, tool calls merged across fragments, and usage. The upstream reports `finish_reason`
`stop` even when it made a tool call; the plugin rewrites that to `tool_calls`.

## Install

Install from the plugin store, or drop the library into CPA's plugin directory:

```bash
unzip traework_<version>_linux_amd64.zip
cp traework.so /path/to/cliproxyapi/plugins/
```

## Login

The login is finished in the **plugin panel**: start it from CPA's plugin auth page, approve the TraeWork
authorization link it returns, then paste the URL from the browser's address bar into the panel's login box
(paste, then click submit).

1. CPA management centre → plugin auth → `traework` → click login → open the returned authorization link and
   approve it in the browser;
2. the browser is then redirected to `http://127.0.0.1:<temporary port>/authorize?authCodeInfo=…` —
   **that page failing to load is expected**; the port only matters when the browser runs on the CPA host.
   Paste **that full URL** into the panel's login box and the plugin takes the code out of it;
3. the panel adopts the attempt CPA's dialog started (the code is bound to that attempt's PKCE verifier, so it
   must be handed to the same attempt), redeems it and the account shows up in the list.

Why not CPA's own "callback URL" box: the host parses it with the `code` + `state` query parameters, while
TraeWork returns the code inside `authCodeInfo` (JSON) and sends no `state`, and the management centre extracts
nothing for plugin providers. That box cannot succeed for TraeWork — the login has to finish in the panel.

"开始登录" in the panel starts a fresh attempt when none is pending, and finishes the same way.

## Configuration

| Key | Type | Description |
|---|---|---|
| `management_key` | string | Optional. When set, this plugin's management routes require this Bearer key on top of the host's own auth (the panel can pass it as `?key=`). Also read from `TW_MANAGEMENT_KEY`. |
| `proxy-url` | string | Optional proxy for every TraeWork request (http/https/socks5/socks5h). Empty keeps the host routing policy; an invalid value fails closed. |
| `checkin_auto` | boolean | Optional, default `true`. Claims each account's daily bonus automatically at 09:00 and 21:00 local time (the evening pass retries a busy morning). The panel toggle only changes the current run. |
| `panel_base_url` | string | Optional. Prefix for the panel login address. **Empty (default) keeps it relative** (`/v0/resource/plugins/traework/panel?login=…`), which resolves on whatever origin serves the management UI — correct behind a reverse proxy or a domain. Set it (e.g. `https://cpa.example.cn`) to get an absolute link that can be copied elsewhere. |

## Auth file

After a successful login `traework-<uid>.json` is written: the host writes it for a host-driven
login, and a panel-driven login goes through `host.auth.save` with the same record shape.

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

`type` is what the host routes on; the parser keeps the file path as the record identity (it never sets `ID`)
so one file cannot be registered twice. User-owned fields (`weight`, `prefix`, `note`, …) survive every rewrite.

## Panel

`/v0/resource/plugins/traework/panel` gives every account a card showing its **quota** — remaining / spent /
pool with a consumption bar, the entitlement packages (name, spent / limit, expiry), today's check-in state —
plus the login entry.

The numbers come from three read-only endpoints: `/trae/api/v2/pay/ide_user_ent_usage` (usage and packages),
`/trae/api/v2/pay/ide_user_pay_status` (plan identity, solo channel availability) and
`/trae/api/v2/ug/checkin_credits/status` (daily bonus). They are cached per account for 60 seconds; the
card's refresh button bypasses the cache.

Token validity, region, uid and the auth file name are credential state: read them in CPA's auth-files
module, the panel does not repeat them. The bonus can be claimed right there: each card has a "签到 / 已签到" button and the toolbar has "全部签到" plus an
automatic-claim toggle. The claim is the plugin's only upstream write; upstream is idempotent for the same account
and day, and business code `9074` ("too many users") is treated as **retryable** rather than a failure.

Automatic claiming is on by default and runs at 09:00 and 21:00 local time — the evening pass retries whatever the
morning pass lost to a busy window. Claims are serialised per account, so two browser tabs cannot claim the same day
twice. Set `checkin_auto: false` in the plugin config to switch it off (the panel toggle only affects the current
run).

## Development

```bash
make build   # CGO_ENABLED=1 go build -buildmode=c-shared -o traework.so .
make test    # go test -race -count=1 ./... + node --test panel.test.js
make lint    # gofmt -l . + go vet + staticcheck
make clean
```

`panel.html` is embedded with `go:embed`, so a change to it needs a rebuild.

## License

MIT