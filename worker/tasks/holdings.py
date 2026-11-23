"""Fetch and normalize the current DEGIRO portfolio."""

from __future__ import annotations

import logging
import asyncio
from datetime import datetime, timezone
from pathlib import Path
import re
from typing import Any
from urllib.parse import parse_qs, urlencode, urlsplit

from patchright.async_api import BrowserContext

from .common import (
    TaskError,
    collect_json_responses,
    first,
    get_page,
)
from .products import PRODUCT_INFO_PATH, resolve_products
from .markets import build_schedule, resolve_exchanges
from .valuation import apply_frontend, base_currency, currency, number

log = logging.getLogger("degitide-worker.holdings")

PORTFOLIO_URL = "https://trader.degiro.nl/trader/#/portfolio/assets"
RESPONSE_PATTERNS = (
    "/v5/update/",
    "/trading/secure/v5/account/info/",
    "/pa/secure/client",
    "/api/portfolio",
    "/api/holdings/",
    PRODUCT_INFO_PATH,
    "/login/secure/config",
    "/config/dictionary",
)


def _portfolio_update_url(captured: list[tuple[str, Any]]) -> str | None:
    # Prefer the actual update request. Always request a complete snapshot,
    # never the SPA's incremental revision or a cached partial response.
    for url, _ in reversed(captured):
        parts = urlsplit(url)
        if parts.hostname != "trader.degiro.nl":
            continue
        match = re.fullmatch(r"/trading/secure/v5/update/(\d+);jsessionid=([A-Za-z0-9._~-]{1,256})", parts.path)
        if match:
            query = parse_qs(parts.query)
            account, token = match.groups()
            if query.get("intAccount", [account]) != [account]:
                raise TaskError("DEGIRO returned inconsistent account identifiers")
            session = (query.get("sessionId") or [token])[0]
            if re.fullmatch(r"[A-Za-z0-9._~-]{1,256}", session):
                return f"https://trader.degiro.nl{parts.path}?" + urlencode({
                    "portfolio": "0", "totalPortfolio": "0",
                    "intAccount": account, "sessionId": session,
                })
    account = token = session = None
    for url, _ in captured:
        parts = urlsplit(url)
        if parts.hostname != "trader.degiro.nl":
            continue
        match = re.fullmatch(r"/trading/secure/v5/account/info/(\d+);jsessionid=([A-Za-z0-9._~-]{1,256})", parts.path)
        if match:
            account, token = match.groups()
        value = (parse_qs(parts.query).get("sessionId") or [None])[0]
        if value and re.fullmatch(r"[A-Za-z0-9._~-]{1,256}", value):
            session = value
    if not account or not token or not session:
        return None
    return f"https://trader.degiro.nl/trading/secure/v5/update/{account};jsessionid={token}?" + urlencode({
        "portfolio": "0", "totalPortfolio": "0", "intAccount": account, "sessionId": session,
    })


def _portfolio_rows(payload: Any) -> list[dict[str, Any]]:
    if not isinstance(payload, dict):
        raise TaskError("DEGIRO returned an unrecognized portfolio response")
    for key in ("portfolio", "holdings", "positions"):
        if key not in payload:
            continue
        value = payload[key]
        if isinstance(value, dict):
            if value.get("isAdded") is False:
                raise TaskError("DEGIRO returned an incremental portfolio instead of a full snapshot")
            value = value.get("value")
        if not isinstance(value, list) or any(not isinstance(row, dict) for row in value):
            raise TaskError("DEGIRO returned an incomplete portfolio")
        if any(row.get("isAdded") is False or row.get("isRemoved") is True for row in value):
            raise TaskError("DEGIRO returned incremental positions instead of a full snapshot")
        return value
    raise TaskError("DEGIRO returned an unrecognized portfolio response")


def _flatten_position(raw: dict[str, Any]) -> dict[str, Any]:
    flattened = dict(raw)
    values = raw.get("value")
    if isinstance(values, list):
        flattened.pop("value", None)
        # The outer name describes the transport row, not the security.
        flattened.pop("name", None)
        for field in values:
            if isinstance(field, dict) and "name" in field:
                flattened[str(field["name"])] = field.get("value")
    return flattened


def normalize_holdings(
    payload: Any,
    products: dict[str, dict[str, str]] | None = None,
    base: str = "",
) -> list[dict[str, Any]]:
    products = products or {}
    items = _portfolio_rows(payload)
    normalized: list[dict[str, Any]] = []
    seen: set[str] = set()
    for item in items:
        if not isinstance(item, dict):
            continue
        raw = _flatten_position(item)
        position_type = str(first(raw, "positionType", "type", default="PRODUCT"))
        quantity = number(first(raw, "size", "quantity"), "position quantity")
        if position_type.upper() == "CASH" or quantity == 0:
            continue
        if position_type.upper() not in ("PRODUCT", "SECURITY"):
            raise TaskError("DEGIRO returned an unsupported position type")
        product_id = first(raw, "productId", "id", default="")
        if not product_id or str(product_id) in seen:
            raise TaskError("DEGIRO returned a missing or duplicate position identifier")
        seen.add(str(product_id))
        product_details = products.get(str(product_id), {})
        product = first(
            raw,
            "symbol",
            "ticker",
            "productName",
            "name",
            "isin",
            default=product_details.get("label") or product_id or "Unknown",
        )
        normalized.append(
            {
                "product_id": str(product_id),
                "symbol": str(product),
                "quantity": quantity,
                "avg_price": number(first(raw, "breakEvenPrice", "averagePrice", "avgPrice", default=0), "average price"),
                "market_value": number(first(raw, "value", "marketValue", "positionValue"), "position value"),
                "currency": currency(
                    first(
                        raw,
                        "currency",
                        "currencyCode",
                        "baseCurrency",
                        default=product_details.get("currency", ""),
                    )
                ),
                "value_currency": base or currency(first(raw, "currency", "currencyCode", default=product_details.get("currency", ""))),
                "price": number(raw.get("price"), "position price", optional=True),
                "price_as_of": "",
                "valuation_source": "broker_api",
                # Fund name and ISIN identify the issuer for dashboard logos.
                "name": str(product_details.get("name") or first(raw, "productName", "name", default="") or ""),
                "isin": str(product_details.get("isin") or first(raw, "isin", default="") or ""),
            }
        )
    return normalized


