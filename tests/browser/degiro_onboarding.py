"""Exercise account setup with synthetic credentials; optionally record the happy path.

ONBOARDING_RECORDING=/absolute/output/directory .venv/bin/python tests/browser/degiro_onboarding.py
"""
import asyncio
from copy import deepcopy
from datetime import datetime, timezone
import json
import os
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from patchright.async_api import async_playwright, expect
from portfolio_valuation import SNAPSHOT, STATIC


async def exercise(browser, width, recording=None):
    options = {"viewport": {"width": width, "height": 900}, "locale": "en-IE"}
    if recording:
        Path(recording).mkdir(parents=True, exist_ok=True)
        options.update(record_video_dir=recording, record_video_size=options["viewport"])
    context = await browser.new_context(**options)
    page = await context.new_page()
    errors, triggers, connections = [], [], []
    page.on("pageerror", lambda error: errors.append(str(error)))
    accounts = [{"id": "default", "name": "Main account", "connected": True}]
    status = {"pull_running": False}
    login_gate, login_failure, automatic = None, False, False
    imported = False

    def progress(percent, account="tilde"):
        nonlocal status, imported
        running = percent < 100
        stage = {25: "Fetching transactions", 50: "Fetching holdings", 75: "Saving snapshot", 100: "Sync complete"}[percent]
        status = {"pull_running": running, "progress": {
            "running": running, "percent": percent, "stage": stage,
            "started_at": status.get("progress", {}).get("started_at") or datetime.now(timezone.utc).isoformat(),
            "finished_at": "0001-01-01T00:00:00Z" if running else datetime.now(timezone.utc).isoformat(),
            "accounts": [{"id": account, "name": "Tilde" if account == "tilde" else "Main account",
                          "state": "running" if running else "complete", "stage": stage}],
        }}
        if not running:
            imported = True
            accounts[1]["last_pull"] = status["progress"]["finished_at"]

    async def route_request(route):
        url = urlparse(route.request.url)
        account_id = parse_qs(url.query).get("account", ["default"])[0]
        path, method = url.path, route.request.method
        code = 200
        if path == "/api/sync/events":
            return await route.fulfill(status=503, body="Synthetic API uses polling")
        if path == "/changelog.json":
            return await route.fulfill(content_type="application/json", body='{"version":"dev","releases":[]}')
        if path == "/api/accounts":
            if method == "POST":
                accounts.append({"id": "tilde", "name": route.request.post_data_json["name"], "connected": False})
                body, code = accounts[-1], 201
            else:
                body = accounts
        elif path == "/api/degiro/reauthenticate":
            connections.append((account_id, route.request.post_data_json))
            await login_gate.wait()
            if login_failure:
                code, body = 422, {"error": "DEGIRO sign-in did not complete; check your username, password, and authenticator setup secret"}
            else:
                accounts[1]["connected"] = True
                if automatic:
                    progress(25, "default" if automatic == "other" else "tilde")
                body = {"status": "connected", "account_id": account_id}
        elif path == "/api/pull/trigger":
            triggers.append(account_id)
            progress(25)
            code, body = 202, {"status": "pull started"}
        elif path == "/api/status":
            body = {**status, "account": next(a for a in accounts if a["id"] == account_id)}
        elif path == "/api/portfolio/snapshot":
            body = deepcopy(SNAPSHOT) if imported or account_id == "default" else {"holdings": [], "transactions": [], "history": []}
        elif path == "/api/versions":
            body = {"app": "demo", "worker": {"version": "demo"}}
        elif path.startswith("/api/"):
            body = []
        else:
            target = STATIC / (path.lstrip("/") or "index.html")
            return await route.fulfill(path=str(target)) if target.is_file() else await route.fulfill(status=404)
        await route.fulfill(status=code, content_type="application/json", body=json.dumps(body))

    async def pause(seconds=1):
        if recording:
            await asyncio.sleep(seconds)

    async def submit_credentials():
        nonlocal login_gate
        login_gate = asyncio.Event()
        await page.locator("#degiroUsername").fill("demo.tilde")
        await page.locator("#degiroPassword").fill("synthetic-password")
        await page.locator("#degiroSeed").fill("A" * 32)
        await pause()
        await page.locator("#submitLoginButton").click()
        await expect(page.locator("#connectProgress")).to_be_visible()
        await expect(page.locator("#connectCredentials")).to_be_hidden()
        await expect(page.locator("#degiroUsername")).to_be_disabled()
        await expect(page.locator("#degiroDialog .close")).to_be_disabled()
        await expect(page.locator("#submitLoginButton")).to_be_disabled()
        await expect(page.locator("#degiroPassword")).to_have_value("")
        await expect(page.locator("#degiroSeed")).to_have_value("")
        await page.keyboard.press("Escape")
        await expect(page.locator("#degiroDialog")).to_be_visible()
        assert await page.locator("#degiroDialog").evaluate("el => el.scrollWidth <= el.clientWidth")

    await page.route("**/*", route_request)
    try:
        await page.goto("http://127.0.0.1:8391/")
        await page.locator('#appNav [data-view="accounts"]').click()
        await pause()
        await page.locator("#addAccountButton").click()
        await page.locator('#accountForm [name="name"]').fill("Tilde")
        await pause()
        await page.locator('#accountForm [type="submit"]').click()
        await expect(page.locator("#connectCopy")).to_contain_text("Connect Tilde")
        await submit_credentials()
        await expect(page.locator("#connectElapsed")).not_to_have_text("Elapsed · 0:00")
        if recording:
            await page.screenshot(path=str(Path(recording) / f"connecting-{width}.png"))
        await pause(4)
        login_gate.set()
        await expect(page.locator("#degiroDialog")).to_be_hidden()
        await expect(page.locator("#accountName")).to_have_text("Tilde")
        await expect(page.locator("#progressValue")).to_have_text("25%")
        await expect(page.locator("#syncProgress")).to_be_visible()
        await expect(page.locator("#pullButton")).to_be_disabled()
        assert triggers == ["tilde"]
        assert connections[0][0] == "tilde"
        assert connections[0][1]["password"] == "synthetic-password"
        for percent in (50, 75, 100):
            await pause(2)
            progress(percent)
            await expect(page.locator("#progressValue")).to_have_text(f"{percent}%")
        await expect(page.locator("#brokerPortfolio")).to_have_text("€1,050.00")
        await expect(page.locator("#pullButton")).to_be_enabled()
        await pause(3)
        assert await page.evaluate("document.documentElement.scrollWidth <= innerWidth")
        if recording:
            await page.screenshot(path=str(Path(recording) / f"first-sync-{width}.png"))
        await page.locator("#changelogButton").click()
        await expect(page.locator("#changelogDialog")).to_be_visible()
        await expect(page.locator("#changelogVersion")).to_have_text("Installed · demo")
        await expect(page.locator("#changelogStatus")).to_have_text("No release notes available for this build.")
        assert await page.locator("#changelogDialog").evaluate("el => el.scrollWidth <= el.clientWidth")
        await pause(3)
        if recording:
            await page.screenshot(path=str(Path(recording) / f"changelog-{width}.png"))
        await page.locator('#changelogDialog [aria-label="Close release notes"]').click()
        await expect(page.locator("#changelogDialog")).to_be_hidden()
        if recording:
            video = page.video
            await page.close()
            await video.save_as(str(Path(recording) / "degitide-onboarding.webm"))
            await video.delete()
            # Remaining regression cases do not go into the demo recording.
            await context.close()
            context = await browser.new_context(viewport=options["viewport"])
            page = await context.new_page()
            page.on("pageerror", lambda error: errors.append(str(error)))
            await page.route("**/*", route_request)
            await page.goto("http://127.0.0.1:8391/")

        # Sign-in errors restore editable fields, preserve username and allow retry.
        await page.locator('#appNav [data-view="accounts"]').click()
        await page.locator('[data-connect="tilde"]').click()
        login_failure = True
        await submit_credentials()
        login_gate.set()
        await expect(page.locator("#loginError")).to_contain_text("DEGIRO sign-in did not complete")
        await expect(page.locator("#connectCredentials")).to_be_visible()
        await expect(page.locator("#degiroUsername")).to_have_value("demo.tilde")
        await expect(page.locator("#degiroPassword")).to_be_focused()
        await expect(page.locator("#submitLoginButton")).to_be_enabled()
        assert triggers == ["tilde"], "failed sign-in must not start an import"

        # An automatic first sync is followed without a duplicate trigger.
        login_failure, automatic = False, True
        await submit_credentials()
        await expect(page.locator("#loginError")).to_be_hidden()
        login_gate.set()
        await expect(page.locator("#degiroDialog")).to_be_hidden()
        await expect(page.locator("#progressValue")).to_have_text("25%")
        assert triggers == ["tilde"]
        progress(100)
        await expect(page.locator("#pullButton")).to_be_enabled()

        # Do not claim this account is importing when a different account is busy.
        await page.locator('#appNav [data-view="accounts"]').click()
        await page.locator('[data-connect="tilde"]').click()
        automatic = "other"
        await submit_credentials()
        login_gate.set()
        await expect(page.locator("#notice")).to_contain_text("Another account is syncing")
        assert triggers == ["tilde"]
        assert not errors, errors
    finally:
        if login_gate:
            login_gate.set()
        await context.close()


async def main():
    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=True)
        for width in (1440, 390):
            await exercise(browser, width, os.environ.get("ONBOARDING_RECORDING") if width == 1440 else None)
        await browser.close()
    print("DEGIRO onboarding passed: waiting, first sync, retry, automatic sync races and mobile layout")


if __name__ == "__main__":
    asyncio.run(main())
