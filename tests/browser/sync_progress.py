"""Exercise real EventSource updates, fallback polling, and sync click races."""
import asyncio
from copy import deepcopy
from datetime import datetime, timezone
import json

from aiohttp import web
from patchright.async_api import async_playwright, expect
from portfolio_valuation import SNAPSHOT, STATIC


def now():
    return datetime.now(timezone.utc).isoformat()


async def exercise(browser, width):
    page = await browser.new_page(viewport={"width": width, "height": 900}, locale="en-IE")
    errors, streams, writes = [], set(), []
    page.on("pageerror", lambda error: errors.append(str(error)))
    snapshot = deepcopy(SNAPSHOT)
    accounts = [{"id": "default", "name": "Main", "connected": True},
                {"id": "savings", "name": "Savings", "connected": True}]
    sync_status = {"pull_running": False, "progress": {"running": False}}
    stream_available, trigger_mode = True, "race"
    status_gate = gate = None
    status_entered = asyncio.Event()

    async def wait_for_stream(connected=True):
        async with asyncio.timeout(10):
            while await page.evaluate("state.syncStreamConnected", isolated_context=False) != connected:
                await asyncio.sleep(0.05)

    def update(percent, *, started=None, failed=False, publish=True):
        nonlocal sync_status
        running = percent < 100
        stage = "Fetching transactions" if percent < 50 else "Fetching holdings" if percent < 75 else "Updating value history"
        if not running:
            stage = "Sync finished with errors" if failed else "Sync complete"
        sync_status = {"pull_running": running, "progress": {
            "running": running, "percent": percent, "stage": stage,
            "started_at": started or sync_status["progress"].get("started_at") or now(),
            "finished_at": "0001-01-01T00:00:00Z" if running else now(),
            "accounts": [{"id": "savings", "name": "Savings", "stage": stage,
                          "state": "running" if running else "failed" if failed else "complete"}],
        }}
        if publish:
            for queue in streams:
                queue.put_nowait(deepcopy(sync_status))

    async def handle(request):
        nonlocal status_gate
        path = request.path
        if path == "/api/sync/events":
            if not stream_available:
                return web.Response(status=503)
            response = web.StreamResponse(headers={"Content-Type": "text/event-stream", "Cache-Control": "no-store"})
            await response.prepare(request)
            queue = asyncio.Queue()
            streams.add(queue)
            queue.put_nowait(deepcopy(sync_status))
            try:
                while (status := await queue.get()) is not None:
                    await response.write(("data: " + json.dumps(status) + "\n\n").encode())
            except (ConnectionResetError, asyncio.CancelledError):
                pass
            finally:
                streams.discard(queue)
            return response
        if path == "/api/status":
            account = next(a for a in accounts if a["id"] == request.query.get("account", "default"))
            body = {**deepcopy(sync_status), "account": deepcopy(account)}
            if status_gate:
                gate, status_gate = status_gate, None
                status_entered.set()
                await gate.wait()
            return web.json_response(body)
        if path == "/api/pull/trigger":
            writes.append(request.query.get("account"))
            if trigger_mode == "blocked":
                return web.json_response({"error": "wait for DEGIRO sign-in to finish before pulling"}, status=409)
            update(25, started=now(), publish=False)
            if trigger_mode == "race":
                return web.json_response({"error": "a pull is already in progress", "code": "sync_in_progress"}, status=409)
            return web.json_response({"status": "pull started"}, status=202)
        if path == "/api/accounts":
            return web.json_response(accounts)
        if path == "/api/portfolio/snapshot":
            return web.json_response(snapshot)
        if path == "/api/versions":
            return web.json_response({"app": "test", "worker": {"version": "test"}})
        if path.startswith("/api/"):
            return web.json_response([])
        target = STATIC / (path.lstrip("/") or "index.html")
        return web.FileResponse(target) if target.is_file() else web.Response(status=404)

    app = web.Application()
    app.router.add_route("*", "/{path:.*}", handle)
    runner = web.AppRunner(app, access_log=None)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    origin = f"http://127.0.0.1:{site._server.sockets[0].getsockname()[1]}"
    try:
        await page.goto(origin)
        await expect(page.locator("#accountName")).to_have_text("Main")
        await expect(page.locator("#syncProgress")).to_be_hidden()
        await wait_for_stream()

        # A scheduled/CLI/other-tab run starts while this page is already idle.
        update(25)
        await expect(page.locator("#progressValue")).to_have_text("25%")
        await expect(page.locator("#syncProgress")).to_be_visible()
        await expect(page.locator("#progressAccounts")).to_contain_text("Savings")
        await expect(page.locator("#pullButton")).to_have_text("↻ Syncing…")
        await expect(page.locator("#pullButton")).to_be_disabled()
        await expect(page.locator("#syncAllButton")).to_be_disabled()
        assert not writes, "observing an automatic run must not trigger another sync"

        # Account navigation and page reload both retain the global running job.
        await page.locator("#accountSelect").select_option("savings")
        await expect(page.locator("#accountName")).to_have_text("Savings")
        await expect(page.locator("#progressValue")).to_have_text("25%")
        for queue in list(streams):
            queue.put_nowait(None)
        await page.reload()
        await expect(page.locator("#syncProgress")).to_be_visible()
        await expect(page.locator("#pullButton")).to_be_disabled()
        await wait_for_stream()

        # A delayed HTTP response must not roll back a newer streamed stage.
        status_gate = gate = asyncio.Event()
        await page.locator("#refreshButton").click()
        await asyncio.wait_for(status_entered.wait(), timeout=5)
        update(50)
        await expect(page.locator("#progressValue")).to_have_text("50%")
        gate.set()
        await expect(page.locator("#refreshButton")).to_be_enabled()
        await expect(page.locator("#progressValue")).to_have_text("50%")
        update(75)
        await expect(page.locator("#progressLabel")).to_have_text("Updating value history")
        assert await page.evaluate("document.documentElement.scrollWidth <= innerWidth")

        # Completion refreshes portfolio data and releases the controls.
        snapshot["valuation"]["portfolio_value"] = 1051.00
        accounts[0]["last_pull"] = now()
        update(100)
        await expect(page.locator("#brokerPortfolio")).to_have_text("€1,051.00")
        await expect(page.locator("#progressLabel")).to_have_text("Sync complete")
        await expect(page.locator("#pullButton")).to_be_enabled()
        await expect(page.locator("#syncAllButton")).to_be_enabled()
        await expect(page.locator("#syncProgress")).to_be_hidden(timeout=7000)

        # Even a whole run between observations refreshes data on its final event.
        snapshot["valuation"]["portfolio_value"] = 1052.00
        update(100, started=now())
        await expect(page.locator("#brokerPortfolio")).to_have_text("€1,052.00")

        # Reproduce the reported click race with no stream event to help the UI.
        await page.locator("#pullButton").click()
        await expect(page.locator("#progressValue")).to_have_text("25%")
        await expect(page.locator("#pullButton")).to_be_disabled()
        await expect(page.locator("#notice")).to_be_hidden()
        update(100, failed=True)
        await expect(page.locator("#syncProgress")).to_have_class("sync-progress failed")
        await expect(page.locator("#pullButton")).to_be_enabled()

        # Other conflicts remain real errors, and the normal manual path works.
        trigger_mode = "blocked"
        await page.locator("#pullButton").click()
        await expect(page.locator("#notice")).to_contain_text("wait for DEGIRO sign-in")
        trigger_mode = "manual"
        await page.locator("#syncAllButton").click()
        await expect(page.locator("#progressValue")).to_have_text("25%")
        await expect(page.locator("#notice")).to_be_hidden()
        assert writes == ["default", "default", None], writes
        update(100)
        await expect(page.locator("#pullButton")).to_be_enabled()

        # Dropped streams fall back to fast polling even when previously idle.
        stream_available = False
        for queue in list(streams):
            queue.put_nowait(None)
        await wait_for_stream(False)
        update(25, started=now(), publish=False)
        await expect(page.locator("#progressValue")).to_have_text("25%", timeout=5000)
        await expect(page.locator("#pullButton")).to_be_disabled()
        stream_available = True
        await wait_for_stream()
        update(75)
        await expect(page.locator("#progressValue")).to_have_text("75%")
        update(100)
        await expect(page.locator("#pullButton")).to_be_enabled()
        assert not errors, errors
    finally:
        if gate:
            gate.set()
        await page.close()
        for queue in list(streams):
            queue.put_nowait(None)
        await runner.cleanup()


async def main():
    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=True)
        for width in (1440, 390):
            await exercise(browser, width)
        await browser.close()
    print("Live sync progress, completion, click races and reconnects passed at desktop/mobile widths")


if __name__ == "__main__":
    asyncio.run(main())
