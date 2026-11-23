#!/usr/bin/env python3
"""Refresh the dashboard's historical-return references from issuer factsheets.

Run weekly by .github/workflows/update-fund-returns.yml. For each configured
fund it downloads the official factsheet PDF, reads the annualized NAV returns
and their as-of date, and looks up the ECB USD/EUR reference rates needed to
express them in EUR. It rewrites internal/server/static/fund-returns.js only
when something changed, and exits non-zero rather than writing data it cannot
verify. Needs Python 3.11+ and `pdftotext` (poppler-utils). The server never
runs this: it keeps no internet access of its own.
"""

from __future__ import annotations

import csv
import io
import json
import re
import subprocess
import sys
import tempfile
import urllib.request
from datetime import date, datetime, timedelta
from pathlib import Path

OUTPUT = Path(__file__).resolve().parent.parent / "internal/server/static/fund-returns.js"
USER_AGENT = "degitide-fund-returns/1 (+https://github.com/tatoalo/degitide)"
ECB_SERIES = "EXR/D.USD.EUR.SP00.A"
ECB_API = "https://data-api.ecb.europa.eu/service/data/" + ECB_SERIES
ECB_PAGE = "https://data.ecb.europa.eu/data/datasets/EXR/EXR.D.USD.EUR.SP00.A"

FUNDS = [
    {
        "symbols": ["VWCE", "VWRA", "VWRP", "IE00BK5BQT80"],
        "name": "Vanguard FTSE All-World UCITS ETF (USD) Accumulating",
        "issuer": "Vanguard",
        "isin": "IE00BK5BQT80",
        "factsheet": "https://fund-docs.vanguard.com/FTSE_All-World_UCITS_ETF_USD_Accumulating_9679_EU_INT_UK_EN.pdf",
        "parser": "vanguard",
    },
]

# Factsheet columns, in order, and the periods the dashboard annualizes.
VANGUARD_COLUMNS = ["1 month", "Quarter", "Year to date", "1 year", "3 years", "5 years", "10 years", "Since inception"]
PERIOD_YEARS = {"1 year": 1, "3 years": 3, "5 years": 5, "10 years": 10}


class FactsheetError(RuntimeError):
    pass


def fetch(url: str) -> bytes:
    request = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
    with urllib.request.urlopen(request, timeout=60) as response:
        return response.read()


def pdf_text(data: bytes) -> str:
    with tempfile.NamedTemporaryFile(suffix=".pdf") as pdf:
        pdf.write(data)
        pdf.flush()
        result = subprocess.run(["pdftotext", "-layout", pdf.name, "-"], capture_output=True, check=True)
    return result.stdout.decode("utf-8", errors="replace")


def long_date(text: str) -> date:
    return datetime.strptime(" ".join(text.split()), "%d %B %Y").date()


def parse_vanguard(text: str, isin: str) -> dict:
    """Read a Vanguard UCITS ETF factsheet's 'Performance summary' table."""
    if isin not in text:
        raise FactsheetError(f"factsheet does not mention {isin}")
    as_of = re.search(r"calculated on closing NAV as at (\d{1,2} \w+ \d{4})", text)
    inception = re.search(r"Inception date:\s*(\d{1,2} \w+ \d{4})", text)
    currency = re.search(r"All performance is calculated in ([A-Z]{3})", text)
    row = re.search(r"^\s*Fund \(Net of expenses\)\s+(.+)$", text, re.MULTILINE)
    if not (as_of and inception and currency and row):
        raise FactsheetError("performance summary not found")
    cells = row.group(1).split()
    if len(cells) != len(VANGUARD_COLUMNS):
        raise FactsheetError(f"expected {len(VANGUARD_COLUMNS)} performance columns, found {len(cells)}")
    values: dict[str, float] = {}
    for column, cell in zip(VANGUARD_COLUMNS, cells):
        if cell in {"—", "-", "–", "n/a"}:
            continue
        match = re.fullmatch(r"(-?\d+(?:\.\d+)?)%", cell)
        if not match:
            raise FactsheetError(f"unreadable {column} return {cell!r}")
        values[column] = float(match.group(1))
    return {
        "asOf": long_date(as_of.group(1)),
        "inception": long_date(inception.group(1)),
        "currency": currency.group(1),
        "values": values,
    }


