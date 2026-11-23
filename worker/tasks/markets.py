"""Resolve listing venues, then cache regular trading sessions for the scheduler.

Never infer a venue from a ticker, currency or an ETF's underlying investments.
The broker's dictionary maps product exchangeId to ISO MIC. Unsupported venues
and non-equity products stay unknown and use the scheduler's daily fallback.
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from functools import lru_cache
import logging
import re
from urllib.parse import parse_qs, urlencode, urlsplit

import exchange_calendars as xcals
import pandas as pd

from .products import _product_info_url

log = logging.getLogger("degitide-worker.markets")
# Keep in sync with the egress proxy's exchange-dictionary route.
DICTIONARY_PATHS = {"/product_search/config/dictionary", "/productsearch/config/dictionary",
                    "/product_search/config/dictionary/", "/productsearch/config/dictionary/",
                    "/productsearch/secure/v1/config/dictionary"}
MIC = re.compile(r"[A-Z0-9]{4}")


def exchange_map(payload):
    if not isinstance(payload, dict):
        return {}
    data = payload.get("data", payload)
    if not isinstance(data, dict) or not isinstance(data.get("exchanges"), list):
        return {}
    return {
        str(row["id"]): {"mic": row["micCode"], "name": str(row.get("name") or row["micCode"])[:80]}
        for row in data["exchanges"]
        if isinstance(row, dict) and row.get("id") is not None
        and isinstance(row.get("micCode"), str) and MIC.fullmatch(row["micCode"])
    }


async def resolve_exchanges(context, captured):
    for url, payload in captured:
        if urlsplit(url).path in DICTIONARY_PATHS:
            mapped = exchange_map(payload)
            if mapped:
                return mapped
    info = _product_info_url(captured)
    if not info:
        return {}
    # Use only the authenticated broker origin and an exact read-only route.
    parts = urlsplit(info)
    if parts.scheme != "https" or parts.netloc != "trader.degiro.nl":
        return {}
    query = parse_qs(parts.query)
    params = {key: query[key][0] for key in ("intAccount", "sessionId") if len(query.get(key, [])) == 1}
    if len(params) != 2:
        return {}
    try:
        configs = [p.get("data", p) for u, p in captured if "/login/secure/config" in u and isinstance(p, dict)]
        if not configs:
            response = await context.request.get("https://trader.degiro.nl/login/secure/config?" + urlencode(params), headers={"Referer": "https://trader.degiro.nl/trader/"}, timeout=15_000)
            if response.ok:
                payload = await response.json()
                configs = [payload.get("data", payload)] if isinstance(payload, dict) else []
        for config in configs:
            if not isinstance(config, dict):
                continue
            target = urlsplit(str(config.get("dictionaryUrl") or ""))
            if (target.scheme != "https" or target.netloc != "trader.degiro.nl"
                    or target.path not in DICTIONARY_PATHS or target.query or target.fragment):
                continue
            response = await context.request.get("https://trader.degiro.nl" + target.path + "?" + urlencode(params), headers={"Referer": "https://trader.degiro.nl/trader/"}, timeout=15_000)
            if response.ok:
                return exchange_map(await response.json())
    except Exception as exc:
        # Request exception messages can contain session-bearing URLs.
        log.warning("exchange metadata unavailable (%s)", type(exc).__name__)
    return {}


@lru_cache(maxsize=128)
def _sessions(mic, day):
    start = day - timedelta(days=14)
    end = day + timedelta(days=30)
    calendar = xcals.get_calendar(mic, start=start, end=end)
    sessions = []
    for _, row in calendar.schedule.loc[str(start):str(end)].iterrows():
        sessions.append({
            key: value.isoformat() for key, value in row.items()
            if key in ("open", "close", "break_start", "break_end") and not pd.isna(value)
        })
    return str(calendar.tz), sessions


def build_schedule(holdings, products, exchanges, now=None):
    now = now or datetime.now(timezone.utc)
    markets, unknown = {}, 0
    for holding in holdings:
        product = products.get(holding["product_id"], {})
        exchange = exchanges.get(product.get("exchange_id"), {})
        mic = exchange.get("mic") or product.get("exchange_mic", "")
        # Equity calendars do not describe derivatives sessions. A missing type
        # is also unknown, rather than accidentally scheduling an option as stock.
        supported_type = product.get("product_type") in ("1", "131")  # stocks, ETFs
        holding["exchange_mic"] = mic if MIC.fullmatch(mic) else ""
        holding["exchange_name"] = exchange.get("name", holding["exchange_mic"])
        try:
            if not supported_type or not MIC.fullmatch(mic):
                raise ValueError("unsupported listing")
            zone, sessions = _sessions(mic, now.date())
            if not sessions:
                raise ValueError("no calendar coverage")
            markets[mic] = {"mic": mic, "name": holding["exchange_name"], "timezone": zone, "sessions": sessions}
        except (ValueError, KeyError, xcals.errors.InvalidCalendarName, xcals.errors.DateOutOfBounds):
            unknown += 1
        except Exception as exc:
            # Optional calendar metadata must never discard a valid portfolio.
            log.warning("trading calendar unavailable (%s)", type(exc).__name__)
            unknown += 1
    return {
        "generated_at": now.isoformat(),
        "valid_until": (now + timedelta(days=28)).isoformat(),
        "holding_count": len(holdings), "unknown_holdings": unknown,
        "markets": [markets[key] for key in sorted(markets)],
    }
