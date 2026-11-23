"""Regression coverage for complete, lossless transaction imports."""

from datetime import date
from types import SimpleNamespace
import unittest
from unittest.mock import AsyncMock, patch
from urllib.parse import parse_qs, urlsplit

from tasks.common import TaskError
from tasks.transactions import (
    TRANSACTIONS_URL,
    _filter_date_range,
    _transaction_items,
    fetch,
    normalize_transactions,
)

REPORT = (
    "https://trader.degiro.nl/portfolio-reports/secure/v4/transactions"
    "?intAccount=123&sessionId=secret&fromDate=01%2F11%2F2026"
    "&toDate=16%2F11%2F2026&groupTransactionsByOrder=true"
)


def trade(**changes):
    return {
        "id": 42,
        "productId": 1234,
        "buysell": "B",
        "date": "2026-08-24T10:20:30+02:00",
        "quantity": 3,
        "price": 18.25,
        "currency": "EUR",
        "totalFeesInBaseCurrency": -2,
        **changes,
    }


class TransactionIntegrityTest(unittest.TestCase):
    def test_supported_envelopes_and_explicit_empty_reports(self):
        for envelope in (
            {"data": [trade()]},
            {"transactions": [trade()]},
            {"data": {"transactions": [trade()]}},
            [trade()],
        ):
            with self.subTest(envelope=envelope):
                self.assertEqual(normalize_transactions(envelope)[0]["id"], "42")
        for envelope in ({"data": []}, {"transactions": []}, []):
            self.assertEqual(normalize_transactions(envelope), [])

    def test_missing_or_product_metadata_envelopes_are_not_empty_reports(self):
        for payload in ({}, None, {"data": None}, {"data": {}}, {"data": {"1234": {"symbol": "TEST"}}}, {"transactions": "[]"}):
            with self.subTest(payload=payload), self.assertRaises(TaskError):
                normalize_transactions(payload)

    def test_pagination_and_count_mismatches_are_rejected(self):
        for extra in (
            {"hasMore": True}, {"has_more": True}, {"nextPage": 2},
            {"nextCursor": "next"}, {"totalCount": 2}, {"totalElements": 2},
        ):
            with self.subTest(extra=extra), self.assertRaises(TaskError):
                _transaction_items({"data": [trade()], **extra})
        self.assertEqual(len(_transaction_items({"data": [trade()], "totalCount": 1, "hasMore": False})), 1)

    def test_invalid_rows_fail_the_whole_report(self):
        bad_rows = [
            None,
            "not a transaction",
            trade(quantity=None), trade(quantity=0), trade(quantity=""),
            trade(quantity="nan"), trade(quantity=float("inf")), trade(quantity=True),
            trade(price="broken"), trade(price=None), trade(price=float("nan")),
            trade(totalFeesInBaseCurrency="unknown"), trade(totalFeesInBaseCurrency=float("inf")),
            trade(date="2026-02-30"), trade(date="2026-08-24garbage"), trade(date=None),
            trade(productId=""), trade(productId={}), trade(productId=True),
            trade(id=""), trade(id={}), trade(id=True),
            trade(buysell="UNKNOWN"), trade(currency="invalid"),
        ]
        for row in bad_rows:
            with self.subTest(row=row), self.assertRaises(TaskError):
                normalize_transactions({"data": [trade(id=1), row]})

    def test_missing_required_fields_are_not_coerced_to_zero(self):
        for key in ("productId", "buysell", "date", "quantity", "price"):
            row = trade()
            del row[key]
            with self.subTest(key=key), self.assertRaises(TaskError):
                normalize_transactions({"data": [row]})

    def test_fee_absence_and_zero_price_remain_valid(self):
        row = trade(price=0)
        del row["totalFeesInBaseCurrency"]
        result = normalize_transactions({"data": [row]})[0]
        self.assertEqual(result["fee"], 0)
        self.assertEqual(result["price"], 0)

    def test_order_id_does_not_collapse_separate_fills(self):
        first = trade(id=None, orderId="order-1", quantity=2)
        second = trade(id=None, orderId="order-1", quantity=1)
        result = normalize_transactions({"data": [first, second]})
        self.assertEqual(len({row["id"] for row in result}), 2)
        self.assertTrue(all(row["id"].startswith("derived-") for row in result))
        reordered = dict(reversed(list(first.items())))
        self.assertEqual(result[0]["id"], normalize_transactions([reordered])[0]["id"])

    def test_duplicate_execution_ids_and_ambiguous_identical_fills_fail(self):
        for rows in ([trade(), trade(quantity=1)], [trade(id=None), trade(id=None)]):
            with self.subTest(rows=rows), self.assertRaises(TaskError):
                normalize_transactions(rows)

    def test_product_enrichment_resolves_currency_and_preserves_raw_fields(self):
        row = trade(counterParty="XAMS", extraField={"retained": True})
        del row["currency"]
        result = normalize_transactions([row], {"1234": {"label": "TEST", "currency": "EUR"}})[0]
        self.assertEqual(result["symbol"], "TEST")
        self.assertEqual(result["currency"], "EUR")
        self.assertEqual(result["counterparty"], "XAMS")
        self.assertIn('"extraField":{"retained":true}', result["raw_json"])

    def test_invalid_dates_are_never_silently_discarded(self):
        with self.assertRaises(TaskError):
            _filter_date_range([{"date": "bad"}], date(2026, 1, 1), date(2026, 12, 31))


