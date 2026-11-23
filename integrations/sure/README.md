# Sure integration: holdings and value history

Tested on 2026-11-16 against the official **Sure v0.7.6** Docker image:

```text
ghcr.io/we-promise/sure@sha256:61cd4c1fa9917200f8db4434b45d968235f375c52cdbdc81bf4a4df555a2f090
```

The unmodified release imported initial holdings and partial sales correctly, but retained a fully sold position (six shares of the synthetic `fund-a`) and ignored `holdings: []`. This left current holdings inconsistent with the feed's securities balance.

`degitide_simplefin.rb` is a Rails initializer that corrects those two paths and imports recorded portfolio value history for explicitly marked, complete Degitide snapshots. The importer persists empty holdings and the current metadata atomically. The holdings processor writes a dated zero position for an instrument absent from the complete snapshot. It preserves past dates, manual holdings, provider ownership, security remapping and locked cost basis. Missing, failed, incomplete, invalid or stale snapshots do not close positions. Other SimpleFIN providers continue through Sure's original code.

When a manual or other-provider row occupies the same instrument/date, it remains untouched. Current holdings use the last accepted complete Degitide snapshot to hide the closed provider position without replacing that row. Already-zero positions do not accumulate daily zero rows.

## Historical portfolio values

The standard SimpleFIN balance and holdings fields describe one current snapshot. They do not transfer historical portfolio values. Without extra support, Sure can create a default opening value two years ago and reconstruct a misleading flat series from the latest holdings.

Degitide now includes a versioned `extra.degitide.balance_history` extension containing every saved observation for that broker account and currency. It includes weekly closing values from DEGIRO position reports and later synced snapshots, with the snapshot taking precedence on the same day. The extension is independent of SimpleFIN's transaction date window and is available with trade activity disabled.

Adapter version **2** imports these observations as native Sure `Valuation` entries, each with a stable source/date identity. They are account valuations, not purchases, spending, income, or cash deposits. Sure's linked-investment chart bounds are extended to include these provider-owned observations. The adapter replaces Sure's untouched, automatically generated two-year opening anchor with the earliest actual observation; it leaves manually dated, previously edited, imported, locked and user-modified opening values intact. If older observations arrive on later backfill syncs, an unmodified adapter-owned opening moves earlier and its previous date remains an observed valuation.

Manual valuations, user edits, locks, exclusions and other-provider entries always take precedence on a conflicting date. An empty or missing history never deletes previously imported observations. Malformed, unordered, duplicate, future-dated or inconsistent advertised history rejects the snapshot before import. Current-day history is validated against the current balance; Sure continues to obtain that day's value from the current snapshot. The snapshot day follows the calendar date and UTC offset in `snapshot_at`, matching Degitide's stored observation date even around midnight.

History remains **securities only**. Weekly observations are not daily market prices, and Sure's calculations between observed dates are estimates. These valuations do not reconstruct historical cash, individual security holdings, deposits, dividends, FX cash flows, or investment returns. In particular, value growth includes purchases and must not be interpreted as investment performance.

Keep **transaction activity disabled**, the default. Sure v0.7.6 imports optional SimpleFIN BUY/SELL activity as ordinary ledger entries, not native investment trades; enabling it can make purchases appear as spending. Historical valuation transfer does not require enabling it.

The extension contract is:

```json
{
  "extra": {
    "degitide": {
      "version": 1,
      "complete_holdings": true,
      "snapshot_at": "2026-11-16T16:05:00Z",
      "history_version": 1,
      "history_scope": "securities_only",
      "balance_history": [
        {"date": "2024-10-25", "balance": "500.00000000", "source": "report"},
        {"date": "2024-11-01", "balance": "515.00000000", "source": "report"}
      ]
    }
  }
}
```

The adapter release version and payload protocol version are separate: adapter `VERSION` is `2`, while the backwards-compatible snapshot marker remains `version: 1`. Older adapters ignore the history extension. Without adapter v2, the feed alone does not fix Sure's historical chart.

This is a compatibility adapter maintained here, **not a change released by Sure**. Its use of Sure internals is version-specific. Test a different Sure image before upgrading; remove the adapter only after verifying equivalent upstream behavior.

