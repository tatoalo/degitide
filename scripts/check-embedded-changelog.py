#!/usr/bin/env python3
"""Verify that a compiled release serves the expected bundled release notes."""
import argparse
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    parser.add_argument("version")
    parser.add_argument("--expected", type=Path)
    args = parser.parse_args()
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryFile() as log:
        env = {"PATH": os.environ["PATH"], "DEGITIDE_BIND": "127.0.0.1", "DEGITIDE_PORT": str(port),
               "DEGITIDE_DB": str(Path(directory) / "test.db"), "DEGITIDE_SCHEDULE": "0 0 1 1 *",
               "DEGITIDE_WORKER_URL": "http://127.0.0.1:1"}
        process = subprocess.Popen([str(args.binary.resolve())], cwd=directory, env=env,
                                   stdout=log, stderr=subprocess.STDOUT)
        try:
            deadline = time.monotonic() + 20
            while True:
                if process.poll() is not None:
                    log.seek(0)
                    raise RuntimeError(log.read().decode())
                try:
                    with urllib.request.urlopen(f"http://127.0.0.1:{port}/changelog.json", timeout=2) as response:
                        data = json.load(response)
                    break
                except urllib.error.HTTPError:
                    raise
                except (urllib.error.URLError, TimeoutError):
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(0.1)
            assert data["version"] == args.version, "changelog does not match the installed version"
            assert data["releases"], "release is missing its notes"
            if args.expected:
                assert data == json.loads(args.expected.read_text()), "compiled notes differ from the generated file"
            print(f"Compiled {args.version} serves its bundled changelog ({len(data['releases'])} releases).")
        finally:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


if __name__ == "__main__":
    main()
