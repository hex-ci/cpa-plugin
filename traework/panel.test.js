// panel.test.js exercises the dashboard script that ships inside panel.html.
// The script is extracted and run in a VM against a small DOM stub, so the
// assertions cover what the browser actually executes.
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const crypto = require("node:crypto");

const panelHTML = fs.readFileSync(path.join(__dirname, "panel.html"), "utf8");
// The server substitutes the host's management base path before serving; the
// test does the same so the script is valid JavaScript.
// The page has two blocks (theme sync in <head>, dashboard in <body>); both run
// in the browser, so the test concatenates them in document order.
const scriptSource = [...panelHTML.matchAll(/<script>([\s\S]*?)<\/script>/g)]
  .map((m) => m[1])
  .join("\n")
  .replace("__TW_MANAGEMENT_BASE_PATH_JSON__", JSON.stringify("/v0/management"));

const SECRET_SALT = "cli-proxy-api-webui::secure-storage";
const ENC_PREFIX = "enc::v1::";

// Mirror of the panel's own obfuscation, used to fabricate the CPA main
// panel's localStorage entry exactly as the host writes it.
function obfuscate(plain, host, userAgent) {
  const key = Buffer.from(`${SECRET_SALT}|${host}|${userAgent}`, "utf8");
  const data = Buffer.from(plain, "utf8");
  const out = Buffer.alloc(data.length);
  for (let i = 0; i < data.length; i += 1) out[i] = data[i] ^ key[i % key.length];
  return ENC_PREFIX + out.toString("base64");
}

function fakeElement(tag = "div") {
  const element = {
    tagName: tag,
    children: [],
    className: "",
    textContent: "",
    innerHTML: "",
    value: "",
    disabled: false,
    style: {},
    listeners: {},
    append(child) {
      this.children.push(child);
      return child;
    },
    appendChild(child) {
      return this.append(child);
    },
    remove() {},
    replaceChildren(...nodes) {
      this.children = nodes;
    },
    addEventListener(name, handler) {
      this.listeners[name] = handler;
    },
    // One element per selector, so assertions can tell .card-name from .uid.
    querySelector(selector) {
      if (!this._q) this._q = new Map();
      if (!this._q.has(selector)) this._q.set(selector, fakeElement());
      return this._q.get(selector);
    },
    // The markup is a template string, so the number of matches is derived from
    // the class the selector ends with.
    querySelectorAll(selector) {
      const last = String(selector).split(/\s+/).pop().replace(/^\./, "");
      const re = new RegExp('class="' + last + '(\\s|")', "g");
      const n = this.innerHTML ? (this.innerHTML.match(re) || []).length : 2;
      const out = [];
      for (let i = 0; i < Math.max(n, 1); i += 1) out.push(fakeElement());
      return out;
    },
    setAttribute() {},
    focus() {
      this.focused = true;
    },
    classList: { add() {}, remove() {} },
  };
  return element;
}

