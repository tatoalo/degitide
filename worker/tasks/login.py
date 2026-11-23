"""Persistent-browser DEGIRO session checks."""

from __future__ import annotations

import logging
from urllib.parse import urlparse, urlsplit

from patchright.async_api import BrowserContext, Page, Response

from .common import TaskError, get_page, request_schema

TRADER_URL = "https://trader.degiro.nl/trader/"
# Set by the egress proxy on its own denials (proxy/policy.py). Those are
# already logged by the proxy and only crowd out broker failures here.
PROXY_DENIAL_HEADER = "x-degitide-egress"
log = logging.getLogger("degitide-worker.login")


def _looks_authenticated(url: str) -> bool:
    parsed = urlparse(url)
    path = parsed.path.lower()
    return (
        parsed.hostname is not None
        and parsed.hostname.endswith("degiro.nl")
        and "/trader" in path
        and "/login" not in path
        and "blocked" not in path
    )


async def _is_authenticated(page: Page) -> bool:
    if not _looks_authenticated(page.url):
        return False
    password_fields = page.locator('input[type="password"]')
    return await password_fields.count() == 0


def _log_unsuccessful_response(response: Response) -> None:
    """Record which trader request failed during a check, without any values.

    Only the method, the masked path, the status and query key names are
    logged: never query values, headers, cookies or bodies. A 304 is a cache
    revalidation, not a failure, and would only add noise. A 403 marked by the
    egress proxy is a policy denial the proxy already logs, not a broker answer.
    """
    try:
        status = response.status
        if 200 <= status < 300 or status == 304:
            return
        if status == 403 and (getattr(response, "headers", None) or {}).get(PROXY_DENIAL_HEADER) == "denied":
            return
        if urlsplit(response.url).hostname != "trader.degiro.nl":
            return
        method, path, query_keys = request_schema(response.request.method, response.url)
    except Exception:  # Diagnostics must never affect the session result.
        return
    log.info(
        "session check response method=%s path=%s status=%s query_keys=%s",
        method,
        path,
        status,
        ",".join(query_keys),
    )


async def check_session(context: BrowserContext) -> dict[str, bool]:
    """Navigate to the authenticated shell and observe whether login is required."""
    page = await get_page(context, new=True)
    page.on("response", _log_unsuccessful_response)
    try:
        await page.goto(TRADER_URL, wait_until="domcontentloaded", timeout=60_000)
        await page.wait_for_timeout(1_000)
        valid = await _is_authenticated(page)
        log.info(
            "session check ended at path %s (authenticated=%s)",
            request_schema("GET", page.url)[1],
            valid,
        )
        return {"valid": valid}
    except Exception as exc:
        raise TaskError(f"could not check DEGIRO session: {exc}") from exc
    finally:
        await page.close()
