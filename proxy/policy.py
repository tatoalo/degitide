"""Default-deny HTTPS egress policy for the DEGIRO browser worker.

The worker has no direct internet route in Docker. Every HTTPS request must pass
through this mitmproxy addon, which permits only the login shell and the small
set of read/report requests used by degitide.
"""

from __future__ import annotations

import hmac
import json
import logging
import os
import re
from collections.abc import Mapping
from pathlib import Path
from typing import NamedTuple
from urllib.parse import parse_qsl, quote, unquote, urlsplit

try:  # Unit tests exercise the pure policy without installing mitmproxy.
    from mitmproxy import http
except ImportError:  # pragma: no cover - mitmproxy is present in its image.
    http = None


log = logging.getLogger("degitide-egress-policy")


def configure_policy_logging() -> None:
    """Keep sanitized policy decisions visible even with mitmdump --quiet.

    Do not enable mitmproxy's flow logger: its URLs can contain broker sessions.
    This logger only receives the fixed route labels and methods below.
    """
    if not any(getattr(handler, "degitide_policy", False) for handler in log.handlers):
        handler = logging.StreamHandler()
        handler.degitide_policy = True
        handler.setFormatter(logging.Formatter("%(asctime)s [%(levelname)s] %(message)s"))
        log.addHandler(handler)
    log.setLevel(logging.INFO)
    log.propagate = False


DEGIRO_HOST = "trader.degiro.nl"
QUOTE_HOSTS = frozenset({"degiro.quotecast.vwdservices.com", "quotecast.vwdservices.com"})
ALLOWED_HOSTS = QUOTE_HOSTS | {DEGIRO_HOST}
MAX_QUERY_LENGTH = 2_048
MAX_LOGIN_BODY = 64 * 1024
MAX_PRODUCT_BODY = 32 * 1024
MAX_REQUEST_BODY = MAX_LOGIN_BODY
LOGIN_CAPABILITY_HEADER = "X-Degitide-Login-Capability"
# Marks responses this proxy generates, so the worker can tell a policy denial
# from a broker response. Keep in sync with worker/tasks/login.py.
EGRESS_DENIED_HEADER = "X-Degitide-Egress"
DENIAL_HEADERS = {"Content-Type": "application/json", "Cache-Control": "no-store",
                  EGRESS_DENIED_HEADER: "denied"}

STATIC_PREFIXES = (
    "/login/fonts/",
    "/login/images/",
    "/login/scripts/",
    "/login/styles/",
    "/trader/fonts/",
    "/trader/images/",
    "/trader/scripts/",
    "/trader/styles/",
)

STATIC_PATHS = {
    "/login",
    "/login/",
    "/login/settings",
    "/trader",
    "/trader/",
}

TRANSACTION_QUERY_KEYS = {
    "fromDate",
    "toDate",
    "intAccount",
    "sessionId",
    "groupTransactionsByOrder",
}

REPORT_QUERY_KEYS = {
    "country",
    "lang",
    "fromDate",
    "toDate",
    "intAccount",
    "sessionId",
}

UPDATE_QUERY_KEYS = {
    "alerts",
    "cashFunds",
    "historicalOrders",
    "orders",
    "portfolio",
    "totalPortfolio",
    "transactions",
    "intAccount",
    "sessionId",
}

ACCOUNT_QUERY_KEYS = {"intAccount", "sessionId"}
TRANSLATION_QUERY_KEYS = {"country", "language", "modules"}
SESSION_QUERY_KEYS = {"sessionId"}

UPDATE_PATH = re.compile(
    r"^/trading/secure/v5/update(?:/[0-9]+(?:;jsessionid=[A-Za-z0-9._~-]{1,256})?)?/?$"
)
ACCOUNT_INFO_PATH = re.compile(
    r"^/trading/secure/v5/account/info(?:/[0-9]+(?:;jsessionid=[A-Za-z0-9._~-]{1,256})?)?/?$"
)
FX_SETTINGS_PATH = re.compile(
    r"^/trading/secure/settings/showFxConversionTab(?:;jsessionid=[A-Za-z0-9._~-]{1,256})?/?$"
)
LOGIN_PATH = re.compile(r"^/login/secure/login(?:/[A-Za-z0-9._~-]+){0,3}/?$")
EVENT_WORKER_PATH = re.compile(r"^/trader/scripts/event-broker-worker\.[a-f0-9]{20}\.js$")
WORKER_DEPENDENCY_PATH = re.compile(
    r"^/trader/scripts/(runtime|vendor|frontend-core)\.[a-f0-9]{20}\.js$"
)
QUOTE_SESSION_PATH = re.compile(r"^/CORS/[A-Za-z0-9_-]{1,256}$")
QUOTE_COMMAND = re.compile(
    r"(?:req|rel)\([A-Za-z0-9_,|.\-]{1,128}\.[A-Za-z][A-Za-z0-9]{0,63}\);"
)
MAX_QUOTE_BODY = 64 * 1024
MAX_QUOTE_COMMANDS = 2_000


