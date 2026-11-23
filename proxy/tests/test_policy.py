from __future__ import annotations

import json
import unittest
from unittest.mock import patch

from proxy.policy import (
    EGRESS_DENIED_HEADER,
    LOGIN_CAPABILITY_HEADER,
    PolicyAddon,
    _login_capability_is_valid,
    decide,
)


BASE_QUERY = "fromDate=01%2F08%2F2026&toDate=25%2F08%2F2026&intAccount=7&sessionId=abc"


class PolicyTests(unittest.TestCase):
    def test_exchange_dictionary_is_an_exact_read_only_route(self):
        for path in ("/product_search/config/dictionary/", "/productsearch/config/dictionary"):
            url = "https://trader.degiro.nl" + path + "?intAccount=7&sessionId=abc"
            self.assert_allowed("GET", url)
            self.assert_denied("POST", url)
            self.assert_denied("GET", url + "&redirect=evil")
            self.assert_denied("GET", url.replace("trader.degiro.nl", "evil.example"))
        self.assert_denied("GET", "https://trader.degiro.nl/productsearch/config/dictionary/other?intAccount=7&sessionId=abc")
        self.assert_denied("GET", "https://trader.degiro.nl/productsearch/config/dictionary//?intAccount=7&sessionId=abc")

    def test_secure_v1_exchange_dictionary_is_an_exact_read_only_route(self):
        base = "https://trader.degiro.nl/productsearch/secure/v1/config/dictionary"
        url = base + "?intAccount=7&sessionId=abc"
        self.assertEqual(decide("GET", url), (True, "exchange-dictionary", ""))
        for query in ("", "?intAccount=7", "?sessionId=abc", "?intAccount=7&sessionId=abc&redirect=evil",
                      "?intAccount=7&sessionId=abc&sessionId=def"):
            decision = decide("GET", base + query)
            self.assertFalse(decision.allowed, decision)
            self.assertEqual(decision.route, "exchange-dictionary")
        for method in ("POST", "HEAD", "PUT", "DELETE"):
            self.assert_denied(method, url)
        self.assert_denied("GET", url.replace("trader.degiro.nl", "evil.example"))
        for suffix in ("/", "/other", "//", "%2F", "%2Fother", "/../dictionary"):
            self.assert_denied("GET", base + suffix + "?intAccount=7&sessionId=abc")
        for path in ("/productsearch/secure/v1/config/dictionary2", "/productsearch/secure/v2/config/dictionary",
                     "/product_search/secure/v1/config/dictionary", "/productsearch/secure/v1/config",
                     "/productsearch/secure/v1//config/dictionary", "/ProductSearch/secure/v1/config/dictionary"):
            self.assert_denied("GET", "https://trader.degiro.nl" + path + "?intAccount=7&sessionId=abc")

    def assert_allowed(self, method: str, url: str, **kwargs) -> None:
        decision = decide(method, url, **kwargs)
        self.assertTrue(decision.allowed, decision)

    def assert_denied(self, method: str, url: str, **kwargs) -> None:
        decision = decide(method, url, **kwargs)
        self.assertFalse(decision.allowed, decision)

    def test_allows_static_shell_without_external_assets(self):
        self.assert_allowed("GET", "https://trader.degiro.nl/login/")
        self.assert_allowed(
            "GET", "https://trader.degiro.nl/login?redirectUrl=%2Ftrader%2F"
        )
        self.assert_allowed("GET", "https://trader.degiro.nl/trader/scripts/app.abc.js")
        self.assert_allowed(
            "GET",
            "https://trader.degiro.nl/login/secure/config?intAccount=7&sessionId=abc",
        )
        self.assert_denied(
            "GET",
            "https://trader.degiro.nl/login/secure/config?redirect=https://evil.example",
        )
        self.assert_denied("GET", "https://consent.cookiebot.eu/uc.js")

    def test_allows_only_bounded_translation_queries(self):
        self.assert_allowed(
            "GET",
            "https://trader.degiro.nl/translations/?language=en&country=IE&modules=login,shared_ui",
        )
        self.assert_denied("GET", "https://trader.degiro.nl/translations/")
        self.assert_denied(
            "GET",
            "https://trader.degiro.nl/translations/?language=en&country=IE&modules=login&redirect=evil",
        )
        self.assert_denied(
            "GET",
            "https://trader.degiro.nl/translations/?language=en&country=IE&modules=login,../admin",
        )

    def test_login_posts_only_work_in_explicit_login_mode(self):
        url = "https://trader.degiro.nl/login/secure/login/totp"
        self.assert_denied("POST", url, body=b"{}")
        self.assert_allowed("POST", url, body=b"{}", login_enabled=True)
        self.assert_denied(
            "POST",
            url,
            body=b"x" * (64 * 1024 + 1),
            login_enabled=True,
        )

    def test_login_capability_requires_one_exact_secret(self):
        self.assertTrue(_login_capability_is_valid(["a" * 32], "a" * 32))
        self.assertFalse(_login_capability_is_valid([], "a" * 32))
        self.assertFalse(_login_capability_is_valid(["wrong"], "a" * 32))
        self.assertFalse(
            _login_capability_is_valid(["a" * 32, "a" * 32], "a" * 32)
        )
        self.assertFalse(_login_capability_is_valid(["a" * 32], ""))

    def test_valid_capability_is_consumed_and_opens_only_login_route(self):
        class Headers(dict):
            def get_all(self, name):
                return [self[name]] if name in self else []

        class Request:
            method = "POST"
            pretty_url = "https://trader.degiro.nl/login/secure/login/totp"
            raw_content = b"{}"
            headers = Headers({LOGIN_CAPABILITY_HEADER: "a" * 32})

        class Flow:
            request = Request()
            metadata = {}
            response = None

        addon = PolicyAddon()
        addon.login_enabled = False
        addon.login_capability = "a" * 32
        flow = Flow()
        addon.request(flow)

        self.assertIsNone(flow.response)
        self.assertNotIn(LOGIN_CAPABILITY_HEADER, flow.request.headers)

    def test_proxy_denials_are_marked_as_generated_by_the_proxy(self):
        class Headers(dict):
            def get_all(self, name):
                return [self[name]] if name in self else []

        class Request:
            method = "GET"
            pretty_url = "https://trader.degiro.nl/pa/secure/crypto?intAccount=7&sessionId=abc"
            raw_content = b""
            headers = Headers()
            host = "evil.example"
            port = 443

        class Flow:
            request = Request()
            metadata = {}
            response = None

        addon = PolicyAddon()
        addon.login_enabled = False
        addon.login_capability = "a" * 32
        for hook in (addon.request, addon.http_connect):
            with self.subTest(hook=hook.__name__), patch("proxy.policy.http") as http:
                flow = Flow()
                flow.metadata = {}
                hook(flow)
                status, _, headers = http.Response.make.call_args.args
                self.assertEqual(status, 403)
                self.assertEqual(headers[EGRESS_DENIED_HEADER], "denied")
                self.assertEqual(headers["Content-Type"], "application/json")

        with patch("proxy.policy.http") as http:
            flow = Flow()
            flow.request = Request()
            flow.request.pretty_url = "https://trader.degiro.nl/productsearch/secure/v1/config/dictionary?intAccount=7&sessionId=abc"
            addon.request(flow)
            http.Response.make.assert_not_called()

    def test_allows_transaction_and_csv_reports_with_strict_queries(self):
        self.assert_allowed(
            "GET",
            f"https://trader.degiro.nl/portfolio-reports/secure/v4/transactions?{BASE_QUERY}&groupTransactionsByOrder=false",
        )
        self.assert_allowed(
            "GET",
            f"https://trader.degiro.nl/portfolio-reports/secure/v3/positionReport/csv?{BASE_QUERY}&country=IE&lang=en",
        )
        self.assert_denied(
            "GET",
            f"https://trader.degiro.nl/portfolio-reports/secure/v4/transactions?{BASE_QUERY}&redirect=https://evil.example",
        )

    def test_allows_holdings_and_bounded_product_lookup(self):
        self.assert_allowed(
            "GET",
            "https://trader.degiro.nl/trading/secure/v5/update/7;jsessionid=abc?portfolio=0&totalPortfolio=0&intAccount=7&sessionId=abc",
        )
        self.assert_allowed(
            "POST",
            "https://trader.degiro.nl/productsearch/secure/v1/products/info?intAccount=7&sessionId=abc",
            body=b'["123",456]',
        )
        self.assert_denied(
            "POST",
            "https://trader.degiro.nl/productsearch/secure/v1/products/info?intAccount=7&sessionId=abc",
            body=b'{"url":"https://evil.example"}',
        )
        self.assert_denied(
            "POST",
            "https://trader.degiro.nl/product_search/secure/v5/products/info?intAccount=7&sessionId=abc",
            body=b'["123"]',
        )

    def test_allows_numeric_account_info_path_only(self):
        self.assert_allowed(
            "GET",
            "https://trader.degiro.nl/trading/secure/v5/account/info/7;jsessionid=abc",
        )
        self.assert_allowed(
            "GET",
            "https://trader.degiro.nl/trading/secure/v5/account/info?sessionId=abc",
        )
        self.assert_denied(
            "GET",
            "https://trader.degiro.nl/trading/secure/v5/account/info/current",
        )
        self.assert_denied(
            "GET",
            "https://trader.degiro.nl/trading/secure/v5/account/info/7;jsessionid=abc%2F..",
        )
        self.assert_denied(
            "POST",
            "https://trader.degiro.nl/trading/secure/v5/account/info/7",
        )

    def test_product_lookup_accepts_native_cash_ids_without_widening_the_route(self):
        url = "https://trader.degiro.nl/productsearch/secure/v1/products/info?intAccount=7&sessionId=abc"
        self.assert_allowed("POST", url, body=b'["123",456,"EUR","USD","FLATEX_EUR"]')
        self.assert_allowed("POST", url, body=json.dumps(["EUR"] * 500).encode())
        for invalid in (True, False, None, -1, 10**32, 1.5, "", "eur", "Euro", "EURO",
                        "JOINT_CASH_EUR", "FLATEX_eur", "FLATEX_EURO", "FLATEX_EUR/../orders",
                        "EUR?order=BUY", "https://example.com", "１２３", "1" * 33,
                        {"id": "EUR"}, ["EUR"]):
            with self.subTest(invalid=invalid):
                self.assert_denied("POST", url, body=json.dumps(["123", invalid]).encode())
        self.assert_denied("POST", url, body=json.dumps(["EUR"] * 501).encode())
        for malformed in (b'["EUR",', b'["\xff"]', b"[" + b"9" * 5000 + b"]",
                          b"[" * 2000 + b"0" + b"]" * 2000):
            self.assert_denied("POST", url, body=malformed)
        for method in ("PUT", "DELETE", "GET"):
            self.assert_denied(method, url, body=b'["EUR"]')
        self.assert_denied("POST", url.replace("/products/info", "/order"), body=b'["EUR"]')

    def test_allows_only_exact_read_only_bootstrap_settings(self):
        self.assert_allowed(
            "GET",
            "https://trader.degiro.nl/settings/secure/v1/web?intAccount=7&sessionId=abc",
        )
        self.assert_allowed(
            "GET",
            "https://trader.degiro.nl/settings/secure/v1/user?intAccount=7&sessionId=abc",
        )
        self.assert_allowed(
            "GET",
            "https://trader.degiro.nl/settings/secure/v1/beta-test?intAccount=7&sessionId=abc",
        )
        self.assert_allowed(
            "GET",
            "https://trader.degiro.nl/trading/secure/settings/showFxConversionTab;jsessionid=abc?sessionId=abc",
        )
        self.assert_denied(
            "GET",
            "https://trader.degiro.nl/settings/secure/v1/web?intAccount=7&sessionId=abc&redirect=evil",
        )
        self.assert_denied(
            "POST", "https://trader.degiro.nl/settings/secure/v1/user"
        )
        self.assert_denied(
            "GET", "https://trader.degiro.nl/settings/secure/v1/user"
        )

    def test_blocks_every_known_order_mutation_and_unknown_path(self):
        for method, path in (
            ("POST", "/trading/secure/v5/checkOrder"),
            ("POST", "/trading/secure/v5/order/confirmation;jsessionid=abc"),
            ("PUT", "/trading/secure/v5/order/123;jsessionid=abc"),
            ("DELETE", "/trading/secure/v5/order/123;jsessionid=abc"),
            ("GET", "/trading/secure/logout"),
            ("POST", "/trading/secure/logout"),
        ):
            self.assert_denied(method, f"https://trader.degiro.nl{path}?intAccount=7&sessionId=abc")
        self.assert_denied("GET", "https://trader.degiro.nl/pa/secure/personal-details")

    def test_blocks_host_scheme_port_and_path_bypasses(self):
        self.assert_denied("GET", "https://evil.example/portfolio-reports/secure/v4/transactions")
        self.assert_denied("GET", "http://trader.degiro.nl/login/")
        self.assert_denied("GET", "https://trader.degiro.nl:444/login/")
        self.assert_denied("GET", "https://trader.degiro.nl./login/")
        self.assert_denied("GET", "https://user@trader.degiro.nl/login/")
        self.assert_denied("GET", "https://trader.degiro.nl/login/#fragment")
        self.assert_denied("GET", "https://trader.degiro.nl/trader/../trading/secure/v5/order")
        self.assert_denied(
            "GET",
            "https://trader.degiro.nl/trader/%252e%252e/trading/secure/v5/order",
        )
        self.assert_denied("GET", "https://trader.degiro.nl.evil.example/login/")

    def test_blocks_tunnels_upgrades_and_bodies_on_safe_methods(self):
        url = "https://trader.degiro.nl/trader/scripts/app.js"
        self.assert_denied("CONNECT", url)
        self.assert_denied("GET", url, headers={"Upgrade": "websocket"})
        self.assert_denied("GET", url, headers={"Connection": "keep-alive, Upgrade"})
        self.assert_denied("GET", url, body=b"unexpected")


if __name__ == "__main__":
    unittest.main()
