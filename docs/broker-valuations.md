# Broker valuation contract

The sync records a point-in-time observation. Collection time is not quote time,
and a successful data import does not imply that every exchange supplies a live
quote.

## What is authoritative

- Request the complete authenticated v5 portfolio with revision zero. A missing,
  malformed, incremental or failed response cannot replace saved holdings.
- Replay the v4 transaction report for the requested date window. Never substitute
  the shorter report that happened to be visible in the trader page. Versioned
  backfill markers trigger one verified full-history refresh after this upgrade.
- Read the trader worker's current calculated portfolio and account resources
  on a fresh page. These use DEGIRO's own price selection, FX, contract-size and
  P/L logic. Each attempt sends only two `lastData` subscriptions and matching
  unsubscriptions on the existing worker port. Replies must match a unique
  page/attempt marker and the current worker generation. The reader retains
  financial fields and the client/account identifiers needed for correlation,
  never configuration, cookies or tokens.
- Match every active security ID and quantity against the full replay. Reconcile
  account balance against portfolio plus cash and reconcile portfolio against
  securities plus explicit corrections and cash-fund compensation. Retry while
  calculations settle. Unsupported/missing calculations fall back to recorded
  broker values with an explicit warning and unavailable account totals/P/L.
- Store the instrument's native price/cost currency separately from the currency
  of its market value. The raw v5 value and the calculated frontend value are in
  account base currency. Never add those amounts as if they were native currency.
- Distinguish a quote from the initial closing price before preserving its
  broker-supplied calendar date. The native worker's time-of-day conversion is
  unreliable, so it must not be stored as a precise UTC quote instant. A midnight
  serialization of the closing date is not evidence of a quote refresh.
- Publish transactions, holdings, account figures and successful-sync metadata
  in one database transaction. The dashboard snapshot API reads them consistently.

Broker total P/L is separate from Degitide's before-cost trading calculation.
The latter omits cash activity, fees, taxes and distributions, and is withheld
when native trade costs cannot be compared with converted valuations. Broker
available-to-trade can include credit; it is not a cash balance.