class Decision(NamedTuple):
    allowed: bool
    route: str
    reason: str = ""


def _load_login_capability() -> str:
    path = os.environ.get("DEGITIDE_PROXY_LOGIN_TOKEN_FILE", "")
    if not path:
        return ""
    try:
        value = Path(path).read_text(encoding="ascii").strip()
    except OSError as exc:
        raise RuntimeError("could not read the proxy login capability") from exc
    if not re.fullmatch(r"[A-Za-z0-9_-]{32,256}", value):
        raise RuntimeError("the proxy login capability is invalid")
    return value


def _login_capability_is_valid(values: list[str], expected: str) -> bool:
    return bool(
        expected
        and len(values) == 1
        and hmac.compare_digest(values[0], expected)
    )


def _take_login_capability(headers) -> list[str]:
    """Remove the internal capability before any request can reach DEGIRO."""
    if hasattr(headers, "get_all"):
        values = list(headers.get_all(LOGIN_CAPABILITY_HEADER))
        if values:
            del headers[LOGIN_CAPABILITY_HEADER]
        return values

    values: list[str] = []
    for key in list(headers):
        if str(key).lower() == LOGIN_CAPABILITY_HEADER.lower():
            values.append(str(headers.pop(key)))
    return values


def _safe_path(path: str) -> bool:
    if not path.startswith("/") or len(path) > 1_024:
        return False
    decoded = path
    for _ in range(4):
        if re.search(r"%(?![0-9A-Fa-f]{2})", decoded):
            return False
        try:
            expanded = unquote(decoded, errors="strict")
        except UnicodeDecodeError:
            return False
        if expanded == decoded:
            break
        decoded = expanded
    else:
        return False
    if "\\" in decoded or any(ord(char) < 0x20 for char in decoded):
        return False
    return not {".", ".."}.intersection(decoded.split("/"))


def _valid_query(raw_query: str, allowed_keys: set[str], *, required: set[str] | None = None) -> bool:
    if len(raw_query) > MAX_QUERY_LENGTH or re.search(r"%(?![0-9A-Fa-f]{2})", raw_query):
        return False
    try:
        pairs = parse_qsl(raw_query, keep_blank_values=True, strict_parsing=False)
    except ValueError:
        return False
    keys: list[str] = []
    for key, value in pairs:
        if key not in allowed_keys or key in keys or len(value) > 256:
            return False
        if "\ufffd" in key or "\ufffd" in value or any(ord(char) < 0x20 for char in value):
            return False
        keys.append(key)
    return required is None or required.issubset(keys)


def _has_protocol_upgrade(headers: Mapping[str, str] | None) -> bool:
    if not headers:
        return False
    normalized = {str(key).lower(): str(value) for key, value in headers.items()}
    connection_tokens = {
        token.strip().lower()
        for token in normalized.get("connection", "").split(",")
        if token.strip()
    }
    return bool(normalized.get("upgrade")) or "upgrade" in connection_tokens


def _valid_translation_query(raw_query: str) -> bool:
    if not _valid_query(
        raw_query,
        TRANSLATION_QUERY_KEYS,
        required=TRANSLATION_QUERY_KEYS,
    ):
        return False
    values = dict(parse_qsl(raw_query, keep_blank_values=True))
    if not re.fullmatch(r"[A-Za-z]{2}", values["country"]):
        return False
    if not re.fullmatch(r"[A-Za-z]{2}", values["language"]):
        return False
    modules = values["modules"].split(",")
    return 1 <= len(modules) <= 10 and all(
        re.fullmatch(r"[A-Za-z0-9_-]{1,32}", module) for module in modules
    )


def _valid_product_body(body: bytes) -> bool:
    if not body or len(body) > MAX_PRODUCT_BODY:
        return False
    try:
        value = json.loads(body)
    except (UnicodeDecodeError, ValueError, RecursionError):
        return False
    if not isinstance(value, list) or not 1 <= len(value) <= 500:
        return False
    # Trader 5.144.0's worker syncWithParsedSnapshot submits every added row's
    # productId/id to frontend-core module 5504, including cash. Its cash-value
    # calculation recognizes currency IDs and FLATEX_<currency> explicitly.
    return all(
        (type(item) is int and 0 <= item < 10**32)
        or (isinstance(item, str) and re.fullmatch(r"[0-9]{1,32}|(?:FLATEX_)?[A-Z]{3}", item))
        for item in value
    )


