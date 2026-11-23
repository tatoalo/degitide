"""Render financial snapshot states with synthetic responses and no network.

Run: .venv/bin/python tests/browser/portfolio_valuation.py
Optional screenshots: VALUATION_SCREENSHOTS=/absolute/output/directory
"""
import asyncio
from copy import deepcopy
import json
import os
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from patchright.async_api import async_playwright, expect

STATIC = Path(__file__).resolve().parents[2] / "internal/server/static"
SNAPSHOT = {
    "holdings": [
        {"product_id": "100", "symbol": "NTH", "name": "Example North Fund", "quantity": 10,
         "avg_price": 20.25, "market_value": 250.00, "currency": "USD", "value_currency": "EUR",
         "exchange_mic": "XETR", "exchange_name": "Xetra",
         "price_as_of": "2026-11-16T16:05:00Z", "valuation_source": "broker_quote"},
        {"product_id": "200", "symbol": "STH", "name": "Example South Fund", "quantity": 16,
         "avg_price": 40.00, "market_value": 800.00, "currency": "EUR", "value_currency": "EUR",
         "exchange_mic": "XETR", "exchange_name": "Xetra",
         "price_as_of": "2026-11-16T16:05:00Z", "valuation_source": "broker_quote"},
    ],
    "transactions": [],
    "history": [{"date": "2024-11-15", "currency": "EUR", "value": 700, "source": "report", "net_invested": None, "profit_loss": None},
                {"date": "2026-11-16", "currency": "EUR", "value": 1050.00, "source": "snapshot", "net_invested": None, "profit_loss": None}],
    "valuation": {"base_currency": "EUR", "portfolio_value": 1050.00, "cash_balance": 12.50,
                  "account_value": 1062.50, "total_pl": 75.25, "day_pl": -4.25,
                  "fetched_at": "2026-11-16T16:10:00Z",
                  "price_status": "quoted", "reconciliation_difference": 0, "warning": ""},
}


