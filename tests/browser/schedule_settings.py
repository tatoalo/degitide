"""Exercise dashboard scheduling controls against a synthetic API at desktop/mobile widths."""
import asyncio
import json
from urllib.parse import parse_qs, urlparse

from patchright.async_api import async_playwright, expect
from portfolio_valuation import SNAPSHOT, STATIC

DEFAULTS = {"mode": "market", "interval_minutes": 60, "close_delay_minutes": 20,
            "maintenance": "CRON_TZ=Europe/Dublin 0 2 * * *", "cron": ""}


async def exercise(browser, width):
    page = await browser.new_page(viewport={"width": width, "height": 900}, timezone_id="America/Los_Angeles")
    accounts = [{"id": "default", "name": "Demo", "connected": True},
                {"id": "second", "name": "Tilde", "connected": True}]
    overrides, writes, errors = {}, [], []
    fail_write = False
    page.on("pageerror", lambda error: errors.append(str(error)))

    async def route_request(route):
        url = urlparse(route.request.url)
        path, method = url.path, route.request.method
        account = parse_qs(url.query).get("account", [""])[0]
        if path == "/api/schedule":
            if method in ("PUT", "DELETE"):
                writes.append((account, method, route.request.post_data_json))
                if fail_write:
                    return await route.fulfill(status=500, content_type="application/json", body='{"error":"Could not save settings"}')
                if method == "PUT":
                    overrides[account] = route.request.post_data_json
                else:
                    overrides.pop(account, None)
            defaults = overrides.get("", DEFAULTS) if account else DEFAULTS
            source = ("account" if account else "dashboard") if account in overrides else ("workspace" if account else "environment")
            body = {"settings": overrides.get(account, defaults), "defaults": defaults, "source": source}
        elif path == "/api/status":
            saved = overrides.get(account, overrides.get("", DEFAULTS))
            body = {"account": next(a for a in accounts if a["id"] == account),
                    "schedule": {**saved, "calendar_available": True, "markets": [{"mic": "XGAT", "name": "Tradegate", "open": True}],
                                 "reason": "Automatic sync paused" if saved["mode"] == "manual" else "Market-hours refresh"}}
            if saved["mode"] == "cron":
                body["schedule"].update(timezone="Europe/Dublin", reason="Custom schedule",
                    next_sync="2026-11-19T18:00:00Z", upcoming_runs=["2026-11-19T18:00:00Z", "2026-11-20T18:00:00Z", "2026-11-23T18:00:00Z"])
        elif path == "/api/accounts":
            body = accounts
        elif path == "/api/portfolio/snapshot":
            body = SNAPSHOT
        elif path == "/api/versions":
            body = {"app": "test", "worker": {"version": "test"}}
        elif path.startswith("/api/"):
            assert method == "GET", "settings must not start a sync"
            body = []
        else:
            target = STATIC / (path.lstrip("/") or "index.html")
            if target.is_file():
                return await route.fulfill(path=str(target))
            return await route.fulfill(status=404, body="Not found")
        await route.fulfill(content_type="application/json", body=json.dumps(body))

    async def open_settings():
        await page.locator("#scheduleSettingsButton").click()
        await expect(page.locator("#scheduleDialog")).to_be_visible()
        assert await page.locator("#scheduleDialog").evaluate("el => el.scrollWidth <= el.clientWidth")

    await page.route("**/*", route_request)
    await page.goto("http://127.0.0.1:8391/")
    await page.get_by_text("How scheduling works", exact=True).click()
    await expect(page.locator(".schedule-explainer")).to_contain_text("10 hours")
    await page.locator("#accountSelect").select_option("second")
    await expect(page.locator("#accountName")).to_have_text("Tilde")
    await open_settings()
    await expect(page.locator("#scheduleTitle")).to_contain_text("Tilde")
    await expect(page.locator("#scheduleSource")).to_contain_text("follows the workspace schedule")
    await expect(page.locator("#scheduleInterval")).to_have_value("60")
    await page.locator("#scheduleInterval").fill("90")
    await page.locator("#scheduleDialog .close").click()
    assert writes == [], "cancel must not save"
    await open_settings()
    await expect(page.locator("#scheduleInterval")).to_have_value("60")
    await page.locator("#scheduleInterval").fill("4")
    await page.locator("#saveSchedule").click()
    assert writes == [], "invalid interval must not submit"
    await page.locator("#scheduleInterval").fill("90")
    await page.locator("#scheduleCloseDelay").fill("35")
    await page.locator("#scheduleSafetyTime").fill("01:45")
    await page.locator("#scheduleTimezone").fill("America/New_York")
    fail_write = True
    await page.locator("#saveSchedule").click()
    await expect(page.locator("#scheduleError")).to_have_text("Could not save settings")
    await expect(page.locator("#scheduleDialog")).to_be_visible()
    assert not overrides
    fail_write = False
    await page.locator("#saveSchedule").click()
    await expect(page.locator("#scheduleDialog")).to_be_hidden()
    assert overrides["second"]["maintenance"] == "CRON_TZ=America/New_York 45 1 * * *"
    await expect(page.locator("#marketCadence")).to_contain_text("Every 90 min")
    await expect(page.locator("#marketCadence")).to_contain_text("35 min after close")
    await page.reload()
    await expect(page.locator("#marketCadence")).to_contain_text("Hourly")
    await page.locator("#accountSelect").select_option("second")
    await expect(page.locator("#accountName")).to_have_text("Tilde")
    await open_settings()
    await expect(page.locator("#scheduleInterval")).to_have_value("90")
    await expect(page.locator("#scheduleSafetyTime")).to_have_value("01:45")
    await expect(page.locator("#scheduleTimezone")).to_have_value("America/New_York")
    await expect(page.locator("#scheduleSource")).to_contain_text("Tilde uses its own schedule")
    await page.locator("#scheduleMode").select_option("manual")
    await page.locator("#saveSchedule").click()
    await expect(page.locator("#marketState")).to_have_text("Automatic sync paused")
    await expect(page.locator("#marketCadence")).to_have_text("")
    await open_settings()
    await page.locator("#scheduleMode").select_option("cron")
    await page.locator("#scheduleCron").fill("CRON_TZ=Europe/Dublin 0 18 * * 1-5")
    await page.locator("#saveSchedule").click()
    await expect(page.locator("#marketState")).to_have_text("Custom schedule")
    await expect(page.locator("#marketCadence")).to_have_text("Weekdays at 18:00 · Europe/Dublin")
    await expect(page.locator("#nextSync")).to_contain_text("18:00")
    await expect(page.locator("#scheduleRuns li")).to_have_count(3)
    await expect(page.locator("#scheduleRuns")).to_contain_text("18:00")
    await expect(page.locator("#scheduleExpression")).to_have_text(overrides["second"]["cron"])
    overrides["second"]["cron"] = "CRON_TZ=Europe/Dublin */15 9-17 * * 1-5"
    await page.locator("#refreshButton").click()
    await expect(page.locator("#marketCadence")).to_have_text("Weekdays · every 15 min, 09:00–17:45 · Europe/Dublin")
    # Complex cron remains visible verbatim; don't invent a simplified meaning.
    overrides["second"]["cron"] = "CRON_TZ=Europe/Dublin 0 9 1 * 1"
    await page.locator("#refreshButton").click()
    await expect(page.locator("#scheduleExpression")).to_have_text(overrides["second"]["cron"])
    await expect(page.locator("#marketCadence")).to_have_text("Custom cron · Europe/Dublin")
    await open_settings()
    await page.locator("#resetSchedule").click()
    await expect(page.locator("#marketCadence")).to_contain_text("Hourly")
    assert not overrides and all(account == "second" for account, _, _ in writes)
    # Workspace defaults remain editable and inherited by both accounts.
    await page.locator('button[data-view="accounts"]').click()
    await page.locator("#workspaceScheduleButton").click()
    await expect(page.locator("#scheduleTitle")).to_have_text("Workspace sync defaults")
    await page.locator("#scheduleInterval").fill("75")
    await page.locator("#saveSchedule").click()
    await expect(page.locator("#scheduleDialog")).to_be_hidden()
    assert overrides[""]["interval_minutes"] == 75
    await page.locator('button[data-view="overview"]').click()
    await expect(page.locator("#marketCadence")).to_contain_text("Every 75 min")
    # Preserve an existing advanced deployment rule when editing another field.
    overrides[""]["maintenance"] = "CRON_TZ=UTC 0 5 * * 1-5"
    await open_settings()
    await expect(page.locator("#customSafety")).to_be_checked()
    await expect(page.locator("#scheduleMaintenance")).to_have_value(overrides[""]["maintenance"])
    await page.locator("#scheduleInterval").fill("120")
    await page.locator("#saveSchedule").click()
    await expect(page.locator("#scheduleDialog")).to_be_hidden()
    assert overrides["second"]["maintenance"] == "CRON_TZ=UTC 0 5 * * 1-5"
    assert await page.evaluate("document.documentElement.scrollWidth <= innerWidth")
    assert not errors, errors
    await page.close()


async def main():
    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=True)
        for width in (1440, 390):
            await exercise(browser, width)
        await browser.close()
    print("Scheduling controls passed at desktop and mobile widths")


if __name__ == "__main__":
    asyncio.run(main())