def _valid_worker_query(raw_query: str) -> bool:
    """Allow the shipped event worker's three same-origin script imports only."""
    if len(raw_query) > MAX_QUERY_LENGTH or re.search(r"%(?![0-9A-Fa-f]{2})", raw_query):
        return False
    pairs = parse_qsl(raw_query, keep_blank_values=True)
    values = dict(pairs)
    required = {"requires", "idleTime", "clientId"}
    if (
        len(pairs) != len(values)
        or not required.issubset(values)
        or not set(values).issubset(required | {"isSharedWorker"})
        or values["idleTime"] not in {"20", "60"}
        or not re.fullmatch(r"[0-9]{1,32}", values["clientId"])
        or values.get("isSharedWorker", "true") != "true"
    ):
        return False
    dependencies = values["requires"].split(",")
    if len(dependencies) != 3:
        return False
    kinds: set[str] = set()
    for dependency in dependencies:
        try:
            parts = urlsplit(dependency)
            match = WORKER_DEPENDENCY_PATH.fullmatch(parts.path)
            if (
                parts.scheme != "https"
                or parts.netloc != DEGIRO_HOST
                or parts.query
                or parts.fragment
                or not match
            ):
                return False
            kinds.add(match.group(1))
        except ValueError:
            return False
    return kinds == {"runtime", "vendor", "frontend-core"}


def _one_field_json(body: bytes, field: str) -> str | None:
    # Duplicate JSON keys must not pass validation under a different parser.
    try:
        pairs = json.loads(body, object_pairs_hook=list)
    except (UnicodeDecodeError, json.JSONDecodeError):
        return None
    if (
        not isinstance(pairs, list)
        or len(pairs) != 1
        or not isinstance(pairs[0], tuple)
        or pairs[0][0] != field
        or not isinstance(pairs[0][1], str)
    ):
        return None
    return pairs[0][1]


def _quote_decision(method: str, path: str, query: str, body: bytes) -> Decision:
    """VWD's HTTP quote subscription protocol; no general-purpose POST route.

    Observed in DEGIRO trader 5.144.0's event-broker-worker bundle: request a
    quote session, subscribe/release named market fields, and poll its updates.
    The server sends heartbeats; the client emits only req(...) and rel(...).
    """
    if path == "/CORS/request_session":
        if method != "POST" or not _valid_query(
            query, {"version", "userToken"}, required={"version", "userToken"}
        ):
            return Decision(False, "quote-session", "invalid quote session request")
        values = dict(parse_qsl(query, keep_blank_values=True))
        referrer = _one_field_json(body, "referrer") if len(body) <= 2_048 else None
        if (
            values["version"] != "1.0.20201211"
            or not re.fullmatch(r"[0-9]{1,32}", values["userToken"])
            or not referrer
        ):
            return Decision(False, "quote-session", "invalid quote session parameters")
        try:
            parts = urlsplit(referrer)
            if (
                parts.scheme != "https"
                or parts.netloc != DEGIRO_HOST
                # Native trader passes location.origin to its event broker;
                # the streamer sends that value unchanged as the referrer.
                or parts.path not in {"", "/", "/trader", "/trader/"}
                or parts.query
                or len(parts.fragment) > 256
                or any(ord(char) < 0x20 for char in referrer)
            ):
                return Decision(False, "quote-session", "invalid quote referrer")
        except ValueError:
            return Decision(False, "quote-session", "invalid quote referrer")
        return Decision(True, "quote-session")
    if not QUOTE_SESSION_PATH.fullmatch(path) or query:
        return Decision(False, "quote-stream", "invalid quote stream URL")
    if method == "GET":
        return Decision(True, "quote-stream")
    if method != "POST" or not body or len(body) > MAX_QUOTE_BODY:
        return Decision(False, "quote-control", "invalid quote control request")
    encoded = _one_field_json(body, "controlData")
    if not encoded:
        return Decision(False, "quote-control", "invalid quote control body")
    try:
        commands = unquote(encoded, errors="strict")
    except UnicodeDecodeError:
        return Decision(False, "quote-control", "invalid quote control encoding")
    # Match JavaScript encodeURI exactly; reject nested/noncanonical encoding.
    if quote(commands, safe=";/?:@&=+$,-_.!~*'()#") != encoded:
        return Decision(False, "quote-control", "invalid quote control encoding")
    matches = list(QUOTE_COMMAND.finditer(commands))
    if (
        not 1 <= len(matches) <= MAX_QUOTE_COMMANDS
        or "".join(match.group(0) for match in matches) != commands
    ):
        return Decision(False, "quote-control", "only quote subscriptions are allowed")
    return Decision(True, "quote-control")


