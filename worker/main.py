"""
degitide browser worker.

Lightweight aiohttp server that manages an ephemeral Patchright browser runtime
and a bounded persistent session checkpoint. Exposes task-based endpoints for
the Go service to call.

Binds 127.0.0.1:9377.
"""

from __future__ import annotations

import asyncio
import hmac
import importlib.metadata
import json
import logging
import os
import platform
import re
import shutil
import signal
import tempfile
import time
from pathlib import Path
from typing import Awaitable, Callable

from aiohttp import web
from patchright.async_api import BrowserContext, Playwright, async_playwright

from tasks import holdings, login, positions, reauthenticate as reauthentication, transactions
from tasks.common import TaskError
import vault
from cryptography.fernet import InvalidToken

# --- Globals ---

PROFILE_DIR = Path(os.environ.get("DEGITIDE_BROWSER_PROFILE", "./browser-profile"))
SESSION_STATE_MAX_BYTES = 1024 * 1024
PROXY = os.environ.get("DEGITIDE_PROXY", "")
WORKER_TOKEN_FILE = os.environ.get("DEGITIDE_WORKER_TOKEN_FILE", "")
LOGIN_CAPABILITY_FILE = os.environ.get("DEGITIDE_PROXY_LOGIN_TOKEN_FILE", "")
WORKER_TOKEN_KEY = web.AppKey("worker_token", str)
LOGIN_CAPABILITY_KEY = web.AppKey("login_capability", str)
LOGIN_CAPABILITY_HEADER = "X-Degitide-Login-Capability"
BROWSER_IDLE_SECONDS = int(os.environ.get("DEGITIDE_BROWSER_IDLE_SECONDS", "300"))
if BROWSER_IDLE_SECONDS < 0:
    raise RuntimeError("DEGITIDE_BROWSER_IDLE_SECONDS cannot be negative")
# Pause before the single confirming session check. Long enough for a
# momentary bounce to clear, short enough that a real logout is still
# reported within seconds.
SESSION_RECHECK_DELAY_SECONDS = 5

browser: BrowserContext | None = None
playwright: Playwright | None = None
runtime_profile_dir: Path | None = None
task_lock = asyncio.Lock()
browser_idle_handle: asyncio.TimerHandle | None = None
browser_activity_generation = 0
active_account = "default"

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
log = logging.getLogger("degitide-worker")


def _proxy_settings(proxy: str) -> dict[str, str] | None:
    if not proxy:
        return None
    # Chromium implicitly bypasses proxies for loopback. Remove that exception
    # so even browser requests to local/container services hit the deny policy.
    return {"server": proxy, "bypass": "<-loopback>"}


def _session_state_path() -> Path:
    configured = os.environ.get("DEGITIDE_SESSION_STATE", "")
    return Path(configured) if configured and active_account == "default" else _account_dir() / "session-cookies.json"


def _account_dir() -> Path:
    return PROFILE_DIR if active_account == "default" else PROFILE_DIR / "accounts" / active_account


def _request_account(request: web.Request) -> str:
    account = request.query.get("account", "default")
    if re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,63}", account) is None:
        raise web.HTTPBadRequest(text='{"error":"invalid account"}', content_type="application/json")
    return account


async def _select_account(account: str) -> None:
    """Only called with task_lock held. Close before changing the vault path."""
    global active_account
    if re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,63}", account) is None:
        raise TaskError("invalid account")
    if account != active_account:
        await shutdown_browser()
        active_account = account
    directory = _account_dir()
    # Never follow a symlink into another account or outside the vault.
    if directory.is_symlink() or (PROFILE_DIR / "accounts").is_symlink():
        raise TaskError("invalid account storage")
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)


def _prune_persistent_profile() -> None:
    """Keep only the bounded session checkpoint in the persistent volume."""
    directory = _account_dir()
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    directory.chmod(0o700)
    session_path = _session_state_path().absolute()
    for child in directory.iterdir():
        if active_account == "default" and child.name == "accounts" and child.is_dir() and not child.is_symlink():
            continue
        if (
            child.absolute() in (session_path, (directory / "credentials.enc").absolute())
            and child.is_file()
            and not child.is_symlink()
        ):
            continue
        if child.is_dir() and not child.is_symlink():
            shutil.rmtree(child)
        else:
            child.unlink(missing_ok=True)


