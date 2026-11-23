"""Validate calculated trader figures without recreating broker accounting.

The trader's raw portfolio value is in account currency. The frontend replaces
it using quotes, contract sizes and FX, and includes closed positions, cash-fund
compensation and non-product charges in account P/L. Preserve those figures;
never reconstruct account P/L from just the open positions.
"""
from __future__ import annotations

from datetime import datetime, timezone
from decimal import Decimal, InvalidOperation
import re
from typing import Any

from .common import TaskError


def number(value: Any, field: str, *, optional: bool = False) -> float | None:
    if value is None and optional:
        return None
    if isinstance(value, bool) or value is None or value == "":
        raise TaskError(f"DEGIRO returned a missing or invalid {field}")
    try:
        parsed = Decimal(str(value))
        result = float(parsed)
        if not parsed.is_finite() or not Decimal(str(result)).is_finite():
            raise ValueError()
        return result
    except (InvalidOperation, ValueError, OverflowError):
        raise TaskError(f"DEGIRO returned an invalid {field}") from None


def currency(value: Any) -> str:
    if not isinstance(value, str) or not re.fullmatch(r"[A-Z]{3}", value):
        raise TaskError("DEGIRO did not identify the valuation currency")
    return value


def timestamp(value: Any) -> str:
    if value in (None, ""):
        return ""
    try:
        text = str(value)
        # A closing date is a date, not a precise quote instant. Keep it a date.
        if re.fullmatch(r"\d{4}-\d{2}-\d{2}", text):
            datetime.strptime(text, "%Y-%m-%d")
            return text
        parsed = datetime.fromisoformat(text.replace("Z", "+00:00"))
        if parsed.tzinfo is None:
            raise ValueError()
        return parsed.astimezone(timezone.utc).isoformat()
    except (TypeError, ValueError, OverflowError):
        raise TaskError("DEGIRO returned an invalid price timestamp") from None


def base_currency(captured: list[tuple[str, Any]], observed: dict[str, Any]) -> str:
    found = []
    value = observed.get("base_currency")
    if value:
        found.append(currency(value))
    for url, payload in captured:
        if "/account/info/" not in url or not isinstance(payload, dict):
            continue
        match = re.search(r"/account/info/(\d+)", url)
        if observed.get("int_account") and match and match.group(1) != observed["int_account"]:
            continue
        data = payload.get("data", payload)
        if isinstance(data, dict) and data.get("baseCurrency"):
            found.append(currency(data["baseCurrency"]))
    if len(set(found)) > 1:
        raise TaskError("DEGIRO returned inconsistent account base currencies")
    return found[0] if found else ""