async def exercise(browser, width):
    page = await browser.new_page(viewport={"width": width, "height": 1000}, locale="en-IE", timezone_id="Europe/Dublin")
    errors, requested = [], []
    page.on("pageerror", lambda error: errors.append(str(error)))
    snapshot = deepcopy(SNAPSHOT)
    sync_times = {"default": "2026-11-16T16:10:00Z", "savings": "2026-11-17T09:08:07Z"}
    schedule = {"mode": "market", "interval_minutes": 60, "calendar_available": True,
                "markets": [{"mic": "XETR", "name": "Xetra", "open": True}], "unknown_holdings": 0,
                "next_sync": "2026-11-16T16:50:00Z", "reason": "After market close",
                "server_timezone": "UTC"}

    async def route_request(route):
        url = urlparse(route.request.url)
        path = url.path
        account_id = parse_qs(url.query).get("account", ["default"])[0]
        requested.append(path)
        if path.startswith("/api/"):
            body = []
            if path == "/api/accounts":
                body = [{"id": account, "name": name, "connected": True, "last_pull": sync_times[account]}
                        for account, name in (("default", "Main account"), ("savings", "Savings"))]
            elif path == "/api/status":
                body = {"account": {"id": account_id, "name": "Savings" if account_id == "savings" else "Main account",
                                    "connected": True, "last_pull": sync_times[account_id]}, "schedule": schedule}
            elif path == "/api/versions":
                body = {"app": "dev", "worker": {"version": "dev"}}
            elif path == "/api/portfolio/snapshot":
                body = snapshot
            await route.fulfill(content_type="application/json", body=json.dumps(body))
        else:
            target = STATIC / (path.lstrip("/") or "index.html")
            if target.is_file():
                await route.fulfill(path=str(target))
            else:
                await route.fulfill(status=404, body="Not found")

    async def screenshot(name):
        assert await page.evaluate("document.documentElement.scrollWidth <= innerWidth")
        if directory := os.environ.get("VALUATION_SCREENSHOTS"):
            Path(directory).mkdir(parents=True, exist_ok=True)
            await page.screenshot(path=str(Path(directory) / f"portfolio-{name}-{width}.png"), full_page=True)

    await page.route("**/*", route_request)
    # Returning browsers with the retired Classic preference migrate too.
    await page.add_init_script("localStorage.setItem('degitide.appearance', 'classic')")
    await page.goto("http://127.0.0.1:8391/")
    await expect(page.locator("#portfolioValue")).to_have_text("€1,050.00")
    await expect(page.locator("#brokerBalance")).to_have_text("€1,062.50")
    await expect(page.locator("#brokerCash")).to_have_text("€12.50")
    await expect(page.locator("#brokerPL")).to_have_text("+€75.25")
    await expect(page.locator("#marketState")).to_have_text("Markets open")
    await expect(page.locator("#marketVenues")).to_have_text("Xetra (XETR)")
    await expect(page.locator("#nextSync")).to_contain_text("16:50")
    await expect(page.locator("#nextSync")).not_to_contain_text("server")
    schedule["server_timezone"] = "Europe/Berlin"
    await page.locator("#refreshButton").click()
    await expect(page.locator("#nextSync")).to_contain_text("16:50")
    await expect(page.locator("#nextSync")).to_contain_text("server")
    await expect(page.locator("#nextSync")).to_contain_text("17:50")
    schedule["server_timezone"] = "UTC"
    await page.locator("#refreshButton").click()
    await expect(page.locator("#nextSync")).not_to_contain_text("server")
    await expect(page.locator("#marketCadence")).to_contain_text("Hourly")
    await expect(page.locator("#brokerDayContext")).to_have_text("At sync · 16 Nov 2026, 16:10")
    await expect(page.locator('[data-metric="total-pl"]')).to_have_attribute("data-direction", "gain")
    assert await page.locator("#brokerPL").evaluate("element => element.getBoundingClientRect().height < parseFloat(getComputedStyle(element).fontSize) * 1.8"), "P&L sign wraps away from the value"
    await expect(page.locator('[data-metric="day-pl"]')).to_have_attribute("data-direction", "loss")
    await expect(page.locator("#summarySyncTime")).to_have_attribute("datetime", sync_times["default"])
    await expect(page.locator("#summaryLastSync")).to_contain_text("16 Nov 2026, 16:10:00 GMT")
    await expect(page.locator("#chartCurrency option")).to_have_count(1)
    await expect(page.locator("#allocationNote")).to_have_text("Share of positive holdings in EUR.")
    await expect(page.locator("#performanceSummary")).to_be_hidden()
    await expect(page.locator("#holdingsBody")).to_contain_text("Quote · 16 Nov 2026, 16:05")
    await expect(page.locator("#holdingsBody")).to_contain_text("US$20.25")
    await screenshot("quoted")
    # Reproduce the reported overnight floating-point residue, then a real loss.
    for value, text, direction in ((-1e-12, "€0.00", "neutral"), (-0.0049, "€0.00", "neutral"), (-0.005, "-€0.01", "loss")):
        snapshot["valuation"]["day_pl"] = value
        await page.locator("#refreshButton").click()
        await expect(page.locator("#brokerDayPL")).to_have_text(text)
        await expect(page.locator('[data-metric="day-pl"]')).to_have_attribute("data-direction", direction)
    snapshot["valuation"]["day_pl"] = -1e-12
    schedule["markets"][0]["open"] = False
    schedule["next_sync"] = "2026-11-17T08:00:00Z"
    await page.locator("#refreshButton").click()
    await expect(page.locator("#marketState")).to_have_text("Markets closed")
    await screenshot("overnight")
    await expect(page.get_by_text("How to read these P&L figures", exact=True)).to_have_count(0)
    snapshot["previous_session_day_pl"] = {"date": "2026-11-16", "currency": "EUR", "value": -4.25,
                                            "recorded_at": "2026-11-16T16:10:00Z"}
    await page.locator("#refreshButton").click()
    await expect(page.locator("#brokerDayLabel")).to_have_text("Previous session P&L")
    await expect(page.locator("#brokerDayPL")).to_have_text("-€4.25")
    await expect(page.locator("#brokerDayContext")).to_contain_text("Recorded · 16 Nov 2026")
    await expect(page.locator('[data-metric="day-pl"]')).to_have_attribute("data-direction", "loss")
    await screenshot("previous-session")
    snapshot["previous_session_day_pl"].update(value=None, recorded_at=None)
    await page.locator("#refreshButton").click()
    await expect(page.locator("#brokerDayPL")).to_have_text("—")
    await expect(page.locator("#brokerDayContext")).to_contain_text("no saved result")
    # A new session's genuine zero restores the current-day label and neutral mark.
    del snapshot["previous_session_day_pl"]
    await page.locator("#refreshButton").click()
    await expect(page.locator("#brokerDayLabel")).to_have_text("Day P&L")
    await expect(page.locator("#brokerDayPL")).to_have_text("€0.00")
    # A fast automatic sync can finish entirely between two 30-second polls.
    snapshot["valuation"]["portfolio_value"] = 1051.00
    sync_times["default"] = "2026-11-16T16:35:00Z"
    await page.evaluate("pollStatus()", isolated_context=False)
    await expect(page.locator("#brokerPortfolio")).to_have_text("€1,051.00")
    await expect(page.locator("#summarySyncTime")).to_have_attribute("datetime", sync_times["default"])
    snapshot["valuation"]["portfolio_value"] = 1050.00

    await expect(page.get_by_label("Dashboard appearance", exact=True)).to_have_count(0)
    await expect(page.locator(".app-header")).to_have_css("background-color", "rgb(26, 28, 22)")
    await expect(page.locator(".broker-direction").first).to_be_visible()
    await page.get_by_label("Broker account", exact=True).select_option("savings")
    await expect(page.locator("#accountName")).to_have_text("Savings")
    await expect(page.locator("#summarySyncTime")).to_have_attribute("datetime", sync_times["savings"])
    await expect(page.locator("#summaryLastSync")).to_contain_text("17 Nov 2026, 09:08:07 GMT")
    await page.get_by_role("button", name="1M", exact=True).click()
    snapshot_requests = requested.count("/api/portfolio/snapshot")
    await expect(page.locator("#accountSelect")).to_have_value("savings")
    await expect(page.locator('[data-range="1M"]')).to_have_attribute("aria-pressed", "true")
    await expect(page.locator("#brokerBalance")).to_have_text("€1,062.50")
    await expect(page.locator("#brokerBalance")).to_be_visible()
    await expect(page.locator(".broker-direction").first).to_be_visible()
    await screenshot("observatory")

    await page.get_by_role("button", name="Simulator", exact=True).click()
    await page.get_by_role("spinbutton", name="Starting value", exact=True).fill("1000")
    await page.get_by_role("spinbutton", name="Monthly contribution", exact=True).fill("100")
    await page.get_by_role("spinbutton", name="Assumed annual return (%)", exact=True).fill("0")
    await expect(page.locator("body")).to_have_attribute("data-current-view", "simulator")
    await page.evaluate("pollStatus()", isolated_context=False)
    await expect(page.locator("#marketSchedule")).to_be_hidden()
    await expect(page.locator("#accountSelect")).to_have_value("savings")
    await expect(page.locator("#simResult")).to_have_text("€13,000.00 after 10 years")
    assert requested.count("/api/portfolio/snapshot") == snapshot_requests

    await page.reload()
    await expect(page.locator("#portfolioValue")).to_have_text("€1,050.00")
    await expect(page.locator(".app-header")).to_have_css("background-color", "rgb(26, 28, 22)")
    await expect(page.locator(".broker-direction").first).to_be_visible()

    for holding in snapshot["holdings"]:
        holding["price_as_of"] = "2026-11-16"
    snapshot["valuation"]["warning"] = "Broker quote dates are shown; precise quote times are unavailable."
    await page.locator("#refreshButton").click()
    await expect(page.locator("#holdingsBody")).to_contain_text("Quote · 16 Nov 2026")
    await expect(page.locator("#holdingsBody")).not_to_contain_text("16:05")
    await expect(page.locator("#valuationNote")).to_contain_text("precise quote times are unavailable")
    await expect(page.locator("#brokerBalance")).to_have_text("€1,062.50")
    await screenshot("quote-date")

    snapshot["valuation"] = {"base_currency": "EUR", "price_status": "unavailable", "warning": "Quote valuation unavailable. Recorded broker values only."}
    schedule.update(calendar_available=False, markets=[], unknown_holdings=2, reason="Daily safety refresh")
    for holding in snapshot["holdings"]:
        holding.update(price_as_of="", valuation_source="broker_api")
    await page.locator("#refreshButton").click()
    await expect(page.locator("#brokerBalance")).to_have_text("—")
    await expect(page.locator("#brokerPL")).to_have_text("—")
    await expect(page.locator("#marketState")).to_have_text("Daily fallback")
    await expect(page.locator("#marketFallback")).to_contain_text("2 holdings")
    await expect(page.locator('[data-metric="total-pl"]')).to_have_attribute("data-direction", "unavailable")
    await expect(page.locator('[data-metric="total-pl"] .broker-direction')).to_be_hidden()
    await expect(page.locator("#valuationNote")).to_contain_text("unavailable")
    await expect(page.locator("#holdingsBody")).to_contain_text("Price time unavailable")
    await screenshot("fallback")

    snapshot["holdings"] = []
    sync_times["default"] = None
    snapshot["history"][-1]["value"] = 0
    snapshot["valuation"] = {"base_currency": "EUR", "portfolio_value": 0, "cash_balance": 50000,
                             "account_value": 50000, "price_status": "closing", "total_pl": 0, "day_pl": 0}
    await page.locator("#refreshButton").click()
    await expect(page.locator("#portfolioValue")).to_have_text("€0.00")
    await expect(page.locator("#brokerBalance")).to_have_text("€50,000.00")
    await expect(page.locator("#holdingsEmpty")).to_be_visible()
    await expect(page.locator('[data-metric="total-pl"]')).to_have_attribute("data-direction", "neutral")
    await expect(page.locator('[data-metric="day-pl"]')).to_have_attribute("data-direction", "neutral")
    await expect(page.locator("#summaryLastSync")).to_be_hidden()
    assert await page.locator("#summarySyncTime").get_attribute("datetime") is None
    await screenshot("empty")
    assert not errors, errors
    assert "/api/holdings" not in requested and "/api/portfolio/history" not in requested
    await page.close()
    print(f"PASS portfolio at {width}px: broker totals, Observatory migration, timestamps, missing totals and sold-out account")


async def main():
    async with async_playwright() as pw:
        browser = await pw.chromium.launch(headless=True)
        try:
            for width in (1440, 390):
                await exercise(browser, width)
        finally:
            await browser.close()


if __name__ == "__main__":
    asyncio.run(main())