def _new_runtime_profile_dir() -> Path:
    """Create an ephemeral Chromium user-data directory on container tmpfs."""
    path = Path(tempfile.mkdtemp(prefix="degitide-chromium-"))
    path.chmod(0o700)
    return path


def _cleanup_runtime_profile() -> None:
    global runtime_profile_dir
    if runtime_profile_dir is not None:
        shutil.rmtree(runtime_profile_dir, ignore_errors=True)
        runtime_profile_dir = None


def _is_degiro_cookie(cookie: object) -> bool:
    if not isinstance(cookie, dict):
        return False
    required = ("name", "value", "domain", "path")
    if not all(isinstance(cookie.get(key), str) for key in required):
        return False
    domain = cookie["domain"].lower().lstrip(".")
    return domain == "degiro.nl" or domain.endswith(".degiro.nl")


def _is_bounded_storage_entry(entry: object) -> bool:
    return (
        isinstance(entry, dict)
        and isinstance(entry.get("name"), str)
        and isinstance(entry.get("value"), str)
        and 0 < len(entry["name"]) <= 256
        and len(entry["value"].encode("utf-8")) <= 64 * 1024
    )


def _filter_storage_origins(origins: object) -> list[dict]:
    if not isinstance(origins, list) or len(origins) > 8:
        return []
    filtered: list[dict] = []
    for origin in origins:
        if not isinstance(origin, dict) or origin.get("origin") != "https://trader.degiro.nl":
            continue
        local = origin.get("localStorage", [])
        session = origin.get("sessionStorage", [])
        if not isinstance(local, list) or not isinstance(session, list):
            continue
        if len(local) > 256 or len(session) > 256:
            continue
        filtered.append(
            {
                "origin": "https://trader.degiro.nl",
                "localStorage": [entry for entry in local if _is_bounded_storage_entry(entry)],
                "sessionStorage": [entry for entry in session if _is_bounded_storage_entry(entry)],
            }
        )
    return filtered


def _read_session_state(path: Path) -> dict[str, list[dict]]:
    try:
        if path.stat().st_size > SESSION_STATE_MAX_BYTES:
            raise ValueError("session state is too large")
        payload = json.loads(vault.unseal(path.read_text(encoding="utf-8"), "session"))
    except FileNotFoundError:
        return {"cookies": [], "origins": []}
    except (OSError, UnicodeError, json.JSONDecodeError, ValueError, InvalidToken) as exc:
        log.warning("ignoring invalid saved browser session: %s", type(exc).__name__)
        return {"cookies": [], "origins": []}

    cookies = payload.get("cookies") if isinstance(payload, dict) else None
    if not isinstance(cookies, list) or len(cookies) > 256:
        log.warning("ignoring invalid saved browser session: cookie list")
        return {"cookies": [], "origins": []}
    return {
        "cookies": [cookie for cookie in cookies if _is_degiro_cookie(cookie)],
        "origins": _filter_storage_origins(payload.get("origins", [])),
    }


def _read_session_cookies(path: Path) -> list[dict]:
    return _read_session_state(path)["cookies"]


def _write_session_state(path: Path, cookies: list[dict], origins: list[dict]) -> None:
    filtered = [cookie for cookie in cookies if _is_degiro_cookie(cookie)]
    payload = json.dumps(
        {"cookies": filtered, "origins": _filter_storage_origins(origins)},
        separators=(",", ":"),
    )
    if len(payload.encode("utf-8")) > SESSION_STATE_MAX_BYTES:
        raise ValueError("session state is too large")
    payload = vault.seal(payload, "session")
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = path.with_name(f".{path.name}.tmp")
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        path.chmod(0o600)
    finally:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass


def _write_session_cookies(path: Path, cookies: list[dict]) -> None:
    _write_session_state(path, cookies, [])


async def _restore_session(context: BrowserContext) -> None:
    state = _read_session_state(_session_state_path())
    cookies = state["cookies"]
    if cookies:
        await context.add_cookies(cookies)
    origins = state["origins"]
    if origins:
        storage_by_origin = {origin["origin"]: origin for origin in origins}
        await context.add_init_script(
            script=f"""
                (() => {{
                    const states = {json.dumps(storage_by_origin, separators=(',', ':'))};
                    const state = states[location.origin];
                    if (!state) return;
                    for (const item of state.localStorage || []) localStorage.setItem(item.name, item.value);
                    for (const item of state.sessionStorage || []) sessionStorage.setItem(item.name, item.value);
                }})();
            """
        )
    if cookies or origins:
        log.info("restored saved DEGIRO browser session")


