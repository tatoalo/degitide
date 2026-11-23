"""Resolve DEGIRO product IDs using the authenticated browser context."""

from __future__ import annotations

import json
import logging
import re
from typing import Any
from urllib.parse import parse_qs, urlencode, urlsplit

from patchright.async_api import BrowserContext

from .common import first

PRODUCT_INFO_PATH = "/productsearch/secure/v1/products/info"
log = logging.getLogger("degitide-worker.products")


def _payload_shape(value: Any, depth: int = 0) -> object:
    if depth >= 3:
        return type(value).__name__
    if isinstance(value, dict):
        return {
            str(key): _payload_shape(item, depth + 1)
            for key, item in sorted(value.items())
        }
    if isinstance(value, list):
        return {
            "type": "list",
            "length": len(value),
            "item": _payload_shape(value[0], depth + 1) if value else None,
        }
    return type(value).__name__


def product_map(payload: Any) -> dict[str, dict[str, str]]:
    if not isinstance(payload, dict):
        return {}
    data = payload.get("data", payload)
    if isinstance(data, dict):
        candidates = list(data.items())
    elif isinstance(data, list):
        candidates = [(None, item) for item in data]
    else:
        return {}

    products: dict[str, dict[str, str]] = {}
    for key, value in candidates:
        if not isinstance(value, dict):
            continue
        product_id = first(value, "id", "productId", default=key)
        if product_id is None:
            continue
        label = first(value, "symbol", "name", "isin", default=product_id)
        products[str(product_id)] = {
            "label": str(label),
            "currency": str(first(value, "currency", "currencyCode", default="")),
            "name": str(first(value, "name", "productName", default="") or "")[:120],
            "isin": str(first(value, "isin", default="") or "")[:12],
            "exchange_id": str(value.get("exchangeId") or ""),
            "exchange_mic": str(value.get("micCode") or ""),
            "product_type": str(value.get("productTypeId") or ""),
        }
    return products


def _product_info_url(captured: list[tuple[str, Any]]) -> str | None:
    for url, _ in captured:
        if PRODUCT_INFO_PATH in url.lower():
            return url

    for url, _ in captured:
        parts = urlsplit(url)
        query = parse_qs(parts.query)
        int_account = (query.get("intAccount") or [None])[0]
        session_id = (query.get("sessionId") or [None])[0]
        if int_account is None:
            account_match = re.search(r"/update/(\d+)", parts.path)
            int_account = account_match.group(1) if account_match else None
        if session_id is None:
            session_match = re.search(r";jsessionid=([^/?;]+)", parts.path, re.IGNORECASE)
            session_id = session_match.group(1) if session_match else None
        if int_account and session_id:
            return f"{parts.scheme}://{parts.netloc}{PRODUCT_INFO_PATH}?{urlencode({'intAccount': int_account, 'sessionId': session_id})}"
    return None


async def resolve_products(
    context: BrowserContext,
    captured: list[tuple[str, Any]],
    product_ids: set[str],
) -> dict[str, dict[str, str]]:
    products: dict[str, dict[str, str]] = {}
    for url, payload in captured:
        if PRODUCT_INFO_PATH in url.lower():
            mapped = product_map(payload)
            products.update(mapped)
            if not mapped:
                log.warning(
                    "unrecognized product-info payload_shape=%s",
                    json.dumps(_payload_shape(payload), separators=(",", ":")),
                )

    missing = sorted(product_ids - products.keys())
    if not missing:
        return products

    info_url = _product_info_url(captured)
    if info_url is None:
        return products
    log.info(
        "product-info replay schema count=%s all_digits=%s max_id_length=%s",
        len(missing),
        all(item.isdigit() for item in missing),
        max((len(item) for item in missing), default=0),
    )
    try:
        response = await context.request.post(
            info_url,
            data=json.dumps(missing, separators=(",", ":")),
            headers={
                "Content-Type": "application/json",
                "Referer": "https://trader.degiro.nl/trader/",
            },
            timeout=60_000,
        )
        log.info("product-info replay response status=%s", response.status)
        if response.status == 403:
            try:
                denial = await response.json()
                log.warning("product-info replay denied reason=%s", denial.get("reason", ""))
            except Exception:
                pass
        if response.ok:
            payload = await response.json()
            mapped = product_map(payload)
            products.update(mapped)
            if not mapped:
                log.warning(
                    "unrecognized product-info replay payload_shape=%s",
                    json.dumps(_payload_shape(payload), separators=(",", ":")),
                )
    except Exception as exc:
        message = re.sub(r"https?://\S+", "<url>", str(exc).splitlines()[0])
        log.warning("product-info replay failed: %s: %s", type(exc).__name__, message[:256])
    return products
