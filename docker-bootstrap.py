"""Initialize named-volume ownership and local service credentials."""

from __future__ import annotations

import os
import base64
import secrets
from pathlib import Path


APP_UID = 10_000
APP_GID = 10_000
WORKER_UID = 10_001
WORKER_GID = 10_001
PROXY_UID = 10_002
PROXY_GID = 10_002
IPC_GID = 10_999
LOGIN_GID = 10_998
NOTIFY_GID = 10_997


def secure_tree(path: Path, uid: int, gid: int) -> None:
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    for root, directories, files in os.walk(path, followlinks=False):
        root_path = Path(root)
        if root_path.is_symlink():
            continue
        os.chown(root_path, uid, gid)
        os.chmod(root_path, 0o700)
        for name in directories:
            child = root_path / name
            if not child.is_symlink():
                os.chown(child, uid, gid)
                os.chmod(child, 0o700)
        for name in files:
            child = root_path / name
            if not child.is_symlink():
                os.chown(child, uid, gid)
                os.chmod(child, 0o600)


def ensure_secret(path: Path, value: str, uid: int, gid: int) -> None:
    if not path.exists():
        temporary = path.with_suffix(".tmp")
        temporary.write_text(value + "\n", encoding="ascii")
        os.chmod(temporary, 0o440)
        os.chown(temporary, uid, gid)
        temporary.replace(path)
    os.chown(path, uid, gid)
    os.chmod(path, 0o440)


def main() -> None:
    os.umask(0o077)
    secure_tree(Path("/data"), APP_UID, APP_GID)
    secure_tree(Path("/browser-profile"), WORKER_UID, WORKER_GID)
    secure_tree(Path("/var/lib/degitide-proxy"), PROXY_UID, PROXY_GID)

    key_dir = Path("/vault-key")
    key_dir.mkdir(parents=True, exist_ok=True, mode=0o755)
    key_dir.chmod(0o755)
    ensure_secret(key_dir / "key", base64.urlsafe_b64encode(secrets.token_bytes(32)).decode(), 0, WORKER_GID)

    public_ca = Path("/proxy-ca")
    public_ca.mkdir(parents=True, exist_ok=True, mode=0o755)
    os.chown(public_ca, PROXY_UID, PROXY_GID)
    os.chmod(public_ca, 0o755)

    secret_dir = Path("/run/degitide-secrets")
    secret_dir.mkdir(parents=True, exist_ok=True, mode=0o751)
    os.chown(secret_dir, 0, IPC_GID)
    # The proxy is not in IPC_GID. Other users receive traverse-only access so
    # its separate LOGIN_GID can read exactly the capability file below while
    # the per-file modes keep dashboard and worker secrets inaccessible.
    os.chmod(secret_dir, 0o751)

    ensure_secret(secret_dir / "worker-token", secrets.token_urlsafe(32), 0, IPC_GID)
    ensure_secret(secret_dir / "dashboard-token", secrets.token_urlsafe(32), 0, APP_GID)
    ensure_secret(secret_dir / "notify-token", secrets.token_urlsafe(32), 0, NOTIFY_GID)
    ensure_secret(
        secret_dir / "proxy-login-token", secrets.token_urlsafe(32), 0, LOGIN_GID
    )
    (secret_dir / "vnc-password").unlink(missing_ok=True)


if __name__ == "__main__":
    main()