async def _storage_origins(context: BrowserContext) -> list[dict]:
    existing = _read_session_state(_session_state_path())["origins"]
    for page in context.pages:
        if not page.url.startswith("https://trader.degiro.nl"):
            continue
        try:
            storage = await page.evaluate(
                """
                () => ({
                    origin: location.origin,
                    localStorage: Object.entries(localStorage).map(([name, value]) => ({name, value})),
                    sessionStorage: Object.entries(sessionStorage).map(([name, value]) => ({name, value})),
                })
                """
            )
        except Exception:
            continue
        filtered = _filter_storage_origins([storage])
        if filtered:
            return filtered
    return existing


async def _save_session(context: BrowserContext) -> None:
    cookies = await context.cookies()
    origins = await _storage_origins(context)
    _write_session_state(_session_state_path(), cookies, origins)
    log.info("saved DEGIRO browser session")


# --- Lifecycle ---

async def ensure_browser() -> None:
    """Launch browser if not already running (cold start)."""
    global browser, playwright, runtime_profile_dir
    if browser is None:
        log.info("cold start: launching browser")
        _prune_persistent_profile()
        if playwright is None:
            playwright = await async_playwright().start()
        runtime_profile_dir = _new_runtime_profile_dir()
        context: BrowserContext | None = None
        try:
            context = await playwright.chromium.launch_persistent_context(
                user_data_dir=str(runtime_profile_dir),
                headless=True,
                viewport={"width": 1366, "height": 900},
                locale=os.environ.get("DEGITIDE_BROWSER_LOCALE", "en-IE"),
                timezone_id=os.environ.get("DEGITIDE_BROWSER_TIMEZONE", "Europe/Dublin"),
                proxy=_proxy_settings(PROXY),
                args=["--disable-quic", "--dns-over-https-mode=off", "--disable-save-password-bubble"],
            )
            await _restore_session(context)
        except Exception:
            if context is not None:
                await context.close()
            _cleanup_runtime_profile()
            raise
        browser = context
        browser.on("close", _browser_closed)
        log.info("browser launched with ephemeral runtime profile")


def _browser_closed(_: object = None) -> None:
    global browser
    browser = None
    _cleanup_runtime_profile()


async def shutdown_browser() -> None:
    global browser, playwright
    context = browser
    driver = playwright
    # Clear the shared references before awaiting. The context close callback
    # may run while checkpointing or closing and must not turn a later
    # `browser.close()` lookup into a None dereference.
    browser = None
    playwright = None
    if context is not None:
        try:
            await _save_session(context)
        except Exception as exc:
            log.warning("could not save browser session: %s", type(exc).__name__)
        try:
            await context.close()
        except Exception as exc:
            log.warning("could not close browser runtime: %s", type(exc).__name__)
    _cleanup_runtime_profile()
    if driver is not None:
        try:
            await driver.stop()
        except Exception as exc:
            log.warning("could not stop browser driver: %s", type(exc).__name__)


async def _discard_browser_runtime(context: BrowserContext) -> None:
    """Revoke a browser context without checkpointing any partial login state."""
    global browser, playwright
    try:
        await context.close()
    except Exception as exc:
        log.warning("could not close browser during capability revocation: %s", type(exc).__name__)
    finally:
        browser = None
        _cleanup_runtime_profile()
        if playwright is not None:
            try:
                await playwright.stop()
            except Exception as exc:
                log.warning("could not stop browser driver during capability revocation: %s", type(exc).__name__)
            playwright = None


def _begin_browser_activity() -> int:
    global browser_activity_generation, browser_idle_handle
    browser_activity_generation += 1
    if browser_idle_handle is not None:
        browser_idle_handle.cancel()
        browser_idle_handle = None
    return browser_activity_generation


def _schedule_idle_shutdown(generation: int) -> None:
    global browser_idle_handle
    if (
        BROWSER_IDLE_SECONDS == 0
        or generation != browser_activity_generation
        or (browser is None and playwright is None)
    ):
        return
    if browser_idle_handle is not None:
        browser_idle_handle.cancel()
    loop = asyncio.get_running_loop()
    browser_idle_handle = loop.call_later(
        BROWSER_IDLE_SECONDS,
        lambda: loop.create_task(_shutdown_browser_if_idle(generation)),
    )


