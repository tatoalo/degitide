"""Fetch DEGIRO position reports for past dates.

Each report lists the securities held at the end of one day, valued at that
day's closing price. They rebuild the portfolio value history from before the
first sync. The CSV route is already on the policy proxy's read/report
allowlist, so this needs no new egress.
"""

from __future__ import annotations

import asyncio
import csv
import io
import re
from datetime import date
from typing import Any
from urllib.parse import parse_qsl, urlencode, urlsplit

from patchright.async_api import BrowserContext

from .common import TaskError, collect_json_responses, get_page
from .transactions import TRANSACTIONS_URL

POSITION_REPORT_URL = "https://trader.degiro.nl/portfolio-reports/secure/v3/positionReport/csv"
MAX_DATES = 160
MAX_REPORT_BYTES = 256 * 1024
# A short pause between reports keeps a long backfill gentle on DEGIRO.
REPORT_INTERVAL_SECONDS = 0.25

_IDENTIFIER = re.compile(r"[A-Z0-9][A-Z0-9.\-]{0,31}")
_CURRENCY = re.compile(r"[A-Z]{3}")
_BASE_CURRENCY = re.compile(r"Value in ([A-Z]{3})", re.IGNORECASE)


def parse_number(text: str) -> float:
    """Parse DEGIRO report numbers: "2500,75", "1.234,56", "1,234.56", "-3"."""
    value = str(text).strip().replace(" ", "").replace(" ", "")
    if not value:
        raise ValueError("empty number")
    if "," in value and "." in value:
        # The right-most separator is the decimal one.
        if value.rfind(",") > value.rfind("."):
            value = value.replace(".", "").replace(",", ".")
        else:
            value = value.replace(",", "")
    elif "," in value:
        value = value.replace(",", ".")
    number = float(value)
    if number != number or number in (float("inf"), float("-inf")):
        raise ValueError("non-finite number")
    return number


def parse_report(text: str) -> dict[str, Any]:
    """Return the securities in one position report. Cash rows are skipped."""
    try:
        rows = list(csv.reader(io.StringIO(text), strict=True))
    except csv.Error:
        raise TaskError("DEGIRO returned a malformed position report") from None
    if not rows or len(rows[0]) < 7 or rows[0][0].strip().lower() != "product":
        raise TaskError("DEGIRO returned an unrecognized position report")
    base = _BASE_CURRENCY.search(rows[0][6] or "")
    if not base:
        raise TaskError("DEGIRO did not identify the position report's base currency")
    positions: list[dict[str, Any]] = []
    for row in rows[1:]:
        if not any(cell.strip() for cell in row):
            continue
        if len(row) < 7:
            raise TaskError("DEGIRO returned a truncated position row")
        name, identifier, amount, closing, currency, local_value, base_value = (
            cell.strip() for cell in row[:7]
        )
        # Cash and cash-fund rows carry no ISIN or symbol.
        if not identifier:
            if amount or closing:
                raise TaskError("DEGIRO returned a security without an identifier")
            continue
        identifier = identifier.upper()
        currency = currency.upper()
        if not _IDENTIFIER.fullmatch(identifier) or not _CURRENCY.fullmatch(currency):
            raise TaskError("DEGIRO returned an unrecognized position row")
        try:
            quantity = parse_number(amount)
            if quantity == 0:
                continue
            positions.append(
                {
                    "identifier": identifier,
                    "name": name[:120],
                    "quantity": quantity,
                    "close": parse_number(closing),
                    "currency": currency,
                    "value": parse_number(local_value),
                    "base_value": parse_number(base_value),
                }
            )
        except ValueError as exc:
            raise TaskError("DEGIRO returned an unreadable position value") from exc
    return {"base_currency": base.group(1).upper() if base else "", "positions": positions}


def _requested_dates(values: list[str]) -> list[date]:
    if not isinstance(values, list) or not values:
        raise TaskError("at least one report date is required")
    if len(values) > MAX_DATES:
        raise TaskError(f"at most {MAX_DATES} report dates per request")
    today = date.today()
    days: list[date] = []
    for value in values:
        try:
            day = date.fromisoformat(str(value))
        except ValueError as exc:
            raise TaskError(f"invalid ISO date {value!r}") from exc
        if day > today:
            raise TaskError("report dates cannot be in the future")
        if day not in days:
            days.append(day)
    return days


def _report_url(session_query: dict[str, str], day: date) -> str:
    stamp = day.strftime("%d/%m/%Y")
    return POSITION_REPORT_URL + "?" + urlencode({**session_query, "fromDate": stamp, "toDate": stamp})


async def _report_session(context: BrowserContext) -> dict[str, str]:
    """Borrow intAccount/sessionId from the SPA's own transaction-report request."""
    page = await get_page(context)

    async def navigate() -> None:
        await page.goto(TRANSACTIONS_URL, wait_until="domcontentloaded", timeout=60_000)

    try:
        captured = await collect_json_responses(page, ("/v4/transactions",), navigate)
    except Exception as exc:
        raise TaskError(f"could not open DEGIRO transactions: {exc}") from exc
    if "/login" in page.url.lower() or "blocked" in page.url.lower():
        raise TaskError("DEGIRO session is not authenticated; complete login first")
    for url, _ in captured:
        query = dict(parse_qsl(urlsplit(url).query))
        if query.get("intAccount") and query.get("sessionId"):
            return {"intAccount": query["intAccount"], "sessionId": query["sessionId"]}
    raise TaskError("DEGIRO did not expose a report session")


async def fetch(context: BrowserContext, dates: list[str]) -> dict[str, object]:
    days = _requested_dates(dates)
    session_query = await _report_session(context)
    snapshots: list[dict[str, Any]] = []
    unavailable: list[str] = []
    for index, day in enumerate(days):
        if index:
            await asyncio.sleep(REPORT_INTERVAL_SECONDS)
        response = await context.request.get(
            _report_url(session_query, day),
            headers={"Referer": TRANSACTIONS_URL},
            timeout=60_000,
        )
        if response.status in (401, 403):
            raise TaskError("DEGIRO or the policy proxy refused the position report")
        if not response.ok:
            # DEGIRO answers 500 for dates before the account existed.
            unavailable.append(day.isoformat())
            continue
        body = await response.body()
        if len(body) > MAX_REPORT_BYTES:
            raise TaskError("DEGIRO returned an oversized position report")
        report = parse_report(body.decode("utf-8-sig", errors="replace"))
        snapshots.append({"date": day.isoformat(), **report})
    return {"snapshots": snapshots, "unavailable": unavailable}