function runPanel({
  search = "",
  payload = null,
  routes = null,
  fail = null,
  storage = null,
  embedded = false,
} = {}) {
  const byId = new Map();
  const html = {
    attrs: {},
    setAttribute(k, v) { this.attrs[k] = String(v); },
    removeAttribute(k) { delete this.attrs[k]; },
    getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
  };
  const document = {
    documentElement: html,
    getElementById(id) {
      if (!byId.has(id)) byId.set(id, fakeElement());
      return byId.get(id);
    },
    createElement(tag) {
      return fakeElement(tag);
    },
  };
  document.querySelector = () => null;
  const calls = [];
  const fetch = async (url, options) => {
    const target = String(url);
    calls.push({
      url: target,
      method: (options && options.method) || "GET",
      headers: (options && options.headers) || {},
      body: options && options.body ? String(options.body) : null,
    });
    if (fail) throw fail;
    if (routes) {
      for (const [fragment, response] of Object.entries(routes)) {
        if (target.includes(fragment)) return { ok: true, status: 200, json: async () => response };
      }
    }
    return { ok: true, status: 200, json: async () => payload };
  };
  const session = new Map();
  const location = {
    origin: "http://127.0.0.1:8317",
    host: "127.0.0.1:8317",
    search,
    href: `http://127.0.0.1:8317/v0/resource/plugins/traework/panel${search}`,
  };
  const context = {
    document,
    location,
    history: {
      replaced: [],
      replaceState(_state, _title, url) {
        this.replaced.push(url);
      },
    },
    navigator: { userAgent: "test-agent" },
    sessionStorage: {
      getItem: (k) => (session.has(k) ? session.get(k) : null),
      setItem: (k, v) => session.set(k, v),
      removeItem: (k) => session.delete(k),
    },
    localStorage: {
      getItem: (k) => (storage && k in storage ? storage[k] : null),
    },
    fetch,
    opened: [],
    open(url) {
      context.opened.push(String(url));
      return null;
    },
    setInterval: () => 1,
    clearInterval: () => {},
    URL,
    URLSearchParams,
    TextEncoder,
    TextDecoder,
    atob: (s) => Buffer.from(s, "base64").toString("binary"),
    btoa: (s) => Buffer.from(s, "binary").toString("base64"),
    console,
    // Toasts schedule animation frames and auto-dismiss timers; the stub keeps
    // the test deterministic and leaves no timers behind.
    requestAnimationFrame: () => 0,
    // The panel defers work (toasts, paste handoff); the stub runs it inline so
    // assertions see the result.
    setTimeout: (fn) => {
      if (typeof fn === "function") fn();
      return 0;
    },
    matchMedia: () => ({ matches: false, addEventListener() {}, addListener() {} }),
  };
  // window must be the global object itself; window.self !== window.top is how
  // the script detects an embedding parent.
  context.window = context;
  context.self = context;
  context.top = embedded ? { parent: true } : context;
  vm.runInNewContext(scriptSource, context);
  return { byId, calls, session, context, opened: context.opened };
}

const tick = () => new Promise((resolve) => setImmediate(resolve));

test("panel requests the accounts endpoint on load", async () => {
  const { calls } = runPanel({ search: "?key=abc", payload: { count: 0, accounts: [] } });
  await tick();
  assert.ok(
    calls.some((c) => /\/v0\/management\/plugins\/traework\/accounts$/.test(c.url)),
    "the dashboard must load the accounts endpoint",
  );
});

test("panel keeps the key from the URL in the session and drops it from the address bar", async () => {
  const { session, context } = runPanel({ search: "?key=abc123", payload: { count: 0, accounts: [] } });
  await tick();
  assert.equal(session.get("traework-mgmt-key"), "abc123");
  const replaced = context.history.replaced;
  assert.ok(replaced.length > 0, "the key must be scrubbed from the visible URL");
  assert.equal(new URL("http://127.0.0.1:8317" + replaced[0]).searchParams.get("key"), null);
});

// Embedded in the management UI there is no ?key= in the URL, so the key has to
// come from the host panel's own store — otherwise every request is refused.
test("panel reads the management key from the CPA panel store when embedded", async () => {
  const raw = obfuscate(
    JSON.stringify({ state: { managementKey: "from-panel-store" } }),
    "127.0.0.1:8317",
    "test-agent",
  );
  const { calls } = runPanel({
    embedded: true,
    storage: { "cli-proxy-auth": raw },
    payload: { count: 0, accounts: [] },
  });
  await tick();
  assert.ok(calls.length >= 1);
  for (const call of calls) {
    assert.equal(call.headers.Authorization, "Bearer from-panel-store");
  }
});

// A keyless request still counts as a failed attempt on the host side, and five
// of them ban the caller's IP for 30 minutes — so the panel must not fire one.
test("panel makes no request at all when no key is available", async () => {
  const { calls, byId } = runPanel({ embedded: true, storage: {}, payload: { count: 0, accounts: [] } });
  await tick();
  assert.equal(calls.length, 0);
  assert.equal(byId.get("authBox").style.display, "block");
  assert.match(byId.get("accounts").children[0].textContent, /management key/);
});

test("panel sends no key when the store holds something unusable", async () => {
  const { calls } = runPanel({
    embedded: true,
    storage: { "cli-proxy-auth": "enc::v1::not-really-obfuscated" },
    payload: { count: 0, accounts: [] },
  });
  await tick();
  assert.equal(calls.length, 0);
});

