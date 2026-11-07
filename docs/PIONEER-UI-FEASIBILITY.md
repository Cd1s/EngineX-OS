# CDJ-3000 / XDJ-AZ UI feasibility

Investigation date: 2026-11-07. This records an initial inspection, not an
implemented Pioneer skin or a claim of hardware compatibility.

## Inputs and current boundary

Both archives were downloaded from the AlphaTheta firmware URLs supplied for
this task, and `unzip -tq` reported no compressed-data errors.

| Product | Archive | Archive SHA-256 | UPD SHA-256 |
| --- | --- | --- | --- |
| CDJ-3000 3.22 | `CDJ3Kv322.zip` | `c7c6a42dbd8fd2db10c5eb1a9e66cccef25b565ce01af46d76c24aa7cf390ac0` | `5c720c9bdd9aec50df5ebf2877d500b9a9a1fa08ef703c9813bed01df1e9ab05` |
| XDJ-AZ 2.00 | `XDJAZv200.zip` | `f56b84c034fec86f319451153ac24e0b7f1ba32cdf5c7366e9f7a75d57e07f6f` | `ac799829427ee0483cc8ba219684cb341b7ecf1efd0a771390508fefae077ef8` |

Each ZIP contains a single `.UPD`. `cryptsetup luksDump` identifies both as
LUKS1, AES-XTS (`xts-plain64`) containers, with a SHA-256 password-derivation
hash and a payload starting at sector 4096. The available files do not expose
the underlying filesystem or UI assets without the corresponding key. No
Pioneer executable, graphics, fonts, QML or other UI resources were recovered
in this inspection. Therefore the Pioneer application architecture and the
possibility of reusing particular assets remain unverified.

The local REA model agent `c1a7157a-1929-4fa0-b0f5-9569ff1afce7`
independently inspected the LUKS headers and Engine 5.3.0 four-deck QML.
It did not decrypt the payload or decompile a Pioneer application binary.

The downloaded files and extracted UPDs are stored outside the repository under
`/root/build/pioneer-ui-20261107-Gk03p7/`. They are not included in EngineX.

## EngineX integration points

The locally inspected 5.0.4 and 5.3.0 player Engine executables are ARM ELF32
hard-float binaries. Other device and Engine versions must be assessed
individually.

- `tools/ui-mod/engine_ui_hook.c` intercepts Qt resource registration and
  replaces existing resource-tree entries using files from the active language
  pack. It does not automatically add new paths to that resource tree, and
  currently selects a language rather than a separate theme. Selectable skins
  would require an explicit selection and fallback design, or separately built
  complete language packs.
- `tools/ui-mod/build_packs.py` patches the Engine QML resource tree and builds
  the existing English, Simplified Chinese and Traditional Chinese packs.
  Compatibility depends on the expected source anchors and resource paths.
- In the inspected Engine 5.3.0 QML tree,
  `Skins/EngineOS/UI/CompositeControls/PerformanceView/MainViewResponsiveWaveforms.qml`
  arranges deck areas, waveforms and pads using Engine controller models.
  `Decks/Decks.qml`, `Waveforms/HorizontalWaveforms.qml` and
  `Waveform/Waveform.qml` bind deck information and waveform rendering to those
  models. Reproducing a different layout must preserve those bindings, deck
  ownership and interactions rather than merely displaying static graphics.
- The current player four-deck unlock in `engine_ui_hook.c` is explicitly
  gated to Engine 5.3.0. It keeps two local stereo outputs and uses the
  configured deck role; four visible lanes do not imply four independent local
  audio outputs. EngineX's QML also manages shadow-deck synchronization and
  load restrictions.

The two vendors' UI components cannot be assumed interchangeable. A viable
adaptation would map recovered visual assets and layout requirements onto
Engine's own QML, waveform and deck controllers, while retaining EngineX
features and testing actual controls and playback on the target device.

## Baseline verification

The current working tree's `build_packs.py` successfully generated the
existing packs locally against the extracted Engine 5.0.4 and 5.3.0 QRC trees.
The archives are `baseline-packs-504.tar.gz` and `baseline-packs-530.tar.gz`
in the investigation directory. They are baseline EngineX packs, not Pioneer
skins. Generation checks are not a QML runtime, audio or hardware test.

Completing the requested skins requires verified access to the Pioneer UI
resources or an independently obtainable, sufficiently detailed visual
specification, followed by implementation and runtime/device validation. No
firmware, UI pack or device was deployed during this inspection.
