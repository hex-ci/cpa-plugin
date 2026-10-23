const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

// Minimal DOM: the panel only needs element stubs and list queries.
function fakeElement() {
  const queries = new Map();
  const element = {
    hidden: false,
    className: "",
    textContent: "",
    innerHTML: "",
    value: "",
    files: [],
    style: {},
    dataset: {},
    children: [],
    checked: false,
    classList: {
      values: new Set(),
      add(v) { this.values.add(v); },
      remove(v) { this.values.delete(v); },
      contains(v) { return this.values.has(v); },
    },
    appendChild(child) { this.children.push(child); return child; },
    querySelector(sel) {
      if (!queries.has(sel)) queries.set(sel, fakeElement());
      return queries.get(sel);
    },
    querySelectorAll() { return []; },
    addEventListener() {},
    focus() {},
    remove() {},
  };
  return element;
}

function loadPanel(overrides = {}) {
  const html = fs.readFileSync(path.join(__dirname, "panel.html"), "utf8");
  const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)];
  // Boot block is stripped: tests drive captureUrlKey/load themselves so no
  // request leaves the sandbox unless a test asks for one.
  const source = scripts.at(-1)[1]
    .replace("__QODER_MANAGEMENT_BASE_PATH_JSON__", JSON.stringify("/v0/management"))
    .replace(/captureUrlKey\(\);\n\/\/[\s\S]*$/, "");
  const elements = new Map();
  const storage = new Map(overrides.sessionEntries || []);
  const document = {
    documentElement: fakeElement(),
    body: fakeElement(),
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, fakeElement());
      return elements.get(id);
    },
    createElement() { return fakeElement(); },
    querySelectorAll() { return []; },
    addEventListener() {},
  };
  const sessionStorage = overrides.sessionStorage || {
    getItem(k) { return storage.has(k) ? storage.get(k) : null; },
    setItem(k, v) { storage.set(k, String(v)); },
    removeItem(k) { storage.delete(k); },
  };
  const local = overrides.localEntries ? new Map(overrides.localEntries) : new Map();
  const context = {
    console,
    document,
    sessionStorage,
    localStorage: overrides.localStorage || {
      getItem(k) { return local.has(k) ? local.get(k) : null; },
      setItem(k, v) { local.set(k, String(v)); },
      removeItem(k) { local.delete(k); },
    },
    location: overrides.location || { href: "http://localhost/panel", search: "", pathname: "/panel", hash: "" },
    history: overrides.history || { replaceState() {} },
    navigator: { userAgent: "node-test" },
    URL,
    URLSearchParams,
    TextEncoder,
    TextDecoder,
    Uint8Array,
    btoa(v) { return Buffer.from(v, "binary").toString("base64"); },
    atob(v) { return Buffer.from(v, "base64").toString("binary"); },
    requestAnimationFrame(fn) { fn(); },
    setTimeout(fn) { fn(); return 1; },
    clearTimeout() {},
    fetch: overrides.fetch || (async () => { throw new Error("unexpected fetch"); }),
  };
  context.window = context;
  context.self = context;
  context.top = context;
  vm.createContext(context);
  vm.runInContext(source, context, { filename: "panel.html" });
  return { context, document, elements, storage, local };
}

function fakeResponse(status, contentType, body) {
  return {
    status,
    ok: status >= 200 && status < 300,
    headers: { get(n) { return n.toLowerCase() === "content-type" ? contentType : null; } },
    async text() { return body; },
    async json() { return JSON.parse(body); },
  };
}

// The panel is the only place a management key is typed; losing it to a stale
// fallback order silently shows another key's data.
test("captureUrlKey stores the query key once and strips it from the URL", () => {
  let replaced = "";
  let localWrites = 0;
  const { context, storage } = loadPanel({
    sessionEntries: [["qoder-mgmt-key", "old-key"]],
    location: { href: "http://h/p?key=new-key&tab=1", search: "?key=new-key&tab=1", pathname: "/p", hash: "" },
    history: { replaceState(_s, _t, url) { replaced = url; } },
    localStorage: { getItem() { return null; }, setItem() { localWrites += 1; }, removeItem() {} },
  });
  assert.equal(context.captureUrlKey(), true);
  assert.equal(storage.get("qoder-mgmt-key"), "new-key");
  assert.equal(replaced, "/p");
  assert.equal(localWrites, 0, "query keys never touch persistent storage");
});

test("captureUrlKey ignores pages without a query key", () => {
  const { context, storage } = loadPanel({ sessionEntries: [["qoder-mgmt-key", "keep"]] });
  assert.equal(context.captureUrlKey(), false);
  assert.equal(storage.get("qoder-mgmt-key"), "keep");
});

test("no key means no request and an explicit prompt", async () => {
  let calls = 0;
  const { context, elements } = loadPanel({
    fetch: async () => { calls += 1; throw new Error("unexpected fetch"); },
  });
  assert.equal(context.getKey(), null);
  assert.equal(await context.load(false), false);
  assert.equal(calls, 0);
  assert.equal(elements.get("authBox").style.display, "block");
});