def decide(
    method: str,
    url: str,
    *,
    body: bytes = b"",
    headers: Mapping[str, str] | None = None,
    login_enabled: bool = False,
) -> Decision:
    """Return the policy decision for one fully decrypted HTTP request."""
    try:
        parts = urlsplit(url)
        host = (parts.hostname or "").lower()
        port = parts.port
        has_userinfo = parts.username is not None or parts.password is not None
    except ValueError:
        return Decision(False, "invalid", "malformed URL")

    method = method.upper()
    if (
        parts.scheme != "https"
        or host not in ALLOWED_HOSTS
        or port not in (None, 443)
        or has_userinfo
        or parts.fragment
    ):
        return Decision(False, "destination", "only approved HTTPS broker and quote hosts are allowed")
    if method == "CONNECT" or _has_protocol_upgrade(headers):
        return Decision(False, "protocol", "tunnels and protocol upgrades are not allowed")
    if len(body) > MAX_REQUEST_BODY:
        return Decision(False, "body", "request body is too large")
    if method in {"GET", "HEAD"} and body:
        return Decision(False, "body", "GET and HEAD requests may not carry a body")
    if not _safe_path(parts.path):
        return Decision(False, "path", "unsafe or non-canonical path")

    if host in QUOTE_HOSTS:
        return _quote_decision(method, parts.path, parts.query, body)

    if method == "GET" and EVENT_WORKER_PATH.fullmatch(parts.path) and parts.query:
        if not _valid_worker_query(parts.query):
            return Decision(False, "event-worker", "invalid event worker bootstrap")
        return Decision(True, "event-worker")

    if method in {"GET", "HEAD"} and (
        parts.path in STATIC_PATHS or parts.path.startswith(STATIC_PREFIXES)
    ):
        if parts.path in {"/login", "/login/"} and not _valid_query(
            parts.query, {"redirectUrl"}
        ):
            return Decision(False, "login-shell", "unexpected login query")
        if parts.path not in {"/login", "/login/"} and parts.query:
            return Decision(False, "static", "static requests may not carry a query")
        return Decision(True, "static")

    if method in {"GET", "HEAD"} and parts.path == "/translations/":
        if not _valid_translation_query(parts.query):
            return Decision(False, "translations", "invalid translation query")
        return Decision(True, "translations")

    if method in {"GET", "HEAD"} and parts.path == "/login/secure/config":
        if not _valid_query(parts.query, ACCOUNT_QUERY_KEYS):
            return Decision(False, "login-config", "invalid login configuration query")
        return Decision(True, "login-config")

    if login_enabled and method in {"GET", "POST"} and LOGIN_PATH.fullmatch(parts.path):
        if parts.query or len(body) > MAX_LOGIN_BODY:
            return Decision(False, "login", "unexpected login query or oversized body")
        return Decision(True, "login")

    if method == "GET" and parts.path == "/portfolio-reports/secure/v4/transactions":
        if not _valid_query(
            parts.query,
            TRANSACTION_QUERY_KEYS,
            required={"fromDate", "toDate", "intAccount", "sessionId"},
        ):
            return Decision(False, "transactions", "invalid transaction-report query")
        return Decision(True, "transactions")

    if method == "GET" and parts.path in {
        "/portfolio-reports/secure/v3/cashAccountReport/csv",
        "/portfolio-reports/secure/v3/positionReport/csv",
    }:
        if not _valid_query(
            parts.query,
            REPORT_QUERY_KEYS,
            required={"fromDate", "toDate", "intAccount", "sessionId"},
        ):
            return Decision(False, "csv-report", "invalid CSV-report query")
        return Decision(True, "csv-report")

    if method == "GET" and UPDATE_PATH.fullmatch(parts.path):
        if not _valid_query(
            parts.query,
            UPDATE_QUERY_KEYS,
            required={"portfolio", "intAccount", "sessionId"},
        ):
            return Decision(False, "holdings", "invalid holdings query")
        return Decision(True, "holdings")

    if method == "GET" and (
        parts.path == "/pa/secure/client" or ACCOUNT_INFO_PATH.fullmatch(parts.path)
    ):
        if not _valid_query(parts.query, ACCOUNT_QUERY_KEYS):
            return Decision(False, "account-bootstrap", "invalid account-bootstrap query")
        return Decision(True, "account-bootstrap")

    if method == "GET" and parts.path == "/settings/secure/v1/web":
        if not _valid_query(
            parts.query,
            ACCOUNT_QUERY_KEYS,
            required=ACCOUNT_QUERY_KEYS,
        ):
            return Decision(False, "settings", "invalid web-settings query")
        return Decision(True, "settings")

    if method == "GET" and parts.path in {
        "/settings/secure/v1/user",
        "/settings/secure/v1/beta-test",
    }:
        if not _valid_query(
            parts.query,
            ACCOUNT_QUERY_KEYS,
            required=ACCOUNT_QUERY_KEYS,
        ):
            return Decision(False, "settings", "invalid account-settings query")
        return Decision(True, "settings")

    if method == "GET" and FX_SETTINGS_PATH.fullmatch(parts.path):
        if not _valid_query(
            parts.query,
            SESSION_QUERY_KEYS,
            required=SESSION_QUERY_KEYS,
        ):
            return Decision(False, "settings", "invalid FX-settings query")
        return Decision(True, "settings")

    # The trader SPA now loads its exchange dictionary from the versioned secure
    # path, observed only without a trailing slash, like the other secure routes.
    if method == "GET" and parts.path in {
        "/product_search/config/dictionary", "/productsearch/config/dictionary",
        "/product_search/config/dictionary/", "/productsearch/config/dictionary/",
        "/productsearch/secure/v1/config/dictionary",
    }:
        if not _valid_query(parts.query, ACCOUNT_QUERY_KEYS, required=ACCOUNT_QUERY_KEYS):
            return Decision(False, "exchange-dictionary", "invalid dictionary query")
        return Decision(True, "exchange-dictionary")

    if method == "POST" and parts.path == "/productsearch/secure/v1/products/info":
        if not _valid_query(
            parts.query,
            ACCOUNT_QUERY_KEYS,
            required={"intAccount", "sessionId"},
        ):
            return Decision(False, "product-info", "invalid product-info query")
        if not _valid_product_body(body):
            return Decision(False, "product-info", "invalid product-info body")
        return Decision(True, "product-info")

    return Decision(False, "default-deny", "request is not on the read/report allowlist")


