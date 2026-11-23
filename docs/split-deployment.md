# Advanced split deployment

Use this deployment when you want separate container networks and credential mounts. The default deployment runs everything in one container; this stack keeps the app, worker, proxy and gateway isolated.

```sh
docker compose -f docker-compose.split.yml up -d --build
docker compose -f docker-compose.split.yml exec app degitide setup
docker compose -f docker-compose.split.yml exec app degitide connect
```

Existing split installations should continue using `docker-compose.split.yml` with the same Compose project name and `.env`. Its service and volume names are unchanged. Switching to the default file does not migrate the existing database, session or key volumes; keep the split file to retain them. Never delete volumes during an upgrade.

For notifications, add `COMPOSE_PROFILES=notifications` and `DEGITIDE_NOTIFY_URL=http://notifier:9380/notify` alongside the provider settings in `.env`. Apply them with the same split Compose command.

The legacy native login fallback is `sh scripts/native-login.sh`. It temporarily enables login at the proxy and stops the worker, then restores normal mode. If interrupted before cleanup, run `docker compose -f docker-compose.split.yml up -d --force-recreate --remove-orphans`. This helper supports the default broker account; the regular CLI and dashboard support every account.

## Container security model

```text
127.0.0.1:8391
      |
dashboard gateway (no volumes or secrets) -- dashboard-ingress
      |
    Go app ---- SQLite volume
      |
  app-worker
      |
browser worker ---- bounded session volume + public proxy CA
      |              Chromium runtime profile on tmpfs only
      |
worker-egress
      |
policy proxy ---- internet ---- trader.degiro.nl:443 + two exact VWD quote hosts

web reauthentication:
authenticated browser -- HTTPS --> app -- isolated app-worker --> worker
                                                           | credentials in memory for one task
                                                           | task-scoped login capability header
                                                           +-- worker-egress --> policy proxy

native fallback:
local terminal -- stdin --> one-shot native-login container
                              | bounded session volume + public CA only
                              | Chromium runtime profile on tmpfs only
                              +-- worker-egress --> policy proxy
```

- The app, browser worker, policy proxy, and ingress gateways use separate internal networks for each intended edge. The proxy cannot resolve the app, the dashboard gateway cannot resolve the worker, and a direct connection from the worker to an external IP has no route.
- The policy proxy is the only component connected to both the worker-facing internal network and an egress network. It decrypts HTTPS so it can validate the real HTTP method, path, query keys, and bounded body—not merely the hostname in the outer `CONNECT`.
- Optional notifications use a separate relay container and egress network. The relay receives only an account label, ID, and failure/recovery state; it has no database, DEGIRO session, or vault-key mount. Enabling it does not give the app or worker direct internet access.
- The worker image refuses to start unless the policy proxy and its public CA are present. QUIC and browser DNS-over-HTTPS are disabled, and Chromium's implicit loopback bypass is removed so browser requests to local addresses also pass through the deny policy.
- The dashboard is published through a tiny non-root gateway because Docker Desktop cannot publish a port from an internal-only network. The gateway has no mounted data, profile, or secret files, can reach only the app-side ingress network, and is bound to loopback only.
- Web reauthentication is available only to an authenticated dashboard session over HTTPS (or loopback). The app relays one bounded JSON request over the isolated, bearer-authenticated `app-worker` network. The worker adds a separate capability only while that serialized browser task runs; the proxy consumes and strips it before upstream, so normal requests cannot open DEGIRO's credential-submission routes and the capability never reaches DEGIRO.
- The native fallback uses a profile-gated one-shot service with only the bounded-session volume, public proxy CA, and `worker-egress` network. It cannot resolve the app or dashboard, has only the encryption-key mount in addition to the session and public CA mounts or published port, and exits after writing the bounded session checkpoint. There is no noVNC/websockify/x11vnc service.
- Chromium's full user-data directory is created under container `/tmp` and removed on exit. The persistent volume retains only the bounded DEGIRO cookie and exact-origin storage checkpoint, not browser cache, history, autofill, or password databases.
- Long-running containers run as distinct non-root users with no Linux capabilities, read-only root filesystems, `no-new-privileges`, private writable volumes, and narrowly shared secret-file permissions. The networkless one-shot bootstrap runs as root with only `CHOWN` and `FOWNER` so it can initialize named-volume ownership, then exits.

### Allowed egress scope

Normal sync mode allows HTTPS on port 443 to `trader.degiro.nl` for these reviewed request classes:

- static login/trader shell assets and configuration;
- bounded login translation loading for the selected two-letter country/language and module list;
- account/session bootstrap needed to discover the numeric account and session identifiers;
- transaction report `GET` requests and cash/position CSV report downloads;
- portfolio update `GET` requests;
- bounded product-ID metadata lookups;
- exact read-only account and trader-settings bootstrap requests.

