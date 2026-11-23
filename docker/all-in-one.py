import os
import secrets
import signal
import socket
import subprocess
import sys
import threading
import time
from pathlib import Path
from urllib.request import urlopen

stopping = threading.Event()
children = []


def health():
    for url in ("http://127.0.0.1:8391/healthz", "http://127.0.0.1:9377/health"):
        with urlopen(url, timeout=2) as response:
            if response.status != 200:
                raise RuntimeError("service is not healthy")
    with socket.create_connection(("127.0.0.1", 8080), timeout=2):
        pass
    if os.environ.get("DEGITIDE_NOTIFY_PROVIDER"):
        with urlopen("http://127.0.0.1:9380/healthz", timeout=2):
            pass


def start(name, command, env):
    process = subprocess.Popen(command, env=env)
    children.append((name, process))
    print(f"Starting {name}", flush=True)


def wait_ready(check):
    deadline = time.monotonic() + 60
    while not stopping.is_set():
        if any(process.poll() is not None for _, process in children):
            raise RuntimeError("a service exited during startup")
        try:
            if check():
                return
        except (OSError, ValueError):
            pass
        if time.monotonic() >= deadline:
            raise RuntimeError("service startup timed out")
        stopping.wait(.2)
    raise InterruptedError


def initialize():
    from cryptography.fernet import Fernet
    os.umask(0o077)
    for directory in ("secrets", "browser-profile", "proxy", "proxy-ca"):
        Path("/data", directory).mkdir(mode=0o700, exist_ok=True)
    for name in ("dashboard-token", "worker-token", "proxy-login-token", "notify-token", "vault-key"):
        path = Path("/data/secrets", name)
        if not path.exists():
            value = Fernet.generate_key().decode() if name == "vault-key" else secrets.token_urlsafe(32)
            temporary = path.with_suffix(".tmp")
            temporary.write_text(value + "\n")
            temporary.replace(path)
        if not path.read_text().strip():
            raise RuntimeError(f"empty secret file: {name}")
        path.chmod(0o600)


def serve():
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: stopping.set())
    try:
        initialize()
        env = os.environ.copy()
        start("proxy", ["/bin/sh", "/app/proxy/entrypoint.sh"], {
            **env, "PATH": "/opt/proxy-venv/bin:" + env["PATH"],
        })
        def proxy_ready():
            with socket.create_connection(("127.0.0.1", 8080), timeout=1):
                return Path(env["DEGITIDE_PROXY_CA_CERT"]).is_file()
        wait_ready(proxy_ready)
        start("worker", ["/bin/sh", "/app/worker-entrypoint.sh"], env)
        wait_ready(lambda: urlopen("http://127.0.0.1:9377/health", timeout=2).close() is None)
        if env.get("DEGITIDE_NOTIFY_PROVIDER"):
            start("notifier", ["degitide", "notify-serve"], env)
            env["DEGITIDE_NOTIFY_URL"] = "http://127.0.0.1:9380/notify"
        start("app", ["degitide", "serve"], env)
        while not stopping.wait(.2):
            for name, process in children:
                if process.poll() is not None:
                    raise RuntimeError(f"{name} exited with status {process.returncode}")
    except InterruptedError:
        pass
    finally:
        for _, process in reversed(children):
            if process.poll() is None:
                process.terminate()
        deadline = time.monotonic() + 30
        for _, process in reversed(children):
            try:
                process.wait(timeout=max(.1, deadline - time.monotonic()))
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


if __name__ == "__main__":
    if sys.argv[1:] == ["health"]:
        health()
    elif sys.argv[1:] not in ([], ["serve"]):
        os.execvp("degitide", ["degitide", *sys.argv[1:]])
    else:
        serve()
