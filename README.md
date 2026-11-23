# Degitide

Degitide saves your DEGIRO holdings and transactions on your own server. It includes a local dashboard, scheduled syncs, exports, and an optional one-way connection to Sure.

[Try the demo](https://degitide.trade/) · [Download a release](https://github.com/tatoalo/degitide/releases/latest)

## Run with Docker

```sh
git clone https://github.com/tatoalo/degitide.git
cd degitide
docker compose up -d --build
```

Open [the dashboard](http://127.0.0.1:8391), create your local administrator, and connect your DEGIRO account. Your data is stored in the Docker volume. For a remote server, use an SSH tunnel.

See the [operating guide](docs/usage.md) for the CLI, backups and remote access, or the [split deployment guide](docs/split-deployment.md) for service isolation.

[MIT license](LICENSE). Degitide is independent of DEGIRO.
