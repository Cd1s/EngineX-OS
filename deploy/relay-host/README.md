# sc5000-relay on relay-host (`https://relay.example.com`)

The web service handles Spotify/Deezer QR login and credential transfer only.
It never downloads or stores audio. SC5000 downloads directly from Deezer using
its loopback-only `sc5000-deemixd` service.

The existing edge reverse proxy and public URL stay in use. The relay listens
on `0.0.0.0:6595` with DynamicUser and persistent configuration at
`/var/lib/sc5000-relay/config.json`. Preserve this file during upgrades.

## Build and upgrade

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/sc5000-relay-amd64 ./cmd/sc5000-relay
# Back up the deployed binary, unit, state and old cache to agent-backup first.
# Copy the binary, unit and installer to the host, then run as root:
sh install.sh ./sc5000-relay-amd64 ./sc5000-deemix-relay.service
# Stop the previous audio process before clearing the legacy cache; with
# DynamicUser the backing directory may be /var/cache/private/sc5000-relay.
```

## Routes (also served under `/sc5000/...`)

| Route | Behavior |
|---|---|
| `GET /healthz` | Health check |
| `POST/GET /api/relay/session` | QR session credential transfer |
| `GET/POST /m/{spotify,deezer}?t=` | Phone login |
| `GET /oauth/spotify/callback` | OAuth callback |
| `/api/audio`, `/api/download-stream`, `/api/download`, `/api/queue` | All methods return 410; no audio cache or queue |
| `GET/POST /api/config` | Password or console cookie required |
| `GET /api/config?secrets=1` | Explicit opt-in includes `config.deezerArl` and the BPM service keys, with `Cache-Control: no-store` |
| `GET /`, `POST /login`, `POST /logout` | Credential management console |

Password authentication supports `X-Deemix-Password`, `Authorization: Bearer`,
legacy `?key=` / `?password=`, or the console cookie. Change the initial
password in the web console and update the deck's `deemix-config.json` to match.
Ten failed attempts per client in five minutes cause HTTP 429.

## Rollback

Use `agent-backup get <backup-id> --output <archive>` to restore the previous
binary and service unit, run `systemctl daemon-reload`, then restart
`sc5000-deemix-relay.service`. Leave the current state/configuration in place
unless the state itself needs recovery. Restoring the previous binary also
restores its cloud audio behavior.