test("panel ignores the panel store when it is not embedded", async () => {
  const raw = obfuscate(
    JSON.stringify({ state: { managementKey: "should-not-be-used" } }),
    "127.0.0.1:8317",
    "test-agent",
  );
  const { calls } = runPanel({ storage: { "cli-proxy-auth": raw }, payload: { count: 0, accounts: [] } });
  await tick();
  // Standalone and keyless: the store is not a legitimate source, so no request.
  assert.equal(calls.length, 0);
});

test("panel renders one card per account plus the quota summary", async () => {
  const { byId } = runPanel({
    search: "?key=abc",
    payload: {
      count: 2,
      accounts: [
        {
          label: "Trae User",
          uid: "1",
          region: "CN",
          file_name: "traework-1.json",
          credits: {
            total_remain: 485.35,
            total_used: 14.65,
            total_size: 500,
            identity: "Free",
            billing: "积分计费",
            pack_count: 1,
            packs: [{ name: "每月登录积分", limit: 500, used: 14.65, expires: "2026-10-31" }],
          },
          checkin: { enabled: true, today_checked_in: false, credits: 100, extra_credits: 100 },
        },
        { label: "Other", uid: "2", region: "CN", disabled: true, note: "已停用" },
      ],
    },
  });
  await tick();
  const list = byId.get("accounts");
  assert.equal(list.children.length, 2);

  const first = list.children[0];
  assert.equal(first.querySelector(".card-name").textContent, "Trae User");
  // 进度条与包明细写在 innerHTML 里，签到行是单独写入的文本节点。
  assert.match(first.innerHTML, /可用 485\.35/);
  assert.match(first.innerHTML, /已用 14\.65 · 3%/);
  assert.match(first.innerHTML, /剩余 485\.35 · 已用 14\.65 · 额度池 500 · 1 个包/);
  assert.match(first.innerHTML, /每月登录积分/);
  assert.match(first.innerHTML, /至 2026-10-31/);
  assert.match(first.querySelector(".row .v").textContent, /今日未签到 · 可领 200/);
  assert.match(first.innerHTML, /免费版/, "the plan badge comes from the quota identity");

  // 卡片只讲额度：认证状态归 CPA 的认证文件模块。
  assert.doesNotMatch(first.innerHTML, /认证文件|访问令牌|到期|traework-1\.json/);

  const summary = byId.get("summaryBox");
  assert.equal(summary.style.display, "block");
  assert.match(summary.innerHTML, />账号总数<\/div><div class="v">2</);
  assert.match(summary.innerHTML, />可用积分<\/div><div class="v ok">485.35</);
  assert.match(summary.innerHTML, />已用<\/div><div class="v">14.65</);
  assert.match(summary.innerHTML, />未签到<\/div><div class="v warn">1</);
  assert.match(byId.get("svtime").textContent, /共 2 个账号/);
});

// 签到按钮只在真有得领的时候出现：没有活动、账号停用、今天已领都不摆。
test("panel offers check-in only while the daily bonus is unclaimed", async () => {
  const { byId } = runPanel({
    search: "?key=abc",
    payload: {
      count: 3,
      checkin_auto: true,
      accounts: [
        { label: "Open", uid: "1", auth_index: "idx-open", credits: { total_remain: 300, total_size: 500 }, checkin: { enabled: true, today_checked_in: false, credits: 100 } },
        { label: "Taken", uid: "2", auth_index: "idx-taken", credits: { total_remain: 300, total_size: 500 }, checkin: { enabled: true, today_checked_in: true, credits: 100 } },
        { label: "NoBonus", uid: "3", auth_index: "idx-none", credits: { total_remain: 300, total_size: 500 }, checkin: { enabled: false } },
      ],
    },
  });
  await tick();
  const cards = byId.get("accounts").children;
  assert.match(cards[0].innerHTML, /data-action="checkin"/);
  assert.match(cards[0].innerHTML, />签到</);
  assert.match(cards[1].innerHTML, /已签到/);
  assert.match(cards[1].innerHTML, /disabled="disabled"/);
  assert.doesNotMatch(cards[2].innerHTML, /data-action="checkin"/);
  assert.equal(byId.get("autoToggle").checked, true, "the toggle mirrors the backend value");
});

