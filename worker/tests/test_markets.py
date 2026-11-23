from datetime import datetime, timezone
from types import SimpleNamespace
from unittest import IsolatedAsyncioTestCase, TestCase
from unittest.mock import AsyncMock

from tasks.markets import build_schedule, exchange_map, resolve_exchanges
from tasks.products import product_map


def at(value):
    return datetime.fromisoformat(value).replace(tzinfo=timezone.utc)


class MarketCalendarTest(TestCase):
    def schedule(self, mic, now="2026-11-18T12:00:00"):
        holdings = [{"product_id": "1", "symbol": "VWCE"}]
        products = {"1": {"exchange_id": "194", "product_type": "131"}}
        plan = build_schedule(holdings, products, {"194": {"mic": mic, "name": "Listing venue"}}, at(now))
        self.assertEqual(plan["unknown_holdings"], 0)
        self.assertEqual(holdings[0]["exchange_mic"], mic)
        return plan["markets"][0]["sessions"]

    def test_product_id_to_listing_mic_not_ticker_or_underlying(self):
        products = product_map({"data": {"1": {"symbol": "VWCE", "exchangeId": 99, "productTypeId": 131}}})
        exchanges = exchange_map({"exchanges": [{"id": 99, "micCode": "XLON", "name": "London"}]})
        holdings = [{"product_id": "1", "symbol": "VWCE", "currency": "EUR"}]
        plan = build_schedule(holdings, products, exchanges, at("2026-11-18T12:00:00"))
        self.assertEqual(plan["markets"][0]["mic"], "XLON")
        self.assertEqual(holdings[0]["exchange_name"], "London")

    def test_xetra_uses_core_session_and_european_dst(self):
        for day, start, end in [("2026-03-27", "08:00", "16:30"), ("2026-03-30", "07:00", "15:30")]:
            rows = self.schedule("XETR", "2026-03-26T12:00:00")
            row = next(row for row in rows if row["open"].startswith(day))
            self.assertEqual(row["open"][11:16], start)
            self.assertEqual(row["close"][11:16], end)

    def test_us_holiday_and_early_close(self):
        rows = self.schedule("XNYS")
        self.assertFalse(any(row["open"].startswith("2026-11-26") for row in rows))
        friday = next(row for row in rows if row["open"].startswith("2026-11-27"))
        self.assertEqual(friday["close"], "2026-11-27T18:00:00+00:00")

    def test_lunch_break_and_weekends(self):
        rows = self.schedule("XHKG")
        self.assertTrue(all(datetime.fromisoformat(row["open"]).weekday() < 5 for row in rows))
        row = next(row for row in rows if row["open"].startswith("2026-11-18"))
        self.assertEqual(row["break_start"], "2026-11-18T04:00:00+00:00")
        self.assertEqual(row["break_end"], "2026-11-18T05:00:00+00:00")

    def test_unknown_missing_and_derivative_venues_fall_back(self):
        holdings = [{"product_id": str(i), "symbol": "VWCE", "currency": "EUR"} for i in range(4)]
        products = {"1": {"exchange_mic": "ZZZZ", "product_type": "131"},
                    "2": {"exchange_mic": "XETR", "product_type": "8"},
                    "3": {"exchange_mic": "XETR"}}
        plan = build_schedule(holdings, products, {}, at("2026-11-18T12:00:00"))
        self.assertEqual(plan["unknown_holdings"], 4)
        self.assertEqual(plan["markets"], [])
        self.assertEqual(holdings[0]["exchange_mic"], "")


