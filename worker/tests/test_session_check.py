import os
import tempfile
import time
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock, patch

from cryptography.fernet import Fernet

import main
import vault
from tasks import login

INVALID = {"valid": False}
VALID = {"valid": True}


class ConfirmedSessionCheckTest(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        key = self.root / "vault-key"
        key.write_bytes(Fernet.generate_key())
        for patcher in (
            patch.dict(os.environ, {"DEGITIDE_VAULT_KEY_FILE": str(key)}),
            patch.object(main, "PROFILE_DIR", self.root),
            patch.object(main, "active_account", "default"),
            patch.object(main, "browser", MagicMock()),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.credentials = self.root / "credentials.enc"

    def save_credentials(self, last_attempt=None):
        stored = {"username": "test", "password": "fake", "seed": "JBSWY3DPEHPK3PXP"}
        if last_attempt is not None:
            stored["last_attempt"] = last_attempt
        vault.save(self.credentials, stored)

    async def recover(self, *results):
        """Run one recovery with the given check results and no real waiting."""
        self.check = AsyncMock(side_effect=list(results))
        self.authenticate = AsyncMock()
        self.sleep = AsyncMock()
        with (
            patch.object(main.login, "check_session", self.check),
            patch.object(main, "_reauthenticate_browser", self.authenticate),
            patch.object(main.asyncio, "sleep", self.sleep),
            self.assertLogs("degitide-worker", level="INFO") as self.logs,
        ):
            # assertLogs needs at least one record even when nothing else logs.
            main.log.info("test recovery started")
            return await main._check_and_recover("capability")

    async def test_transient_redirect_is_confirmed_valid_without_login(self):
        self.save_credentials(last_attempt=1.0)
        before = self.credentials.read_bytes()

        result = await self.recover(INVALID, VALID)

        self.assertEqual(result, VALID)
        self.assertEqual(self.check.await_count, 2)
        self.sleep.assert_awaited_once_with(main.SESSION_RECHECK_DELAY_SECONDS)
        self.authenticate.assert_not_awaited()
        self.assertEqual(self.credentials.read_bytes(), before)
        self.assertEqual(vault.load(self.credentials)["last_attempt"], 1.0)
        self.assertIn(
            "session check: transient login redirect; recheck authenticated",
            "\n".join(self.logs.output),
        )

    async def test_confirmed_expiry_uses_stored_login_when_throttle_is_open(self):
        self.save_credentials(last_attempt=time.time() - 901)

        result = await self.recover(INVALID, INVALID, VALID)

        self.assertEqual(result, VALID)
        self.assertEqual(self.check.await_count, 3)
        self.sleep.assert_awaited_once_with(main.SESSION_RECHECK_DELAY_SECONDS)
        self.authenticate.assert_awaited_once()
        self.assertGreater(vault.load(self.credentials)["last_attempt"], time.time() - 60)
        self.assertNotIn("transient login redirect", "\n".join(self.logs.output))

    async def test_confirmed_expiry_while_throttled_reports_login_required(self):
        attempted = time.time() - 60
        self.save_credentials(last_attempt=attempted)

        result = await self.recover(INVALID, INVALID)

        self.assertEqual(result, INVALID)
        self.assertEqual(self.check.await_count, 2)
        self.authenticate.assert_not_awaited()
        self.assertEqual(vault.load(self.credentials)["last_attempt"], attempted)
        self.assertIn("recheck still requires login", "\n".join(self.logs.output))

    async def test_recheck_also_applies_without_stored_credentials(self):
        result = await self.recover(INVALID, VALID)
        self.assertEqual(result, VALID)
        self.assertEqual(self.check.await_count, 2)

        result = await self.recover(INVALID, INVALID)
        self.assertEqual(result, INVALID)
        self.assertEqual(self.check.await_count, 2)
        self.authenticate.assert_not_awaited()
        self.assertFalse(self.credentials.exists())

    async def test_valid_first_check_is_not_repeated(self):
        self.save_credentials(last_attempt=1.0)

        result = await self.recover(VALID)

        self.assertEqual(result, VALID)
        self.check.assert_awaited_once()
        self.sleep.assert_not_awaited()
        self.authenticate.assert_not_awaited()

    async def test_negative_recheck_is_never_checkpointed(self):
        save = AsyncMock()
        with (
            patch.object(main.login, "check_session", AsyncMock(return_value=INVALID)),
            patch.object(main, "SESSION_RECHECK_DELAY_SECONDS", 0),
            patch.object(main, "ensure_browser", AsyncMock()),
            patch.object(main, "_save_session", save),
            patch.object(main, "_schedule_idle_shutdown"),
        ):
            result = await main.run_task(
                "check-session",
                lambda: main._check_and_recover("capability"),
                checkpoint_if=lambda value: value.get("valid") is True,
            )

        self.assertEqual(result, INVALID)
        save.assert_not_awaited()


class SessionCheckDiagnosticsTest(unittest.TestCase):
    @staticmethod
    def response(url, status, method="get", headers=None):
        return SimpleNamespace(url=url, status=status, request=SimpleNamespace(method=method),
                               headers=headers or {})

    def test_logs_masked_path_status_and_query_key_names_only(self):
        url = (
            "https://trader.degiro.nl/trading/secure/v5/account/info/12345678"
            ";jsessionid=SECRETSESSION.prod_b_1?intAccount=12345678&sessionId=SECRETSESSION"
        )
        with self.assertLogs("degitide-worker.login", level="INFO") as captured:
            login._log_unsuccessful_response(self.response(url, 401))

        self.assertEqual(
            captured.output,
            [
                "INFO:degitide-worker.login:session check response method=GET "
                "path=/trading/secure/v5/account/info/{id};jsessionid={token} "
                "status=401 query_keys=intAccount,sessionId"
            ],
        )
        self.assertNotIn("12345678", captured.output[0])
        self.assertNotIn("SECRETSESSION", captured.output[0])

    def test_ignores_success_cache_revalidation_and_other_hosts(self):
        with self.assertNoLogs("degitide-worker.login", level="INFO"):
            for response in (
                self.response("https://trader.degiro.nl/settings/secure/v1/web?intAccount=1", 200),
                self.response("https://trader.degiro.nl/trader/static/app.js", 304),
                self.response("https://example.com/trader/?token=secret", 500),
                self.response("not a url", 500),
            ):
                login._log_unsuccessful_response(response)

    def test_skips_only_403s_marked_as_proxy_denials(self):
        url = "https://trader.degiro.nl/pa/secure/crypto?intAccount=1&sessionId=secret"
        marked = {login.PROXY_DENIAL_HEADER: "denied"}
        with self.assertNoLogs("degitide-worker.login", level="INFO"):
            login._log_unsuccessful_response(self.response(url, 403, headers=marked))
        with self.assertLogs("degitide-worker.login", level="INFO") as captured:
            # An unmarked 403 came from DEGIRO and still matters. The marker
            # never hides any other status.
            login._log_unsuccessful_response(self.response(url, 403))
            login._log_unsuccessful_response(self.response(url, 401, headers=marked))
            login._log_unsuccessful_response(self.response(url, 403, headers={login.PROXY_DENIAL_HEADER: "other"}))
        self.assertEqual([line.split(" status=")[1].split()[0] for line in captured.output], ["403", "401", "403"])

    def test_malformed_response_never_raises(self):
        broken = SimpleNamespace(url="https://trader.degiro.nl/trader/", status=500, request=None)
        with self.assertNoLogs("degitide-worker.login", level="INFO"):
            login._log_unsuccessful_response(broken)


class SessionCheckPageTest(unittest.IsolatedAsyncioTestCase):
    async def test_each_check_uses_a_fresh_page_with_diagnostics(self):
        page = MagicMock()
        page.url = "https://trader.degiro.nl/login/it;jsessionid=SECRETSESSION?redirectUrl=secret"
        page.goto = AsyncMock()
        page.wait_for_timeout = AsyncMock()
        page.close = AsyncMock()
        context = MagicMock()
        with (
            patch.object(login, "get_page", AsyncMock(return_value=page)) as get_page,
            self.assertLogs("degitide-worker.login", level="INFO") as captured,
        ):
            result = await login.check_session(context)

        self.assertEqual(result, INVALID)
        get_page.assert_awaited_once_with(context, new=True)
        page.on.assert_called_once_with("response", login._log_unsuccessful_response)
        page.close.assert_awaited_once_with()
        logs = "\n".join(captured.output)
        self.assertIn("ended at path /login/it;jsessionid={token} (authenticated=False)", logs)
        self.assertNotIn("secret", logs.lower())


if __name__ == "__main__":
    unittest.main()