async def _shutdown_browser_if_idle(generation: int) -> None:
    global browser_activity_generation, browser_idle_handle
    browser_idle_handle = None
    async with task_lock:
        if generation != browser_activity_generation:
            return
        browser_activity_generation += 1
        log.info("browser idle for %d seconds; releasing runtime", BROWSER_IDLE_SECONDS)
        await shutdown_browser()


def _cancel_idle_shutdown() -> None:
    _begin_browser_activity()


# --- Request handlers ---

async def health(request: web.Request) -> web.Response:
    return web.json_response({"status": "ok", "browser_connected": browser is not None})


async def info(request: web.Request) -> web.Response:
    # Process metadata only: this endpoint never starts Chromium or contacts DEGIRO.
    return web.json_response({
        "version": os.environ.get("DEGITIDE_VERSION", "dev"),
        "python": platform.python_version(),
        "patchright": importlib.metadata.version("patchright"),
    })


async def run_task(
    name: str,
    task: Callable[[], Awaitable[dict]],
    *,
    checkpoint_if: Callable[[dict], bool] | None = None,
    account: str = "default",
) -> dict:
    generation = _begin_browser_activity()
    async with task_lock:
        started = time.monotonic()
        log.info("task started: %s", name)
        try:
            await _select_account(account)
            await ensure_browser()
            result = await task()
            if (
                checkpoint_if is not None
                and checkpoint_if(result)
                and browser is not None
            ):
                try:
                    await _save_session(browser)
                except Exception as exc:
                    log.warning(
                        "could not refresh browser session checkpoint: %s",
                        type(exc).__name__,
                    )
            return result
        finally:
            log.info("task finished: %s (%.2fs)", name, time.monotonic() - started)
            _schedule_idle_shutdown(generation)


async def _confirmed_session_check() -> dict[str, bool]:
    """Report expiry only when a second check on a fresh page agrees."""
    result = await login.check_session(browser)
    if result.get("valid"):
        return result
    # A warm, valid session has been seen to bounce to /login once and load
    # normally later from the same state. Check exactly once more before the
    # negative can spend the throttled stored-credential login or surface as
    # expiry.
    await asyncio.sleep(SESSION_RECHECK_DELAY_SECONDS)
    result = await login.check_session(browser)
    if result.get("valid"):
        log.info("session check: transient login redirect; recheck authenticated")
    else:
        log.info("session check: recheck still requires login")
    return result


async def _check_and_recover(capability):
    result = await _confirmed_session_check()
    path = _account_dir() / "credentials.enc"
    if result.get("valid") or not path.exists() or not capability:
        return result
    credentials = vault.load(path)
    # Persist a backoff before contacting DEGIRO, including across restarts.
    if time.time() - credentials.get("last_attempt", 0) < 900:
        return result
    credentials["last_attempt"] = time.time()
    vault.save(path, credentials)
    await _reauthenticate_browser(
        credentials["username"], credentials["password"],
        lambda: vault.code(credentials["seed"]), capability,
    )
    return await login.check_session(browser)


async def check_session(request: web.Request) -> web.Response:
    result = await run_task(
        "check-session",
        lambda: _check_and_recover(request.app[LOGIN_CAPABILITY_KEY]),
        checkpoint_if=lambda value: value.get("valid") is True,
        account=_request_account(request),
    )
    return web.json_response(result)


async def fetch_transactions(request: web.Request) -> web.Response:
    data = await request.json()
    from_date = data.get("from", "")
    to_date = data.get("to", "")
    result = await run_task(
        "fetch-transactions",
        lambda: transactions.fetch(browser, from_date, to_date),
        checkpoint_if=lambda _: True,
        account=_request_account(request),
    )
    return web.json_response(result)


async def fetch_positions(request: web.Request) -> web.Response:
    data = await request.json()
    dates = data.get("dates") if isinstance(data, dict) else None
    if not isinstance(dates, list) or not all(isinstance(day, str) for day in dates):
        return web.json_response({"error": "dates must be a list of ISO dates"}, status=400)
    result = await run_task(
        "fetch-positions",
        lambda: positions.fetch(browser, dates),
        checkpoint_if=lambda _: True,
        account=_request_account(request),
    )
    return web.json_response(result)


