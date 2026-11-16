# Sampler Sequencer 1.0.0

Official MIT-licensed 8-step × 8-slot pattern sequencer for EngineX OS's
built-in sampler ([#293](https://github.com/Cd1s/EngineX-OS/issues/293)).
The decks have no Python, compiler, or package manager: this Mod ships a static
Go helper (`bin/enginex-mod-helper-armv7` for `linux/arm` `GOARM=7` on all
gen-1 and gen-2 armhf decks, and `bin/enginex-mod-helper-arm64` for PRIME 4 G2
/ SYSTEM ONE), picked by busybox `sh` `start.sh` via `uname -m`. Requires EngineX OS `5.3.0.5` or later and QtQuick 2.12 / Controls 2.5 / Layouts 1.12.

Loader lifecycle: `sh start.sh start` / `stop`. Manual foreground execution:
`sh start.sh serve`. QML uses `127.0.0.1:17839`; host overlay component:
`EngineXMods/sampler-sequencer/Panel.qml`.

## Features

- **8 × 8 step grid**: 8 sampler slots × 8 steps, with current step highlight
  and live loaded/playing indicators from `/Engine/Sampler<1..8>/Loaded` and
  `/Engine/Sampler<1..8>/Playing`.
- **Transport & groove**: Play/Stop, step division (`1/4`, `1/8`, `1/16`),
  swing (`0%`, `25%`, `50%`), and Clear.
- **MASTER clock following**: Follows the MASTER deck BPM via
  `http://127.0.0.1:8080/v1/enginex/clock`, with a
  manual BPM fallback (`40–240` BPM).
- **Persistence**: Saves the active pattern and settings to
  `/data/enginex/mods-state/sampler-sequencer/pattern.json` (override parent
  with `ENGINEX_MOD_STATE`).

## Public Sampler Trigger Path

Engine exposes each sampler slot `N` (`1..8`) through public Planck/StateMap
properties (verified on `5.0.4` Gen-1, `5.3.0` Gen-2, and `AZ04` / SC LIVE 4):

- `/Engine/Sampler<N>/Loaded` (`translator.state`, read-only bool): `true` when
  a sample is loaded into slot `N`.
- `/Engine/Sampler<N>/Playing` (`translator.state`, read-only bool): `true`
  while slot `N` is playing.
- `/Engine/Sampler<N>/Triggered` (`translator.state = true`): triggers playback
  of slot `N` from QML without touching the audio thread.
- `/Engine/Sampler<N>/Stop` (`translator.state = true`): stops slot `N`.
- `/Engine/Sampler<N>/Label` (`translator.string`): slot display label.

Timing runs in a single-shot self-correcting QML `Timer` (`repeat: false`,
`interval = Math.max(1, Math.round(nextStepTimeMs - Date.now()))`) on the UI
thread so there is no HTTP round-trip on step edges and zero busy polling when
stopped.

Local API: `GET /status`, JSON `POST /action`:

- `{"action":"toggle","slot":1,"step":1}` (optional `"enabled":true|false`)
- `{"action":"set","bpm":124,"division":"1/16","swing":25,"followDeck":true}`
- `{"action":"clear"}`