// Upstream errors can quote account details; the panel must not surface them.
test("sanitizePanelErrors replaces error text and keeps the rest", () => {
  const { context } = loadPanel();
  const out = context.sanitizePanelErrors({
    accounts: [{ nickname: "hex", error: "12153 TOKEN_EXPIRE uid=42" }],
    error: "upstream said no",
  });
  assert.equal(out.error, "请求失败");
  assert.equal(out.accounts[0].error, "请求失败");
  assert.equal(out.accounts[0].nickname, "hex");
});

test("non-JSON panel responses fail with a fixed message", async () => {
  const { context } = loadPanel({
    sessionEntries: [["qoder-mgmt-key", "k"]],
    fetch: async () => fakeResponse(200, "text/html", "<html>gateway</html>"),
  });
  await assert.rejects(context.api("/accounts"), (err) => /响应格式无效/.test(err.message) && !/gateway/.test(err.message));
});

test("transport failures never echo the underlying error", async () => {
  const { context } = loadPanel({
    sessionEntries: [["qoder-mgmt-key", "k"]],
    fetch: async () => { throw new Error("ECONNREFUSED 10.0.0.5:18317"); },
  });
  await assert.rejects(context.api("/accounts"), (err) => !/ECONNREFUSED|10\.0\.0\.5/.test(err.message));
});

test("region, exhausted and search filters narrow the list", () => {
  const { context } = loadPanel();
  const accounts = [
    { nickname: "alpha", region: "cn", credits: { total_remain: 10, total_used: 1, total_size: 11 } },
    { nickname: "beta", region: "global", exhausted: true, credits: { total_remain: 0, total_used: 5, total_size: 5 } },
  ];
  const names = (filter, term) => accounts.filter((a) => context.matchesFilter(a, filter, term)).map((a) => a.nickname);
  assert.deepEqual(names("global", ""), ["beta"]);
  assert.deepEqual(names("cn", ""), ["alpha"]);
  assert.deepEqual(names("exhausted", ""), ["beta"]);
  assert.deepEqual(names("all", "alp"), ["alpha"]);
  assert.deepEqual(names("cn", "bet"), []);
});

test("credit sort keeps unknown balances last in both directions", () => {
  const { context } = loadPanel();
  const accounts = [
    { nickname: "unknown" },
    { nickname: "low", credits: { total_remain: 5, total_used: 1, total_size: 6 } },
    { nickname: "high", credits: { total_remain: 90, total_used: 1, total_size: 91 } },
  ];
  assert.deepEqual(context.sortAccounts(accounts, 1).map((a) => a.nickname), ["high", "low", "unknown"]);
  assert.deepEqual(context.sortAccounts(accounts, 2).map((a) => a.nickname), ["low", "high", "unknown"]);
  assert.deepEqual(context.sortAccounts(accounts, 0).map((a) => a.nickname), ["unknown", "low", "high"]);
  assert.equal(accounts[0].nickname, "unknown", "sorting must not mutate the source list");
});

// Account identity stays off the card face: the panel is often projected on a
// shared screen, and uid/file name are the fields that leak who owns it.
test("cards show region and never expose uid or file name", () => {
  const { context } = loadPanel();
  const html = context.card({
    auth_index: "a1", name: "qoder-uid-42.json", uid: "uid-42", nickname: "hex", region: "global",
    plan: "Pro", disabled: false, exhausted: false,
    credits: { total_remain: 100, total_used: 20, total_size: 120, packages: [] },
  });
  assert.match(html, /Global/);
  assert.doesNotMatch(html, /uid-42/);
  assert.doesNotMatch(html, /qoder-uid-42\.json/);
});

test("summary counts only spendable accounts as available", () => {
  const { context, elements } = loadPanel();
  context.renderSummary([
    { nickname: "cn", region: "cn", credits: { total_remain: 100, total_used: 20, total_size: 120 } },
    { nickname: "off", region: "cn", disabled: true, credits: { total_remain: 50, total_used: 10, total_size: 60 } },
    { nickname: "gone", region: "global", exhausted: true, credits: { total_remain: 0, total_used: 30, total_size: 30 } },
  ]);
  const text = elements.get("summaryBox").innerHTML;
  assert.match(text, /可用 100/);
  // Disabled and exhausted accounts stay out of both the headline and the
  // per-region split, so the two numbers never disagree.
  assert.doesNotMatch(text, /CN 可用 150/);
  assert.match(text, /CN 可用 100/);
});
// Region totals are per-account facts; summing them twice makes the panel claim
// twice the credits the account actually has.
test("region totals count each account exactly once", () => {
  const { context, elements } = loadPanel();
  context.renderSummary([
    { nickname: "a", region: "cn", credits: { total_remain: 1200, total_used: 800, total_size: 2000 } },
  ]);
  const text = elements.get("summaryBox").innerHTML;
  assert.match(text, /CN 可用 1200/);
  assert.doesNotMatch(text, /CN 可用 2400/);
});

// An empty quota pool is "nothing to spend", not "everything used". A full red
// bar next to "已用 0" reads as the opposite of what the account state is.
test("zero-pool cards render an empty bar", () => {
  const { context } = loadPanel();
  const html = context.progressHTML({ total_remain: 0, total_used: 0, total_size: 0 }, false);
  assert.match(html, /width:0%/);
  assert.doesNotMatch(html, /width:100%/);
});