async def fetch_holdings(request: web.Request) -> web.Response:
    result = await run_task(
        "fetch-holdings",
        lambda: holdings.fetch(browser),
        checkpoint_if=lambda _: True,
        account=_request_account(request),
    )
    return web.json_response(result)


async def _reauthenticate_browser(
    username: str, password: str, totp: str, capability: str
) -> dict[str, bool]:
    if browser is None:
        raise TaskError("browser runtime unavailable")

    context = browser
    stage = "prepare"
    page = None

    def set_stage(value: str) -> None:
        nonlocal stage
        stage = value
        log.info("DEGIRO sign-in stage=%s", value)

    try:
        await context.set_extra_http_headers({LOGIN_CAPABILITY_HEADER: capability})
        page = await context.new_page()
        from urllib.parse import urlsplit

        async def login_response(response):
            path = urlsplit(response.url).path
            if path.startswith("/login/secure/login"):
                # Never log URLs, request values, or response bodies.
                status_code = None
                try:
                    payload = await response.json()
                    value = payload.get("status") if isinstance(payload, dict) else None
                    if type(value) is int:
                        status_code = value
                except Exception:
                    pass
                log.info("DEGIRO login response http=%s status=%s", response.status, status_code)

        page.on("response", login_response)
        await reauthentication.authenticate_page(
            page,
            username,
            password,
            totp,
            set_stage=set_stage,
        )
        stage = "save_session"
        await _save_session(context)
        return {"authenticated": True, "session_saved": True}
    except Exception as exc:
        log.warning(
            "DEGIRO reauthentication failed stage=%s error=%s",
            stage,
            type(exc).__name__,
        )
        raise TaskError(
            "DEGIRO sign-in did not complete; check the credentials and use a fresh verification code"
        ) from exc
    finally:
        capability_cleared = False
        try:
            await context.set_extra_http_headers({})
            capability_cleared = True
        except Exception as exc:
            log.warning("could not clear login capability header: %s", type(exc).__name__)
        if not capability_cleared:
            # Closing the entire ephemeral context is the fail-closed revocation
            # path if Patchright cannot remove the per-attempt header.
            await _discard_browser_runtime(context)
        elif page is not None:
            try:
                await page.close()
            except Exception as exc:
                log.warning("could not close reauthentication page: %s", type(exc).__name__)


async def reauthenticate_session(request: web.Request) -> web.Response:
    account = _request_account(request)
    capability = request.app[LOGIN_CAPABILITY_KEY]
    if not capability:
        return web.json_response(
            {"error": "web reauthentication is not configured"}, status=503
        )
    # Do not accept and retain credentials while another browser task is active.
    if task_lock.locked():
        return web.json_response(
            {"error": "the browser worker is busy; try again shortly"}, status=409
        )
    # asyncio.Lock acquisition does not yield when the lock is free. Reserve
    # the browser before reading the body so credential-bearing requests never
    # wait in memory behind a pull or session check.
    await task_lock.acquire()
    generation = _begin_browser_activity()
    started = time.monotonic()
    task_started = False
    try:
        try:
            data = await request.json()
        except (json.JSONDecodeError, UnicodeDecodeError):
            return web.json_response({"error": "invalid request"}, status=400)
        if not isinstance(data, dict):
            return web.json_response({"error": "invalid request"}, status=400)
        username = data.get("username")
        password = data.get("password")
        totp = data.get("totp")
        seed = data.get("seed", "")
        data.clear()
        if seed:
            try:
                seed = vault.normalize_seed(seed)
                totp = vault.code(seed)
            except (ValueError, TypeError, AttributeError):
                return web.json_response({"error": "invalid authenticator secret"}, status=400)
        if (
            not isinstance(username, str)
            or not 1 <= len(username) <= 256
            or not isinstance(password, str)
            or not 1 <= len(password) <= 512
            or not isinstance(totp, str)
            or re.fullmatch(r"[0-9]{6}", totp) is None
        ):
            return web.json_response({"error": "invalid sign-in fields"}, status=400)

        log.info("task started: reauthenticate")
        task_started = True
        await _select_account(account)
        credential_path = _account_dir() / "credentials.enc"
        if credential_path.exists():
            previous = vault.load(credential_path)
            if previous.get("username", "").casefold() != username.casefold():
                return web.json_response({"error": "use a new account for a different DEGIRO username"}, status=409)
        await ensure_browser()
        result = await _reauthenticate_browser(
            username, password, (lambda: vault.code(seed)) if seed else totp, capability
        )
        if seed:
            vault.save(credential_path, {"username": username, "password": password, "seed": seed})
        else:
            credential_path.unlink(missing_ok=True)
        return web.json_response(result)
    finally:
        if task_started:
            log.info(
                "task finished: reauthenticate (%.2fs)", time.monotonic() - started
            )
        task_lock.release()
        _schedule_idle_shutdown(generation)


