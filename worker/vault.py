"""Authenticated encryption for persisted browser state and login credentials."""
import base64
import hashlib
import hmac
import json
import os
import struct
import tempfile
import time
from pathlib import Path
from urllib.parse import urlsplit, parse_qs

from cryptography.fernet import Fernet

def cipher():
    path = os.environ.get("DEGITIDE_VAULT_KEY_FILE")
    if not path:
        raise RuntimeError("DEGITIDE_VAULT_KEY_FILE is required for persistent authentication")
    return Fernet(Path(path).read_bytes().strip())

def seal(payload: str, purpose: str) -> str:
    return cipher().encrypt(json.dumps({"purpose": purpose, "value": payload}).encode()).decode()

def unseal(payload: str, purpose: str) -> str:
    envelope = json.loads(cipher().decrypt(payload.encode()))
    if envelope["purpose"] != purpose:
        raise ValueError("invalid vault purpose")
    return envelope["value"]

def save(path: Path, value: dict):
    encoded = seal(json.dumps(value), "credentials")
    fd, temporary = tempfile.mkstemp(dir=path.parent)
    try:
        with os.fdopen(fd, "w") as handle:
            handle.write(encoded)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        Path(temporary).unlink(missing_ok=True)

def load(path: Path):
    if path.stat().st_size > 16384:
        raise ValueError("invalid credential vault size")
    return json.loads(unseal(path.read_text(), "credentials"))

def normalize_seed(seed: str) -> str:
    if not isinstance(seed, str) or len(seed) > 2048:
        raise ValueError("invalid authenticator secret")
    seed = seed.strip()
    if seed.lower().startswith("otpauth://"):
        uri = urlsplit(seed)
        fields = parse_qs(uri.query)
        if uri.netloc.lower() != "totp" or any(
            len(values) != 1 for values in fields.values()
        ):
            raise ValueError("invalid authenticator secret")
        if (fields.get("algorithm", ["SHA1"])[0].upper() != "SHA1"
                or fields.get("digits", ["6"])[0] != "6"
                or fields.get("period", ["30"])[0] != "30"):
            raise ValueError("unsupported authenticator parameters")
        seed = fields.get("secret", [""])[0]
    seed = "".join(seed.split()).upper().rstrip("=")
    if not 16 <= len(seed) <= 128:
        raise ValueError("invalid authenticator secret")
    base64.b32decode(seed + "=" * (-len(seed) % 8))
    return seed

def code(seed: str, timestamp=None) -> str:
    seed = normalize_seed(seed)
    key = base64.b32decode(seed + "=" * (-len(seed) % 8))
    counter = int(time.time() if timestamp is None else timestamp) // 30
    digest = hmac.new(key, struct.pack(">Q", counter), hashlib.sha1).digest()
    offset = digest[-1] & 15
    number = struct.unpack(">I", digest[offset:offset + 4])[0] & 0x7fffffff
    return f"{number % 1000000:06d}"
