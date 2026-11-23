"""Financial snapshot regressions: complete reports, currencies and freshness."""
from copy import deepcopy
from datetime import datetime
from types import SimpleNamespace
import unittest
from unittest.mock import AsyncMock, patch
from urllib.parse import parse_qs, urlsplit

from tasks.common import TaskError
from tasks.holdings import (
    PORTFOLIO_URL, _flatten_position, _portfolio_rows,
    fetch, normalize_holdings,
)
from tasks.valuation import apply_frontend, base_currency, number, timestamp


UPDATE = "https://trader.degiro.nl/trading/secure/v5/update/123;jsessionid=secret?portfolio=81&totalPortfolio=67&intAccount=123&sessionId=secret"
ACCOUNT = "https://trader.degiro.nl/trading/secure/v5/account/info/123;jsessionid=secret"


def position(**changes):
    return {
        "id": "100", "symbol": "TEST", "positionType": "PRODUCT", "size": 2,
        "breakEvenPrice": 90, "currency": "USD", "price": 110, "value": 200,
        **changes,
    }


def envelope(row=None):
    row = position() if row is None else row
    return {"portfolio": {"isAdded": True, "value": [{
        "id": row["id"], "name": "positionrow", "isAdded": True,
        "value": [{"name": key, "value": value, "isAdded": True} for key, value in row.items()],
    }]}}


def observation():
    return {
        "base_currency": "EUR", "int_account": "123", "changed_at": 1, "totals_at": 2,
        "positions": {"100": {
            **position(price=115, value=210), "lastUpdate": "2026-11-16T16:18:00Z",
            "productInfo": {"id": "100", "currency": "USD", "closePrice": 110,
                            "closePriceDate": "2026-11-13", "contractSize": 1},
        }},
        "totals": {"portfolio": 210, "totalCash": 12.50, "total": 222.50,
                   "totalPl": 32.17, "todayPl": -1.23, "availableToSpend": 50.25},
    }