// 上游 9074 是「人太多，稍后再试」：可重试，不能报成失败。
test("panel reports a busy check-in as retryable and refreshes the card", async () => {
  const { byId, calls, context } = runPanel({
    search: "?key=abc",
    payload: { count: 1, accounts: [{ label: "Trae User", uid: "1", auth_index: "idx-1", credits: { total_remain: 300, total_size: 500 }, checkin: { enabled: true, today_checked_in: false } }] },
    routes: {
      "/checkin": {
        results: [{ auth_index: "idx-1", success: false, retryable: true, message: "当前参与用户太多，请稍后再试" }],
        summary: { total: 1, claimed: 0, already: 0, busy: 1, fail: 0 },
      },
      "/credits": { auth_index: "idx-1", credits: { total_remain: 300, total_size: 500 }, checkin: { enabled: true, today_checked_in: true } },
    },
  });
  await tick();
  await tick();
  await context.checkinOne(null, "idx-1");
  await tick();
  await tick();

  const posted = calls.filter((c) => c.method === "POST" && c.url.endsWith("/checkin"));
  assert.equal(posted.length, 1);
  assert.match(posted[0].body, /idx-1/);
  assert.ok(
    calls.some((c) => c.url.includes("/credits") && c.url.includes("fresh=1")),
    "the balance must be read again after a claim attempt",
  );
  const toast = byId.get("toasts").children[byId.get("toasts").children.length - 1];
  assert.match(toast.querySelector(".d").textContent, /稍后再试/);
});

test("panel claims every account and reloads the list", async () => {
  const { byId, calls, context } = runPanel({
    search: "?key=abc",
    payload: { count: 1, accounts: [{ label: "Trae User", uid: "1", auth_index: "idx-1", credits: { total_remain: 300, total_size: 500 }, checkin: { enabled: true, today_checked_in: false } }] },
    routes: { "/checkin": { results: [], summary: { total: 1, claimed: 1, already: 0, busy: 0, fail: 0 } } },
  });
  await tick();
  await context.checkinAll(null);
  await tick();
  await tick();
  const posted = calls.filter((c) => c.method === "POST" && c.url.endsWith("/checkin"));
  assert.equal(posted.length, 1);
  assert.equal(posted[0].body, "{}", "no auth_index means every account");
  assert.ok(calls.filter((c) => c.url.endsWith("/accounts")).length >= 2, "the list must be reloaded");
  const toast = byId.get("toasts").children[byId.get("toasts").children.length - 1];
  assert.match(toast.querySelector(".d").textContent, /成功 1/);
});

test("the auto check-in toggle posts the new state", async () => {
  const { byId, calls, context } = runPanel({
    search: "?key=abc",
    payload: { count: 0, accounts: [], checkin_auto: true },
    routes: { "/checkin/config": { checkin_auto: false, persistent: false } },
  });
  await tick();
  await context.toggleAuto(false);
  await tick();
  const posted = calls.filter((c) => c.method === "POST" && c.url.endsWith("/checkin/config"));
  assert.equal(posted.length, 1);
  assert.equal(posted[0].body, '{"enabled":false}');
  const toast = byId.get("toasts").children[byId.get("toasts").children.length - 1];
  assert.match(toast.querySelector(".t").textContent, /自动签到已关闭/);
  assert.match(toast.querySelector(".d").textContent, /重启后以插件配置/);
});

// 列表接口只回缓存，落在卡片上的额度由 /credits 补齐；点刷新要绕过缓存。
test("panel fills each card from /credits and refreshes on demand", async () => {
  const { byId, calls, context } = runPanel({
    search: "?key=abc",
    payload: { count: 1, accounts: [{ label: "Trae User", uid: "1", auth_index: "idx-1" }] },
    routes: {
      "/credits": {
        auth_index: "idx-1",
        credits: { total_remain: 300, total_used: 200, total_size: 500, packs: [] },
        checkin: { enabled: true, today_checked_in: true, credits: 100 },
      },
    },
  });
  await tick();
  await tick();
  const card = byId.get("accounts").children[0];
  assert.match(card.innerHTML, /可用 300/);
  assert.match(card.querySelector(".row .v").textContent, /今日已签到/);
  const lazy = calls.filter((c) => c.url.includes("/credits"));
  assert.equal(lazy.length, 1);
  assert.doesNotMatch(lazy[0].url, /fresh=1/, "a page load must be allowed to hit the cache");

  // 假 DOM 不派发点击事件，直接走按钮绑定的那条路。
  await context.refreshCredits("idx-1", card, null, true);
  await tick();
  const forced = calls.filter((c) => c.url.includes("fresh=1"));
  assert.equal(forced.length, 1, "the refresh button forces a fresh read");
  assert.match(forced[0].url, /auth_index=idx-1/);
});

