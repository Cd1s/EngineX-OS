# Sampler Pad Studio 1.0.0

Official MIT-licensed independent eight-slot file manager for
[#120](https://github.com/Cd1s/EngineX-OS/issues/120) and
[#137](https://github.com/Cd1s/EngineX-OS/issues/137).
Requires Linux, Python >=3.9 and QtQuick 2.12 / Controls 2.5 / Layouts 1.12.
WAV loop slicing uses Python's wave module; imports and compressed sources need
an **already installed** ffmpeg. No dependency installation or audio-thread hooks.

Loader lifecycle: `sh start.sh start` / `stop`. Manual foreground execution:
`sh start.sh serve`. QML uses `127.0.0.1:17836`; host overlay component:
`EngineXMods/sampler-pad-studio/Panel.qml`.

Select one of eight slots, enter an audio path under `/media` or `/mnt`, import,
rename or clear the slot. Loop IN/OUT are integer **sample offsets**, not
milliseconds; sampleRate must match the source. End is exclusive. Direct PCM WAV
slicing validates rate/bounds and preserves channel/sample format. Other formats
are decoded by ffmpeg with sample-accurate atrim. A slice is capped at 10 minutes.
Empty or truncated output is rejected. Renaming updates the bank's display name;
clearing removes only its reference, retaining exports for recovery.

Bank and exported WAVs are stored in
`/data/enginex/mods-state/sampler-pad-studio` (override parent with
`ENGINEX_MOD_STATE`). The selected exported path is displayed in the panel.
**Load that WAV from the native browser into a native sampler slot.**
Slot numbers in this Mod identify its independent file bank. There is no proven
public native pad URL binding, triggering or playback API, so this Mod does not
pretend to load/rename/clear a native pad or to read the deck's active loop
automatically. Enter loop points manually; native pad integration remains a
separate firmware task. All-model hardware operation remains unverified.

Local API: `GET /status`, JSON `POST /action`:

```json
{"action":"slice","pad":1,"source":"/media/USB/song.wav","inSample":11025,"outSample":33075,"sampleRate":44100}
```

Other actions: `import` with pad/source, `rename` with pad/name, `clear` with pad.
Source files are never modified or deleted.
