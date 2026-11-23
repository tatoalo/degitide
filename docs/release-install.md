# Degitide @VERSION@

Download assets from the [GitHub release](https://github.com/@OWNER@/degitide/releases/tag/@VERSION@).

## Server

Use Docker Engine with Compose v2 on Linux amd64 or arm64. Download `degitide_@VERSION@_docker.tar.gz` and `SHA256SUMS` to the server, then verify the archive:

```sh
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf degitide_@VERSION@_docker.tar.gz
cd degitide
cp .env.example .env
docker compose pull
docker compose up -d --no-build
docker compose exec app degitide setup
docker compose exec app degitide connect
docker compose exec app degitide sync
```

The default is one container, one persistent volume at `/data`, and one port at `127.0.0.1:8391`. The bundle pins the image to this release and requires no source checkout.

The published `latest` image tag tracks stable releases. The `main` tag follows development builds between releases; prerelease tags do not update `latest`. This bundle uses an explicit version tag so pulling it again does not opt into a newer release.

For an existing split installation or separate network/credential isolation, use `docker compose -f docker-compose.split.yml` for every command above. Keep your existing Compose project name and `.env`; the split service and volume names are unchanged. See [advanced deployment](docs/split-deployment.md). Switching files does not migrate data between layouts.

## Automatic one-way sync into Sure

The bundle includes the Compose override and [compatibility adapter instructions](integrations/sure/README.md). Follow the [versioned network and connection setup](https://github.com/@OWNER@/degitide/blob/@VERSION@/docs/usage.md#automatic-one-way-sync-into-sure).

Existing connections also need the bundled adapter updated in **both Sure web
and worker**, then those services recreated with the same Compose overrides.
This release includes adapter **2**, which imports saved historical valuations
without enabling purchases as spending. Replacing only the Degitide image will
not update a separately copied adapter file on the Sure host.

After both services report adapter version `2`, run a Degitide sync and then
sync its existing connection in Sure. Do not create a second connection or
delete the opening value manually: the adapter repairs an untouched automatic
opening value and preserves user edits. Historical report refreshes are bounded
to 120 dates per sync, so older base-currency history may need several syncs.
Compare Sure with Degitide's **holdings value** and DEGIRO's **portfolio**;
DEGIRO's **balance** also includes cash. Quote timestamps can differ, and an
explicit unavailable/closing-price message means the values are not all live.

## Laptop CLI

Choose `darwin` for macOS or `linux` for Linux, with `arm64` for Apple Silicon/ARM or `amd64` for Intel/AMD. Download that archive and `SHA256SUMS`; compare its digest using `shasum -a 256 <archive>` on macOS or `sha256sum <archive>` on Linux.

For example, on Apple Silicon:

```sh
tar -xzf degitide_@VERSION@_darwin_arm64.tar.gz
mkdir -p "$HOME/.local/bin"
install -m 755 degitide "$HOME/.local/bin/degitide"
"$HOME/.local/bin/degitide" version
"$HOME/.local/bin/degitide" open --ssh user@your-vps
```

The CLI uses your installed SSH client and normal SSH configuration. The tunnel stays in the foreground; Ctrl+C closes it while server syncs continue. macOS binaries are not Developer ID signed or notarized.

## Updates

Back up the entire `degitide` volume, including the encryption key under `/data/secrets`. For the advanced stack, back up its existing data, browser-profile, vault-key and secret volumes. Stop the stack with `docker compose down` without `--volumes`, extract the next Docker bundle over the same directory, then run `docker compose pull` and `docker compose up -d --no-build`. Continue passing `-f docker-compose.split.yml` if using the advanced layout. Keep your existing `.env`; do not repeat setup. The project name stays `degitide` across releases. Existing installations with a different project name must retain it through `COMPOSE_PROJECT_NAME`.

Keep passing your existing network override, including `-f docker-compose.sure.yml`
when used, so Sure retains access to Degitide after the update. The initial sync
refreshes the full transaction report once and migrates the saved valuation
schema; keep the pre-upgrade volume backup if you need to roll back.

To adopt market-aware sync on an existing installation, set `DEGITIDE_SCHEDULE=market`
in your retained `.env` and use the new Compose files. Old explicit cron values
remain overrides. The default refreshes every hour while a held stock/ETF
market is open, 20 minutes after close, and at 02:00 Dublin when a safety refresh
is needed before Sure's overnight read. The app, worker and proxy must all be
updated; the dashboard shows discovered venues, unknown coverage and the next
sync. Scheduled collection still depends on the broker's quote availability.

Account management and optional Telegram/Pushover settings are documented in the [operating guide](https://github.com/@OWNER@/degitide/blob/@VERSION@/docs/usage.md).
