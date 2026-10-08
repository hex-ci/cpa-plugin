#!/usr/bin/env python3
"""
Qoder CN (qoder.com.cn) 手机号验证码登录 → 创建 PAT (pt-)

用法:
  python3 qoder_cn_pat_login.py --phone <你的手机号>        # 交互式: 发码后终端输入验证码
  python3 qoder_cn_pat_login.py --phone <你的手机号> --code-file ./qw_web/sms_code.txt   # 文件模式(配合后台运行)
  python3 qoder_cn_pat_login.py --phone <你的手机号> --name my-pat --days 3650              # PAT 名称/有效期(天)

流程:
  qoder.com.cn/users/sign-in → 使用阿里云登录 → passport.aliyun.com SSO (SMS iframe)
  → 填手机号 → 勾选协议 → 发验证码 → 输入验证码 → 登录回跳
  → POST /api/v1/me/personal-access-tokens (web session) → pt-xxx

产物:
  --out 指定 JSON: {pat, token_id, expires_at, user:{id,name}, storage_state, created_at}
  storage_state 文件可复用(已登录 cookie)，之后加 --reuse-state 可跳过短信直接造新 PAT。

依赖: playwright + chromium (python3 -m playwright install chromium)
注意: --single-process 在 arm64 主机上必需，否则 page crash。
"""
from __future__ import annotations

import argparse
import asyncio
import json
import sys
import time
from pathlib import Path

from playwright.async_api import async_playwright  # pyright: ignore[reportMissingImports]

LAUNCH_ARGS = [
    "--no-sandbox", "--disable-dev-shm-usage", "--disable-gpu",
    "--single-process", "--no-zygote",
]
UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
SIGNIN_URL = "https://qoder.com.cn/users/sign-in"


def log(*a):
    print(*a, flush=True)


async def shot(page, path: Path):
    try:
        await page.screenshot(path=str(path), timeout=12000)
    except Exception:
        pass


async def read_code_interactive() -> str:
    """终端提示输入验证码（async 不阻塞事件循环）。"""
    loop = asyncio.get_running_loop()
    return (await loop.run_in_executor(None, lambda: input("📨 请输入收到的短信验证码: "))).strip()


async def read_code_from_file(path: Path, timeout_s: int = 420) -> str | None:
    t0 = time.time()
    while time.time() - t0 < timeout_s:
        if path.exists():
            code = path.read_text().strip()
            if code:
                return code
        await asyncio.sleep(2)
    return None


async def wait_sms_frame(page, timeout_s: int = 30):
    t0 = time.time()
    while time.time() - t0 < timeout_s:
        f = next((x for x in page.frames if "appEntrance=qoder_sms" in x.url), None)
        if f:
            return f
        await asyncio.sleep(1)
    return None


async def do_login(page, phone: str, args, workdir: Path) -> bool:
    log("[1] 打开 qoder 登录页")
    await page.goto(SIGNIN_URL, wait_until="domcontentloaded", timeout=60000)
    await page.wait_for_timeout(5000)
    await page.locator("text=使用阿里云登录").first.click(timeout=8000)
    await page.wait_for_timeout(12000)

    sms_frame = await wait_sms_frame(page)
    if not sms_frame:
        log("❌ 未找到 SMS 登录 iframe")
        await shot(page, workdir / "err_frame.png")
        return False
    log("[2] SMS iframe 就绪")

    phone_input = sms_frame.locator("input[name='fm-sms-login-id']")
    await phone_input.click()
    await phone_input.fill(phone)

    try:
        cb = sms_frame.locator("input[name='fm-agreement-checkbox']")
        if not await cb.is_checked():
            await cb.check(force=True)
    except Exception as e:
        log("⚠️  勾选协议失败（可能已勾选）:", e)

    await sms_frame.locator("a.send-btn-link").first.click()
    log(f"[3] 验证码已发送到 {phone}")
    await page.wait_for_timeout(3000)
    await shot(page, workdir / "10_sent.png")

    if args.code_file:
        log(f"[4] 等待验证码文件 {args.code_file} ...")
        code = await read_code_from_file(Path(args.code_file))
        if not code:
            log("❌ 超时未收到验证码文件")
            return False
    else:
        code = await read_code_interactive()
    log(f"[5] 使用验证码 {code}")

    code_input = sms_frame.locator("input[name='fm-smscode']")
    await code_input.click()
    await code_input.fill(code)
    await page.wait_for_timeout(500)

    await sms_frame.locator(
        "button:has-text('登录'), button:has-text('登录/注册'), a:has-text('登录/注册'), [class*=submit]"
    ).first.click()
    log("[6] 已提交登录，等待回跳 qoder.com.cn ...")

    t0 = time.time()
    while time.time() - t0 < 90:
        u = page.url
        if "qoder.com.cn" in u and "sign-in" not in u and "sso" not in u:
            break
        await asyncio.sleep(1)
    log("[7] 登录落地:", page.url)
    await page.wait_for_timeout(5000)
    return True


