"""Fetch complete, validated DEGIRO transaction reports."""

from __future__ import annotations

import hashlib
import json
import math
import re
from datetime import date, datetime
from typing import Any
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit

from patchright.async_api import BrowserContext

from .common import TaskError, collect_json_responses, compact_json, first, get_page
from .products import PRODUCT_INFO_PATH, resolve_products

TRANSACTIONS_URL = "https://trader.degiro.nl/trader/#/transactions"
TRANSACTION_REPORT_PATH = "/portfolio-reports/secure/v4/transactions"
RESPONSE_PATTERNS = (TRANSACTION_REPORT_PATH, PRODUCT_INFO_PATH)
_CURRENCY = re.compile(r"[A-Z]{3}")


def _parse_date(value: str) -> date:
    try:
        return date.fromisoformat(value)
    except (TypeError, ValueError) as exc:
        raise TaskError("invalid transaction report date") from exc


def _date_only(value: Any) -> str:
    if not isinstance(value, str) or not value.strip():
        raise TaskError("DEGIRO returned a transaction without a date")
    text = value.strip()
    try:
        if len(text) == 10:
            return date.fromisoformat(text).isoformat()
        return datetime.fromisoformat(text.replace("Z", "+00:00")).date().isoformat()
    except ValueError:
        for fmt in ("%d/%m/%Y", "%d-%m-%Y", "%Y/%m/%d"):
            try:
                return datetime.strptime(text, fmt).date().isoformat()
            except ValueError:
                pass
    raise TaskError("DEGIRO returned an invalid transaction date")


def _identifier(value: Any, field: str) -> str:
    if isinstance(value, bool) or not isinstance(value, (str, int)) or not str(value).strip():
        raise TaskError(f"DEGIRO returned an invalid transaction {field}")
    return str(value).strip()


def _number(value: Any, field: str) -> float:
    if isinstance(value, bool) or value is None:
        raise TaskError(f"DEGIRO returned an invalid transaction {field}")
    try:
        number = float(value)
    except (TypeError, ValueError, OverflowError) as exc:
        raise TaskError(f"DEGIRO returned an invalid transaction {field}") from exc
    if not math.isfinite(number):
        raise TaskError(f"DEGIRO returned a non-finite transaction {field}")
    return number


def _transaction_id(item: dict[str, Any]) -> str:
    explicit = first(item, "id", "transactionId")
    if explicit is not None:
        return _identifier(explicit, "ID")
    # One order can have several executions. An orderId is not a transaction
    # ID; use the whole execution record, with stable JSON object ordering.
    encoded = json.dumps(item, sort_keys=True, ensure_ascii=False, separators=(",", ":"), allow_nan=False)
    digest = hashlib.sha256(encoded.encode("utf-8")).hexdigest()
    return f"derived-{digest[:24]}"


def _transaction_items(payload: Any, depth: int = 0) -> list[Any]:
    """Recognize a report, never interpret a missing/wrong envelope as empty."""
    if isinstance(payload, list):
        return payload
    if not isinstance(payload, dict) or depth > 2:
        raise TaskError("DEGIRO returned an unrecognized transaction report")
    # The v4 report is unpaginated. Fail closed if it starts advertising
    # pagination; otherwise a successful pull could silently lose executions.
    if any(payload.get(key) for key in ("hasMore", "has_more", "nextPage", "nextCursor")):
        raise TaskError("DEGIRO returned an incomplete transaction report")
    items = None
    for key in ("transactions", "items", "results", "data"):
        if key not in payload:
            continue
        value = payload[key]
        if isinstance(value, dict) and key != "data" and isinstance(value.get("value"), list):
            value = value["value"]
        items = _transaction_items(value, depth + 1)
        break
    if items is None:
        raise TaskError("DEGIRO returned an unrecognized transaction report")
    for key in ("totalCount", "totalElements"):
        if key in payload and payload[key] != len(items):
            raise TaskError("DEGIRO returned an incomplete transaction report")
    return items