Upstream work is tracked in [Sure #4893](https://github.com/we-promise/sure/pull/4893) for current/empty snapshots and [Sure #4255](https://github.com/we-promise/sure/pull/4255) for positions missing from a nonempty snapshot. Keep the adapter until the deployed release handles both paths.

## Install in Sure

Keep the adapter file on the Docker host. From your Sure deployment directory, add the supplied override to your normal Compose invocation:

```bash
export DEGITIDE_SOURCE=/absolute/path/to/degitide
docker compose -f compose.yml \
  -f "$DEGITIDE_SOURCE/integrations/sure/compose.compat.yml" \
  up -d --force-recreate web worker
```

Adjust `compose.yml`, `web` and `worker` if your deployment uses different names. Include the override in future updates too. It mounts the initializer read-only at `/rails/config/initializers/degitide_simplefin.rb` in **both** Rails processes. Restarting without recreating containers will not add a new mount. The override does not connect the Docker networks; follow the private-network instructions in the [main README](../../README.md#automatic-one-way-sync-into-sure).

Confirm each process loads the adapter:

```bash
docker compose -f compose.yml \
  -f "$DEGITIDE_SOURCE/integrations/sure/compose.compat.yml" \
  exec web bin/rails runner 'puts DegitideSimplefin::VERSION'
docker compose -f compose.yml \
  -f "$DEGITIDE_SOURCE/integrations/sure/compose.compat.yml" \
  exec worker bin/rails runner 'puts DegitideSimplefin::VERSION'
```

Both should print `2`. Complete a fresh Degitide pull, then sync the existing connection in Sure. No reconnection or historical-data deletion is required. Already imported positions with the connector's stable `CUSTOM:DEGIRO_…` identity are reconciled on that sync.

## Run the real integration suite

From this repository, with Docker Compose and Python 3 available:

```bash
make test-sure
```

This builds the real Go application and starts official Sure, Sidekiq, PostgreSQL and Redis containers. Only the broker HTTP boundary is a deterministic fixture; it never accesses DEGIRO, saved browser sessions or production databases. Account creation, token claim, account linking, syncing, revocation and reconnection use the real CLI or HTTP controllers. Assertions read Sure's persisted models and rendered holdings pages, including the `current_holdings` relation used by its UI.

The fixed project name is `degitide-sure-e2e`, with separate named volumes and a dedicated network. Published ports bind only to `127.0.0.1` and are assigned by Docker. The test prints the current URLs and a temporary directory containing JSON portfolio snapshots. Existing application containers are unaffected. These are deliberately public **test-only** credentials:

| Service | Username | Password |
| --- | --- | --- |
| Sure | `integration@example.invalid` | `Sure-integration-2026!` |
| Degitide | `integration` | `Sure-integration-2026!` |

The suite covers:

- Initial import across two broker accounts and EUR/USD, including identical product and transaction IDs in different accounts.
- Partial sale, full sale, all positions sold, and reopening a sold instrument.
- More than 100 varying historical observations across two accounts, predating Sure's two-year opening default; native Valuation amounts and materialized chart balances agree on every reported date.
- No ordinary spending transactions in the default connection, plus stable valuation and holding IDs on repeat syncs. The optional `--with-transactions` runner flag exercises all nine gross ledger activity entries.
- A failed broker pull: the previously imported portfolio survives and Sure exposes the error.
- Sure's recurring `SyncAllJob` executed through Sidekiq, including its family/provider syncs, without either UI open. The suite enqueues it directly instead of waiting for the daily timer.
- A sync queued while Sidekiq is stopped, followed by worker recovery.
- Restart of the entire stack, including PostgreSQL, Redis and Degitide, with credentials, zero positions and record IDs preserved.
- Access revocation followed by Sure's reconnect flow, preserving account links and records.
- Real Sure model checks for historical/manual holdings, security remapping, locked cost basis, manual valuation/date collisions, edited or locked opening anchors, empty-history preservation, aggregate chart coverage, and rejected incomplete/missing/stale/future/invalid/other-provider snapshots. These checks roll back their temporary database changes.

CI runs this suite and removes its test volumes afterward. Locally it leaves the stack available for inspection. To remove **only this synthetic stack and its data**, or before testing from empty volumes:

```bash
make test-sure-down
```

To reproduce the original upstream failure without the adapter, start from empty test volumes:

```bash
make test-sure-down
docker compose -f tests/integration/sure/compose.yml up -d --build --wait
python3 tests/integration/sure/run.py --stock
```

The stock run is expected to fail at the full-sale assertion. To restore the supported configuration, run `make test-sure-down` then `make test-sure`.

Cash, dividends, FX cash flows and native Sure investment trades remain outside this connector. Historical account valuations use recorded DEGIRO report values; the tests use synthetic observations and do not validate a live broker session or historical per-security prices.