async def create_pat(page, name: str, days: int) -> dict:
    expires_ms = int(time.time() * 1000) + days * 24 * 3600 * 1000
    log(f"[8] 创建 PAT name={name} days={days}")
    result = await page.evaluate(
        """async ({name, expires}) => {
            const r = await fetch('/api/v1/me/personal-access-tokens', {
                method: 'POST', credentials: 'include',
                headers: {'Content-Type': 'application/json',
                          'Accept': 'application/json',
                          'x-requested-with': 'XMLHttpRequest'},
                body: JSON.stringify({name, expires_at: expires})
            });
            return {status: r.status, body: await r.text()};
        }""",
        {"name": name, "expires": expires_ms},
    )
    log("[9] 创建响应:", result["status"], result["body"][:300])
    if result["status"] >= 300:
        raise RuntimeError(f"PAT 创建失败 HTTP {result['status']}: {result['body'][:300]}")
    data = json.loads(result["body"])
    pat = data.get("token") or (data.get("data") or {}).get("token")
    if not pat:
        raise RuntimeError(f"响应中无 token: {result['body'][:300]}")
    return data


async def fetch_me(page) -> dict:
    r = await page.evaluate(
        """async () => {
            const r = await fetch('/api/v1/me', {credentials: 'include'});
            return {status: r.status, body: await r.text()};
        }"""
    )
    if r["status"] != 200:
        return {}
    try:
        return json.loads(r["body"])
    except Exception:
        return {}


async def main() -> int:
    ap = argparse.ArgumentParser(description="Qoder CN 手机验证码登录 → 创建 PAT")
    ap.add_argument("--phone", required=True, help="手机号（必填，不设默认值以免误用他人号码）")
    ap.add_argument("--name", default="cpa-auto", help="PAT 名称")
    ap.add_argument("--days", type=int, default=36500, help="PAT 有效期(天)，默认 100 年")
    ap.add_argument("--code-file", default=None, help="从文件读验证码(后台模式); 默认终端交互输入")
    ap.add_argument("--reuse-state", default=None, help="复用已登录 storage_state JSON，跳过短信登录直接造 PAT")
    ap.add_argument("--out", default="./qoderwork_pat.json", help="结果输出 JSON 路径")
    ap.add_argument("--workdir", default="./qw_web", help="截图/中间产物目录")
    args = ap.parse_args()

    workdir = Path(args.workdir)
    workdir.mkdir(parents=True, exist_ok=True)
    if args.code_file:
        Path(args.code_file).unlink(missing_ok=True)

    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=True, args=LAUNCH_ARGS)
        reuse = bool(args.reuse_state) and Path(args.reuse_state).exists()
        if reuse:
            log(f"[0] 复用登录态 {args.reuse_state}")
            ctx = await browser.new_context(
                viewport={"width": 1440, "height": 900}, locale="zh-CN", user_agent=UA,
                storage_state=args.reuse_state,
            )
        else:
            ctx = await browser.new_context(
                viewport={"width": 1440, "height": 900}, locale="zh-CN", user_agent=UA,
            )
        page = await ctx.new_page()

        try:
            if reuse:
                await page.goto("https://qoder.com.cn/", wait_until="domcontentloaded", timeout=60000)
                await page.wait_for_timeout(3000)
                me = await fetch_me(page)
                if not me.get("id"):
                    log("❌ 登录态已失效，请去掉 --reuse-state 重新登录")
                    return 1
                log(f"[✓] 登录态有效: {me.get('name')} ({me.get('id')})")
            else:
                ok = await do_login(page, args.phone, args, workdir)
                if not ok:
                    return 1
                me = await fetch_me(page)
                log(f"[✓] 已登录: {me.get('name', '?')} ({me.get('id', '?')})")

            state_path = workdir / "storage_loggedin.json"
            await ctx.storage_state(path=str(state_path))

            pat_data = await create_pat(page, args.name, args.days)

            result = {
                "pat": pat_data.get("token"),
                "token_id": pat_data.get("token_id"),
                "name": pat_data.get("name"),
                "created_at": pat_data.get("created_at"),
                "expires_at": pat_data.get("expires_at"),
                "user": {"id": me.get("id"), "name": me.get("name"), "username": me.get("username")},
                "storage_state": str(state_path),
                "realm": "cn",
            }
            out_path = Path(args.out)
            out_path.write_text(json.dumps(result, ensure_ascii=False, indent=2))
            log(f"\n🔑 PAT: {result['pat']}")
            log(f"📁 已保存: {out_path}")
            log(f"🍪 登录态: {state_path}  (下次可用 --reuse-state 跳过短信)")
            return 0
        finally:
            await browser.close()


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
