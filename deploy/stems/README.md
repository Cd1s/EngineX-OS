# Remote Stems renderer (`sc5000-stemsd`)

Engine 5.0.4 runs its Stems separator as a child process. On SC5000 that child
is `stems-processor`, which forwards the child's byte stream unchanged to a
remote renderer; Engine still writes the encrypted `.stems` file itself.

```text
Engine ─stdin/stdout─ stems-processor ══HTTPS══ sc5000-relay /v1/engine ── 127.0.0.1:6596 sc5000-stemsd ── worker.py
          (deck)          console password          (public)      internal token        (loopback)
```

The separator in [`tools/stems/worker.py`](../../tools/stems/worker.py) is a
causal spectral HPSS / centre-harmonic approximation, not a neural model. The
four stems sum back to the input.

## Code

| Path | Role |
|---|---|
| `internal/stems` | Wire protocol: client `Proxy`, renderer `Backend`, client settings `ResolveClient` |
| `cmd/stems-processor` | Static ARMv7 deck client, installed as `/data/sc5000-custom/bin/stems-processor` |
| `cmd/sc5000-stemsd` | Renderer HTTP service; one session at a time |
| `cmd/sc5000-relay` | Public entry point (`POST /v1/engine`, `GET /healthz/stems`) |
| `tools/stems` | `worker.py`, `render.py` (WAV → four WAVs through any client), `test_worker.py` |

## Deck client settings

With no environment variables the client reads stream-sync's
`/data/sc5000-custom/state/deemix-config.json` — the same relay address
(`relayUrl`, default `https://denon.cheek.day`) and password (`deemixPassword`)
the deck console already manages — and posts to `<relayUrl>/v1/engine`. No key
file, LAN address or extra port is needed on the deck.

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
remote-build -- '. /etc/profile.d/buildtools.sh; GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o out/stems-processor-armv7 ./cmd/stems-processor'
```

`render.py in.wav outdir --client <stems-processor>` runs a real WAV through
any client and writes vocals/bass/drums/melody WAVs.

## Deploy

1. Back up the deployed relay binary, unit, `/opt/sc5000-stems`, the stemsd
   unit and `/etc/sc5000-stems` to agent-backup first.
2. Renderer: fresh installs use `install-backend.sh <staging-dir>` (refuses to
   overwrite). It generates `/etc/sc5000-stems/token` (0600) and starts
   [`sc5000-stemsd.service`](sc5000-stemsd.service) on `127.0.0.1:6596`. The
   worker needs system `python3` with NumPy and SciPy.
3. Relay: install the new `sc5000-relay` binary and the credential drop-in
   [`../clawbot/50-stems.conf`](../clawbot/50-stems.conf); restart it and check
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