The dashboard rounds monetary figures to displayed cents before choosing the
sign, colour and direction mark: a floating-point residue is neutral `0.00`,
never a red `-0.00`. Day P/L is explicitly labelled with its collection date and
time. DEGIRO resets that figure at midnight; an overnight zero does not prove
that the previous trading session was flat. See the broker's
[key-figure definitions](https://www.degiro.ie/helpdesk/trading-platform/what-do-different-key-figures-my-account-mean).

Day P/L observations are retained atomically with each successful snapshot. When
a zero was collected overnight and complete calendar coverage identifies the
previous trading session, the dashboard shows that session's last saved reading
with its collection timestamp. A missing reading stays unavailable; it is never
inferred from changes in portfolio value. Genuine zero results during or after a
session stay zero. The latest raw broker valuation is unchanged. Upgrade retains
the latest existing reading, but cannot recover readings already overwritten.

## Market scheduling

The worker retains each product's `exchangeId` and resolves its MIC through the
broker's configured exchange dictionary. The policy permits only GET on the
exact dictionary paths with bounded account/session queries. Nothing is inferred
from an ETF's name, currency, ticker or underlying exposure. Stock and ETF listing
venues supported by [exchange_calendars](https://github.com/gerrymanoim/exchange_calendars)
provide UTC sessions including holidays, early closes and breaks; other products
or missing metadata use the daily fallback. Xetra uses its core session, matching
[DEGIRO's supported hours](https://www.degiro.ie/products-and-markets/exchange-opening-times),
not extended retail trading.

Plans cover the previous two weeks and next four weeks, are validated and saved
atomically with the portfolio, and are refreshed on every successful pull. A
missing plan replaces old metadata rather than scheduling a former portfolio.
An expired plan cannot claim that markets are closed. The Go scheduler checks
local state once per minute, refreshes every hour in regular sessions and
20 minutes after close, and combines overlapping held markets into one account
sync. It does not contact the broker merely because a status page is open.

The default 02:00 Dublin safety refresh skips snapshots younger than ten hours.
It leaves room for a 03:00 Sure read and the existing 36-hour freshness gate on
weekends and DST changes. Failed accounts retain their old snapshot and retry
with persistent bounded backoff, independently of other accounts. Manual sync
bypasses the retry wait. Automatic historical report backfill is limited to one
attempt per 24 hours; holdings and incremental transactions still update together
on every sync. Explicit cron overrides keep their existing behavior.
The dashboard shows custom schedules inline, including the configured expression,
timezone and next three actual runs. Common daily and weekday time windows also
have a readable summary; previews follow the same cron parser used for execution.

Historical reports retain both native and base-currency values. Old reports are
refreshed in bounded batches to recover base values. Durable snapshot-currency
markers preserve zero balances after sales, including after later repurchases.
Unknown historical FX is never inferred from today's exchange rate.

Sure receives these saved observations through a versioned history extension.
The supplied adapter v2 imports native valuations without enabling BUY/SELL
ledger activity. See [installation and limits](../integrations/sure/README.md).

## Regression checks

- `make test`: Go storage/scheduler/API tests, worker parsers and proxy rules.
- `make test-browser`: real Chromium observes both native worker types, checks
  account/worker replacement and failures, and renders quoted, unavailable and
  zero-holdings dashboard states on desktop and mobile.
- `make test-sure`: real Sure imports synthetic dated valuations and verifies
  chart balances, idempotency, manual-edit protection, restarts and reconnects.
- `python3 tests/integration/all-in-one/run.py`: packaged browser sync,
  failed-pull preservation, sold/reopened positions, schedule and crash recovery.

## Shipped frontend evidence

Inspected the public DEGIRO trader **5.144.0** assets on 2026-11-16:

- [Event worker](https://trader.degiro.nl/trader/scripts/event-broker-worker.4127af8b985b52108a12.js)
  SHA-256 `bd3f2b84fc0d8ced2ae676ac46cdd54d35c9fa4f55214cbd0d90b6b468ad22bc`.
- [Application](https://trader.degiro.nl/trader/scripts/app.fa91249e165ad7f05a14.js)
  defines dedicated/shared worker bootstrap and the JSON message envelope.

The event worker calculates position values from quotes, product metadata and
FX. Its total-portfolio resource includes closed-position P/L, non-product
charges and cash-fund compensation. It uses VWD `request_session`, bounded polling
and `req`/`rel` quote subscriptions. The policy permits those read-only shapes on
two exact quote hosts, and the exact same-origin worker bootstrap dependencies.
Order requests and arbitrary upstream destinations remain denied.

The product-info lookup must include native cash identifiers (`EUR`,
`FLATEX_EUR`, and the same forms for other currencies). The worker batches them
with security IDs. Rejecting one cash ID rejects the whole batch: positions
remain unenriched, with zero prices and no timestamps, and account totals are
never calculated. The policy accepts these exact identifier forms on the
existing product-info route; it does not remove cash rows or allow writes.

This failure was confirmed on 2026-11-16 using SharedWorker CDP network evidence
from a v0.1.2 deployment and reproduced locally with the unmodified trader
worker and its three shipped dependencies. With a numeric-only lookup policy,
the native worker retained raw positions and emitted only `lastData` events.
Accepting the mixed lookup restored product details, `reset` events and
reconciled account figures. Dictionary and feature requests remained denied in
the local reproduction. A quote session can still start for other products;
page-level network listeners do not capture all SharedWorker traffic.

The page can subscribe only to an initial `portfolio:lastData` event while the
worker continues recalculating positions and emitting total-portfolio changes.
Repeatedly reading the captured page state then pairs old positions with fresh
totals, including after the market closes. A controlled replay of the actual
worker with VWD quote packets reproduced this mismatch; a fresh resource read
returned the updated native position price, timestamp and value. The production
reader reconciled those positions against totals including cash and cash-fund
compensation without changing the accounting tolerance.

Both resource reads reply synchronously in the native worker and their cleanup
is queued immediately on the same port. Replies time out after 1.5 seconds;
worker/account replacement invalidates an in-flight attempt. The two reads are
not atomic together, so arithmetic/identity checks and the bounded retry window
remain necessary. Missing, late or unrelated replies cannot substitute earlier
passive observations. No market tick is required to refresh the snapshot.

The worker's `CombinedLastDateTime` copies the broker's `LastDate` but parses
`LastTime` using the browser timezone and today's offset. Native replay with
UTC, Dublin and Amsterdam browser zones showed different serialized `Z` times
for identical quote packets, independent of `currentClient.timezone`. A summer
quote replayed in winter used the winter offset. Changing the browser timezone
or subtracting a fixed hour would therefore remain incorrect. Preserve the
quote date and explicitly mark precise quote times unavailable; `fetched_at`
continues to record the actual UTC collection instant. Price-return calculations
accept both date-only values and older RFC3339 records.

The proxy writes sanitized route/method decisions independently of mitmproxy's
quiet flow logger. The worker writes one `portfolio observation` outcome per
holdings fetch, with a fixed rejection reason and enrichment/timestamp counts
when unavailable. These diagnostics omit URLs, identifiers, credentials and
amounts. A real quiet-mode proxy regression checks both visibility and redaction.

REA static evidence: `ev_aa4dbca5b4bd828732a2330b6cf572389a4a76431bc40c293d1f94bf7faf4f1b`;
summary `ev_d0b9e2d9cb92f1976c4ecca3c06fbd80f8e28d69547ae5b2acb825e6df4f4639`.
The graph has partial semantic coverage; the relevant protocol and calculations
were also inspected directly in the shipped source. Static evidence does not
prove authenticated runtime behavior. Browser tests use a synthetic broker with
real Chromium, including both worker types; live broker verification is a
separate deployment check.

When DEGIRO changes this private protocol, fail visibly or show the explicit
recorded-value fallback. Do not quietly fabricate missing prices or account P/L.