class ValuationNormalizationTest(unittest.TestCase):
    def test_flat_numeric_value_is_preserved_and_nested_transport_name_is_discarded(self):
        self.assertEqual(_flatten_position(position())["value"], 200)
        row = _flatten_position(envelope()["portfolio"]["value"][0])
        self.assertEqual(row["value"], 200)
        self.assertNotIn("name", row)
        self.assertEqual(normalize_holdings({"portfolio": [position()]}, base="EUR")[0]["market_value"], 200)

    def test_price_currency_and_account_value_currency_are_distinct(self):
        row = normalize_holdings(envelope(), base="EUR")[0]
        self.assertEqual((row["currency"], row["value_currency"]), ("USD", "EUR"))
        self.assertEqual((row["avg_price"], row["price"], row["market_value"]), (90, 110, 200))
        self.assertEqual(row["price_as_of"], "")
        self.assertEqual(row["valuation_source"], "broker_api")

    def test_explicit_empty_is_different_from_malformed_or_incremental(self):
        for payload in ({"portfolio": []}, {"portfolio": {"isAdded": True, "value": []}}):
            self.assertEqual(normalize_holdings(payload), [])
        for payload in ({}, [], None, {"data": []}, {"portfolio": None}, {"portfolio": {}},
                        {"portfolio": {"isAdded": False, "value": []}}, {"portfolio": [None]},
                        {"portfolio": "[]"}):
            with self.subTest(payload=payload), self.assertRaises(TaskError):
                _portfolio_rows(payload)

    def test_invalid_required_numbers_and_duplicate_positions_fail(self):
        for changes in ({"size": None}, {"size": "nan"}, {"size": True}, {"value": None},
                        {"value": "broken"}, {"value": float("inf")}, {"price": float("nan")},
                        {"currency": ""}, {"id": ""}, {"positionType": "UNKNOWN"}):
            with self.subTest(changes=changes), self.assertRaises(TaskError):
                normalize_holdings(envelope(position(**changes)), base="EUR")
        with self.assertRaises(TaskError):
            normalize_holdings({"portfolio": [position(), position()]}, base="EUR")

    def test_full_snapshot_rejects_incremental_or_removed_transport_rows(self):
        for flags in ({"isAdded": False}, {"isRemoved": True}):
            payload = envelope()
            payload["portfolio"]["value"][0].update(flags)
            with self.subTest(flags=flags), self.assertRaises(TaskError):
                normalize_holdings(payload, base="EUR")

    def test_cash_and_closed_positions_are_excluded_from_security_values(self):
        rows = [position(), position(id="EUR", positionType="CASH", size=12.50), position(id="closed", size=0)]
        self.assertEqual(len(normalize_holdings({"portfolio": rows}, base="EUR")), 1)

    def test_optional_missing_is_not_silently_zero_and_numeric_overflow_is_rejected(self):
        self.assertIsNone(number(None, "optional", optional=True))
        for invalid in ("", None, True, "NaN", "Infinity", "1e9999", {}, []):
            with self.subTest(invalid=invalid), self.assertRaises(TaskError):
                number(invalid, "amount")
        self.assertEqual(number(0, "amount"), 0)

    def test_timestamp_preserves_close_date_and_normalizes_real_quote_instant(self):
        self.assertEqual(timestamp("2026-11-13"), "2026-11-13")
        self.assertEqual(timestamp("2026-11-16T17:18:00+01:00"), "2026-11-16T16:18:00+00:00")
        for value in ("2026-02-30", "2026-11-16T16:18:00", "bad", 123):
            with self.subTest(value=value), self.assertRaises(TaskError):
                timestamp(value)

    def test_base_currency_must_not_conflict_with_account_bootstrap(self):
        captured = [(ACCOUNT, {"data": {"baseCurrency": "EUR"}})]
        self.assertEqual(base_currency(captured, {}), "EUR")
        self.assertEqual(base_currency(captured, {"base_currency": "EUR"}), "EUR")
        with self.assertRaises(TaskError):
            base_currency(captured, {"base_currency": "USD"})

    def test_frontend_calculation_replaces_value_and_preserves_original_input(self):
        rows = normalize_holdings(envelope(), base="EUR")
        before = deepcopy(rows)
        result = apply_frontend(rows, observation(), "EUR")
        self.assertEqual(rows, before)
        self.assertEqual(result["holdings"][0]["market_value"], 210)
        self.assertEqual(result["holdings"][0]["price"], 115)
        self.assertEqual(result["holdings"][0]["valuation_source"], "broker_quote")
        self.assertEqual(result["valuation"]["total_pl"], 32.17)
        self.assertEqual(result["valuation"]["cash_balance"], 12.50)
        self.assertEqual(result["valuation"]["reconciliation_difference"], 0)

    def test_frontend_requires_exact_ids_quantities_and_product_identity(self):
        rows = normalize_holdings(envelope(), base="EUR")
        cases = []
        data = observation(); data["positions"] = {}; cases.append(data)
        data = observation(); data["positions"]["101"] = deepcopy(data["positions"]["100"]); cases.append(data)
        data = observation(); data["positions"]["100"]["size"] = 3; cases.append(data)
        data = observation(); data["positions"]["100"]["productInfo"]["id"] = "101"; cases.append(data)
        data = observation(); del data["positions"]["100"]["productInfo"]; cases.append(data)
        for data in cases:
            with self.subTest(data=data):
                self.assertIsNone(apply_frontend(rows, data, "EUR"))

    def test_currency_disagreement_and_nonfinite_frontend_values_fail(self):
        rows = normalize_holdings(envelope(), base="EUR")
        data = observation(); data["positions"]["100"]["productInfo"]["currency"] = "GBP"
        with self.assertRaisesRegex(TaskError, "currenc"):
            apply_frontend(rows, data, "EUR")
        for field in ("price", "value", "size"):
            data = observation(); data["positions"]["100"][field] = float("nan")
            with self.subTest(field=field), self.assertRaises(TaskError):
                apply_frontend(rows, data, "EUR")

    def test_missing_or_inconsistent_totals_cannot_be_published(self):
        rows = normalize_holdings(envelope(), base="EUR")
        for key in ("portfolio", "total", "totalCash", "totalPl", "todayPl"):
            data = observation(); del data["totals"][key]
            with self.subTest(key=key):
                self.assertIsNone(apply_frontend(rows, data, "EUR"))
        data = observation(); data["totals"]["total"] += 1
        self.assertIsNone(apply_frontend(rows, data, "EUR"))
        data = observation(); del data["totals"]["availableToSpend"]
        self.assertIsNone(apply_frontend(rows, data, "EUR")["valuation"]["available_to_trade"])

    def test_closed_positions_do_not_invalidate_frontend_account_profit(self):
        rows = normalize_holdings(envelope(), base="EUR")
        data = observation(); data["positions"]["closed"] = {"size": 0, "positionType": "PRODUCT"}
        self.assertEqual(apply_frontend(rows, data, "EUR")["valuation"]["total_pl"], 32.17)

    def test_zero_crypto_cash_and_null_adjustments_follow_broker_fallbacks(self):
        rows = normalize_holdings(envelope(), base="EUR")
        data = observation()
        data["totals"].update(cryptoTotalCash=0, cashFundCompensation=None,
                              cashFundCompensationPending=None)
        data["positions"]["100"]["portfolioValueCorrection"] = None
        result = apply_frontend(rows, data, "EUR")
        self.assertEqual(result["valuation"]["cash_balance"], 12.50)
        self.assertEqual(result["valuation"]["account_value"], 222.50)

    def test_stale_totals_are_rejected_but_explicit_adjustments_reconcile(self):
        rows = normalize_holdings(envelope(), base="EUR")
        data = observation()
        data["totals"]["portfolio"] = 200
        data["totals"]["total"] = 212.50
        self.assertIsNone(apply_frontend(rows, data, "EUR"))
        data = observation()
        data["positions"]["100"]["portfolioValueCorrection"] = 2
        data["totals"].update(portfolio=215, total=227.50, cashFundCompensation=1,
                              cashFundCompensationPending=2, cashFundCompensationCurrency="EUR")
        result = apply_frontend(rows, data, "EUR")
        self.assertEqual(result["valuation"]["reconciliation_difference"], 5)
        self.assertIn("adjustment", result["valuation"]["warning"])
        data["totals"]["cashFundCompensationCurrency"] = "USD"
        self.assertIsNone(apply_frontend(rows, data, "EUR"))

    def test_serialized_midnight_close_is_not_mistaken_for_a_quote(self):
        rows = normalize_holdings(envelope(), base="EUR")
        for close in ("2026-11-13", "2026-11-13T00:00:00.000Z"):
            data = observation(); row = data["positions"]["100"]
            row["productInfo"]["closePriceDate"] = close
            row["lastUpdate"] = "2026-11-13T00:00:00.000Z"
            with self.subTest(close=close):
                result = apply_frontend(rows, data, "EUR")
                self.assertEqual(result["valuation"]["price_status"], "closing")
                self.assertEqual(result["holdings"][0]["valuation_source"], "broker_close")

    def test_native_wall_clock_quote_preserves_date_without_claiming_a_utc_instant(self):
        rows = normalize_holdings(envelope(), base="EUR")
        # Native LastTime parsing depends on the runtime timezone and today's
        # offset, even for a quote on a different side of a DST boundary.
        for reported in ("2026-11-16T19:18:58.000Z", "2026-07-01T18:18:58.000Z"):
            data = observation()
            data["positions"]["100"]["lastUpdate"] = reported
            data["positions"]["100"]["productInfo"]["closePriceDate"] = "2026-06-26"
            result = apply_frontend(rows, data, "EUR")
            with self.subTest(reported=reported):
                self.assertEqual(result["holdings"][0]["price_as_of"], reported[:10])
                self.assertEqual(result["valuation"]["price_status"], "quoted")
                self.assertIn("precise quote times are unavailable", result["valuation"]["warning"])


