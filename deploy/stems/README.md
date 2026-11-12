# Remote Stems renderer (`enginex-stemsd`)

> `enginex-stemsd` was called `sc5000-stemsd`; the old name, `sc5000-stemsd.service`, `/opt/sc5000-stems/` and the `cmd/sc5000-*` directories remain valid. `enginex-relay` was `sc5000-relay`, `enginex-suite` was `sc5000-suite`.

Engine 5.0.4 runs its Stems separator as a child process. On SC5000 that child
is `stems-processor`, which forwards the child's byte stream unchanged to a
remote renderer; Engine still writes the encrypted `.stems` file itself.

```text
Engine ─stdin/stdout─ stems-processor ══HTTPS══ enginex-relay /v1/engine ── 127.0.0.1:6596 enginex-stemsd ── worker.py
          (deck)          console password          (public)      internal token        (loopback)
```

The separator in [`tools/stems/worker.py`](../../tools/stems/worker.py) supports
both a fast causal spectral HPSS / centre-harmonic separator (`--backend hpss`,
default) and an optional HTDemucs 4-stem neural GPU/CPU separator
(`--backend demucs|auto`). `enginex-stemsd` also includes a built-in Python-free
`CGO_ENABLED=0` engine (`-native -backend tiered|spleeter|native-htdemucs|native-hpss`,
with `-download-models` / `-auto-download-models` for SHA-256-verified MIT
Spleeter + HTDemucs weights and a content-addressed HQ cache) speaking the
exact same `/v1/engine` wire protocol. In all modes the four stems sum back to
the input.

## Code

| Path | Role |
|---|---|
| `internal/stems` | Wire protocol: client `Proxy`, renderer `Backend`, LAN mDNS/UDP discovery (`FindLAN`), on-deck `RenderLocal` |
| `internal/stemsproc` | Deck client, run as `enginex-suite stems-processor` (`/usr/local/bin/stems-processor`) |
| `cmd/sc5000-stemsd` | Renderer HTTP service with multi-session support and LAN mDNS/UDP responder |
| `cmd/sc5000-relay` | Public entry point (`POST /v1/engine`, `GET /healthz/stems`) |
| `tools/stems` | `worker.py`, `run-home-worker.sh`, `run-home-worker.ps1`, `render.py`, `test_worker.py` |

## Deck client settings

With no environment variables the client reads stream-sync's
`/data/enginex/state/deemix-config.json` — the optional relay address
(`relayUrl`, e.g. `https://relay.example.com`) and password (`deemixPassword`)
the deck console manages — and posts to `<relayUrl>/v1/engine`. If no cloud
`relayUrl` is configured, `stems-processor` automatically discovers a home PC
running `enginex-stemsd` on the LAN via mDNS (`_enginex-stems._tcp.local.`) /
UDP broadcast (`17891`), or falls back to on-deck `RenderLocal` (`CPU3`).

| Variable | Effect |
|---|---|
| `SC5000_STEMS_URL` | Talk to this renderer or relay directly instead |
| `SC5000_STEMS_TOKEN` | Credential to present (replaces the config password, or the token file in direct mode) |
| `SC5000_STEMS_TOKEN_FILE` | Direct-mode token file, default `/etc/sc5000-stems/token` |
| `SC5000_STEMS_CONFIG` | Alternative relay settings file |

Addresses must be `https`, or `http` to a loopback, private or tailnet address.

## Build and test (on the build host, never on the Mac)

```sh
remote-build -- '. /etc/profile.d/buildtools.sh; go test -race ./internal/stems ./cmd/sc5000-relay && (cd tools/stems && python3 -m unittest test_worker)'
remote-build -- '. /etc/profile.d/buildtools.sh; CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o out/sc5000-stemsd ./cmd/sc5000-stemsd'
remote-build -- '. /etc/profile.d/buildtools.sh; GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o out/sc5000-suite ./cmd/sc5000-suite'
```

`render.py in.wav outdir --client <stems-processor>` runs a real WAV through
any client and writes vocals/bass/drums/melody WAVs.

## Session end

The client sends `X-Stems-Framing: 1`; `enginex-stemsd` then wraps its reply in
`'D'` + uint32 length frames and closes with `'E'` + `ok|failed` only after the
worker exited cleanly. The relay passes the header through. The client exits 0
only on `E ok` with a session `internal/stems.Session` finds complete (done,
every frame returned, `returnFrames=0` last), so it no longer waits for an
HTTP trailer the deck's edge proxy never delivered (2026-11-03: a 120 s idle
timeout and exit 1 after every render). Old clients or backends fall back to
the trailer. A greeting-only run (`</dev/null`) still exits 0.

`stems-probe [-seconds N] [-block N] <client>` plays Engine against a client:
lockstep blocks, `done`, stdin left open, and reports per-block wait and how
long the child took to exit. See `docs/STEMS-REMOTE-ACCEPTANCE-20261103.md`.

## Deploy

1. Back up the deployed relay binary, unit, `/opt/sc5000-stems`, the stemsd
   unit and `/etc/sc5000-stems` to agent-backup first.
2. Renderer: fresh installs use `install-backend.sh <staging-dir>` (refuses to
   overwrite). It generates `/etc/sc5000-stems/token` (0600) and starts
   [`sc5000-stemsd.service`](sc5000-stemsd.service) on `127.0.0.1:6596`. The
   worker needs system `python3` with NumPy and SciPy.
3. Relay: install the new `enginex-relay` binary and the credential drop-in
   [`../relay-host/50-stems.conf`](../relay-host/50-stems.conf); restart it and check
   `GET /healthz/stems` with the console password from a program that reads it
   from a file (not on a command line).
4. Deck: install the new `stems-processor`. The old client posted straight to
   the renderer's LAN address with `/etc/sc5000-stems/token`; that token file is
   no longer needed on the deck.

Upgrade order matters on a host still running the old LAN listener
(`192.168.6.104:6596`): either move the renderer to loopback before switching
the relay, or start the relay with `-stems-target http://192.168.6.104:6596`
until old deck clients are replaced.

## Rollback

Restore the previous relay binary/unit and remove the drop-in; restore the
previous stemsd unit (LAN listener) and the previous deck client plus its
token file from the backup. Keep user audio and databases untouched.
