# Mod helper template (Go)

The decks run Engine OS without Python, a compiler or a package manager. A Mod is:

- QML (`Panel.qml` and other files listed in `mod.json` → `qmlFiles`);
- optionally a busybox `sh` lifecycle script (`mod.json` → `script`), run as `start.sh start` / `start.sh stop`;
- optionally a **static helper binary per CPU**: `linux/arm` `GOARM=7` (all gen-1 and gen-2 armhf decks) and `linux/arm64` (PRIME 4 G2 / SYSTEM ONE). Go with `CGO_ENABLED=0` is recommended: no runtime dependency on the deck.

Build both helpers on your computer:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" -o bin/helper-armv7 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o bin/helper-arm64 .
```

Then pack `mod.json`, your QML, `start.sh` and `bin/` (keep the executable bits) into a `.tar.gz` (up to 16 MiB).
`main.go` shows the contract the official Mods follow: loopback-only JSON on a fixed port, `GET /status`, `POST /action`,
a `health` command for `start.sh`, the PID in `$ENGINEX_MOD_STATE/<id>/helper.pid`, and SIGTERM to stop.
The official Mods' shared helper (`../helper`, `../modkit`) is a larger example of the same contract.
