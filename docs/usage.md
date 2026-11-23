# Running Degitide

## Default deployment

One container runs the dashboard, scheduler, browser worker, policy proxy and optional notifications. It stores the database, encrypted credentials, bounded browser sessions, keys and proxy certificates in one `degitide` volume mounted at `/data`, and publishes only `127.0.0.1:8391`.

The image runs as a non-root user. Worker, proxy and notification listeners bind to container loopback. Browser traffic still uses the read/report policy proxy, but processes share a volume and network namespace: this mode does not enforce the split stack's network or credential isolation. Use the [advanced split deployment](split-deployment.md) for those boundaries and for existing split installations.

The worker stores credentials and session checkpoints with Fernet encryption and keeps Chromium's disposable profile under `/tmp`. Each broker account has separate state. Back up the entire volume, including its encryption key; possession of that backup allows decrypting the stored credentials.

## Updates and backups

Back up the data volume before upgrading. It contains the database and encryption keys. Keep the same Compose project name, volumes, configuration, and deployment layout. Never pass `--volumes` to `docker compose down` during an upgrade.

For a source install, check out the next release and run `docker compose up -d --build`. For a packaged install, unpack the new Docker bundle in the existing directory, keep your `.env`, and run `docker compose pull` followed by `docker compose up -d --no-build`. Include the same Compose overrides you used originally. A running container picks up an update when it is recreated. An exact version or digest stays pinned until you change it.

## Docker usage

Build and start the all-in-one container:

```bash
docker compose up --build -d
```

Open <http://127.0.0.1:8391>. On an uninitialized database, Degitide opens a one-time setup page where you choose an administrator username and password. The first successfully created account becomes the sole administrator and `/setup` then locks; complete this step before sharing or exposing the URL. These are Degitide-only credentials and stay separate from the DEGIRO values accepted later by the clearly labelled **Connect DEGIRO** dialog.

Successful setup or login creates an HTTP-only, same-site browser-session cookie signed with an internal generated key. You never need to display or copy that key for normal browser use. Closing the browser session or using **Sign out** ends dashboard access.

For an HTTPS reverse proxy, include its public hostname in the deployment environment and preserve the incoming `Host` and `X-Forwarded-Proto: https` headers. Preserving `Origin` and `Sec-Fetch-Site` adds defense in depth. Degitide refuses to render remote setup/login forms over plaintext HTTP and marks the resulting dashboard cookie secure. Caddy's normal `reverse_proxy` and `forward_auth` behavior already preserves the browser request while adding the forwarded scheme, so Authelia does not need to be added as an application origin:

```dotenv
DEGITIDE_ALLOWED_HOSTS=127.0.0.1,localhost,[::1],portfolio.example.org
```

For proxy and browser compatibility, setup and login forms include a random CSRF token paired with a host-only, HTTP-only, same-site browser-session cookie. A valid cookie is reused and rendered into every form, so refreshes, concurrent tabs, password-manager detours, and authentication-proxy bounces do not invalidate an open form. Missing, stale, and mismatched tokens are reported separately, re-render the form without losing the username, and emit a reason-only server log. Degitide uses `Referrer-Policy: same-origin`, which preserves same-origin form identity without sending a referrer cross-origin; for cached pages or user agents that still submit `Origin: null`, only a setup/login form marked `Sec-Fetch-Site: same-origin` may continue to the mandatory token check. Explicit mismatched origins, other opaque-origin metadata, and browser-reported cross-site requests remain denied. API writes retain the strict canonical same-origin requirement.

Open **Accounts**, choose **Connect**, and enter the DEGIRO username, password, and authenticator setup secret. The dialog is enabled only after the dashboard administrator has signed in. A successful attempt verifies an authenticated trader reload and replaces that account's bounded session checkpoint; it saves encrypted credentials for automatic reconnection.

**Connect and sync** shows connection activity and elapsed time while DEGIRO signs in, then opens the overview and starts importing the account’s holdings. An automatic sync already importing that account is followed instead. If a different account is syncing, the dashboard explains when to start this account’s import. Failed sign-ins return to the form with the username preserved and secret fields cleared for retry.