class PolicyAddon:
    def __init__(self) -> None:
        self.login_enabled = os.environ.get("DEGITIDE_PROXY_LOGIN_MODE", "false").lower() == "true"
        self.login_capability = _load_login_capability()

    def http_connect(self, flow) -> None:
        """Reject CONNECT destinations before mitmproxy opens an upstream socket."""
        flow.metadata["degitide_connect_envelope"] = True
        host = (flow.request.host or "").lower()
        if host not in ALLOWED_HOSTS or flow.request.port != 443:
            flow.response = http.Response.make(
                403,
                b'{"error":"egress destination denied"}\n',
                DENIAL_HEADERS,
            )
            log.warning("blocked CONNECT destination")

    def request(self, flow) -> None:
        # mitmproxy also invokes this hook for the CONNECT envelope. Destination
        # validation for that envelope happens in http_connect; the decrypted
        # HTTPS request arrives here afterwards with its real method and URL.
        if flow.request.method.upper() == "CONNECT" and flow.metadata.pop(
            "degitide_connect_envelope", False
        ):
            return
        capability_values = _take_login_capability(flow.request.headers)
        capability_valid = _login_capability_is_valid(
            capability_values, self.login_capability
        )
        if capability_values and not capability_valid:
            log.warning("rejected invalid login capability")
        decision = decide(
            flow.request.method,
            flow.request.pretty_url,
            body=flow.request.raw_content or b"",
            headers=flow.request.headers,
            login_enabled=self.login_enabled or capability_valid,
        )
        if decision.allowed:
            if urlsplit(flow.request.pretty_url).hostname in QUOTE_HOSTS:
                # VWD authenticates its read subscription with its own query
                # token. Broker cookies/bearer credentials never belong here.
                for name in list(flow.request.headers):
                    if str(name).lower() in {"authorization", "cookie", "proxy-authorization"}:
                        del flow.request.headers[name]
            log.info("allowed route=%s method=%s", decision.route, flow.request.method)
            return
        flow.response = http.Response.make(
            403,
            json.dumps({"error": "egress request denied", "reason": decision.reason}).encode() + b"\n",
            DENIAL_HEADERS,
        )
        log.warning("blocked route=%s method=%s", decision.route, flow.request.method)


if http is not None:
    configure_policy_logging()
addons = [PolicyAddon()] if http is not None else []
