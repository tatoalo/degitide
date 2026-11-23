from datetime import date
import unittest

from tasks.common import json_body_schema, request_schema
from tasks.holdings import _portfolio_update_url, _position_product_ids, normalize_holdings
from tasks.products import product_map
from tasks.transactions import _filter_date_range, _with_date_range, normalize_transactions


class TransactionNormalizationTest(unittest.TestCase):
    def test_redacts_request_schema_values_and_path_tokens(self):
        self.assertEqual(
            request_schema(
                "get",
                "https://trader.degiro.nl/trading/secure/v5/update/123;jsessionid=secret?sessionId=secret&portfolio=0",
            ),
            (
                "GET",
                "/trading/secure/v5/update/{id};jsessionid={token}",
                ("portfolio", "sessionId"),
            ),
        )

    def test_reports_product_body_shape_without_values(self):
        self.assertEqual(
            json_body_schema('{"products":[123,456],"limit":2}'),
            {
                "root": "object",
                "fields": {
                    "limit": {"type": "int"},
                    "products": {"type": "list", "length": 2},
                },
            },
        )

    def test_normalizes_current_reporting_shape(self):
        result = normalize_transactions(
            {
                "data": [
                    {
                        "id": 42,
                        "buysell": "B",
                        "date": "2026-08-24T10:20:30+02:00",
                        "productId": 1234,
                        "quantity": 3,
                        "price": 18.25,
                        "totalFeesInBaseCurrency": -2.0,
                        "counterParty": "XAMS",
                    }
                ]
            }
        )

        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]["id"], "42")
        self.assertEqual(result[0]["type"], "BUY")
        self.assertEqual(result[0]["date"], "2026-08-24")
        self.assertEqual(result[0]["product_id"], "1234")
        self.assertEqual(result[0]["symbol"], "1234")
        self.assertEqual(result[0]["fee"], -2.0)
        self.assertIn('"counterParty":"XAMS"', result[0]["raw_json"])

    def test_derives_stable_id_when_endpoint_omits_one(self):
        payload = {"transactions": [{"date": "24/08/2026", "product": "ABC", "productId": "1234", "buysell": "B", "quantity": 1, "price": 20}]}
        first = normalize_transactions(payload)[0]
        second = normalize_transactions(payload)[0]
        self.assertTrue(first["id"].startswith("derived-"))
        self.assertEqual(first["id"], second["id"])

    def test_rewrites_report_dates_without_losing_session_params(self):
        result = _with_date_range(
            "https://trader.degiro.nl/portfolio-reports/secure/v4/transactions?intAccount=7&sessionId=abc",
            date(2026, 8, 18),
            date(2026, 8, 25),
        )
        self.assertIn("intAccount=7", result)
        self.assertIn("sessionId=abc", result)
        self.assertIn("fromDate=18%2F08%2F2026", result)
        self.assertIn("toDate=25%2F08%2F2026", result)

    def test_fallback_transactions_cannot_escape_requested_date_range(self):
        rows = [
            {"id": "old", "date": "2026-08-01"},
            {"id": "inside", "date": "2026-08-24"},
            {"id": "future", "date": "2026-08-26"},
        ]
        self.assertEqual(
            _filter_date_range(rows, date(2026, 8, 18), date(2026, 8, 25)),
            [{"id": "inside", "date": "2026-08-24"}],
        )


class HoldingNormalizationTest(unittest.TestCase):
    def test_derives_bounded_portfolio_update_from_bootstrap_urls(self):
        result = _portfolio_update_url(
            [
                (
                    "https://trader.degiro.nl/trading/secure/v5/account/info/123;jsessionid=session-token",
                    {},
                ),
                (
                    "https://trader.degiro.nl/pa/secure/client?sessionId=query-token",
                    {},
                ),
            ]
        )
        self.assertEqual(
            result,
            "https://trader.degiro.nl/trading/secure/v5/update/123;jsessionid=session-token?portfolio=0&totalPortfolio=0&intAccount=123&sessionId=query-token",
        )

    def test_flattens_nested_update_response_and_skips_cash_and_closed_positions(self):
        payload = {
                "portfolio": {
                    "value": [
                        {
                            "id": "1234",
                            "name": "positionrow",
                            "value": [
                                {"name": "positionType", "value": "PRODUCT"},
                                {"name": "size", "value": 4},
                                {"name": "breakEvenPrice", "value": 20.5},
                                {"name": "value", "value": 90.0},
                                {"name": "currency", "value": "EUR"},
                            ],
                        },
                        {
                            "id": "CASH",
                            "value": [
                                {"name": "positionType", "value": "CASH"},
                                {"name": "size", "value": 10},
                            ],
                        },
                        {
                            "id": "closed",
                            "value": [
                                {"name": "positionType", "value": "PRODUCT"},
                                {"name": "size", "value": 0},
                            ],
                        },
                    ]
                }
            }
        result = normalize_holdings(payload)

        self.assertEqual(
            result,
            [
                {
                    "product_id": "1234",
                    "symbol": "1234",
                    "quantity": 4.0,
                    "avg_price": 20.5,
                    "market_value": 90.0,
                    "currency": "EUR",
                    "value_currency": "EUR", "price": None, "price_as_of": "", "valuation_source": "broker_api",
                    "name": "",
                    "isin": "",
                }
            ],
        )
        self.assertEqual(_position_product_ids(payload), {"1234"})

    def test_holdings_carry_product_name_and_isin_for_issuer_logos(self):
        products = product_map(
            {"data": {"1234": {"id": "1234", "symbol": "VWCE", "currency": "EUR",
                               "name": "VANGUARD FTSE ALL-WORLD UCITS ETF USD ACC", "isin": "IE00BK5BQT80"}}}
        )
        holding = normalize_holdings(
            {"portfolio": {"value": [{"id": "1234", "value": [
                {"name": "positionType", "value": "PRODUCT"},
                {"name": "size", "value": 2},
                {"name": "value", "value": 300.0},
            ]}]}},
            products,
        )[0]
        self.assertEqual(holding["symbol"], "VWCE")
        self.assertEqual(holding["name"], "VANGUARD FTSE ALL-WORLD UCITS ETF USD ACC")
        self.assertEqual(holding["isin"], "IE00BK5BQT80")


class ProductResolutionTest(unittest.TestCase):
    def test_maps_product_id_to_symbol_and_currency(self):
        products = product_map(
            {"data": {"1234": {"id": "1234", "symbol": "EXAMPLE", "currency": "EUR"}}}
        )
        transaction = normalize_transactions(
            {"data": [{"id": 1, "productId": 1234, "date": "2026-08-24", "buysell": "B", "quantity": 1, "price": 20}]}, products
        )[0]
        holding = normalize_holdings(
            {
                "portfolio": {
                    "value": [
                        {
                            "id": "1234",
                            "name": "positionrow",
                            "value": [
                                {"name": "positionType", "value": "PRODUCT"},
                                {"name": "size", "value": 1},
                                {"name": "value", "value": 20},
                            ],
                        }
                    ]
                }
            },
            products,
        )[0]
        self.assertEqual(transaction["symbol"], "EXAMPLE")
        self.assertEqual(transaction["product_id"], "1234")
        self.assertEqual(transaction["currency"], "EUR")
        self.assertEqual(holding["symbol"], "EXAMPLE")
        self.assertEqual(holding["product_id"], "1234")
        self.assertEqual(holding["currency"], "EUR")


if __name__ == "__main__":
    unittest.main()
