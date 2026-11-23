"""Shared browser steps for an interactive DEGIRO reauthentication."""

from __future__ import annotations

from collections.abc import Callable
from urllib.parse import urlsplit

from patchright.async_api import Page

from .login import _is_authenticated


LOGIN_URL = "https://trader.degiro.nl/login"
TRADER_URL = "https://trader.degiro.nl/trader/"


async def _totp_input(page: Page):
    selectors = (
        'input[autocomplete="one-time-code"]',
        'input[name*="otp" i]',
        'input[name*="code" i]',
        'input[inputmode="numeric"]',
        'input[type="tel"]',
    )
    for _ in range(120):
        for selector in selectors:
            locator = page.locator(selector)
            if await locator.count() and await locator.first.is_visible():
                return locator.first
        await page.wait_for_timeout(100)
    raise RuntimeError("DEGIRO did not present a verification-code input")


async def _submit_password(page: Page, username: str, password: str) -> None:
    username_field = page.locator('input[name="username"]')
    password_field = page.locator('input[type="password"]')
    await username_field.fill(username)
    await password_field.fill(password)

    submit_controls = page.locator('button[type="submit"], input[type="submit"]')
    for index in range(await submit_controls.count()):
        control = submit_controls.nth(index)
        if await control.is_visible():
            await control.click()
            return

    await password_field.press("Enter")


async def authenticate_page(
    page: Page,
    username: str,
    password: str,
    totp: str,
    *,
    set_stage: Callable[[str], None] | None = None,
) -> None:
    """Complete login and prove that the resulting session survives a reload."""

    def stage(value: str) -> None:
        if set_stage is not None:
            set_stage(value)

    stage("open_login")
    await page.goto(LOGIN_URL, wait_until="domcontentloaded", timeout=60_000)
    stage("submit_password")
    await _submit_password(page, username, password)

    stage("find_totp")
    otp_field = await _totp_input(page)
    stage("submit_totp")
    await otp_field.fill(totp() if callable(totp) else totp)
    async with page.expect_response(
        lambda response: (
            urlsplit(response.url).hostname == "trader.degiro.nl"
            and urlsplit(response.url).path.startswith("/login/secure/login")
            and response.request.method == "POST"
        ),
        timeout=30_000,
    ) as pending:
        await otp_field.press("Enter")
    response = await pending.value
    payload = await response.json()
    if response.status != 200 or not isinstance(payload, dict) or payload.get("status") != 0:
        raise RuntimeError("DEGIRO rejected the verification step")

    # The login SPA can stay on its login route after successful authentication.
    # Verify the new session with an explicit authenticated-shell navigation.
    stage("verify_reload")
    await page.goto(TRADER_URL, wait_until="domcontentloaded", timeout=60_000)
    await page.wait_for_timeout(2_000)
    if not await _is_authenticated(page):
        raise RuntimeError("DEGIRO session did not survive a page reload")


async def storage_snapshot(page: Page) -> dict[str, object]:
    return await page.evaluate(
        """
        () => ({
            origin: location.origin,
            localStorage: Object.entries(localStorage).map(([name, value]) => ({name, value})),
            sessionStorage: Object.entries(sessionStorage).map(([name, value]) => ({name, value})),
        })
        """
    )