def apply_frontend(
    holdings: list[dict[str, Any]], observed: dict[str, Any], base: str,
    *, diagnostics: dict[str, str] | None = None,
) -> dict[str, Any] | None:
    """Only adopt a complete calculated set matching the authoritative replay.

    Missing frontend data is an explicit fallback. Contradictory data is never
    accepted as current. The caller retries while quotes/products settle.
    """
    def unavailable(reason: str) -> None:
        if diagnostics is not None:
            diagnostics["reason"] = reason
        return None

    if diagnostics is not None:
        diagnostics.clear()
    if observed.get("error"):
        raise TaskError("DEGIRO portfolio observation failed")
    if observed.get("snapshot_error"):
        return unavailable("fresh_snapshot_unavailable")
    positions, totals = observed.get("positions"), observed.get("totals")
    if not base or not isinstance(positions, dict) or not isinstance(totals, dict):
        return unavailable("missing_observation")
    if any(totals.get(key) is None for key in ("portfolio", "total", "totalCash", "totalPl", "todayPl")):
        return unavailable("missing_calculated_totals")
    expected = {row["product_id"]: row for row in holdings}
    active = {}
    for key, row in positions.items():
        if not isinstance(row, dict):
            return unavailable("invalid_position_shape")
        if row.get("positionType") not in ("PRODUCT", "SECURITY"):
            continue
        quantity = number(row.get("size"), "position quantity")
        if quantity:
            active[str(key)] = row
    if active.keys() != expected.keys():
        return unavailable("position_set_mismatch")
    replacement = []
    for key, original in expected.items():
        row = active[key]
        if number(row.get("size"), "position quantity") != original["quantity"]:
            return unavailable("quantity_mismatch")
        info = row.get("productInfo")
        if not isinstance(info, dict) or info.get("id") is None or str(info["id"]) != key:
            return unavailable("missing_product_identity")
        if currency(info.get("currency")) != original["currency"]:
            raise TaskError("DEGIRO returned inconsistent instrument currencies")
        if row.get("price") is None or row.get("lastUpdate") is None:
            return unavailable("missing_price_or_time")
        price = number(row["price"], "quote price")
        value = number(row.get("value"), "calculated position value")
        as_of = timestamp(row["lastUpdate"])
        close = timestamp(info.get("closePriceDate"))
        # lastUpdate starts at closePriceDate and is replaced by the quote feed.
        # Never infer a quote merely because the page or sync ran recently.
        quoted = bool(close and as_of and "T" in as_of and
                      datetime.fromisoformat(as_of) > datetime.fromisoformat(close).replace(
                          tzinfo=timezone.utc))
        # Native CombinedLastDateTime combines the broker's LastDate with
        # LastTime parsed using the browser timezone and TODAY's DST offset.
        # Its serialized Z suffix does not establish a UTC quote instant.
        # Preserve the independently supplied calendar date, not a guessed
        # timezone correction. Classify quote vs initial close before truncating.
        as_of = as_of[:10]
        replacement.append({**original, "market_value": value,
                            "value_currency": base, "price": price,
                            "price_as_of": as_of,
                            "valuation_source": "broker_quote" if quoted else "broker_close"})
    portfolio = number(totals["portfolio"], "portfolio value")
    cash = number(totals.get("cryptoTotalCash") or totals["totalCash"] or 0, "cash balance")
    balance = number(totals["total"], "account value")
    if abs(Decimal(str(balance)) - Decimal(str(portfolio)) - Decimal(str(cash))) > Decimal("0.02"):
        return unavailable("account_balance_mismatch")
    difference = float(Decimal(str(portfolio)) - sum(
        (Decimal(str(row["market_value"])) for row in replacement), Decimal(0)))
    # The frontend applies these explicit adjustments to the portfolio sum.
    # Reject a mismatched generation or an adjustment we cannot convert, rather
    # than accepting arbitrary differences as a successfully reconciled sync.
    adjustments = sum((Decimal(str(number(row.get("portfolioValueCorrection") or 0, "portfolio correction")))
                       for row in positions.values()), Decimal(0))
    compensation = sum((Decimal(str(number(totals.get(key) or 0, "cash-fund compensation")))
                        for key in ("cashFundCompensation", "cashFundCompensationPending")), Decimal(0))
    if compensation and totals.get("cashFundCompensationCurrency") != base:
        return unavailable("unknown_compensation_currency")
    adjustments += compensation
    if abs(Decimal(str(difference)) - adjustments) > Decimal("0.02"):
        return unavailable("portfolio_value_mismatch")
    warning = ""
    if abs(difference) > 0.02:
        warning = "Broker portfolio includes cash-fund compensation or explicit valuation adjustments."
    price_status = "quoted" if replacement and all(
        row["valuation_source"] == "broker_quote" for row in replacement) else "closing"
    if replacement and price_status != "quoted":
        warning = "Some holdings use closing prices; live quotes were not available for every position. " + warning
    if any(row["valuation_source"] == "broker_quote" for row in replacement):
        warning = "Broker quote dates are shown; precise quote times are unavailable. " + warning
    return {"holdings": replacement, "valuation": {
        "base_currency": base, "portfolio_value": portfolio,
        "cash_balance": cash, "account_value": balance,
        "total_pl": number(totals["totalPl"], "total P/L"),
        "day_pl": number(totals["todayPl"], "day P/L"),
        "available_to_trade": number(totals.get("availableToSpend"), "available to trade", optional=True),
        "source": "broker_frontend", "price_status": price_status,
        "reconciliation_difference": difference, "warning": warning.strip(),
    }}
