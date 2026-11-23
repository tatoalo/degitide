# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/degitide ./cmd/degitide \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/degitide-gateway ./cmd/gateway

FROM debian:bookworm-slim AS app-runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN groupadd --gid 10999 degitide-ipc \
    && groupadd --gid 10997 degitide-notify \
    && groupadd --gid 10000 degitide-app \
    && useradd --uid 10000 --gid 10000 --groups 10999,10997 --create-home --shell /usr/sbin/nologin degitide-app
COPY --from=go-builder /out/degitide /usr/local/bin/degitide
USER degitide-app
WORKDIR /app
EXPOSE 8391
ENTRYPOINT ["/usr/local/bin/degitide"]

FROM debian:bookworm-slim AS gateway-runtime
RUN groupadd --gid 10003 degitide-gateway \
    && useradd --uid 10003 --gid 10003 --create-home --shell /usr/sbin/nologin degitide-gateway
COPY --from=go-builder /out/degitide-gateway /usr/local/bin/degitide-gateway
USER degitide-gateway
WORKDIR /app
EXPOSE 8391
ENTRYPOINT ["/usr/local/bin/degitide-gateway"]

FROM python:3.12-slim-bookworm AS worker-base
ENV DEBIAN_FRONTEND=noninteractive \
    PATH=/opt/venv/bin:$PATH \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

RUN apt-get update \
    && apt-get install -y --no-install-recommends libnss3-tools tini \
    && rm -rf /var/lib/apt/lists/* \
    && python -m venv /opt/venv

COPY worker/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt \
    && patchright install --with-deps chromium \
    && rm -f /tmp/requirements.txt

RUN groupadd --gid 10999 degitide-ipc \
    && groupadd --gid 10998 degitide-login \
    && groupadd --gid 10001 degitide-worker \
    && useradd --uid 10001 --gid 10001 --groups 10998,10999 --create-home --shell /usr/sbin/nologin degitide-worker \
    && mkdir -p /app/worker /browser-profile /ms-playwright \
    && chown -R degitide-worker:degitide-worker /app /browser-profile /ms-playwright

COPY --chown=degitide-worker:degitide-worker worker /app/worker
COPY --chown=degitide-worker:degitide-worker docker-entrypoint.sh /app/worker-entrypoint.sh

USER degitide-worker
WORKDIR /app/worker
FROM worker-base AS worker-runtime
ARG VERSION=dev
ENV DEGITIDE_VERSION=${VERSION}
EXPOSE 9377
ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/bin/sh", "/app/worker-entrypoint.sh"]

FROM python:3.12-slim-bookworm AS proxy-runtime
ENV PATH=/opt/venv/bin:$PATH \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1
RUN python -m venv /opt/venv
COPY proxy/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt \
    && rm -f /tmp/requirements.txt \
    && groupadd --gid 10998 degitide-login \
    && groupadd --gid 10002 degitide-proxy \
    && useradd --uid 10002 --gid 10002 --groups 10998 --create-home --shell /usr/sbin/nologin degitide-proxy \
    && mkdir -p /app/proxy /var/lib/degitide-proxy /proxy-ca \
    && chown -R degitide-proxy:degitide-proxy /app /var/lib/degitide-proxy /proxy-ca
COPY --chown=degitide-proxy:degitide-proxy proxy /app/proxy
USER degitide-proxy
WORKDIR /app/proxy
EXPOSE 8080
ENTRYPOINT ["/bin/sh", "/app/proxy/entrypoint.sh"]

FROM python:3.12-slim-bookworm AS bootstrap-runtime
COPY docker-bootstrap.py /usr/local/bin/degitide-bootstrap.py
ENTRYPOINT ["python", "/usr/local/bin/degitide-bootstrap.py"]

FROM worker-base AS all-in-one-runtime
USER root
COPY proxy/requirements.txt /tmp/proxy-requirements.txt
RUN python -m venv /opt/proxy-venv \
    && /opt/proxy-venv/bin/pip install --no-cache-dir -r /tmp/proxy-requirements.txt \
    && rm /tmp/proxy-requirements.txt \
    && mkdir /data && chown 10001:10001 /data && chmod 0700 /data
COPY --from=go-builder /out/degitide /usr/local/bin/degitide
COPY proxy /app/proxy
COPY docker/all-in-one.py /app/all-in-one.py
ARG VERSION=dev
ENV DEGITIDE_VERSION=${VERSION} \
    DEGITIDE_BIND=0.0.0.0 \
    DEGITIDE_DB=/data/degitide.db \
    DEGITIDE_WORKER_URL=http://127.0.0.1:9377 \
    DEGITIDE_WORKER_BIND=127.0.0.1 \
    DEGITIDE_WORKER_TOKEN_FILE=/data/secrets/worker-token \
    DEGITIDE_API_TOKEN_FILE=/data/secrets/dashboard-token \
    DEGITIDE_VAULT_KEY_FILE=/data/secrets/vault-key \
    DEGITIDE_BROWSER_PROFILE=/data/browser-profile \
    DEGITIDE_PROXY=http://127.0.0.1:8080 \
    DEGITIDE_PROXY_BIND=127.0.0.1 \
    DEGITIDE_PROXY_PRIVATE_DIR=/data/proxy \
    DEGITIDE_PROXY_PUBLIC_DIR=/data/proxy-ca \
    DEGITIDE_PROXY_CA_CERT=/data/proxy-ca/mitmproxy-ca-cert.pem \
    DEGITIDE_PROXY_LOGIN_TOKEN_FILE=/data/secrets/proxy-login-token \
    NODE_EXTRA_CA_CERTS=/data/proxy-ca/mitmproxy-ca-cert.pem \
    DEGITIDE_NOTIFY_BIND=127.0.0.1:9380 \
    DEGITIDE_NOTIFY_TOKEN_FILE=/data/secrets/notify-token
USER degitide-worker
WORKDIR /app
VOLUME /data
EXPOSE 8391
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --start-interval=2s \
    CMD ["python", "/app/all-in-one.py", "health"]
ENTRYPOINT ["/usr/bin/tini", "--", "python", "/app/all-in-one.py"]
