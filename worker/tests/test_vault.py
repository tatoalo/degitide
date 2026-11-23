import asyncio
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import AsyncMock, patch
from cryptography.fernet import Fernet, InvalidToken
import vault
import main

class VaultTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.key = self.root / "key"
        self.key.write_bytes(Fernet.generate_key())
        env = patch.dict(os.environ, {"DEGITIDE_VAULT_KEY_FILE": str(self.key)})
        env.start()
        self.addCleanup(env.stop)

    def test_restart_and_tamper(self):
        path = self.root / "credentials.enc"
        vault.save(path, {"password": "private-password", "seed": "JBSWY3DPEHPK3PXP"})
        self.assertNotIn(b"private-password", path.read_bytes())
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        subprocess.run([sys.executable, "-c",
            "import vault,sys; from pathlib import Path; assert vault.load(Path(sys.argv[1]))['password'] == 'private-password'",
            str(path)], check=True)
        encrypted = path.read_bytes()
        path.write_bytes(encrypted[:25] + b"!" + encrypted[26:])
        with self.assertRaises(InvalidToken):
            vault.load(path)
        path.write_bytes(encrypted)
        self.key.write_bytes(Fernet.generate_key())
        with self.assertRaises(InvalidToken):
            vault.load(path)

    def test_totp_rfc_vector_and_purpose(self):
        self.assertEqual(vault.code("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", 59), "287082")
        with self.assertRaises(ValueError):
            vault.unseal(vault.seal("{}", "session"), "credentials")

    def test_authenticator_export_formats(self):
        expected = "JBSWY3DPEHPK3PXP"
        self.assertEqual(vault.normalize_seed("  jbsw y3dp\nehpk3pxp  "), expected)
        self.assertEqual(vault.normalize_seed(
            "otpauth://totp/DEGIRO?secret=" + expected + "&issuer=DEGIRO"), expected)
        for value in ("123456", "not-a-setup-secret", "otpauth://hotp/x?secret=" + expected,
                      "otpauth://totp/x?secret=" + expected + "&period=60",
                      "otpauth://totp/x?secret=" + expected + "&digits=8"):
            with self.assertRaises(ValueError):
                vault.normalize_seed(value)

    def test_recovery_backoff_survives_new_call(self):
        vault.save(self.root / "credentials.enc", {
            "username": "test", "password": "fake", "seed": "JBSWY3DPEHPK3PXP"})
        async def run():
            with patch.object(main, "PROFILE_DIR", self.root), patch.object(
                main, "SESSION_RECHECK_DELAY_SECONDS", 0
            ), patch.object(
                main.login, "check_session", AsyncMock(return_value={"valid": False})
            ), patch.object(main, "_reauthenticate_browser", AsyncMock()) as authenticate:
                await main._check_and_recover("capability")
                await main._check_and_recover("capability")
                authenticate.assert_awaited_once()
                self.assertEqual(len(authenticate.call_args.args[2]()), 6)
        asyncio.run(run())
