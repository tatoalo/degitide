from __future__ import annotations

import asyncio
import json
import stat
import tempfile
import unittest
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, call, patch

from aiohttp.test_utils import TestClient, TestServer

import main as worker_main
from tasks import reauthenticate
from tasks.common import TaskError


class WorkerAuthenticationTest(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        import os
        from cryptography.fernet import Fernet
        key = Path(self.temporary.name) / "vault-key"
        key.write_bytes(Fernet.generate_key())
        env = patch.dict(os.environ, {"DEGITIDE_VAULT_KEY_FILE": str(key)})
        env.start()
        self.addCleanup(env.stop)
        token_file = Path(self.temporary.name) / "worker-token"
        token_file.write_text("worker-secret\n", encoding="ascii")
        capability_file = Path(self.temporary.name) / "proxy-login-token"
        capability_file.write_text("a" * 43 + "\n", encoding="ascii")
        self.original_token_file = worker_main.WORKER_TOKEN_FILE
        self.original_capability_file = worker_main.LOGIN_CAPABILITY_FILE
        worker_main.WORKER_TOKEN_FILE = str(token_file)
        worker_main.LOGIN_CAPABILITY_FILE = str(capability_file)
        self.client = TestClient(TestServer(worker_main.create_app()))
        await self.client.start_server()

    async def asyncTearDown(self) -> None:
        worker_main._cancel_idle_shutdown()
        await self.client.close()
        worker_main.WORKER_TOKEN_FILE = self.original_token_file
        worker_main.LOGIN_CAPABILITY_FILE = self.original_capability_file
        self.temporary.cleanup()

    async def test_health_is_public_but_tasks_require_bearer_token(self) -> None:
        health = await self.client.get("/health")
        self.assertEqual(health.status, 200)

        task = await self.client.get("/tasks/check-session")
        self.assertEqual(task.status, 401)
        self.assertEqual(task.headers.get("WWW-Authenticate"), "Bearer")

        removed_login = await self.client.post(
            "/tasks/login-qr",
            headers={"Authorization": "Bearer worker-secret"},
            json={},
        )
        self.assertEqual(removed_login.status, 404)

    async def test_reauthentication_endpoint_validates_fields_and_does_not_queue(self) -> None:
        headers = {"Authorization": "Bearer worker-secret"}
        invalid = await self.client.post(
            "/tasks/reauthenticate",
            headers=headers,
            json={"username": "owner", "password": "secret", "totp": "12"},
        )
        self.assertEqual(invalid.status, 400)

        await worker_main.task_lock.acquire()
        try:
            busy = await self.client.post(
                "/tasks/reauthenticate",
                headers=headers,
                json={"username": "owner", "password": "secret", "totp": "123456"},
            )
        finally:
            worker_main.task_lock.release()
        self.assertEqual(busy.status, 409)

    async def test_info_is_authenticated_and_never_starts_browser(self) -> None:
        import os
        denied = await self.client.get("/info")
        self.assertEqual(denied.status, 401)
        await worker_main.task_lock.acquire()
        try:
            with (
                patch.dict(os.environ, {"DEGITIDE_VERSION": "v2.3.4"}),
                patch.object(worker_main, "ensure_browser", new=AsyncMock()) as start,
            ):
                response = await self.client.get("/info", headers={"Authorization": "Bearer worker-secret"})
                self.assertEqual(response.status, 200)
                data = await response.json()
                self.assertEqual(data["version"], "v2.3.4")
                self.assertTrue(data["python"])
                self.assertTrue(data["patchright"])
                start.assert_not_awaited()
        finally:
            worker_main.task_lock.release()

    async def test_reauthentication_endpoint_runs_one_authenticated_task(self) -> None:
        with (
            patch.object(worker_main, "ensure_browser", new=AsyncMock()),
            patch.object(
                worker_main,
                "_reauthenticate_browser",
                new=AsyncMock(
                    return_value={"authenticated": True, "session_saved": True}
                ),
            ) as reauthenticate,
        ):
            response = await self.client.post(
                "/tasks/reauthenticate",
                headers={"Authorization": "Bearer worker-secret"},
                json={
                    "username": "account-user",
                    "password": "account-password",
                    "totp": "123456",
                },
            )

        self.assertEqual(response.status, 200)
        self.assertEqual(
            await response.json(), {"authenticated": True, "session_saved": True}
        )
        reauthenticate.assert_awaited_once_with(
            "account-user", "account-password", "123456", "a" * 43
        )

    async def test_seed_login_persists_only_after_success(self):
        import vault
        root = Path(self.temporary.name)
        with patch.object(worker_main, "PROFILE_DIR", root), patch.object(
            worker_main, "ensure_browser", AsyncMock()
        ), patch.object(worker_main, "_reauthenticate_browser", AsyncMock(
            return_value={"authenticated": True, "session_saved": True}
        )) as authenticate:
            fields = {"username": "owner", "password": "private", "seed": "JBSWY3DPEHPK3PXP"}
            response = await self.client.post("/tasks/reauthenticate",
                headers={"Authorization": "Bearer worker-secret"}, json=fields)
            self.assertEqual(response.status, 200)
            self.assertEqual(vault.load(root / "credentials.enc")["seed"], fields["seed"])
            self.assertTrue(callable(authenticate.call_args.args[2]))
            before = (root / "credentials.enc").read_bytes()
            authenticate.side_effect = TaskError("login failed")
            response = await self.client.post("/tasks/reauthenticate",
                headers={"Authorization": "Bearer worker-secret"}, json=fields)
            self.assertEqual(response.status, 422)
            self.assertEqual((root / "credentials.enc").read_bytes(), before)

    async def test_reauthentication_capability_is_scoped_and_failures_are_redacted(self) -> None:
        context = MagicMock()
        context.set_extra_http_headers = AsyncMock()
        page = MagicMock()
        page.close = AsyncMock()
        context.new_page = AsyncMock(return_value=page)
        original_browser = worker_main.browser
        worker_main.browser = context
        try:
            with (
                patch.object(
                    worker_main.reauthentication,
                    "authenticate_page",
                    new=AsyncMock(side_effect=RuntimeError("upstream rejected login")),
                ),
                self.assertLogs("degitide-worker", level="WARNING") as captured,
            ):
                with self.assertRaises(TaskError):
                    await worker_main._reauthenticate_browser(
                        "private-user", "private-password", "654321", "capability"
                    )
        finally:
            worker_main.browser = original_browser

        self.assertEqual(
            context.set_extra_http_headers.await_args_list,
            [
                call({worker_main.LOGIN_CAPABILITY_HEADER: "capability"}),
                call({}),
            ],
        )
        page.close.assert_awaited_once_with()
        logs = "\n".join(captured.output)
        self.assertNotIn("private-user", logs)
        self.assertNotIn("private-password", logs)
        self.assertNotIn("654321", logs)

    async def test_successful_reauthentication_saves_before_clearing_capability(self) -> None:
        context = MagicMock()
        context.set_extra_http_headers = AsyncMock()
        page = MagicMock()
        page.close = AsyncMock()
        context.new_page = AsyncMock(return_value=page)
        original_browser = worker_main.browser
        worker_main.browser = context
        try:
            with (
                patch.object(
                    worker_main.reauthentication,
                    "authenticate_page",
                    new=AsyncMock(),
                ) as authenticate_page,
                patch.object(
                    worker_main, "_save_session", new=AsyncMock()
                ) as save_session,
            ):
                result = await worker_main._reauthenticate_browser(
                    "account-user", "account-password", "123456", "capability"
                )
        finally:
            worker_main.browser = original_browser

        self.assertEqual(result, {"authenticated": True, "session_saved": True})
        authenticate_page.assert_awaited_once()
        save_session.assert_awaited_once_with(context)
        self.assertEqual(
            context.set_extra_http_headers.await_args_list,
            [
                call({worker_main.LOGIN_CAPABILITY_HEADER: "capability"}),
                call({}),
            ],
        )
        page.close.assert_awaited_once_with()

    async def test_capability_clear_failure_closes_browser_context(self) -> None:
        context = MagicMock()
        context.set_extra_http_headers = AsyncMock(
            side_effect=[None, RuntimeError("clear failed")]
        )
        context.close = AsyncMock()
        page = MagicMock()
        page.close = AsyncMock()
        context.new_page = AsyncMock(return_value=page)
        original_browser = worker_main.browser
        worker_main.browser = context
        try:
            with (
                patch.object(
                    worker_main.reauthentication,
                    "authenticate_page",
                    new=AsyncMock(),
                ),
                patch.object(worker_main, "_save_session", new=AsyncMock()),
            ):
                result = await worker_main._reauthenticate_browser(
                    "account-user", "account-password", "123456", "capability"
                )
        finally:
            worker_main.browser = original_browser

        self.assertEqual(result, {"authenticated": True, "session_saved": True})
        context.close.assert_awaited_once_with()
        page.close.assert_not_awaited()

    async def test_capability_revocation_stops_driver_when_context_close_fails(self) -> None:
        context = MagicMock()
        context.close = AsyncMock(side_effect=RuntimeError("close failed"))
        driver = MagicMock()
        driver.stop = AsyncMock()
        original_browser = worker_main.browser
        original_playwright = worker_main.playwright
        worker_main.browser = context
        worker_main.playwright = driver
        try:
            await worker_main._discard_browser_runtime(context)
            self.assertIsNone(worker_main.browser)
            self.assertIsNone(worker_main.playwright)
        finally:
            worker_main.browser = original_browser
            worker_main.playwright = original_playwright

        context.close.assert_awaited_once_with()
        driver.stop.assert_awaited_once_with()

    async def test_proxy_settings_remove_chromiums_implicit_loopback_bypass(self) -> None:
        self.assertEqual(
            worker_main._proxy_settings("http://egress-proxy:8080"),
            {"server": "http://egress-proxy:8080", "bypass": "<-loopback>"},
        )
        self.assertIsNone(worker_main._proxy_settings(""))

    async def test_session_cookie_state_is_private_and_degiro_only(self) -> None:
        state_path = Path(self.temporary.name) / "profile" / "session-cookies.json"
        worker_main._write_session_state(
            state_path,
            [
                {"name": "session", "value": "secret", "domain": ".degiro.nl", "path": "/"},
                {"name": "foreign", "value": "drop", "domain": "example.com", "path": "/"},
            ],
            [
                {
                    "origin": "https://trader.degiro.nl",
                    "localStorage": [{"name": "client", "value": "state"}],
                    "sessionStorage": [{"name": "session", "value": "state"}],
                },
                {
                    "origin": "https://example.com",
                    "localStorage": [{"name": "foreign", "value": "drop"}],
                    "sessionStorage": [],
                },
            ],
        )

        self.assertEqual(stat.S_IMODE(state_path.stat().st_mode), 0o600)
        self.assertNotIn("secret", state_path.read_text())
        stored = worker_main._read_session_state(state_path)
        self.assertEqual([cookie["name"] for cookie in stored["cookies"]], ["session"])
        self.assertEqual(
            [origin["origin"] for origin in stored["origins"]],
            ["https://trader.degiro.nl"],
        )
        self.assertEqual(len(worker_main._read_session_cookies(state_path)), 1)

    async def test_invalid_or_oversized_session_state_fails_closed(self) -> None:
        state_path = Path(self.temporary.name) / "session-cookies.json"
        state_path.write_text("not-json", encoding="utf-8")
        self.assertEqual(worker_main._read_session_cookies(state_path), [])

        with state_path.open("wb") as handle:
            handle.truncate(worker_main.SESSION_STATE_MAX_BYTES + 1)
        self.assertEqual(worker_main._read_session_cookies(state_path), [])

    async def test_persistent_profile_keeps_only_bounded_session_state(self) -> None:
        profile = Path(self.temporary.name) / "profile"
        profile.mkdir()
        state_path = profile / "session-cookies.json"
        state_path.write_text('{"cookies":[],"origins":[]}', encoding="utf-8")
        (profile / "Browser History").write_text("legacy", encoding="utf-8")
        cache = profile / "Cache"
        cache.mkdir()
        (cache / "entry").write_text("legacy", encoding="utf-8")

        original_profile = worker_main.PROFILE_DIR
        worker_main.PROFILE_DIR = profile
        try:
            worker_main._prune_persistent_profile()
        finally:
            worker_main.PROFILE_DIR = original_profile

        self.assertEqual(
            [child.name for child in profile.iterdir()], ["session-cookies.json"]
        )
        self.assertEqual(stat.S_IMODE(profile.stat().st_mode), 0o700)

    async def test_runtime_browser_profile_is_ephemeral_and_private(self) -> None:
        runtime_profile = worker_main._new_runtime_profile_dir()
        try:
            self.assertNotEqual(runtime_profile, worker_main.PROFILE_DIR)
            self.assertEqual(stat.S_IMODE(runtime_profile.stat().st_mode), 0o700)
        finally:
            runtime_profile.rmdir()

    async def test_browser_runtime_is_released_after_idle_timeout(self) -> None:
        original_idle_seconds = worker_main.BROWSER_IDLE_SECONDS
        original_browser = worker_main.browser
        original_playwright = worker_main.playwright
        worker_main.BROWSER_IDLE_SECONDS = 0.01
        worker_main.browser = MagicMock()
        task = AsyncMock(return_value={"valid": True})
        try:
            with (
                patch.object(worker_main, "ensure_browser", new=AsyncMock()),
                patch.object(
                    worker_main, "shutdown_browser", new=AsyncMock()
                ) as shutdown,
            ):
                result = await worker_main.run_task("test", task)
                await asyncio.sleep(0.03)

                self.assertEqual(result, {"valid": True})
                task.assert_awaited_once_with()
                shutdown.assert_awaited_once_with()
        finally:
            worker_main._cancel_idle_shutdown()
            worker_main.BROWSER_IDLE_SECONDS = original_idle_seconds
            worker_main.browser = original_browser
            worker_main.playwright = original_playwright

    async def test_only_selected_successful_task_refreshes_session_checkpoint(self) -> None:
        context = MagicMock()
        original_browser = worker_main.browser
        worker_main.browser = context
        valid_task = AsyncMock(return_value={"valid": True})
        invalid_task = AsyncMock(return_value={"valid": False})
        try:
            with (
                patch.object(worker_main, "ensure_browser", new=AsyncMock()),
                patch.object(worker_main, "_save_session", new=AsyncMock()) as save,
                patch.object(worker_main, "_schedule_idle_shutdown"),
            ):
                valid = await worker_main.run_task(
                    "test",
                    valid_task,
                    checkpoint_if=lambda value: value.get("valid") is True,
                )
                invalid = await worker_main.run_task(
                    "test",
                    invalid_task,
                    checkpoint_if=lambda value: value.get("valid") is True,
                )
        finally:
            worker_main.browser = original_browser

        self.assertEqual(valid, {"valid": True})
        self.assertEqual(invalid, {"valid": False})
        save.assert_awaited_once_with(context)

    async def test_shutdown_tolerates_browser_close_during_checkpoint(self) -> None:
        context = MagicMock()
        context.close = AsyncMock(side_effect=RuntimeError("already closed"))
        driver = MagicMock()
        driver.stop = AsyncMock()
        original_browser = worker_main.browser
        original_playwright = worker_main.playwright
        worker_main.browser = context
        worker_main.playwright = driver

        async def close_during_save(_: object) -> None:
            worker_main._browser_closed(context)
            raise RuntimeError("target closed")

        try:
            with (
                patch.object(
                    worker_main,
                    "_save_session",
                    new=AsyncMock(side_effect=close_during_save),
                ),
                self.assertLogs("degitide-worker", level="WARNING") as captured,
            ):
                await worker_main.shutdown_browser()
            self.assertIsNone(worker_main.browser)
            self.assertIsNone(worker_main.playwright)
        finally:
            worker_main.browser = original_browser
            worker_main.playwright = original_playwright

        context.close.assert_awaited_once_with()
        driver.stop.assert_awaited_once_with()
        logs = "\n".join(captured.output)
        self.assertIn("could not save browser session: RuntimeError", logs)
        self.assertIn("could not close browser runtime: RuntimeError", logs)

    async def test_reauthentication_clicks_visible_submit_control(self) -> None:
        page = MagicMock()
        username_field = MagicMock()
        username_field.fill = AsyncMock()
        password_field = MagicMock()
        password_field.fill = AsyncMock()
        password_field.press = AsyncMock()
        submit_control = MagicMock()
        submit_control.is_visible = AsyncMock(return_value=True)
        submit_control.click = AsyncMock()
        submit_controls = MagicMock()
        submit_controls.count = AsyncMock(return_value=1)
        submit_controls.nth.return_value = submit_control
        page.locator.side_effect = {
            'input[name="username"]': username_field,
            'input[type="password"]': password_field,
            'button[type="submit"], input[type="submit"]': submit_controls,
        }.__getitem__

        await reauthenticate._submit_password(page, "user", "password")

        username_field.fill.assert_awaited_once_with("user")
        password_field.fill.assert_awaited_once_with("password")
        submit_control.click.assert_awaited_once_with()
        password_field.press.assert_not_awaited()


if __name__ == "__main__":
    unittest.main()
