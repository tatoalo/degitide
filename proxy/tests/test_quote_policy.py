from __future__ import annotations

import json
import unittest
from types import SimpleNamespace
from unittest.mock import patch
from urllib.parse import quote, urlencode

from proxy.policy import LOGIN_CAPABILITY_HEADER, PolicyAddon, QUOTE_HOSTS, decide


SCRIPT_HASH = "0123456789abcdef0123"
WORKER_URL = f"https://trader.degiro.nl/trader/scripts/event-broker-worker.{SCRIPT_HASH}.js"
DEPENDENCIES = ",".join(
    f"https://trader.degiro.nl/trader/scripts/{name}.{SCRIPT_HASH}.js"
    for name in ("runtime", "vendor", "frontend-core")
)
QUOTE_ROOT = "https://degiro.quotecast.vwdservices.com"
SESSION_QUERY = "version=1.0.20201211&userToken=12345"
SESSION_BODY = json.dumps({"referrer": "https://trader.degiro.nl/trader/#/portfolio/assets"}).encode()


def control_body(commands: str) -> bytes:
    return json.dumps({"controlData": quote(commands, safe=";/?:@&=+$,-_.!~*'()#")}).encode()


class QuotePolicyTests(unittest.TestCase):
    def assert_allowed(self, method, url, **kwargs):
        decision = decide(method, url, **kwargs)
        self.assertTrue(decision.allowed, decision)

    def assert_denied(self, method, url, **kwargs):
        decision = decide(method, url, **kwargs)
        self.assertFalse(decision.allowed, decision)

    def test_event_worker_bootstrap_imports_only_the_three_shipped_dependencies(self):
        query = dict(requires=DEPENDENCIES, idleTime="60", clientId="12345")
        self.assert_allowed("GET", WORKER_URL + "?" + urlencode(query))
        self.assert_allowed("GET", WORKER_URL + "?" + urlencode({**query, "idleTime": "20", "isSharedWorker": "true"}))
        for bad in (
            {**query, "requires": DEPENDENCIES.replace("trader.degiro.nl", "evil.example", 1)},
            {**query, "requires": DEPENDENCIES.replace("runtime.", "orders.")},
            {**query, "requires": DEPENDENCIES.replace("frontend-core.", "vendor.")},
            {**query, "requires": DEPENDENCIES + ",https://evil.example/payload.js"},
            {**query, "requires": DEPENDENCIES.replace("https://", "https://user@", 1)},
            {**query, "requires": DEPENDENCIES.replace(".js,", ".js?secret=x,", 1)},
            {**query, "requires": DEPENDENCIES.replace("/scripts/", "/scripts/../", 1)},
            {**query, "requires": DEPENDENCIES.replace("runtime", "%72untime", 1)},
            {**query, "idleTime": "3600"},
            {**query, "clientId": "12345&admin=true"},
            {**query, "isSharedWorker": "false"},
            {**query, "admin": "true"},
        ):
            with self.subTest(query=bad):
                self.assert_denied("GET", WORKER_URL + "?" + urlencode(bad))
        self.assert_denied("GET", WORKER_URL + "?" + urlencode(query) + "&clientId=12345")
        self.assert_denied("POST", WORKER_URL + "?" + urlencode(query))
        self.assert_denied("GET", WORKER_URL.replace("event-broker-worker", "app") + "?" + urlencode(query))

    def test_both_exact_quote_hosts_support_session_subscription_release_and_poll(self):
        for host in QUOTE_HOSTS:
            root = "https://" + host
            for referrer in ("https://trader.degiro.nl", "https://trader.degiro.nl/",
                             "https://trader.degiro.nl/trader/#/portfolio/assets"):
                self.assert_allowed("POST", root + "/CORS/request_session?" + SESSION_QUERY,
                                    body=json.dumps({"referrer": referrer}).encode())
            self.assert_allowed("GET", root + "/CORS/session-123")
            self.assert_allowed("POST", root + "/CORS/session-123", body=control_body(
                "req(123.LastPrice);req(123.LastDate);req(123.LastTime);"
                "req(EURUSD.LastPrice);req(ABC|DEF.BidPrice);rel(123.LastPrice);"
            ))

    def test_session_requests_are_bounded_and_do_not_accept_arbitrary_urls_or_fields(self):
        url = QUOTE_ROOT + "/CORS/request_session?" + SESSION_QUERY
        for body in (
            b"{}", b'[]', b'[["referrer","https://trader.degiro.nl/trader/"]]',
            b'{"referrer":"https://evil.example/"}',
            b'{"referrer":"https://trader.degiro.nl/trader/","command":"order"}',
            b'{"referrer":"https://trader.degiro.nl/trader/","referrer":"https://evil.example/"}',
            b'{"referrer":"https://user@trader.degiro.nl/trader/"}',
            b'{"referrer":"https://trader.degiro.nl/trader/?sessionId=secret"}',
            b'{"referrer":"https://trader.degiro.nl/trading/secure/v5/order"}',
            b'{"referrer":"https://trader.degiro.nl/trader/#' + b"x" * 257 + b'"}',
        ):
            with self.subTest(body=body[:80]):
                self.assert_denied("POST", url, body=body)
        for query in (
            "version=1.0.20201211", SESSION_QUERY + "&redirect=evil", SESSION_QUERY + "&userToken=1",
            "version=other&userToken=12345", "version=1.0.20201211&userToken=abc",
            "version=1.0.20201211&userToken=" + "1" * 33,
        ):
            self.assert_denied("POST", QUOTE_ROOT + "/CORS/request_session?" + query, body=SESSION_BODY)
        self.assert_denied("GET", url)
        self.assert_denied("DELETE", url, body=SESSION_BODY)

    def test_control_data_only_accepts_complete_req_and_rel_commands(self):
        url = QUOTE_ROOT + "/CORS/session-123"
        for commands in (
            "", "req(123.LastPrice)", "req(123.LastPrice);garbage", "order(123.LastPrice);",
            "req(123.LastPrice);order(BUY,100);", "req(123.LastPrice);\n", "req(123);",
            "req(https://evil.example/LastPrice);", "req(123.LastPrice);delete();",
            "req(123.LastPrice);" * 2001, "req(123.LastPrice,order());", "req(123.LastPrice%29);",
        ):
            with self.subTest(commands=commands[:100]):
                self.assert_denied("POST", url, body=control_body(commands))
        for body in (
            b'{"controlData":"req(123.LastPrice);","order":"buy"}',
            b'{"controlData":"req(123.LastPrice);","controlData":"order();"}',
            b'{"controlData":"req%28123.LastPrice%29;"}',
            b'{"controlData":"req(ABC%257CDEF.LastPrice);"}',
            b'{"controlData":"req(ABC%7cDEF.LastPrice);"}',
            b'{"controlData":"req(ABC%ZZDEF.LastPrice);"}',
            b'{"controlData":123}',
        ):
            self.assert_denied("POST", url, body=body)

    def test_quote_host_does_not_open_other_routes_protocols_or_hosts(self):
        for method, path in (
            ("POST", "/trading/secure/v5/order"), ("GET", "/trader/"),
            ("GET", "/CORS/session/extra"), ("GET", "/CORS/%73ession"),
            ("GET", "/CORS/session?redirect=evil"), ("PUT", "/CORS/session"),
            ("DELETE", "/CORS/session"), ("HEAD", "/CORS/session"),
        ):
            self.assert_denied(method, QUOTE_ROOT + path)
        for root in (
            "https://evil.quotecast.vwdservices.com", "https://degiro.quotecast.vwdservices.com.evil.example",
            "http://degiro.quotecast.vwdservices.com", QUOTE_ROOT + ":444",
            "https://user@degiro.quotecast.vwdservices.com", QUOTE_ROOT + ".",
        ):
            self.assert_denied("GET", root + "/CORS/session")
        self.assert_denied("GET", QUOTE_ROOT + "/CORS/session", headers={"Upgrade": "websocket"})
        self.assert_denied("GET", QUOTE_ROOT + "/CORS/session", body=b"unexpected")

    def test_connect_and_credential_stripping_apply_to_quote_hosts(self):
        addon = PolicyAddon()
        headers = {
            LOGIN_CAPABILITY_HEADER: "unused", "Authorization": "Bearer secret",
            "Cookie": "JSESSIONID=secret", "Proxy-Authorization": "secret", "Origin": "https://trader.degiro.nl",
        }
        request = SimpleNamespace(
            method="GET", pretty_url=QUOTE_ROOT + "/CORS/session-123", raw_content=b"", headers=headers,
            host="degiro.quotecast.vwdservices.com", port=443,
        )
        flow = SimpleNamespace(request=request, metadata={}, response=None)
        addon.http_connect(flow)
        self.assertIsNone(flow.response)
        addon.request(flow)
        self.assertIsNone(flow.response)
        self.assertEqual(headers, {"Origin": "https://trader.degiro.nl"})
        response = object()
        request.host = "other.vwdservices.com"
        with patch("proxy.policy.http", SimpleNamespace(Response=SimpleNamespace(make=lambda *args: response))):
            addon.http_connect(flow)
        self.assertIs(flow.response, response)


if __name__ == "__main__":
    unittest.main()
