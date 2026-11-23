"""Shared helpers for browser-backed DEGIRO tasks."""

from __future__ import annotations

import asyncio
import json
import logging
import re
from collections.abc import Awaitable, Callable, Iterable
from typing import Any
from urllib.parse import parse_qsl, urlsplit

from patchright.async_api import BrowserContext, Page, Response

log = logging.getLogger("degitide-worker.tasks")


class TaskError(RuntimeError):
    """A task failed in a way that can be shown to the service operator."""


def request_schema(method: str, url: str) -> tuple[str, str, tuple[str, ...]]:
    parsed = urlsplit(url)
    path = re.sub(r"(?<=/)[0-9]+(?=/|;|$)", "{id}", parsed.path)
    path = re.sub(r";jsessionid=[A-Za-z0-9._~-]+", ";jsessionid={token}", path)
    query_keys = tuple(sorted({key for key, _ in parse_qsl(parsed.query)}))
    return method.upper(), path, query_keys


def json_body_schema(raw: str | None) -> dict[str, object]:
    try:
        value = json.loads(raw or "")
    except json.JSONDecodeError:
        return {"root": "invalid"}
    if isinstance(value, list):
        return {
            "root": "list",
            "length": len(value),
            "item_types": sorted({type(item).__name__ for item in value}),
        }
    if isinstance(value, dict):
        return {
            "root": "object",
            "fields": {
                str(key): (
                    {"type": "list", "length": len(item)}
                    if isinstance(item, list)
                    else {"type": type(item).__name__}
                )
                for key, item in sorted(value.items())
            },
        }
    return {"root": type(value).__name__}


async def get_page(context: BrowserContext, *, new: bool = False) -> Page:
    """Return a usable page from a persistent context."""
    if not new:
        for page in reversed(context.pages):
            if not page.is_closed():
                return page
    return await context.new_page()


async def collect_json_responses(
    page: Page,
    patterns: Iterable[str],
    action: Callable[[], Awaitable[None]],
    *,
    settle_ms: int = 3_000,
) -> list[tuple[str, Any]]:
    """Collect matching JSON responses emitted while ``action`` runs."""
    lowered_patterns = tuple(pattern.lower() for pattern in patterns)
    captured: list[tuple[str, Any]] = []
    pending: set[asyncio.Task[None]] = set()

    async def read_response(response: Response) -> None:
        try:
            content_type = (await response.header_value("content-type") or "").lower()
            if "json" not in content_type:
                return
            captured.append((response.url, await response.json()))
        except Exception as exc:  # A matching response can disappear during navigation.
            log.debug("could not read response %s: %s", response.url, exc)

    def on_response(response: Response) -> None:
        url = response.url.lower()
        trader_response = urlsplit(url).hostname == "trader.degiro.nl"
        _, response_path, _ = request_schema(response.request.method, response.url)
        if response_path == "/productsearch/secure/v1/products/info":
            log.info(
                "product-info response status=%s content_type=%s",
                response.status,
                response.headers.get("content-type", "").split(";", 1)[0],
            )
        if response.status == 403 and trader_response:
            method, path, query_keys = request_schema(response.request.method, response.url)
            log.warning(
                "blocked response method=%s path=%s query_keys=%s",
                method,
                path,
                ",".join(query_keys),
            )
            if method == "POST" and path == "/productsearch/secure/v1/products/info":
                log.warning(
                    "blocked product-info body_schema=%s",
                    json.dumps(
                        json_body_schema(response.request.post_data),
                        separators=(",", ":"),
                        sort_keys=True,
                    ),
                )
        if not any(pattern in url for pattern in lowered_patterns):
            return
        task = asyncio.create_task(read_response(response))
        pending.add(task)
        task.add_done_callback(pending.discard)

    page.on("response", on_response)
    try:
        await action()
        await page.wait_for_timeout(settle_ms)
        if pending:
            await asyncio.gather(*tuple(pending), return_exceptions=True)
    finally:
        page.remove_listener("response", on_response)
    return captured


def compact_json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), default=str)


def as_float(value: Any, default: float = 0.0) -> float:
    if value is None or value == "":
        return default
    try:
        return float(value)
    except (TypeError, ValueError):
        return default


def first(mapping: dict[str, Any], *names: str, default: Any = None) -> Any:
    for name in names:
        if name in mapping and mapping[name] is not None:
            return mapping[name]
    return default


def extract_list(payload: Any, *keys: str) -> list[Any]:
    """Find the first list under one of ``keys`` in a shallow API envelope."""
    if isinstance(payload, list):
        return payload
    if not isinstance(payload, dict):
        return []
    for key in keys:
        value = payload.get(key)
        if isinstance(value, list):
            return value
        if isinstance(value, dict) and isinstance(value.get("value"), list):
            return value["value"]
    data = payload.get("data")
    if isinstance(data, list):
        return data
    if isinstance(data, dict):
        return extract_list(data, *keys)
    return []
