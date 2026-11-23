"""Render automatically generated release history at desktop and mobile widths."""
import asyncio
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
from urllib.parse import urlparse

from patchright.async_api import async_playwright, expect
from portfolio_valuation import SNAPSHOT, STATIC

spec = importlib.util.spec_from_file_location("build_changelog", STATIC.parents[2] / "scripts/build_changelog.py")
generator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(generator)


async def exercise(browser, width):
    page = await browser.new_page(viewport={"width": width, "height": 900})
    errors, fail = [], False
    page.on("pageerror", lambda error: errors.append(str(error)))
    with tempfile.TemporaryDirectory() as directory:
        def git(*args):
            subprocess.run(["git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
                            "-c", "commit.gpgsign=false", *args], cwd=directory, check=True, capture_output=True)

        git("init", "-b", "main")
        literal_title = 'Display <img src=x onerror="alert(1)"> as plain text (#1)'
        git("commit", "--allow-empty", "-m", literal_title)
        git("tag", "v9.0.0")
        git("commit", "--allow-empty", "-m", "Generate changelogs for every release (#2)")
        data = generator.build_changelog("v9.0.1", directory)

        async def route_request(route):
            url = urlparse(route.request.url)
            assert url.hostname == "127.0.0.1", "changelog must not need runtime GitHub access"
            if url.path == "/changelog.json":
                return await route.fulfill(status=503 if fail else 200, content_type="application/json", body=json.dumps(data))
            if url.path == "/api/versions":
                body = {"app": data["version"]}
            elif url.path == "/api/accounts":
                body = [{"id": "default", "name": "Demo"}]
            elif url.path == "/api/status":
                body = {"account": {"id": "default", "name": "Demo"}}
            elif url.path == "/api/portfolio/snapshot":
                body = SNAPSHOT
            elif url.path.startswith("/api/"):
                body = []
            else:
                target = STATIC / (url.path.lstrip("/") or "index.html")
                return await route.fulfill(path=str(target)) if target.is_file() else await route.fulfill(status=404)
            await route.fulfill(content_type="application/json", body=json.dumps(body))

        async def open_notes():
            await page.locator("#changelogButton").click()
            await expect(page.locator("#changelogDialog")).to_be_visible()
            assert await page.locator("#changelogDialog").evaluate("el => el.scrollWidth <= el.clientWidth")

        async def close_notes():
            await page.get_by_role("button", name="Close release notes").click()
            await expect(page.locator("#changelogDialog")).to_be_hidden()

        await page.route("**/*", route_request)
        await page.goto("http://127.0.0.1:8391/")
        await open_notes()
        await expect(page.locator("#changelogVersion")).to_have_text("Installed · v9.0.1")
        await expect(page.locator("#changelogEntries h3")).to_have_text(["v9.0.1", "v9.0.0"])
        await expect(page.locator("#changelogEntries li").last).to_have_text(literal_title)
        await expect(page.locator("#changelogEntries img")).to_have_count(0)
        await expect(page.locator("#changelogStatus")).to_be_hidden()
        await close_notes()

        # Another release appears just by regenerating its build artifact.
        git("tag", "v9.0.1")
        git("commit", "--allow-empty", "-m", "Keep sync progress visible after reconnect (#3)")
        data = generator.build_changelog("v9.0.2", directory)
        await page.reload()
        await open_notes()
        await expect(page.locator("#changelogVersion")).to_have_text("Installed · v9.0.2")
        await expect(page.locator("#changelogEntries h3")).to_have_text(["v9.0.2", "v9.0.1", "v9.0.0"])
        await expect(page.locator("#changelogEntries li").first).to_have_text("Keep sync progress visible after reconnect (#3)")
        await close_notes()

        fail = True
        await open_notes()
        await expect(page.locator("#changelogStatus")).to_contain_text("Couldn’t load release notes")
        await expect(page.locator("#changelogEntries article")).to_have_count(0)
        await close_notes()
        fail = False
        await open_notes()
        await expect(page.locator("#changelogEntries article")).to_have_count(3)
        await close_notes()
        data = {"version": "dev", "releases": []}
        await open_notes()
        await expect(page.locator("#changelogStatus")).to_have_text("No release notes available for this build.")
        assert not errors, errors
    await page.close()


async def main():
    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=True)
        for width in (1440, 390):
            await exercise(browser, width)
        await browser.close()
    print("Generated changelog passed: consecutive releases, safe text, retry, empty builds and mobile layout")


if __name__ == "__main__":
    asyncio.run(main())
