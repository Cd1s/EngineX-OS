# CPU & RAM Load

Displays live CPU and RAM load in the top status bar next to the clock.

- **Status bar overlay**: maps `TimeIndicator.qml` to `Skins/Embedded/Views/StatusBar/Indicators/TimeIndicator.qml` via `mods-overlay`.
- **Loopback helper**: `enginex-mod-helper cpu-ram-monitor serve` reads `/proc/stat` and `/proc/meminfo` and serves `GET /status` on `127.0.0.1:17840` (static Go binary for armhf and arm64, no Python).
- **Graceful fallback**: if the helper is not running, `TimeIndicator.qml` hides the load label and keeps the stock clock unchanged.
