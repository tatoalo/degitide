"""Exercise the production --quiet proxy without upstream network access."""
from __future__ import annotations

import http.client
import os
from pathlib import Path
import socket
import ssl
import subprocess
import sys
import tempfile
import time
import unittest


MITMDUMP = Path(sys.executable).with_name("mitmdump")
POLICY = Path(__file__).resolve().parents[1] / "policy.py"


@unittest.skipUnless(MITMDUMP.is_file(), "mitmproxy is installed in the proxy image and CI")
class QuietPolicyLoggingTest(unittest.TestCase):
    def test_quiet_proxy_reports_decisions_without_session_or_payload_data(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            # Policy runs first; satisfy allowed requests locally. Never contact
            # DEGIRO, including when the request passes the real allowlist.
            stub = root / "upstream.py"
            stub.write_text(
                "from mitmproxy import http\n"
                "def request(flow):\n"
                "    if flow.response is None:\n"
                "        flow.response = http.Response.make(200, b'{}')\n"
            )
            with socket.socket() as reservation:
                reservation.bind(("127.0.0.1", 0))
                port = reservation.getsockname()[1]
            with (root / "output.log").open("w+") as output:
                process = subprocess.Popen(
                    [str(MITMDUMP), "--quiet", "--listen-host", "127.0.0.1",
                     "--listen-port", str(port), "--set", f"confdir={root / 'ca'}",
                     "--set", "connection_strategy=lazy", "-s", str(POLICY), "-s", str(stub)],
                    stdout=output, stderr=subprocess.STDOUT,
                    env={key: value for key, value in os.environ.items()
                         if key not in {"DEGITIDE_PROXY_LOGIN_TOKEN_FILE", "DEGITIDE_PROXY_LOGIN_MODE"}},
                )
                try:
                    deadline = time.monotonic() + 20
                    while True:
                        if process.poll() is not None:
                            output.seek(0)
                            self.fail("proxy exited before listening: " + output.read())
                        try:
                            with socket.create_connection(("127.0.0.1", port), timeout=.2):
                                break
                        except OSError:
                            if time.monotonic() >= deadline:
                                self.fail("proxy did not start")
                            time.sleep(.05)
                    connection = http.client.HTTPSConnection(
                        "127.0.0.1", port, context=ssl._create_unverified_context(), timeout=10,
                    )
                    connection.set_tunnel("trader.degiro.nl", 443)
                    try:
                        for path, body, status in (
                            ("/productsearch/secure/v1/products/info", b'["123","EUR","FLATEX_EUR"]', 200),
                            ("/trading/secure/v5/order", b'{"private":"synthetic-body-secret"}', 403),
                        ):
                            connection.request(
                                "POST", path + "?intAccount=123&sessionId=synthetic-session-secret",
                                body=body, headers={"Content-Type": "application/json",
                                                    "Cookie": "synthetic-cookie-secret"},
                            )
                            response = connection.getresponse()
                            response.read()
                            self.assertEqual(response.status, status)
                    finally:
                        connection.close()
                finally:
                    process.terminate()
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)
                output.seek(0)
                messages = output.read()
                self.assertIn("allowed route=product-info method=POST", messages)
                self.assertIn("blocked route=default-deny method=POST", messages)
                for private in ("synthetic-session-secret", "synthetic-body-secret",
                                "synthetic-cookie-secret", "intAccount=", "trader.degiro.nl", "FLATEX_EUR"):
                    self.assertNotIn(private, messages)


if __name__ == "__main__":
    unittest.main()
