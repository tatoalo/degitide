#!/bin/sh
set -eu

umask 077

token_file="${DEGITIDE_WORKER_TOKEN_FILE:-/run/degitide-secrets/worker-token}"
ca_file="${DEGITIDE_PROXY_CA_CERT:-/proxy-ca/mitmproxy-ca-cert.pem}"
if [ ! -s "$token_file" ]; then
    echo "worker authentication token is missing" >&2
    exit 1
fi
if [ ! -s "$ca_file" ]; then
    echo "egress proxy CA certificate is missing" >&2
    exit 1
fi
if [ "${DEGITIDE_PROXY:-}" != "http://egress-proxy:8080" ] && [ "${DEGITIDE_PROXY:-}" != "http://127.0.0.1:8080" ]; then
    echo "the container worker requires the policy-controlled egress proxy" >&2
    exit 1
fi

nss_dir="${HOME}/.pki/nssdb"
mkdir -p "$nss_dir"
if [ ! -f "$nss_dir/cert9.db" ]; then
    certutil -N --empty-password -d "sql:$nss_dir"
fi
certutil -D -d "sql:$nss_dir" -n "degitide-egress-proxy" >/dev/null 2>&1 || true
certutil -A -d "sql:$nss_dir" -n "degitide-egress-proxy" -t "C,," -i "$ca_file"

exec python /app/worker/main.py