Remote dashboard reauthentication is deliberately unavailable over plaintext HTTP. Keep Caddy/Authelia in front, preserve the public `Host`, `Origin`, `Sec-Fetch-Site`, and `X-Forwarded-Proto: https` values, and leave the Docker port loopback-only. A long-running pull or another login attempt returns a conflict rather than queueing credential-bearing requests.

For the legacy native login fallback, see the [advanced split deployment](split-deployment.md).

### Terminal setup and multiple accounts

Run these commands on the server from the project folder:

```bash
docker compose up --build -d
docker compose exec app degitide setup
docker compose exec app degitide connect
docker compose exec app degitide accounts add --name "Long-term savings" --id savings
docker compose exec app degitide connect --account savings
docker compose exec app degitide sync
docker compose exec app degitide status --account savings
```

`setup` creates the Degitide administrator once. `connect` prompts for DEGIRO credentials with password and authenticator secret hidden. Use `connect --code-only` for a fresh six-digit code instead of saving credentials for automatic login. Inside Docker the CLI reads its mounted dashboard token; there is no token to copy into your browser.

`sync` pulls every account and continues if one fails. `sync --account savings` pulls only that account. Both the CLI and UI report completed stages: connection check, transactions, holdings, and snapshot save. The percentage measures stages, not elapsed time. A failed sync exits nonzero in the CLI and leaves the last successful holdings available in the dashboard.

The dashboard follows sync progress live, including scheduled runs and syncs started from another browser or the CLI. It disables both sync controls while a run is active and refreshes the saved portfolio when the run finishes. If the live connection drops, it polls for status until the connection recovers.

Choose **What’s new** in the dashboard footer to read the changelog for the installed app and recent releases.
Release builds generate these notes automatically from commit and merged-PR titles between stable Git tags. Docker images and native CLI packages bundle the latest ten entries, so the dialog works without GitHub access, API tokens, or a network connection to GitHub. Rebuilding an older version keeps its own release history. For a local development build, run `python3 scripts/build_changelog.py dev` before building to include notes.

`accounts` lists account IDs. `status --json` returns stored status without waking Chromium. `sync --json` emits a JSON progress object whenever the stage changes. For noninteractive setup or connection, pass `--credentials-stdin` and pipe a single JSON object with `username`, `password`, and, for DEGIRO, either `seed` or `totp`. Use `docker compose exec -T` for this mode. Do not put credentials in command arguments or shell history.

The Go service owns the schedule, so sync continues after the dashboard, SSH tunnel, or CLI closes. By default each account refreshes every hour while one of its held stock/ETF markets is open, then 20 minutes after each market closes. The dashboard shows the detected listing venues, market state and next sync, with an expandable explanation of the rules.

Open **Overview → Sync settings** to change the interval (5–240 minutes), after-close delay (0–120 minutes), daily safety time and timezone, or pause automatic sync. A custom cron mode is also available. These settings apply to all accounts; each account still plans from its own markets and last successful sync. Dashboard settings are saved in SQLite, override deployment environment defaults, and apply without restarting. Saving never starts a worker request; a sync already in progress finishes normally. **Restore deployment defaults** removes the saved override. Closing the dialog without saving changes nothing.

Accounts share one Degitide administrator, but their credentials, transactions, snapshots, schedules and currency totals stay separate. Existing records migrate into **Main account** automatically; back up the SQLite volume before upgrading because the schema changes.

Market scheduling uses DEGIRO's product exchange IDs and exchange dictionary, together with `exchange_calendars` regular sessions, holidays, early closes, lunch breaks and daylight-saving rules. It follows the security's listing venue, not the countries inside an ETF. Unsupported venues or product types use a daily fallback; currencies and tickers are never used to guess an exchange. More frequent collection does not turn delayed or closing prices into live quotes.

A safety refresh at 02:00 Europe/Dublin runs when the last successful snapshot is at least ten hours old, leaving time before a 03:00 Sure import and keeping weekend snapshots within the feed's 36-hour limit. A normal evening closing sync already covers the next overnight import and skips that safety refresh. On restart, only overdue accounts sync; the first run after upgrading discovers missing market metadata. Failures retry after 15, 30, 60, 120 and then 240 minutes, with backoff preserved across restarts. Manual sync remains available. Automatic syncs attempt historical value backfill at most once per 24 hours; manual syncs can continue the backfill immediately.

