# enginex-relay self-hosted deployment

> `enginex-relay` was called `sc5000-relay`; the old name, the `cmd/sc5000-relay` source directory, the `/var/lib/sc5000-relay` state directory and the `sc5000-deemix-relay.service` unit keep working unchanged.

`https://relay.example.com` below is a placeholder: set `-public` in the unit to
this relay's real public URL before the first install. `install.sh` refuses the
placeholder on a fresh host and keeps an installed unit's `-public` on upgrade.

The web service handles Spotify/Deezer QR login and credential transfer. It
never downloads or stores music. SC5000 downloads directly from Deezer using
its loopback-only `sc5000-deemixd` service.

It is also the single public entry point for remote Stems rendering: the deck
streams PCM to `POST /v1/engine` with the console password and the relay
forwards it, unstored, to the loopback `enginex-stemsd` (see
[`deploy/stems`](../stems/README.md)).

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
| `GET/POST /m/{spotify,deezer,soundcloud}?t=` | Phone login. Spotify and SoundCloud redirect straight to their own login page; `&manual=1` shows the fallback form |
| `GET /oauth/spotify/callback` | OAuth callback |
| `GET /oauth/soundcloud/callback` | OAuth callback. Register this URL as the SoundCloud app's redirect, or its authorize page stays blank |
| `/api/audio`, `/api/download-stream`, `/api/download`, `/api/queue` | All methods return 410; no audio cache or queue |
| `POST /v1/engine` | Remote Stems session, full duplex. Console password in `Authorization: Bearer` or `X-Deemix-Password` (headers only); forwarded to `-stems-target` with the daemon's own token. Ends with trailer `X-Stems-Status: ok\|failed` |
| `GET /healthz/stems` | Same password; reports the `enginex-stemsd` health JSON, or 502 when it is down or rejects the relay |
| `GET/POST /api/config` | Password or console cookie required |
| `GET /api/config?secrets=1` | Explicit opt-in includes `config.deezerArl` and the BPM service keys, with `Cache-Control: no-store` |
| `GET /`, `POST /login`, `POST /logout` | Credential management console |

Password authentication supports `X-Deemix-Password`, `Authorization: Bearer`,
or the console cookie; the password is no longer accepted in the URL query.
On first start the relay writes a random password to
`/var/lib/sc5000-relay/initial-password` (0600) and logs only that path. Change
it in the web console and update the deck's `deemix-config.json` to match. A
config still using the old fixed default may log in but gets 403 for
`?secrets=1` and Stems until the password is changed.
Ten failed attempts per client (IPv6: per /64) in five minutes cause HTTP 429
(shared with the Stems routes). `X-Forwarded-For` and `X-Real-IP` are trusted
only from loopback peers (a proxy or tunnel on the same host); a proxy on
another host or in a container network must be listed with
`-trusted-proxies <proxy IP or CIDR>`. Every other client is limited by its
own address.

## Stems flags

| Flag | Default | Meaning |
|---|---|---|
| `-stems-target` | `http://127.0.0.1:6596` | `enginex-stemsd` base URL; `host:port` and a trailing `/v1/engine` are accepted |
| `-stems-token-file` | `$CREDENTIALS_DIRECTORY/stems-token` if present, else `/etc/sc5000-stems/token` | Daemon token, re-read per session; a missing file sends no token |

The relay runs as a DynamicUser and cannot read the root-only token itself.
Install [`50-stems.conf`](50-stems.conf) as
`/etc/systemd/system/sc5000-deemix-relay.service.d/50-stems.conf`, then
`systemctl daemon-reload` and restart the relay.

The edge reverse proxy in front of the relay must stream request bodies
without buffering and pass HTTP/1.1 trailers through; otherwise the deck sees
no greeting or reports `remote renderer did not finish successfully`.

## Rollback

Use `agent-backup get <backup-id> --output <archive>` to restore the previous
binary and service unit, run `systemctl daemon-reload`, then restart
`sc5000-deemix-relay.service`. Leave the current state/configuration in place
unless the state itself needs recovery. Restoring the previous binary also
restores its cloud audio behavior.
