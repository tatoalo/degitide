#!/bin/sh
set -eu

echo_enabled=true
restore_stack=false

cleanup() {
    if [ "$echo_enabled" = false ]; then
        stty echo 2>/dev/null || true
        printf '\n' >&2
    fi
    unset username password totp
    if [ "$restore_stack" = true ]; then
        restore_stack=false
        if docker compose -f docker-compose.split.yml up --force-recreate --remove-orphans -d >/dev/null; then
            printf 'Restored normal headless sync mode.\n' >&2
        else
            printf 'Could not restore normal mode; run make docker-sync-mode.\n' >&2
        fi
    fi
}
trap cleanup EXIT HUP INT TERM

printf 'DEGIRO username: ' >&2
IFS= read -r username
printf 'DEGIRO password: ' >&2
stty -echo
echo_enabled=false
IFS= read -r password
stty echo
echo_enabled=true
printf '\nDEGIRO six-digit TOTP: ' >&2
stty -echo
echo_enabled=false
IFS= read -r totp
stty echo
echo_enabled=true
printf '\n' >&2

case "$totp" in
    [0-9][0-9][0-9][0-9][0-9][0-9]) ;;
    *)
        printf 'TOTP must contain exactly six digits.\n' >&2
        exit 2
        ;;
esac
if [ -z "$username" ] || [ -z "$password" ]; then
    printf 'Username and password must not be empty.\n' >&2
    exit 2
fi

use_prebuilt="${DEGITIDE_USE_PREBUILT:-}"
if [ -z "$use_prebuilt" ]; then
    use_prebuilt=$(docker compose -f docker-compose.split.yml config --environment 2>/dev/null | \
        sed -n 's/^DEGITIDE_USE_PREBUILT=//p' | tail -n 1)
fi

if [ "$use_prebuilt" = true ]; then
    docker compose -f docker-compose.split.yml pull worker egress-proxy >/dev/null
else
    docker compose -f docker-compose.split.yml build worker egress-proxy >/dev/null
fi
docker compose -f docker-compose.split.yml up -d bootstrap egress-proxy >/dev/null
docker compose -f docker-compose.split.yml stop worker >/dev/null 2>&1 || true
restore_stack=true
docker compose -f docker-compose.split.yml -f docker-compose.login.yml \
    up -d --force-recreate --no-deps egress-proxy >/dev/null

healthy=false
attempt=0
while [ "$attempt" -lt 30 ]; do
    proxy_container_id=$(docker compose -f docker-compose.split.yml -f docker-compose.login.yml \
        ps -q egress-proxy 2>/dev/null || true)
    if [ -n "$proxy_container_id" ] && \
        [ "$(docker inspect -f '{{.State.Health.Status}}' "$proxy_container_id" 2>/dev/null || true)" = healthy ]; then
        healthy=true
        break
    fi
    attempt=$((attempt + 1))
    sleep 1
done
if [ "$healthy" != true ]; then
    printf 'Login policy proxy did not become healthy.\n' >&2
    exit 1
fi

if printf '%s\n%s\n%s\n' "$username" "$password" "$totp" | \
    docker compose -f docker-compose.split.yml -f docker-compose.login.yml \
        run --rm -T --no-deps native-login
then
    printf 'DEGIRO login and restart-safe session checkpoint succeeded.\n' >&2
else
    status=$?
    printf 'DEGIRO native login failed.\n' >&2
    exit "$status"
fi