**Upgrading from the daily schedule:** choose **Follow held markets** in the dashboard’s **Sync settings**, or update your Compose files and set `DEGITIDE_SCHEDULE=market` in any existing `.env` file. Without a saved dashboard override, an explicit cron schedule, including the old `CRON_TZ=Europe/Dublin 0 6 * * *`, is preserved. Update the worker and proxy alongside the app so exchange metadata and calendars are available. For a different Sure import time, choose a daily safety time before it in the dashboard (or set `DEGITIDE_MAINTENANCE_SCHEDULE` as a deployment default). Custom schedules and paused sync can let the saved snapshot exceed Sure’s 36-hour freshness limit.

### Open a VPS dashboard through SSH

Download the matching macOS/Linux CLI archive from [Releases](https://github.com/tatoalo/degitide/releases), or build it from this checkout with Go 1.25 or newer:

```bash
go build -o ./bin/degitide ./cmd/degitide
./bin/degitide open --ssh user@your-vps
```

An SSH host alias from `~/.ssh/config` works too. The command forwards local `127.0.0.1:18391` to the server's loopback port `8391`, opens your browser, and keeps the tunnel in the foreground. Sign in with the administrator created during setup. Ctrl+C closes the tunnel; scheduled syncs keep running. Use `--local-port 18392` if the local port is occupied, `--port` for a different remote dashboard port, or `--no-open` to print the address without launching a browser. SSH authentication and host verification use your normal SSH configuration.

No hosted relay is needed, and Docker's dashboard port stays bound to loopback. A direct local installation can use `degitide open`; CLI API commands also accept `--url` or `DEGITIDE_URL` plus `DEGITIDE_API_TOKEN_FILE`. Remote HTTP origins are rejected; use HTTPS or a loopback SSH tunnel.

### Failure notifications

Choose one provider per deployment. Add its settings to the ignored `.env` file. For Telegram, use your bot token and destination chat ID; start a conversation with the bot first. See the [Telegram bot documentation](https://core.telegram.org/bots/api#sendmessage).

```dotenv
DEGITIDE_NOTIFY_PROVIDER=telegram
DEGITIDE_TELEGRAM_TOKEN=your-bot-token
DEGITIDE_TELEGRAM_CHAT_ID=your-chat-id
```

For Pushover, replace the provider settings with your [application token and user key](https://pushover.net/api):

```dotenv
DEGITIDE_NOTIFY_PROVIDER=pushover
DEGITIDE_PUSHOVER_TOKEN=your-application-token
DEGITIDE_PUSHOVER_USER=your-user-key
```

Apply the configuration with `docker compose up -d`. The container starts the notification process automatically when a provider is configured; no extra container, port or volume is needed.

You get one message when an account first fails, then one when it recovers. Repeated failures stay quiet across restarts. Messages include the account label and state, with no balances, transactions, credentials, or raw worker errors. Failed message delivery is logged and retried on the next sync; it does not block the worker. Inspect delivery errors with `docker compose logs app`.

These messages cover sync failures, including an unavailable worker. They cannot report a complete host or app outage; that needs monitoring outside this deployment. To disable notifications, clear `DEGITIDE_NOTIFY_PROVIDER` and run `docker compose up -d`.

## Configuration

Important application and worker variables are:

| Variable | Default | Description |
| --- | --- | --- |
| `DEGITIDE_BIND` | `127.0.0.1` | Go app bind address; a non-loopback bind requires dashboard authentication |
| `DEGITIDE_PORT` | `8391` | Go app port |
| `DEGITIDE_API_TOKEN` / `_FILE` | empty | Internal dashboard session-signing key or secret-file path; Compose generates it automatically |
| `DEGITIDE_ALLOWED_HOSTS` | `127.0.0.1,localhost,[::1]` | Accepted dashboard Host header names |
| `DEGITIDE_WORKER_URL` | `http://127.0.0.1:9377` | Browser worker URL |
| `DEGITIDE_WORKER_TOKEN` / `_FILE` | empty | Internal worker bearer token or secret-file path |
| `DEGITIDE_BROWSER_IDLE_SECONDS` | `300` | Seconds after the last worker task before Chromium and Patchright are released; `0` keeps them running |
| `DEGITIDE_DB` | `./data/degitide.db` | SQLite path |
| `DEGITIDE_SCHEDULE` | `market` | Per-account market scheduling, or an explicit cron expression to retain a fixed schedule |
| `DEGITIDE_SYNC_INTERVAL_MINUTES` | `60` | Refresh interval while a held market is open, from 5 to 240 minutes |
| `DEGITIDE_MAINTENANCE_SCHEDULE` | `CRON_TZ=Europe/Dublin 0 2 * * *` | Daily fallback/safety check for market mode; runs only when the last successful sync is at least ten hours old |
| `DEGITIDE_NOTIFY_URL` | empty | Optional notification relay endpoint; Compose uses `http://notifier:9380/notify` |
| `DEGITIDE_NOTIFY_TOKEN` / `_FILE` | empty | Shared app-to-relay token; generated by Compose |
| `DEGITIDE_NOTIFY_PROVIDER` | empty | Relay provider: `telegram` or `pushover` |
| `DEGITIDE_BROWSER_PROFILE` | `./browser-profile` | Persistent directory for the bounded session checkpoint only |
| `DEGITIDE_BROWSER_LOCALE` | `en-IE` | Browser locale |
| `DEGITIDE_BROWSER_TIMEZONE` | `Europe/Dublin` | Browser timezone |

Compose supplies the worker URL, tokens, proxy, CA, binds, and volumes itself. Do not replace the worker's proxy in the hardened deployment.
Chromium trusts the generated public CA through NSS, while Patchright's API-request client receives the same pinned certificate through `NODE_EXTRA_CA_CERTS`; TLS verification is never disabled.

## Local development

Local processes are convenient for development but do not have Docker's no-route egress boundary. Use the Compose deployment for containment.

```bash
make install-worker
make install-browser

# separate terminals; this development worker is always headless
make worker
make run
```

The local service binds to loopback by default, but it does not reproduce Docker's network boundary or provide an interactive credential flow. Use `docker compose exec app degitide connect` in the Docker deployment for authentication and use local mode only with an existing compatible session checkpoint or for non-live development.

## API and tests

Dashboard service endpoints:

```text
GET  /healthz
GET  /api/accounts
POST /api/accounts
GET  /api/status
POST /api/pull/trigger
POST /api/degiro/reauthenticate
GET  /api/transactions
GET  /api/transactions/export
GET  /api/holdings
GET  /api/portfolio/history
GET  /api/portfolio/valuation
GET  /api/portfolio/snapshot
```

Account-scoped endpoints accept `?account=<id>`; omitted IDs use `default`. The exception is `POST /api/pull/trigger`: omitting the account syncs all accounts. Unknown account IDs are rejected. `/api/status` returns persisted account health and shared sync progress without invoking the worker.

### Valuation accuracy

Sync captures the figures calculated by DEGIRO's trader, including portfolio,
cash, account balance and account P/L, and stores each holding's price time.
The collection time in the footer is separate from quote time. If quotes or
calculated totals are unavailable, the dashboard explicitly identifies recorded
broker values and leaves unavailable account totals blank. Failed or incomplete
portfolio/transaction requests keep the previous successful snapshot.

Instrument price currency and market-value currency are stored separately.
The first sync after upgrading repeats the full transaction import to verify
older history; historical base-currency valuations refresh in bounded batches.
See [the valuation contract](broker-valuations.md) for data sources,
reconciliation, migration behavior and limits.

### Transaction exports

Open **Transactions** and choose **CSV**, **PDF**, or **JSON**. Downloads include every saved transaction for the selected account, not just the latest 100 displayed in the table. They do not fetch older data from DEGIRO. Account IDs, transaction IDs, dates, instruments, quantities, prices, currencies, fees, and counterparties stay attached to their account; raw broker response payloads are excluded.

The authenticated endpoint is `/api/transactions/export?account=default&format=csv` (also `pdf` or `json`). CSV uses UTF-8 and protects text cells that could be interpreted as spreadsheet formulas. JSON preserves the original text. PDFs include repeated table headings, page numbers, and an export timestamp. Fees are copied as recorded; a fee currency is not inferred from the trade currency.

Successful sync feedback disappears five seconds after completion and stays dismissed on refresh. Failed syncs remain visible so they can be investigated.

### Automatic one-way sync into Sure

Degitide serves a read-only [SimpleFIN](https://www.simplefin.org/protocol.html) feed that [Sure](https://github.com/we-promise/sure) can refresh on its own schedule. No Sure API key or paid SimpleFIN Bridge account is needed. The scheduled DEGIRO worker saves data; Sure reads that saved data through a separate, scoped endpoint in the Go app. The configured browser proxy policy stays unchanged, and neither dashboard has to stay open.

**Scope:** current long positions, their combined securities value, and saved historical valuations, with a separate Investment account for each broker account/value currency. These are securities-only accounts: broker cash and account P/L are displayed separately in Degitide; deposits, withdrawals, dividends and FX transactions are not exported here. Instruments use stable offline `CUSTOM:DEGIRO_…` identities because the saved product label is not necessarily an exchange ticker. Values come from the latest saved Degitide snapshot, whose price source and timestamps are shown in Degitide. Foreign-currency average costs are omitted when a matching converted cost is unavailable. Accounts added after token creation are not shared automatically.

**Tested compatibility:** the real Docker integration suite exercises Sure **v0.7.6**. That release retains sold positions, ignores an explicit empty holdings array, and does not import Degitide's history extension by itself. Install version 2 of the supplied [Sure compatibility adapter](../integrations/sure/README.md) in **both Sure web and worker** to reconcile complete snapshots and import native dated valuations, including zero balances. The adapter preserves manual corrections and only repairs Sure's artificial opening value when it can prove the entry is untouched. This is a local adapter, not an upstream Sure fix. Re-run `make test-sure` against the intended image before upgrading Sure.

Sure's [SimpleFIN transaction processor](https://github.com/we-promise/sure/blob/v0.7.6/app/models/simplefin_entry/processor.rb) creates ordinary ledger transactions, not native investment trades. Transaction activity is therefore off by default. The integration tests use synthetic broker responses with the real Degitide scheduler, Sure web controllers, Sidekiq, PostgreSQL and Redis; no live DEGIRO account is needed.

#### Connect containers on the same VPS

Create a private network and add the supplied override to your usual Compose invocation:

```bash
docker network create --internal degitide-sure
docker compose -f docker-compose.yml -f docker-compose.sure.yml up -d --build
```

Attach **both Sure's web and background-worker services** to that external network in Sure's Compose configuration (adjust `web`/`worker` to your service names; keep their existing networks):

```yaml
services:
  web:
    networks: [default, degitide]
  worker:
    networks: [default, degitide]
networks:
  degitide:
    external: true
    name: degitide-sure
```

Install the [compatibility adapter](../integrations/sure/README.md#install-in-sure), then recreate both affected Sure services. Degitide's private network alias is `degitide-sure`; no additional port or worker access is exposed. Sure must run in self-hosted mode to accept a private HTTP provider URL. For separate machines, use a reachable HTTPS reverse-proxy origin and add its hostname to `DEGITIDE_ALLOWED_HOSTS`. A temporary laptop SSH tunnel is for interactive dashboard access, not for Sure's scheduled server-to-server reads.

1. Complete a successful Degitide sync.
2. Open **Accounts → Connect Sure**. The guided setup has three steps: **Prepare**, **Connect**, and **Check**. Choose **Same Docker host** (the private address is filled in) or **HTTPS address**, follow the expandable network and adapter instructions if needed, choose the accounts, and create a connection token. Or run:

   ```bash
   docker compose exec app degitide sure token --base-url http://degitide-sure:8391
   # Restrict the connection to selected account IDs:
   docker compose exec app degitide sure token --base-url http://degitide-sure:8391 --account default,savings
   ```

3. Paste the token into **Sure → Add account → SimpleFIN**. It can be claimed once within 30 minutes. Treat it as a password.
4. Link each currency account as **Investment / Brokerage** in Sure. If reconnecting, map to your existing accounts to avoid creating another copy. Sure's scheduled/background sync must be enabled; it controls when the next read happens. Degitide does not push immediately after a pull.
5. Use **Check connection** in the setup dialog or next to an existing connection in **Accounts**. It distinguishes an unused token, an accepted token awaiting its first read, and a recorded read from Sure. Expired or revoked connections offer a new token. Trigger a sync in Sure to test immediately, then verify the imported holdings in Sure: a recorded feed read does not prove that account linking or import succeeded. The CLI equivalent is `degitide sure status`.

Revoke a connection from Accounts or the CLI:

```bash
docker compose exec app degitide sure status
docker compose exec app degitide sure revoke --id CONNECTION_ID
```

Revocation stops new reads, including after restart; it does not erase records already in Sure. Access credentials and setup tokens are stored only as hashes in SQLite. A read token cannot reach the dashboard API, trigger a pull, access broker sessions or trade. The feed returns a retry response during a pull and withholds failed, unsupported or older-than-36-hour account snapshots. Sure receives a readable error until the next successful sync. Unknown currency and short positions are withheld instead of being converted or guessed.

#### Optional transaction activity

Add `--include-transactions` when creating the token to include saved BUY/SELL activity. This opts into **ordinary Sure ledger entries that can affect spending reports**, not native trades. Amounts are signed gross consideration; fees are retained in metadata and excluded from the amount because the worker does not record fee currency. Full saved history within Sure's requested date window is available; the dashboard's 100-row display limit does not apply. Each account's first successful sync imports its full DEGIRO transaction history; later syncs resume a week before the previous successful sync, so a missed week of syncs leaves no gap. Stable account-scoped transaction IDs let Sure update the same entries on repeated reads. CSV/PDF/JSON exports remain available independently.

Browser users authenticate with the administrator account created at `/setup`. Existing command-line integrations may continue to use HTTP Basic username `degitide` with the internal dashboard key, but this is an optional compatibility path and is not required for browser setup or login.

The internal worker exposes an unauthenticated health check and bearer-protected task endpoints. It is not published to the host. The dashboard footer shows the app and worker build versions, plus the worker's Patchright and Python versions. The authenticated `/api/versions` endpoint reads the worker's bearer-protected `/info` metadata without starting Chromium or contacting DEGIRO; an unavailable worker does not block the saved portfolio. Container builds pass `VERSION` to both components; local development defaults to `dev`.

Published app and worker versions use release tags, such as `v0.1.0`. Builds
between releases include their distance from the preceding tag, such as
`v0.1.0-dev.38`; these are development builds, not a new release. The exact
commit remains in the image's OCI revision metadata and immutable `sha-...`
tag. Legacy hash-only versions display as “Development” in the footer, with
the revision available on hover.

```bash
make fmt
make test
make build
docker compose config --quiet
```

The policy tests include allowed read/report requests, login-mode gating, exact-host validation, path traversal, malformed queries/bodies, tunnel and protocol-upgrade denial, and known order mutations.

The `CI` workflow runs Go formatting/tests/vet, Python compilation and tests, shell/Compose validation, and the website build/tests on pull requests and `main`. The read-only `Container builds` workflow separately proves that every Docker target builds before allowing a merge. `Publish images` has the write permission and runs only from trusted `main`, version-tag, or manually dispatched events.

### Portfolio charts

The dashboard plots holdings values in their recorded value currency, using
DEGIRO's base-currency conversions when supplied. It never adds amounts labelled
with different currencies. Synced daily snapshots are joined by weekly
values from before the first sync: during historical backfill the worker reads DEGIRO's
own position report for up to 120 missing Fridays since the account's first
transaction, newest first, so a long history fills in over a few syncs. Each
report values the securities held at the end of that day at its closing price;
cash rows are left out to match the synced holdings. The report route was
already on the policy proxy's read/report allowlist. A synced snapshot replaces
the report value for the same day, and a failed or partial backfill is retried
on the next manual sync or the next daily automatic history attempt without failing the portfolio sync.

The chart compares portfolio value with **net invested**: cumulative gross
purchases minus sale proceeds in the same currency through each plotted day.
**Trading P&L** is portfolio value minus net invested, including realized and
unrealized trading gains. It is before fees and taxes and excludes cash and
dividends; transfers or missing trades can distort it. Net invested is not the
cash deposited into DEGIRO, and this is not a time-weighted return.

The calculation uses all saved account transactions, not the dashboard's latest
100 rows. It appears after the full transaction-history import completes, and
stays unavailable for currencies with no saved trades or when historical trade
costs cannot be matched to converted portfolio values. Changing the displayed
period never resets the cumulative investment basis. Historical market values
still come from DEGIRO reports and snapshots, never transaction prices.

Hover or touch anywhere in the plot to inspect the nearest date, exact value,
net invested and trading P&L. Keyboard users can focus the chart and use Left,
Right, Home and End to inspect points, or Escape to dismiss. The simulator has
the same controls for projected value, contributions and simulated growth.

The growth simulator starts from the selected currency's latest saved value,
and its monthly contribution defaults to that currency's average net buys per
month over the last 12 months (gross buys minus sells, never below zero; over
the months covered when the saved transactions start less than a year ago).
Starting value, monthly contribution, hypothetical annual return and horizon are
editable. It uses effective monthly compounding and month-end contributions,
and shows contributed capital separately from simulated growth. It excludes
fees, tax, inflation and market volatility. The 5% default is an illustrative
assumption, not a forecast.

Older transactions with missing labels or currencies are repaired from other
saved trades for the same product ID. Conflicting known currencies are left
unresolved. Each pull refreshes transactions from a week before the previous
successful pull.

Holdings and transactions show the fund issuer's logo, matched from the
DEGIRO product name (or a known ticker when the row has no name). Vanguard,
iShares, Amundi and VanEck logos are bundled in `internal/server/static/logos`:
public-domain wordmarks from Wikimedia Commons, reduced to plain drawing
elements with no scripts, links or styles, because an SVG opened directly runs
under the dashboard's own origin. Other known issuers get a text badge. No logo
is fetched from the internet, so the image CSP stays `'self'`.

The annual-return panel also shows each holding's own price return from
DEGIRO's weekly closes (the position reports behind the value history), in the
holding's currency: 1, 3 and 5 years where the security was held at the start,
plus since first held, annualized only over a year or more. It covers funds with
no issuer reference too, such as iShares, whose site refuses automated factsheet
downloads. It is a price return while held: income is included only for
accumulating funds, and your own buys and sells do not affect it.

Run projection math checks with `node --test scripts/tests/portfolio.test.cjs`.

The annual-return info panel matches current holdings in the selected currency
against verified issuer references. VWCE (and the same share class's VWRA/VWRP
tickers) uses the official Vanguard factsheet. Its published annualized returns
are in USD, explicitly distinguished from EUR holdings, with EUR estimates from
ECB reference rates. Unknown holdings receive no substituted return, and mixed
holdings are not averaged.

The figures are dated issuer snapshots in the generated
`internal/server/static/fund-returns.js`, not a live market-data feed. The
weekly **Update fund returns** workflow (`scripts/update-fund-returns.py`)
re-reads each factsheet and the ECB rates for every period start, commits the
file when the factsheet has a newer as-of date, and dispatches **Publish
images**. It fails instead of writing figures it cannot parse or that fall
outside plausible ranges. The server never fetches this data itself. To add a
fund, add an entry with a parser for its issuer's factsheet layout to `FUNDS`.

For EUR holdings the return panel additionally computes approximate EUR annualized
returns from the rounded issuer USD figures and dated ECB USD-per-EUR rates:
`(1 + rUSD) * (FXstart / FXend)^(1 / years) - 1`.
The 3-year period uses the preceding business-day fixing (29 September 2023);
since inception uses actual days divided by 365.25. Fixing times and issuer
rounding mean these are estimates rather than official EUR exchange-price
returns. Exact rates, dates, and primary sources are disclosed in the panel.