class ExchangeLookupTest(IsolatedAsyncioTestCase):
    async def test_fetches_only_the_configured_read_only_dictionary(self):
        get = AsyncMock(return_value=SimpleNamespace(ok=True, json=AsyncMock(return_value={"exchanges": [{"id": 194, "micCode": "XETR", "name": "Xetra"}]})))
        context = SimpleNamespace(request=SimpleNamespace(get=get))
        captured = [("https://trader.degiro.nl/productsearch/secure/v1/products/info?intAccount=7&sessionId=test", {}),
                    ("https://trader.degiro.nl/login/secure/config", {"data": {"dictionaryUrl": "https://trader.degiro.nl/product_search/config/dictionary/"}})]
        self.assertEqual((await resolve_exchanges(context, captured))["194"]["mic"], "XETR")
        self.assertEqual(get.call_args.args[0], "https://trader.degiro.nl/product_search/config/dictionary/?intAccount=7&sessionId=test")
        for url in ("https://evil.example/product_search/config/dictionary/", "https://trader.degiro.nl/orders", "https://trader.degiro.nl/product_search/config/dictionary/?redirect=evil"):
            get.reset_mock()
            captured[-1][1]["data"]["dictionaryUrl"] = url
            self.assertEqual(await resolve_exchanges(context, captured), {})
            get.assert_not_awaited()

    async def test_captured_dictionary_needs_no_replay(self):
        context = SimpleNamespace(request=SimpleNamespace(get=AsyncMock()))
        result = await resolve_exchanges(context, [("https://trader.degiro.nl/productsearch/config/dictionary/", {"exchanges": [{"id": 194, "micCode": "XETR"}]})])
        self.assertEqual(result["194"]["mic"], "XETR")
        context.request.get.assert_not_awaited()

    async def test_captured_secure_v1_dictionary_schedules_listing(self):
        # Public DEGIRO clients document these dictionary fields; some rows lack a MIC.
        dictionary = {"countries": [], "productTypes": [], "exchanges": [
            {"id": 194, "code": "XET", "hiqAbbr": "XET", "country": "DE", "city": "Frankfurt", "micCode": "XETR", "name": "Xetra"},
            {"id": 663, "code": "TDG", "hiqAbbr": "TDG", "country": "DE", "city": "Hamburg", "name": "Tradegate"},
            {"id": 999, "code": "OTC", "micCode": "otc", "name": "Off-book"},
        ]}
        context = SimpleNamespace(request=SimpleNamespace(get=AsyncMock()))
        exchanges = await resolve_exchanges(context, [
            ("https://trader.degiro.nl/productsearch/secure/v1/config/dictionary?intAccount=7&sessionId=test", dictionary),
        ])
        context.request.get.assert_not_awaited()
        self.assertEqual(exchanges, {"194": {"mic": "XETR", "name": "Xetra"}})
        products = product_map({"data": [{"id": "1", "exchangeId": "194", "productTypeId": 131}]})
        holdings = [{"product_id": "1", "symbol": "VWCE"}]
        plan = build_schedule(holdings, products, exchanges, at("2026-11-18T12:00:00"))
        self.assertEqual((plan["unknown_holdings"], plan["markets"][0]["mic"]), (0, "XETR"))

    async def test_follows_a_configured_secure_v1_dictionary(self):
        payload = {"data": {"exchanges": [{"id": 194, "micCode": "XETR", "name": "Xetra"}]}}
        get = AsyncMock(return_value=SimpleNamespace(ok=True, json=AsyncMock(return_value=payload)))
        context = SimpleNamespace(request=SimpleNamespace(get=get))
        dictionary = "https://trader.degiro.nl/productsearch/secure/v1/config/dictionary"
        captured = [("https://trader.degiro.nl/productsearch/secure/v1/products/info?intAccount=7&sessionId=test", {}),
                    ("https://trader.degiro.nl/login/secure/config", {"data": {"dictionaryUrl": dictionary}})]
        self.assertEqual((await resolve_exchanges(context, captured))["194"]["mic"], "XETR")
        self.assertEqual(get.call_args.args[0], dictionary + "?intAccount=7&sessionId=test")
        for url in (dictionary + "/", dictionary + "/other", dictionary + "?intAccount=8",
                    "https://evil.example/productsearch/secure/v1/config/dictionary"):
            get.reset_mock()
            captured[-1][1]["data"]["dictionaryUrl"] = url
            self.assertEqual(await resolve_exchanges(context, captured), {})
            get.assert_not_awaited()
