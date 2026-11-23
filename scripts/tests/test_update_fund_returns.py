import importlib.util
import unittest
from datetime import date
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent.parent / "update-fund-returns.py"
spec = importlib.util.spec_from_file_location("update_fund_returns", SCRIPT)
updater = importlib.util.module_from_spec(spec)
spec.loader.exec_module(updater)

# Same layout as `pdftotext -layout` output of a Vanguard UCITS ETF factsheet
# (synthetic values).
FACTSHEET = """
Factsheet | 30 September 2026
Vanguard FTSE All-World UCITS ETF
Inception date: 23 July 2019
Legal entity      Investment method      ISIN            Dividends
Irish             Physical               IE00BK5BQT80    Accumulating
                                                         Year to                                              Since
Annualised performance**     1 month    Quarter          date      1 year    3 years    5 years   10 years   inception
Fund (Net of expenses)        -1.16%      1.64%        13.00%      16.71%     21.65%     11.56%          —      12.93%

Benchmark                     -1.18%      1.61%        12.93%      16.66%     21.65%     11.57%     12.35%      12.94%
Performance and Data is calculated on closing NAV as at 30 September 2026.
performance is NAV to NAV. Basis of index performance is total return. All performance is calculated in USD, net of fees.
"""


class VanguardFactsheetTest(unittest.TestCase):
    def test_reads_fund_row_dates_and_currency(self):
        sheet = updater.parse_vanguard(FACTSHEET, "IE00BK5BQT80")
        self.assertEqual(sheet["asOf"], date(2026, 9, 30))
        self.assertEqual(sheet["inception"], date(2019, 7, 23))
        self.assertEqual(sheet["currency"], "USD")
        self.assertEqual(sheet["values"]["3 years"], 21.65)
        self.assertEqual(sheet["values"]["Since inception"], 12.93)
        self.assertEqual(sheet["values"]["1 month"], -1.16)
        self.assertNotIn("10 years", sheet["values"])  # "—" means not yet available

    def test_rejects_the_wrong_fund_and_changed_layouts(self):
        with self.assertRaises(updater.FactsheetError):
            updater.parse_vanguard(FACTSHEET, "IE00B3RBWM25")
        with self.assertRaises(updater.FactsheetError):
            updater.parse_vanguard(FACTSHEET.replace("     12.93%\n\nBenchmark", "\n\nBenchmark"), "IE00BK5BQT80")
        with self.assertRaises(updater.FactsheetError):
            updater.parse_vanguard(FACTSHEET.replace("21.65%     11.56%", "21,65%     11.56%", 1), "IE00BK5BQT80")
        with self.assertRaises(updater.FactsheetError):
            updater.parse_vanguard(FACTSHEET.replace("calculated on closing NAV", "calculated on NAV"), "IE00BK5BQT80")

    def test_period_start_dates(self):
        self.assertEqual(updater.years_before(date(2026, 9, 30), 3), date(2023, 9, 30))
        self.assertEqual(updater.years_before(date(2028, 2, 29), 1), date(2027, 2, 28))

    def test_rendered_module_exports_the_references(self):
        text = updater.render([{"symbols": ["X"], "name": "Fund"}])
        self.assertIn("const fundReturnReferences = [", text)
        self.assertIn("module.exports = { fundReturnReferences }", text)


if __name__ == "__main__":
    unittest.main()