def normalize_transactions(
    payload: Any,
    products: dict[str, dict[str, str]] | None = None,
) -> list[dict[str, Any]]:
    products = products or {}
    items = _transaction_items(payload)
    normalized: list[dict[str, Any]] = []
    seen: set[str] = set()
    for raw in items:
        if not isinstance(raw, dict):
            raise TaskError("DEGIRO returned an invalid transaction row")
        side = str(first(raw, "buysell", "buySell", "type", default="")).upper()
        side = {"B": "BUY", "S": "SELL"}.get(side, side)
        if side not in ("BUY", "SELL"):
            raise TaskError("DEGIRO returned an unrecognized transaction side")
        product_id = _identifier(raw.get("productId"), "product ID")
        product_details = products.get(product_id, {})
        product = first(
            raw, "symbol", "ticker", "productName", "product", "isin",
            default=product_details.get("label") or product_id,
        )
        quantity = _number(first(raw, "quantity", "size"), "quantity")
        if quantity == 0:
            raise TaskError("DEGIRO returned a zero-quantity transaction")
        price = _number(first(raw, "price", "unitPrice"), "price")
        day = _date_only(first(raw, "date", "created", "timestamp"))
        # An omitted fee stays zero for compatibility; malformed supplied fees
        # must not silently become zero. The raw record retains fee currency.
        fee = _number(first(raw, "fee", "totalFeesInBaseCurrency", "feeInBaseCurrency", default=0), "fee")
        currency = str(first(raw, "currency", "currencyCode", "baseCurrency", default=product_details.get("currency", ""))).strip().upper()
        if currency and not _CURRENCY.fullmatch(currency):
            raise TaskError("DEGIRO returned an invalid transaction currency")
        try:
            identity = _transaction_id(raw)
        except (TypeError, ValueError) as exc:
            raise TaskError("DEGIRO returned an invalid transaction record") from exc
        if identity in seen:
            # Even identical rows without execution IDs are ambiguous: dropping
            # one could erase a distinct fill. Do not publish a partial report.
            raise TaskError("DEGIRO returned duplicate transaction identities")
        seen.add(identity)
        normalized.append(
            {
                "id": identity,
                "type": side,
                "date": day,
                "product_id": product_id,
                "symbol": str(product),
                "quantity": quantity,
                "price": price,
                "currency": currency,
                "fee": fee,
                "counterparty": str(first(raw, "counterparty", "counterParty", "tradingVenue", default="")),
                "raw_json": compact_json(raw),
            }
        )
    return normalized


def _with_date_range(url: str, from_date: date, to_date: date) -> str:
    parts = urlsplit(url)
    query = dict(parse_qsl(parts.query, keep_blank_values=True))
    query["fromDate"] = from_date.strftime("%d/%m/%Y")
    query["toDate"] = to_date.strftime("%d/%m/%Y")
    query["groupTransactionsByOrder"] = "false"
    return urlunsplit((parts.scheme, parts.netloc, parts.path, urlencode(query), parts.fragment))


def _filter_date_range(
    transactions: list[dict[str, Any]], start: date, end: date
) -> list[dict[str, Any]]:
    filtered: list[dict[str, Any]] = []
    for transaction in transactions:
        transaction_date = _parse_date(transaction.get("date"))
        if start <= transaction_date <= end:
            filtered.append(transaction)
    return filtered


async def fetch(context: BrowserContext, from_date: str, to_date: str) -> dict[str, object]:
    start = _parse_date(from_date)
    end = _parse_date(to_date)
    if start > end:
        raise TaskError("transaction start date must be on or before end date")

    # A fresh page guarantees bootstrap requests even when the previous task
    # was already showing the same SPA route.
    page = await get_page(context, new=True)

    async def navigate() -> None:
        await page.goto(TRANSACTIONS_URL, wait_until="domcontentloaded", timeout=60_000)

    try:
        captured = await collect_json_responses(page, RESPONSE_PATTERNS, navigate)
        page_url = page.url
    except Exception as exc:
        raise TaskError("could not open DEGIRO transactions") from exc
    finally:
        await page.close()

    if "/login" in page_url.lower() or "blocked" in page_url.lower():
        raise TaskError("DEGIRO session is not authenticated; complete login first")

    # Only a successful explicit v4 request for the requested window is a
    # complete report. Captured page responses often cover a shorter period.
    report_url = next(
        (
            url for url, _ in reversed(captured)
            if urlsplit(url).scheme == "https"
            and urlsplit(url).hostname == "trader.degiro.nl"
            and urlsplit(url).path == TRANSACTION_REPORT_PATH
        ),
        None,
    )
    if report_url is None:
        raise TaskError("DEGIRO did not expose a transaction report session")
    try:
        response = await context.request.get(
            _with_date_range(report_url, start, end),
            headers={"Referer": TRANSACTIONS_URL},
            timeout=60_000,
        )
        if not response.ok:
            raise TaskError(f"DEGIRO transaction report request failed ({response.status})")
        payload = await response.json()
    except TaskError:
        raise
    except Exception as exc:
        # No raw request URL: it contains session credentials.
        raise TaskError("could not fetch the complete DEGIRO transaction report") from exc

    items = _transaction_items(payload)
    product_ids = {
        str(item["productId"])
        for item in items
        if isinstance(item, dict) and item.get("productId") is not None
    }
    products = await resolve_products(context, captured, product_ids)
    transactions = normalize_transactions(payload, products)
    if any(not transaction["currency"] for transaction in transactions):
        raise TaskError("DEGIRO transaction currency could not be resolved")
    filtered = _filter_date_range(transactions, start, end)
    if len(filtered) != len(transactions):
        raise TaskError("DEGIRO returned transactions outside the requested report dates")
    return {"transactions": transactions}
