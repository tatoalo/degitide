from datetime import date, timedelta
import unittest

from tasks.common import TaskError
from tasks.positions import _report_url, _requested_dates, parse_number, parse_report

# Same shape as a real DEGIRO positionReport/csv response (synthetic values).
REPORT = (
    "Product,Symbol/ISIN,Amount,Closing,Local value,,Value in EUR\n"
    'CASH & CASH FUND & FTX CASH (EUR),,,,EUR,"-25,50","-25,50"\n'
    'EXAMPLE SOUTH FUND,ZZ0000000002,5,"12,50",EUR,"62,50","62,50"\n'
    'EXAMPLE NORTH FUND,ZZ0000000001,8,"24,25",EUR,"194,00","194,00"\n'
)


class PositionReportTest(unittest.TestCase):
    def test_parses_securities_and_skips_cash(self):
        report = parse_report(REPORT)
        self.assertEqual(report["base_currency"], "EUR")
        self.assertEqual(
            report["positions"],
            [
                {
                    "identifier": "ZZ0000000002",
                    "name": "EXAMPLE SOUTH FUND",
                    "quantity": 5.0,
                    "close": 12.50,
                    "currency": "EUR",
                    "value": 62.50,
                    "base_value": 62.50,
                },
                {
                    "identifier": "ZZ0000000001",
                    "name": "EXAMPLE NORTH FUND",
                    "quantity": 8.0,
                    "close": 24.25,
                    "currency": "EUR",
                    "value": 194.00,
                    "base_value": 194.00,
                },
            ],
        )

    def test_header_only_report_holds_nothing(self):
        self.assertEqual(
            parse_report("Product,Symbol/ISIN,Amount,Closing,Local value,,Value in EUR\n")["positions"],
            [],
        )

    def test_foreign_currency_keeps_local_value(self):
        report = parse_report(
            "Product,Symbol/ISIN,Amount,Closing,Local value,,Value in EUR\n"
            'EXAMPLE FOREIGN FUND,ZZ0000000003,10,"95,12",USD,"951,20","880,05"\n'
        )
        position = report["positions"][0]
        self.assertEqual((position["currency"], position["value"], position["base_value"]), ("USD", 951.20, 880.05))

    def test_rejects_unrecognized_reports_and_rows(self):
        with self.assertRaises(TaskError):
            parse_report('{"status":500}')
        with self.assertRaises(TaskError):
            parse_report(
                "Product,Symbol/ISIN,Amount,Closing,Local value,,Value in EUR\n"
                'X,<script>,1,"1,0",EUR,"1,0","1,0"\n'
            )
        with self.assertRaises(TaskError):
            parse_report(
                "Product,Symbol/ISIN,Amount,Closing,Local value,,Value in EUR\n"
                "X,ZZ0000000002,1,abc,EUR,1,1\n"
            )

    def test_number_formats(self):
        self.assertEqual(parse_number("2500,75"), 2500.75)
        self.assertEqual(parse_number("-175,25"), -175.25)
        self.assertEqual(parse_number("1.234,56"), 1234.56)
        self.assertEqual(parse_number("1,234.56"), 1234.56)
        self.assertEqual(parse_number("12"), 12.0)
        with self.assertRaises(ValueError):
            parse_number("")
        with self.assertRaises(ValueError):
            parse_number("nan")

    def test_partial_report_cannot_become_a_smaller_or_empty_portfolio(self):
        header = "Product,Symbol/ISIN,Amount,Closing,Local value,,Value in EUR\n"
        for row in ("X,ZZ0000000002,1\n", "X,,1,2,EUR,2,2\n",
                    "X,ZZ0000000002,1,2,EUR,2,\n", '"unterminated'):
            with self.subTest(row=row), self.assertRaises(TaskError):
                parse_report(header + row)
        with self.assertRaises(TaskError):
            parse_report(header.replace("Value in EUR", "Value"))
        self.assertEqual(parse_report(header + "\n,,,,,,\n")["positions"], [])

    def test_requested_dates_are_bounded_unique_and_not_future(self):
        self.assertEqual(
            _requested_dates(["2022-12-30", "2022-12-30", "2024-06-28"]),
            [date(2022, 12, 30), date(2024, 6, 28)],
        )
        for bad in ([], ["30/12/2022"], [(date.today() + timedelta(days=1)).isoformat()], [f"2022-01-{d:02d}" for d in range(1, 29)] * 6):
            with self.assertRaises(TaskError):
                _requested_dates(bad)

    def test_report_url_uses_only_allowlisted_query_keys(self):
        url = _report_url({"intAccount": "1", "sessionId": "s"}, date(2022, 12, 30))
        self.assertEqual(
            url,
            "https://trader.degiro.nl/portfolio-reports/secure/v3/positionReport/csv"
            "?intAccount=1&sessionId=s&fromDate=30%2F12%2F2022&toDate=30%2F12%2F2022",
        )


if __name__ == "__main__":
    unittest.main()