# --- App setup ---

@web.middleware
async def errors(request: web.Request, handler: web.RequestHandler) -> web.StreamResponse:
    try:
        return await handler(request)
    except TaskError as exc:
        log.warning("task failed: %s", exc)
        return web.json_response({"error": str(exc)}, status=422)
    except web.HTTPException:
        raise
    except Exception as exc:
        log.exception("worker request failed")
        return web.json_response({"error": "internal worker error"}, status=500)


def _load_worker_token() -> str:
    if not WORKER_TOKEN_FILE:
        return ""
    try:
        return Path(WORKER_TOKEN_FILE).read_text(encoding="ascii").strip()
    except OSError as exc:
        raise RuntimeError(f"could not read worker token file: {exc}") from exc


def _load_login_capability() -> str:
    if not LOGIN_CAPABILITY_FILE:
        return ""
    try:
        value = Path(LOGIN_CAPABILITY_FILE).read_text(encoding="ascii").strip()
    except OSError as exc:
        raise RuntimeError(f"could not read proxy login capability: {exc}") from exc
    if re.fullmatch(r"[A-Za-z0-9_-]{32,256}", value) is None:
        raise RuntimeError("proxy login capability is invalid")
    return value


@web.middleware
async def authenticate(request: web.Request, handler: web.RequestHandler) -> web.StreamResponse:
    if request.path == "/health":
        return await handler(request)
    expected = request.app[WORKER_TOKEN_KEY]
    if expected:
        scheme, _, supplied = request.headers.get("Authorization", "").partition(" ")
        if scheme.lower() != "bearer" or not hmac.compare_digest(supplied, expected):
            return web.json_response(
                {"error": "worker authentication required"},
                status=401,
                headers={"WWW-Authenticate": "Bearer"},
            )
    return await handler(request)


def create_app() -> web.Application:
    token = _load_worker_token()
    login_capability = _load_login_capability()
    bind = os.environ.get("DEGITIDE_WORKER_BIND", "127.0.0.1")
    if bind not in ("127.0.0.1", "::1", "localhost") and not token:
        raise RuntimeError("a worker token is required when binding outside loopback")
    app = web.Application(middlewares=[errors, authenticate], client_max_size=64 * 1024)
    app[WORKER_TOKEN_KEY] = token
    app[LOGIN_CAPABILITY_KEY] = login_capability
    app.router.add_get("/health", health)
    app.router.add_get("/info", info)
    app.router.add_get("/tasks/check-session", check_session)
    app.router.add_post("/tasks/fetch-transactions", fetch_transactions)
    app.router.add_post("/tasks/fetch-holdings", fetch_holdings)
    app.router.add_post("/tasks/fetch-positions", fetch_positions)
    app.router.add_post("/tasks/reauthenticate", reauthenticate_session)
    return app


async def main():
    os.umask(0o077)
    runner: web.AppRunner | None = None
    try:
        app = create_app()
        # Health checks are intentionally frequent during startup and do not
        # need one access-log line per successful probe. Task lifecycle and
        # failures are logged explicitly above.
        runner = web.AppRunner(app, access_log=None)
        await runner.setup()
        host = os.environ.get("DEGITIDE_WORKER_BIND", "127.0.0.1")
        port = int(os.environ.get("DEGITIDE_WORKER_PORT", "9377"))
        site = web.TCPSite(runner, host, port)
        await site.start()
        log.info("worker listening on %s:%d", host, port)

        # Block until signal
        stop = asyncio.get_event_loop().create_future()
        for sig in (signal.SIGINT, signal.SIGTERM):
            try:
                asyncio.get_event_loop().add_signal_handler(
                    sig, lambda: not stop.done() and stop.set_result(None)
                )
            except NotImplementedError:
                pass

        await stop

    finally:
        _cancel_idle_shutdown()
        if runner:
            await runner.cleanup()
        await shutdown_browser()
        log.info("worker shut down cleanly")


if __name__ == "__main__":
    asyncio.run(main())