test("panel only shows the note block for a broken credential", async () => {
  const { byId } = runPanel({
    search: "?key=abc",
    payload: {
      count: 2,
      accounts: [
        { label: "Trae User", uid: "1", region: "CN", expires_in: 90000, file_name: "traework-1.json" },
        { label: "TraeWork", file_name: "traework-broken.json", note: "凭据无法解析：bad json" },
      ],
    },
  });
  await tick();
  const cards = byId.get("accounts").children;
  assert.equal((cards[0].innerHTML.match(/class="pkgs/g) || []).length, 0);
  assert.equal((cards[1].innerHTML.match(/pkgs warn/g) || []).length, 1);
  assert.match(cards[1].querySelector(".pkgs").textContent, /凭据无法解析/);
});

test("panel shows the login hint when no account exists", async () => {
  const { byId } = runPanel({ search: "?key=abc", payload: { count: 0, accounts: [] } });
  await tick();
  assert.equal(byId.get("accounts").children.length, 1);
  // The empty state points at the panel's own login card, not the CPA dialog.
  assert.match(byId.get("accounts").children[0].innerHTML, /开始登录/);
});

test("panel reveals the key prompt when the host refuses the key it sent", async () => {
  const { byId } = runPanel({ search: "?key=stale", fail: new Error("invalid management key") });
  await tick();
  const box = byId.get("accounts").children[0];
  assert.match(box.textContent, /读取失败/);
  assert.match(box.textContent, /invalid management key/);
  assert.equal(byId.get("authBox").style.display, "block");
});

test("panel surfaces a failed request instead of a blank page", async () => {
  const { byId } = runPanel({ search: "?key=stale", fail: new Error("unauthorized") });
  await tick();
  const box = byId.get("accounts").children[0];
  assert.match(box.textContent, /读取失败/);
  assert.match(box.textContent, /unauthorized/);
});

test("panel html wires the controls and the host base path placeholder", () => {
  assert.match(panelHTML, /id="reload"/);
  assert.match(panelHTML, /id="accounts" class="grid"/);
  assert.match(panelHTML, /id="summaryBox"/);
  assert.match(panelHTML, /id="toasts"/);
  assert.match(panelHTML, /id="keyInput"/);
  assert.match(panelHTML, /__TW_MANAGEMENT_BASE_PATH_JSON__/);
});

// The dashboard shares the CPA management panel's theme tokens so an embedded
// page looks like the rest of the UI instead of a foreign dark sheet.
test("panel html carries the CPA theme tokens and parent theme sync", () => {
  for (const token of ["--bg", "--card", "--border", "--acc", "--ok-soft", "--radius-md"]) {
    assert.ok(panelHTML.includes(token + ":"), `missing theme token ${token}`);
  }
  assert.match(panelHTML, /:root\[data-theme="white"\]/);
  assert.match(panelHTML, /:root\[data-theme="dark"\]/);
  assert.match(panelHTML, /window\.parent\.document\.documentElement/);
  assert.match(panelHTML, /__twThemeSync/);
});

/* ---------- 登录（面板是唯一入口） ---------- */

const pendingLogin = {
  status: "pending",
  state: "s-1",
  authorize_url: "https://www.trae.cn/authorization?client_id=x",
  callback_port: 38471,
  message: "等待浏览器回调 127.0.0.1:38471",
};

test("panel starts the login through the plugin's own endpoint and opens the authorize page", async () => {
  const { calls, byId, context, opened } = runPanel({
    search: "?key=abc",
    payload: { count: 0, accounts: [] },
    routes: { "/login/start": pendingLogin },
  });
  await tick();
  await context.startLogin();
  await tick();
  const start = calls.find((c) => c.url.includes("/login/start"));
  assert.ok(start, "the login must start through the plugin endpoint");
  assert.equal(start.method, "POST");
  assert.equal(start.headers.Authorization, "Bearer abc");
  assert.equal(byId.get("loginPending").style.display, "block");
  assert.equal(byId.get("loginIdle").style.display, "none");
  assert.equal(byId.get("loginCallback").textContent, "http://127.0.0.1:38471/authorize");
  // A configured login_callback_url is what the operator must be shown.
  const proxied = runPanel({
    search: "?key=abc",
    payload: { count: 0, accounts: [] },
    routes: { "/login/start": Object.assign({}, pendingLogin, { callback_url: "https://cpa.example.cn/authorize" }) },
  });
  await tick();
  await proxied.context.startLogin();
  await tick();
  assert.equal(proxied.byId.get("loginCallback").textContent, "https://cpa.example.cn/authorize");
  assert.match(byId.get("loginUrl").textContent, /trae\.cn/);
  assert.deepEqual(opened, [pendingLogin.authorize_url]);
});

test("panel submits the pasted callback URL for the attempt it started", async () => {
  const { calls, byId, context } = runPanel({
    search: "?key=abc",
    payload: { count: 0, accounts: [] },
    routes: {
      "/login/start": pendingLogin,
      "/login/callback": { status: "success", message: "登录成功", label: "Trae User", file_name: "traework-1.json" },
    },
  });
  await tick();
  await context.startLogin();
  await tick();
  context.document.getElementById("loginInput").value =
    "http://127.0.0.1:38471/authorize?authCodeInfo=%7B%22AuthCode%22%3A%22abc%22%7D";
  await context.submitCallback();
  await tick();
  const submit = calls.find((c) => c.url.includes("/login/callback"));
  assert.ok(submit, "the pasted value must go to the plugin");
  assert.equal(submit.method, "POST");
  const body = JSON.parse(submit.body);
  assert.equal(body.state, "s-1");
  assert.match(body.input, /authCodeInfo/);
  assert.equal(byId.get("loginDone").style.display, "block");
  assert.match(byId.get("loginDoneMsg").textContent, /登录成功/);
  assert.match(byId.get("loginDoneMsg").textContent, /Trae User/);
});

test("panel reports a polled success and refreshes the account list", async () => {
  const { calls, byId, context } = runPanel({
    search: "?key=abc",
    payload: { count: 0, accounts: [] },
    routes: {
      "/login/start": pendingLogin,
      "/login/status": { status: "success", message: "登录成功", label: "Trae User", file_name: "traework-1.json" },
    },
  });
  await tick();
  await context.startLogin();
  await tick();
  const before = calls.length;
  await context.pollLogin();
  await tick();
  assert.equal(byId.get("loginDone").style.display, "block");
  assert.ok(calls.length > before, "a completed login must reload the accounts");
});

test("panel shows the polled failure instead of a silent login card", async () => {
  const { byId, context } = runPanel({
    search: "?key=abc",
    payload: { count: 0, accounts: [] },
    routes: {
      "/login/start": pendingLogin,
      "/login/status": { status: "error", message: "换取令牌失败：invalid code" },
    },
  });
  await tick();
  await context.startLogin();
  await tick();
  await context.pollLogin();
  await tick();
  assert.match(byId.get("loginStatus").textContent, /invalid code/);
  assert.equal(byId.get("loginPending").style.display, "block");
  // The state badge carries the outcome instead of a grey word.
  assert.equal(byId.get("loginMeta").className, "badge err");
});

test("panel offers a copy action for the authorize link and degrades without a clipboard", async () => {
  const { byId, context } = runPanel({
    search: "?key=abc",
    payload: { count: 0, accounts: [] },
    routes: { "/login/start": pendingLogin },
  });
  await tick();
  await context.startLogin();
  await tick();
  assert.equal(byId.get("loginUrl").title, pendingLogin.authorize_url);
  await context.copyAuthorize();
  await tick();
  // No clipboard API in this context, so the operator gets told what to do.
  assert.match(byId.get("loginStatus").textContent, /手动复制/);
});

// Starting a login keyless would be a request the host counts as a failure.
test("panel starts no login request without a key", async () => {
  const { calls, context } = runPanel({ embedded: true, storage: {}, payload: { count: 0, accounts: [] } });
  await tick();
  await context.startLogin();
  await tick();
  assert.equal(calls.length, 0);
});

test("panel adopts the attempt the server is still waiting for instead of starting a second one", async () => {
  const { calls, byId, context } = runPanel({
    search: "?key=abc",
    payload: { count: 0, accounts: [] },
    routes: { "/login/current": pendingLogin },
  });
  await tick();
  assert.ok(calls.some((c) => c.url.includes("/login/current")), "the panel must ask which attempt is pending");
  assert.equal(byId.get("loginPending").style.display, "block");
  assert.equal(byId.get("loginCallback").textContent, "http://127.0.0.1:38471/authorize");

  // Starting from the panel must not fork a second attempt while one is live:
  // the code belongs to the attempt the dialog started.
  await context.startLogin();
  await tick();
  assert.equal(
    calls.filter((c) => c.url.includes("/login/start")).length,
    0,
    "a live attempt must be adopted, not replaced",
  );
});

test("panel adopts the login attempt named in the address bar", async () => {
  const { calls } = runPanel({
    search: "?key=abc&login=s-9",
    payload: { count: 0, accounts: [] },
    routes: { "/login/status": { status: "pending", state: "s-9", callback_port: 38471, message: "等待授权" } },
  });
  await tick();
  const status = calls.find((c) => c.url.includes("/login/status"));
  assert.ok(status, "the panel must query the adopted attempt");
  assert.match(status.url, /state=s-9/);
});

test("panel html wires the login card", () => {
  for (const id of ["loginBox", "loginStart", "loginPending", "loginInput", "loginStatus", "loginDoneMsg", "loginCopy"]) {
    assert.match(panelHTML, new RegExp('id="' + id + '"'), `missing login element ${id}`);
  }
  assert.match(panelHTML, /\/login\/start/);
  assert.match(panelHTML, /\/login\/callback/);
});

// The host HTML-escapes every string in a plugin management response, so the
// panel has to decode before it uses a URL or shows a message.
test("panel decodes the host's HTML escaping in management responses", async () => {
  const escapedLogin = {
    status: "pending",
    state: "s-1",
    authorize_url: "https://www.trae.cn/authorization?auth_from=solo&amp;client_id=abc&amp;x=1",
    callback_port: 38471,
    message: "换取令牌失败：{&#34;Code&#34;:&#34;10101&#34;}",
  };
  const { byId, context, opened } = runPanel({
    search: "?key=abc",
    routes: { "/login/start": escapedLogin },
    payload: { count: 1, accounts: [{ label: "Trae &amp; Co", uid: "1", note: "a &lt;b&gt;", file_name: "traework-1.json" }] },
  });
  await tick();
  await context.startLogin();
  await tick();
  assert.equal(byId.get("loginUrl").textContent, "https://www.trae.cn/authorization?auth_from=solo&client_id=abc&x=1");
  assert.equal(byId.get("loginUrl").title, "https://www.trae.cn/authorization?auth_from=solo&client_id=abc&x=1");
  assert.match(byId.get("loginStatus").textContent, /\{"Code":"10101"\}/);
  assert.deepEqual(opened, ["https://www.trae.cn/authorization?auth_from=solo&client_id=abc&x=1"]);
  const card = byId.get("accounts").children[0];
  assert.equal(card.querySelector(".card-name").textContent, "Trae & Co");
  assert.equal(card.querySelector(".pkgs").textContent, "a <b>");
});

test("panel submits as soon as a callback URL is pasted", async () => {
  const { calls, byId, context } = runPanel({
    search: "?key=abc",
    routes: {
      "/login/start": pendingLogin,
      "/login/callback": { status: "success", message: "登录成功", label: "Trae User", file_name: "traework-1.json" },
    },
    payload: { count: 0, accounts: [] },
  });
  await tick();
  await context.startLogin();
  await tick();
  const input = context.document.getElementById("loginInput");
  assert.equal(input.focused, true, "the paste box must be ready for the operator");
  input.value = "http://127.0.0.1:38471/authorize?authCodeInfo=%7B%22AuthCode%22%3A%22abc%22%7D";
  input.listeners.paste();
  await tick();
  const submit = calls.find((c) => c.url.includes("/login/callback"));
  assert.ok(submit, "pasting must submit without an extra click");
  assert.match(JSON.parse(submit.body).input, /authCodeInfo/);
  assert.equal(byId.get("loginDoneMsg").textContent.includes("登录成功"), true);
});
