import unittest
from unittest.mock import AsyncMock, MagicMock, patch
from tasks import reauthenticate

class ReauthenticationNavigationTest(unittest.IsolatedAsyncioTestCase):
    async def test_success_without_spa_redirect_verifies_reload(self):
        page = MagicMock()
        page.goto = AsyncMock()
        page.wait_for_timeout = AsyncMock()
        otp = MagicMock(fill=AsyncMock(), press=AsyncMock())
        response = MagicMock(status=200, json=AsyncMock(return_value={"status": 0}))
        async def response_value():
            return response
        pending = MagicMock()
        pending.value = response_value()
        manager = MagicMock()
        manager.__aenter__ = AsyncMock(return_value=pending)
        manager.__aexit__ = AsyncMock(return_value=False)
        page.expect_response.return_value = manager
        with patch.object(reauthenticate, "_submit_password", AsyncMock()), patch.object(
            reauthenticate, "_totp_input", AsyncMock(return_value=otp)
        ), patch.object(reauthenticate, "_is_authenticated", AsyncMock(return_value=True)):
            await reauthenticate.authenticate_page(page, "fake", "fake", lambda: "123456")
        otp.fill.assert_awaited_once_with("123456")
        self.assertEqual(page.goto.await_args_list[-1].args[0], reauthenticate.TRADER_URL)
        page.wait_for_url.assert_not_called()