class TransactionFetchIntegrityTest(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.page = SimpleNamespace(url=TRANSACTIONS_URL, close=AsyncMock(), goto=AsyncMock())
        self.response = SimpleNamespace(ok=True, status=200, json=AsyncMock(return_value={"data": [trade()]}))
        self.context = SimpleNamespace(request=SimpleNamespace(get=AsyncMock(return_value=self.response)))
        self.captured = [(REPORT, {"data": []})]
        self.get_page = patch("tasks.transactions.get_page", AsyncMock(return_value=self.page)).start()
        self.capture = patch("tasks.transactions.collect_json_responses", AsyncMock(return_value=self.captured)).start()
        self.products = patch("tasks.transactions.resolve_products", AsyncMock(return_value={})).start()
        self.addCleanup(patch.stopall)

    async def test_replays_requested_full_window_not_captured_page_window(self):
        result = await fetch(self.context, "2000-01-01", "2026-11-16")
        self.assertEqual(len(result["transactions"]), 1)
        query = parse_qs(urlsplit(self.context.request.get.call_args.args[0]).query)
        self.assertEqual(query["fromDate"], ["01/01/2000"])
        self.assertEqual(query["toDate"], ["16/11/2026"])
        self.assertEqual(query["groupTransactionsByOrder"], ["false"])
        self.assertEqual(query["sessionId"], ["secret"])
        self.get_page.assert_awaited_once_with(self.context, new=True)
        self.page.close.assert_awaited_once()

    async def test_failed_replay_does_not_use_captured_shorter_report(self):
        self.captured[0] = (REPORT, {"data": [trade()]})
        self.response.ok, self.response.status = False, 503
        with self.assertRaisesRegex(TaskError, "503"):
            await fetch(self.context, "2000-01-01", "2026-11-16")
        self.products.assert_not_awaited()

    async def test_network_failure_is_redacted_and_does_not_fallback(self):
        self.context.request.get.side_effect = RuntimeError(REPORT)
        with self.assertRaises(TaskError) as caught:
            await fetch(self.context, "2000-01-01", "2026-11-16")
        self.assertNotIn("secret", str(caught.exception))
        self.assertNotIn("https", str(caught.exception))

    async def test_no_canonical_report_session_rejects_page_data(self):
        for url in (
            "https://trader.degiro.nl/api/transactions",
            "https://other.example/portfolio-reports/secure/v4/transactions",
            "https://trader.degiro.nl/productsearch/secure/v1/products/info",
        ):
            self.capture.return_value = [(url, {"data": [trade()]})]
            with self.subTest(url=url), self.assertRaisesRegex(TaskError, "report session"):
                await fetch(self.context, "2000-01-01", "2026-11-16")
        self.context.request.get.assert_not_awaited()

    async def test_successful_http_with_wrong_or_incomplete_envelope_fails(self):
        for payload in ({"data": {}}, {"data": [trade()], "hasMore": True}, {"data": [None]}):
            self.response.json.return_value = payload
            with self.subTest(payload=payload), self.assertRaises(TaskError):
                await fetch(self.context, "2000-01-01", "2026-11-16")

    async def test_out_of_range_rows_fail_instead_of_publishing_a_partial_report(self):
        self.response.json.return_value = {"data": [trade(), trade(id=43, date="2027-01-01")]}
        with self.assertRaisesRegex(TaskError, "outside"):
            await fetch(self.context, "2000-01-01", "2026-11-16")

    async def test_missing_currency_after_product_resolution_fails(self):
        row = trade()
        del row["currency"]
        self.response.json.return_value = {"data": [row]}
        with self.assertRaisesRegex(TaskError, "currency"):
            await fetch(self.context, "2000-01-01", "2026-11-16")
        self.products.return_value = {"1234": {"label": "TEST", "currency": "EUR"}}
        result = await fetch(self.context, "2000-01-01", "2026-11-16")
        self.assertEqual(result["transactions"][0]["currency"], "EUR")

    async def test_explicit_empty_report_is_valid(self):
        self.response.json.return_value = {"data": []}
        self.assertEqual(await fetch(self.context, "2000-01-01", "2026-11-16"), {"transactions": []})

    async def test_navigation_failure_still_closes_page(self):
        self.capture.side_effect = RuntimeError("navigation failure")
        with self.assertRaises(TaskError):
            await fetch(self.context, "2000-01-01", "2026-11-16")
        self.page.close.assert_awaited_once()

    async def test_invalid_requested_range_never_opens_a_page(self):
        with self.assertRaises(TaskError):
            await fetch(self.context, "2026-11-16", "2026-01-01")
        self.get_page.assert_not_awaited()


if __name__ == "__main__":
    unittest.main()
