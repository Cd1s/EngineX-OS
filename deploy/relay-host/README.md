# enginex-relay self-hosted deployment


Run from a current checkout on Debian 12+ / Ubuntu 24.04+ (amd64 or arm64):

```sh
sudo bash deploy/one-click-backend.sh
# With an existing HTTPS reverse proxy:
sudo bash deploy/one-click-backend.sh --public https://relay.example.com
```

The script builds the Go relay and HPSS Stems renderer, installs their systemd
units and starts them. Missing Python/FFmpeg dependencies are installed with
apt; Go is bootstrapped from go.dev with its published checksum only when
needed. With no public URL a fresh install starts a Cloudflare Quick Tunnel;
an upgrade retains its existing public URL, password, sessions and renderer
token. Optional Apple Music helper services start when their files already
exist under `/opt/enginex-amd` and `/opt/wrapper-lite`; these files and an Apple
subscription are not installed by this script. The summary states whether that
helper is available. Tokens and passwords are not printed.

The canonical command/service/state are `enginex-relay`,
`enginex-relay.service` and `/var/lib/enginex-relay`. The upgrade installer
stops/disables previous units, moves their state (including DynamicUser state),
keeps the former state path as a symlink, and keeps command/service aliases for
one release. It also accepts the intermediate `enginex-deemix-relay.service`
name. Conflicting old/new state or overrides are refused rather than overwritten.

The web service handles Spotify/Deezer QR login and credential transfer. It
never downloads or stores music. SC5000 downloads directly from Deezer using
its loopback-only `enginex-deemixd` service.

It is also the single public entry point for remote Stems rendering: the deck
streams PCM to `POST /v1/engine` with the console password and the relay
forwards it, unstored, to the loopback `enginex-stemsd` (see
[`deploy/stems`](../stems/README.md)).

The existing edge reverse proxy and public URL stay in use. The relay listens
on `0.0.0.0:6595` with DynamicUser and persistent configuration at
`/var/lib/enginex-relay/config.json`. Preserve this file during upgrades.

## Build and upgrade

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/enginex-relay-amd64 ./cmd/enginex-relay
# Back up the deployed binary, unit, state and old cache to agent-backup first.
# Copy the binary, unit and installer to the host, then run as root:
sh install.sh ./enginex-relay-amd64 ./enginex-relay.service
# Stop the previous audio process before clearing the legacy cache; with
# DynamicUser the backing directory may be /var/cache/private/enginex-relay.
```

## Routes (also served under `/enginex/...`)

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
`/var/lib/enginex-relay/initial-password` (0600) and logs only that path. Change
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
| `-stems-token-file` | `$CREDENTIALS_DIRECTORY/stems-token` if present, else `/etc/enginex-stems/token` | Daemon token, re-read per session; a missing file sends no token |

The relay runs as a DynamicUser and cannot read the root-only token itself.
Install [`50-stems.conf`](50-stems.conf) as
`/etc/systemd/system/enginex-relay.service.d/50-stems.conf`, then
`systemctl daemon-reload` and restart the relay.

The edge reverse proxy in front of the relay must stream request bodies
without buffering and pass HTTP/1.1 trailers through; otherwise the deck sees
no greeting or reports `remote renderer did not finish successfully`.

## Rollback

Use `agent-backup get <backup-id> --output <archive>` to restore the previous
binary and service unit, run `systemctl daemon-reload`, then restart
`enginex-relay.service`. Leave the current state/configuration in place
unless the state itself needs recovery. Restoring the previous binary also
restores its cloud audio behavior.

## Switching an existing host

Back up the deployed binary, units/drop-ins and relay state before upgrading.
Run the same one-click command; the migration runs automatically. Existing
custom service arguments and drop-ins are retained. A low-level upgrade can
still use `sh deploy/relay-host/install.sh <new binary> [unit file]`; without a
unit argument it retains an installed one or uses the bundled default.

Open `http://<backend-ip>:6595` and read the initial password locally with
`sudo cat /var/lib/enginex-relay/initial-password`; change it in the console.
After flashing, run `enginex-installer --setup-backend`, enter the deck address
and its screen's Console Access Code, then the backend URL and password. The
installer uses the existing deck API and installs no service on the computer.
Alternatively set them in `http://<deck-ip>:8080`. The same relay settings route
Stems and synchronize configured Apple Music helper settings to the deck.

If a state conflict is reported, preserve both directories and resolve which
configuration to keep before rerunning. No server changes occur just by updating
this repository.