def _position_product_ids(payload: Any) -> set[str]:
    ids: set[str] = set()
    for item in _portfolio_rows(payload):
        raw = _flatten_position(item)
        kind = str(first(raw, "positionType", "type", default="PRODUCT")).upper()
        quantity = number(first(raw, "size", "quantity"), "position quantity")
        if kind in ("PRODUCT", "SECURITY") and quantity != 0:
            value = first(raw, "productId", "id")
            if value is None or str(value) in ids:
                raise TaskError("DEGIRO returned a missing or duplicate position identifier")
            ids.add(str(value))
    return ids


async def fetch(context: BrowserContext) -> dict[str, object]:
    page = await get_page(context, new=True)
    try:
        await page.add_init_script(path=str(Path(__file__).with_name("portfolio_observer.js")))

        async def navigate() -> None:
            await page.goto(PORTFOLIO_URL, wait_until="domcontentloaded", timeout=60_000)

        captured = await collect_json_responses(page, RESPONSE_PATTERNS, navigate)
        if "/login" in page.url.lower() or "blocked" in page.url.lower():
            raise TaskError("DEGIRO session is not authenticated; complete login first")
        update_url = _portfolio_update_url(captured)
        if update_url is None:
            raise TaskError("DEGIRO did not expose a complete portfolio request")
        response = await context.request.get(update_url, headers={"Referer": PORTFOLIO_URL}, timeout=60_000)
        if not response.ok:
            raise TaskError(f"DEGIRO portfolio request failed (HTTP {response.status})")
        payload = await response.json()
        product_ids = _position_product_ids(payload)
        products = await resolve_products(context, captured + [(update_url, payload)], product_ids)
        observed = await page.evaluate("window.__degitidePortfolio || {}", isolated_context=False)
        base = base_currency(captured, observed)
        # Canonical nested update values are always in account currency. Do not
        # label them using the product currency if account metadata is absent.
        if not base and isinstance(payload.get("portfolio"), dict):
            raise TaskError("DEGIRO did not identify the account base currency")
        holdings = normalize_holdings(payload, products, base)
        result = None
        diagnostics: dict[str, str] = {}
        deadline = asyncio.get_running_loop().time() + 12
        while True:
            # The page may subscribe only to an initial lastData snapshot. The
            # worker's internal quotes can change without another position event.
            # Read a matched fresh pair; retain reconciliation for interleaving
            # updates between the worker's two resource reads.
            observed = await page.evaluate("window.__degitideReadPortfolio()", isolated_context=False)
            if observed.get("int_account") and observed["int_account"] != parse_qs(urlsplit(update_url).query)["intAccount"][0]:
                raise TaskError("DEGIRO calculated a different account's portfolio")
            candidate = apply_frontend(holdings, observed, base, diagnostics=diagnostics)
            if candidate is not None:
                result = candidate
                if candidate["valuation"]["price_status"] == "quoted" or not holdings:
                    break
            if asyncio.get_running_loop().time() >= deadline or not observed.get("int_account"):
                break
            await page.wait_for_timeout(500)
        if result is None:
            positions = observed.get("positions")
            positions = positions if isinstance(positions, dict) else {}
            rows = [row for row in positions.values() if isinstance(row, dict)]
            # Never log account/product IDs, amounts, request URLs or messages.
            log.warning(
                "portfolio observation unavailable reason=%s worker_seen=%s expected_positions=%d "
                "observed_positions=%d enriched_positions=%d timed_positions=%d",
                diagnostics.get("reason", "missing_observation"), bool(observed.get("int_account")),
                len(holdings), len(rows), sum(bool(row.get("productInfo")) for row in rows),
                sum(bool(row.get("lastUpdate")) for row in rows),
            )
            result = {"holdings": holdings, "valuation": {
                "base_currency": base or (holdings[0]["value_currency"] if holdings else ""),
                "source": "broker_api", "price_status": "unavailable",
                "warning": "Quote valuation unavailable. These are recorded broker values; price times and account P/L are unavailable.",
            }}
        else:
            log.info("portfolio observation adopted price_status=%s positions=%d",
                     result["valuation"]["price_status"], len(result["holdings"]))
        if result["valuation"]["base_currency"]:
            result["valuation"]["fetched_at"] = datetime.now(timezone.utc).isoformat()
        else:
            result["valuation"] = None
        exchanges = {}
        if any(products.get(h["product_id"], {}).get("exchange_id") for h in result["holdings"]):
            exchanges = await resolve_exchanges(context, captured + [(update_url, payload)])
        result["market_schedule"] = build_schedule(result["holdings"], products, exchanges)
        return result
    except TaskError:
        raise
    except Exception as exc:
        # Browser/request exceptions may contain session-bearing URLs.
        raise TaskError(f"could not fetch a complete DEGIRO portfolio ({type(exc).__name__})") from None
    finally:
        await page.close()
