"""One-shot native DEGIRO login for the login-only Docker profile.

The process reads username, password, and a fresh TOTP as three lines on stdin.
It never exposes credentials through the dashboard or worker HTTP APIs.
"""

from __future__ import annotations

import asyncio
import json
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path
from urllib.parse import parse_qsl, urlsplit

from patchright.async_api import async_playwright

from main import (
    PROFILE_DIR,
    PROXY,
    _new_runtime_profile_dir,
    _proxy_settings,
    _prune_persistent_profile,
    _write_session_state,
)
from tasks.reauthenticate import (
    authenticate_page,
    storage_snapshot,
)


CURRENT_STAGE = "startup"


def _request_schema(method: str, url: str) -> dict[str, object]:
    parsed = urlsplit(url)
    path = re.sub(r"(?<=/)[0-9]+(?=/|;|$)", "{id}", parsed.path)
    path = re.sub(r";jsessionid=[A-Za-z0-9._~-]+", ";jsessionid={token}", path)
    return {
        "method": method.upper(),
        "path": path,
        "query_keys": sorted({key for key, _ in parse_qsl(parsed.query)}),
    }


def _read_credentials() -> tuple[str, str, str]:
    values = sys.stdin.read().splitlines()
    if len(values) != 3 or not all(values):
        raise RuntimeError("expected username, password, and TOTP on stdin")
    username, password, totp = values
    if len(totp) != 6 or not totp.isdigit():
        raise RuntimeError("expected a six-digit TOTP")
    return username, password, totp


def _install_proxy_ca() -> None:
    ca_file = Path(
        os.environ.get("DEGITIDE_PROXY_CA_CERT", "/proxy-ca/mitmproxy-ca-cert.pem")
    )
    if not ca_file.is_file():
        raise RuntimeError("proxy CA certificate is missing")
    nss_dir = Path.home() / ".pki" / "nssdb"
    nss_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    database = f"sql:{nss_dir}"
    if not (nss_dir / "cert9.db").exists():
        subprocess.run(
            ["certutil", "-N", "--empty-password", "-d", database],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    subprocess.run(
        ["certutil", "-D", "-d", database, "-n", "degitide-egress-proxy"],
        check=False,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    subprocess.run(
        [
            "certutil",
            "-A",
            "-d",
            database,
            "-n",
            "degitide-egress-proxy",
            "-t",
            "C,,",
            "-i",
            str(ca_file),
        ],
        check=True,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )


async def _run(username: str, password: str, totp: str) -> dict[str, object]:
    global CURRENT_STAGE

    CURRENT_STAGE = "prepare_profile"
    _install_proxy_ca()
    _prune_persistent_profile()
    runtime_profile = _new_runtime_profile_dir()
    async with async_playwright() as patchright:
        CURRENT_STAGE = "launch_browser"
        try:
            context = await patchright.chromium.launch_persistent_context(
                user_data_dir=str(runtime_profile),
                headless=True,
                viewport={"width": 1366, "height": 900},
                locale=os.environ.get("DEGITIDE_BROWSER_LOCALE", "en-IE"),
                timezone_id=os.environ.get("DEGITIDE_BROWSER_TIMEZONE", "Europe/Dublin"),
                proxy=_proxy_settings(PROXY),
                args=["--disable-quic", "--dns-over-https-mode=off", "--disable-save-password-bubble"],
            )
        except Exception:
            shutil.rmtree(runtime_profile, ignore_errors=True)
            raise
        try:
            denied: list[dict[str, object]] = []

            def capture_denied(response) -> None:
                if response.status != 403:
                    return
                schema = _request_schema(response.request.method, response.url)
                if schema not in denied:
                    denied.append(schema)

            page = context.pages[0] if context.pages else await context.new_page()
            page.on("response", capture_denied)
            await authenticate_page(
                page,
                username,
                password,
                totp,
                set_stage=_set_stage,
            )

            CURRENT_STAGE = "save_session"
            cookies = await context.cookies()
            storage = await storage_snapshot(page)
            _write_session_state(
                PROFILE_DIR / "session-cookies.json", cookies, [storage]
            )
            return {
                "authenticated": True,
                "session_saved": True,
                "denied_schemas": denied,
            }
        finally:
            await context.close()
            shutil.rmtree(runtime_profile, ignore_errors=True)


def _set_stage(value: str) -> None:
    global CURRENT_STAGE
    CURRENT_STAGE = value


def main() -> None:
    os.umask(0o077)
    try:
        result = asyncio.run(_run(*_read_credentials()))
    except Exception as exc:
        print(
            json.dumps(
                {
                    "authenticated": False,
                    "stage": CURRENT_STAGE,
                    "error": type(exc).__name__,
                }
            )
        )
        raise SystemExit(1) from None
    print(json.dumps(result))


if __name__ == "__main__":
    main()