The calculated portfolio also needs its event worker and quote stream. The
worker bootstrap may import only the three hashed, same-origin runtime/vendor/
frontend-core dependencies. On the exact hosts
`degiro.quotecast.vwdservices.com` and `quotecast.vwdservices.com`, the policy
allows bounded VWD quote-session creation, polling, and `req`/`rel` field
subscriptions. Broker cookies, authorization and internal capability headers
are stripped from requests to these quote hosts. Other paths on them are denied.

Everything else is denied, including other hosts, HTTP, alternate ports, unknown paths, unexpected query fields, oversized/malformed bodies, userinfo and fragment tricks, recursively encoded traversal, tunneled `CONNECT`, WebSocket/other protocol upgrades, and unreviewed methods. Known `checkOrder`, create-order, update-order, and delete-order requests are explicitly covered by denial tests. The holdings update response may include readable account/portfolio data; the security boundary prevents arbitrary exfiltration and trade mutations, not the worker's ability to read the data it is meant to sync.

In normal mode the `/login/secure/login...` `POST` subtree requires a separate generated capability whose header the worker attaches only during the serialized reauthentication task; the proxy strips that internal header before forwarding the request. The browser clears it in a `finally` path, and if clearing fails the worker closes the entire ephemeral browser context. The native helper remains a fallback: it temporarily recreates the policy proxy with that login subtree enabled, stops the long-running worker to prevent profile contention, runs the isolated login container, and then restores normal mode. Logout, known order mutations, and every unreviewed request remain denied in both paths.

### Credentials and session storage

The authenticated dashboard accepts a DEGIRO username, password, and base32 authenticator setup secret. After a verified login, the worker stores the credentials and bounded session using Fernet authenticated encryption. Codes are generated immediately before submission using standard six-digit, 30-second SHA-1 TOTP. The legacy API/native flow still accepts a fresh code for a session-only login; a successful web code-only login removes previously saved automatic-login credentials.

The encryption key lives in the separate `degitide-vault-key` volume, mounted read-only into the worker and native login helper. Bootstrap creates it once; normal worker restarts and container recreation retain it. The key file is root-owned and readable by the worker group only. Neither the app nor policy proxy mounts this volume. Protect the host, Docker daemon, key volume, and backups: anyone with both the key and ciphertext can recover the credentials. Encryption does not protect against a compromised running worker.

A session check first tries the saved session. If the trader page ends at the login page, the worker waits five seconds and checks once more on a fresh page, so a single transient redirect is not reported as expiry. Only when both checks require login does the session count as expired. If it has expired, the worker uses encrypted credentials to log in through the existing capability-scoped proxy flow. A persisted 15-minute delay between automatic attempts limits repeated failures across restarts. Failed login attempts do not replace saved credentials. Reconnect manually to update credentials. Older plaintext session checkpoints are ignored and replaced with encrypted state after a new login; previously copied files and backups are not erased by this upgrade.

Credentials pass transiently through browser, app, worker, and TLS proxy memory. Password and secret fields are cleared on dispatch. The proxy logs routes and methods, not request bodies. Session and credential files have mode 0600. The native helper streams credentials through stdin and also writes encrypted session state. Neither flow reads `CREDENTIALS.md`.

Back up the encryption key separately and securely. Losing it requires reconnecting the account. Local non-container workers must provide `DEGITIDE_VAULT_KEY_FILE` pointing to a Fernet key file.

Each added account has its own session and credentials under `accounts/<id>` in the browser-profile volume. The existing installation remains the `default` account and retains its original storage paths. The worker serializes tasks and closes the current browser before switching accounts. Reconnecting a saved account with a different DEGIRO username is rejected; add a separate account instead.

## What this does not protect against

- A copied session checkpoint can potentially be replayed somewhere without this egress policy and used with the full privileges DEGIRO grants it.
- Host root, Docker-daemon access, a container/kernel escape, or replacement of the proxy image/config can bypass the model.
- The TLS policy proxy is trusted by the ephemeral Chromium runtime. Its private CA key must remain confined to its named volume.
- Loopback setup/login may send the chosen Degitide administrator password over loopback HTTP. Remote setup/login is accepted only when the request is HTTPS or a trusted reverse proxy forwards the original HTTPS scheme. Keep the Docker port loopback-only, add the proxy hostname to `DEGITIDE_ALLOWED_HOSTS`, and use an authenticated encrypted private-network proxy.
- Browser automation may break or violate service terms. Policy updates should be based on captured read-only traffic and accompanied by denial regression tests.