class HoldingsFetchIntegrityTest(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.page = SimpleNamespace(
            url=PORTFOLIO_URL, close=AsyncMock(), goto=AsyncMock(), add_init_script=AsyncMock(),
            evaluate=AsyncMock(return_value={}), wait_for_timeout=AsyncMock(),
        )
        self.response = SimpleNamespace(ok=True, status=200, json=AsyncMock(return_value=envelope()))
        self.context = SimpleNamespace(request=SimpleNamespace(get=AsyncMock(return_value=self.response)))
        self.captured = [(ACCOUNT, {"data": {"baseCurrency": "EUR"}}), (UPDATE, envelope())]
        self.get_page = patch("tasks.holdings.get_page", AsyncMock(return_value=self.page)).start()
        patch("tasks.holdings.collect_json_responses", AsyncMock(return_value=self.captured)).start()
        patch("tasks.holdings.resolve_products", AsyncMock(return_value={})).start()
        self.addCleanup(patch.stopall)

    async def test_explicit_replay_resets_incremental_cursors_and_does_not_fake_metrics(self):
        result = await fetch(self.context)
        query = parse_qs(urlsplit(self.context.request.get.call_args.args[0]).query)
        self.assertEqual(query["portfolio"], ["0"])
        self.assertEqual(query["totalPortfolio"], ["0"])
        self.assertEqual(result["holdings"][0]["market_value"], 200)
        self.assertEqual(result["valuation"]["price_status"], "unavailable")
        self.assertIsNone(result["valuation"].get("total_pl"))
        self.assertIsNone(result["valuation"].get("account_value"))
        self.assertIsNotNone(datetime.fromisoformat(result["valuation"]["fetched_at"]).tzinfo)
        self.page.close.assert_awaited_once()

    async def test_failed_replay_never_falls_back_to_captured_portfolio(self):
        self.response.ok, self.response.status = False, 503
        with self.assertRaisesRegex(TaskError, "503"):
            await fetch(self.context)
        self.page.close.assert_awaited_once()

    async def test_malformed_and_incremental_replays_fail_before_publishing(self):
        for payload in ({"portfolio": {}}, {"portfolio": {"isAdded": False, "value": []}}):
            self.response.json.return_value = payload
            with self.subTest(payload=payload), self.assertRaises(TaskError):
                await fetch(self.context)

    async def test_canonical_values_require_known_base_currency(self):
        self.captured[:] = [(UPDATE, envelope())]
        with self.assertRaisesRegex(TaskError, "base currency"):
            await fetch(self.context)

    async def test_other_account_observation_cannot_be_used(self):
        data = observation(); data["int_account"] = "456"
        self.page.evaluate.return_value = data
        with self.assertRaisesRegex(TaskError, "different account"):
            await fetch(self.context)

    async def test_quoted_frontend_values_are_saved_and_page_is_closed(self):
        self.page.evaluate.return_value = observation()
        result = await fetch(self.context)
        self.assertEqual(result["holdings"][0]["market_value"], 210)
        self.assertEqual(result["valuation"]["price_status"], "quoted")
        self.page.evaluate.assert_awaited_with("window.__degitideReadPortfolio()", isolated_context=False)
        self.page.close.assert_awaited_once()

    async def test_fresh_worker_snapshot_replaces_stale_positions_and_reconciles_compensation(self):
        fresh = observation()
        fresh["totals"].update(portfolio=210.06, total=222.56, cashFundCompensation=.06,
                               cashFundCompensationCurrency="EUR")
        stale = deepcopy(fresh)
        stale["positions"]["100"].update(price=110, value=200, lastUpdate="2026-11-13T00:00:00Z")
        # Initial passive observation, first resource pair before settlement,
        # then a fresh pair. No position-change event is needed.
        self.page.evaluate.side_effect = [stale, stale, fresh]
        result = await fetch(self.context)
        self.assertEqual(result["holdings"][0]["market_value"], 210)
        self.assertEqual(result["valuation"]["portfolio_value"], 210.06)
        self.assertEqual(result["valuation"]["account_value"], 222.56)
        self.assertAlmostEqual(result["valuation"]["reconciliation_difference"], .06)
        self.assertEqual(result["valuation"]["price_status"], "quoted")
        self.assertEqual(self.page.evaluate.await_count, 3)
        self.page.wait_for_timeout.assert_awaited_once_with(500)

    async def test_incomplete_fresh_snapshot_never_uses_earlier_matching_passive_values(self):
        initial = observation()
        timed_out = {**initial, "snapshot_error": "timeout"}
        self.page.evaluate.side_effect = [initial, timed_out]
        with patch("tasks.holdings.asyncio.get_running_loop") as loop:
            loop.return_value.time.side_effect = [0, 13]
            result = await fetch(self.context)
        self.assertEqual(result["valuation"]["source"], "broker_api")
        self.assertEqual(result["valuation"]["price_status"], "unavailable")
        self.assertIsNone(result["valuation"].get("account_value"))

    async def test_unenriched_broker_positions_report_a_safe_diagnostic(self):
        data = observation()
        data["int_account"] = "123"
        data["positions"]["100"].update(price=0, lastUpdate=None)
        data["positions"]["100"].pop("productInfo")
        data["totals"] = {"totalCash": 12.50}
        data["sessionId"] = "private-session-must-not-be-logged"
        self.page.evaluate.return_value = data
        with patch("tasks.holdings.asyncio.get_running_loop") as loop:
            loop.return_value.time.side_effect = [0, 13]
            with self.assertLogs("degitide-worker.holdings", level="WARNING") as logs:
                result = await fetch(self.context)
        self.assertEqual(result["valuation"]["price_status"], "unavailable")
        self.assertIsNone(result["valuation"].get("portfolio_value"))
        text = " ".join(logs.output)
        self.assertIn("reason=missing_calculated_totals", text)
        self.assertIn("enriched_positions=0 timed_positions=0", text)
        for private in ("private-session", "secret", "12.50", "123", "https://"):
            self.assertNotIn(private, text)

    async def test_empty_account_is_valid_and_network_errors_do_not_leak_tokens(self):
        self.response.json.return_value = {"portfolio": {"isAdded": True, "value": []}}
        self.assertEqual((await fetch(self.context))["holdings"], [])
        self.context.request.get.side_effect = RuntimeError(UPDATE)
        with self.assertRaises(TaskError) as caught:
            await fetch(self.context)
        self.assertNotIn("secret", str(caught.exception))
        self.assertNotIn("https://", str(caught.exception))


if __name__ == "__main__":
    unittest.main()
