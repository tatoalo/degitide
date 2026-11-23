"""Tradegate BSX stock and ETF sessions for MIC XGAT.

Source: https://www.tradegatebsx.com/handelskalender.php?lang=en
Extend coverage when the exchange publishes the next annual calendar.
"""

from datetime import date, datetime, time, timedelta, timezone
from zoneinfo import ZoneInfo

BERLIN = ZoneInfo("Europe/Berlin")
FIRST_DAY = date(2026, 1, 1)
VALID_UNTIL = datetime(2028, 1, 1, tzinfo=BERLIN)
# Exceptions to the regular Monday–Friday 07:30–22:00 session, in Berlin time.
CLOSED = {
    date(2026, 1, 1), date(2026, 4, 3), date(2026, 4, 6), date(2026, 5, 1),
    date(2026, 12, 24), date(2026, 12, 25), date(2026, 12, 31),
    date(2027, 1, 1), date(2027, 3, 26), date(2027, 3, 29),
    date(2027, 12, 24), date(2027, 12, 31),
}
EARLY_CLOSE = {
    date(2026, 5, 14): time(20), date(2026, 5, 25): time(20), date(2026, 12, 30): time(14),
    date(2027, 5, 6): time(20), date(2027, 5, 17): time(20), date(2027, 12, 30): time(14),
}


def sessions(day, start, end):
    rows = []
    if FIRST_DAY <= day < VALID_UNTIL.date():
        current = max(start, FIRST_DAY)
        while current <= end and current < VALID_UNTIL.date():
            if current.weekday() < 5 and current not in CLOSED:
                opening = datetime.combine(current, time(7, 30), BERLIN)
                closing = datetime.combine(current, EARLY_CLOSE.get(current, time(22)), BERLIN)
                rows.append({"open": opening.astimezone(timezone.utc).isoformat(),
                             "close": closing.astimezone(timezone.utc).isoformat()})
            current += timedelta(days=1)
    return BERLIN.key, rows
