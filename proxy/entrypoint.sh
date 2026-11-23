#!/bin/sh
set -eu

umask 077

private_dir="${DEGITIDE_PROXY_PRIVATE_DIR:-/var/lib/degitide-proxy}"
public_dir="${DEGITIDE_PROXY_PUBLIC_DIR:-/proxy-ca}"
mkdir -p "$private_dir" "$public_dir"

mitmdump \
    --quiet \
    --listen-host "${DEGITIDE_PROXY_BIND:-0.0.0.0}" \
    --listen-port 8080 \
    --set "confdir=$private_dir" \
    --set connection_strategy=lazy \
    --set block_global=false \
    -s /app/proxy/policy.py &
proxy_pid=$!

terminate() {
    kill -TERM "$proxy_pid" 2>/dev/null || true
}
trap terminate INT TERM EXIT

attempt=0
while [ ! -s "$private_dir/mitmproxy-ca-cert.pem" ]; do
    if ! kill -0 "$proxy_pid" 2>/dev/null; then
        wait "$proxy_pid"
        exit $?
    fi
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 100 ]; then
        echo "egress proxy did not generate its public CA certificate" >&2
        exit 1
    fi
    sleep 0.1
done

cp "$private_dir/mitmproxy-ca-cert.pem" "$public_dir/mitmproxy-ca-cert.pem.tmp"
chmod 0444 "$public_dir/mitmproxy-ca-cert.pem.tmp"
mv "$public_dir/mitmproxy-ca-cert.pem.tmp" "$public_dir/mitmproxy-ca-cert.pem"

wait "$proxy_pid"
status=$?
trap - INT TERM EXIT
exit "$status"