PARSERS = {"vanguard": parse_vanguard}


def ecb_rate(on_or_before: date) -> tuple[date, float]:
    """Last ECB USD-per-EUR reference rate published on or before a date."""
    query = f"?startPeriod={(on_or_before - timedelta(days=14)).isoformat()}&endPeriod={on_or_before.isoformat()}&format=csvdata"
    rows = list(csv.DictReader(io.StringIO(fetch(ECB_API + query).decode("utf-8"))))
    observations = sorted(
        (date.fromisoformat(r["TIME_PERIOD"]), float(r["OBS_VALUE"])) for r in rows if r.get("OBS_VALUE")
    )
    if not observations:
        raise FactsheetError(f"no ECB USD/EUR rate in the 14 days before {on_or_before}")
    return observations[-1]


def years_before(day: date, years: int) -> date:
    try:
        return day.replace(year=day.year - years)
    except ValueError:  # 29 February
        return day.replace(year=day.year - years, day=28)


def build_reference(fund: dict) -> dict:
    sheet = PARSERS[fund["parser"]](pdf_text(fetch(fund["factsheet"])), fund["isin"])
    as_of: date = sheet["asOf"]
    if not date(2000, 1, 1) < as_of <= date.today():
        raise FactsheetError(f"implausible as-of date {as_of}")
    needs_fx = sheet["currency"] == "USD"
    periods: list[tuple[str, float, float, date]] = []
    for label, years in PERIOD_YEARS.items():
        if label in sheet["values"]:
            periods.append((label, sheet["values"][label], years, years_before(as_of, years)))
    if "Since inception" in sheet["values"]:
        span = (as_of - sheet["inception"]).days / 365.25
        periods.append(("Since inception", sheet["values"]["Since inception"], span, sheet["inception"]))
    if not periods:
        raise FactsheetError("no annualized returns found")
    annualized = []
    for label, value, years, start in periods:
        if not -100 < value < 200:
            raise FactsheetError(f"implausible {label} return {value}")
        entry = {"period": label, "value": value, "years": round(years, 6)}
        if needs_fx:
            fx_date, fx = ecb_rate(start)
            entry.update({"fxStart": fx, "fxDate": fx_date.isoformat(), "fxSource": ECB_PAGE})
        annualized.append(entry)
    reference = {
        "symbols": fund["symbols"],
        "name": fund["name"],
        "issuer": fund["issuer"],
        "isin": fund["isin"],
        "currency": sheet["currency"],
        "asOf": as_of.isoformat(),
        "inception": sheet["inception"].isoformat(),
        "annualized": annualized,
        "source": fund["factsheet"],
    }
    if needs_fx:
        end_date, fx_end = ecb_rate(as_of)
        reference.update({"fxEnd": fx_end, "fxEndDate": end_date.isoformat(), "fxEndSource": ECB_PAGE})
    for value in [reference.get("fxEnd")] + [a.get("fxStart") for a in annualized]:
        if value is not None and not 0.5 < value < 2.5:
            raise FactsheetError(f"implausible USD/EUR rate {value}")
    return reference


def render(references: list[dict]) -> str:
    body = json.dumps(references, indent=2, ensure_ascii=False)
    return (
        "// Generated by scripts/update-fund-returns.py from issuer factsheets and\n"
        "// ECB reference rates. Do not edit by hand: the weekly \"Update fund\n"
        "// returns\" workflow rewrites it. Dated issuer snapshots, not live quotes.\n"
        f"const fundReturnReferences = {body};\n"
        'if (typeof module !== "undefined") module.exports = { fundReturnReferences };\n'
    )


def main() -> int:
    try:
        references = [build_reference(fund) for fund in FUNDS]
    except (FactsheetError, OSError, ValueError, subprocess.CalledProcessError) as exc:
        print(f"update-fund-returns: {exc}", file=sys.stderr)
        return 1
    text = render(references)
    if OUTPUT.exists() and OUTPUT.read_text(encoding="utf-8") == text:
        print("update-fund-returns: no change")
        return 0
    OUTPUT.write_text(text, encoding="utf-8")
    for reference in references:
        print(f"update-fund-returns: {reference['name']} as of {reference['asOf']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
