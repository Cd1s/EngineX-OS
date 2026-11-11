# Issue #176: SC LIVE 4 Stems processor detection

## Report and installation layout

The SC LIVE 4 reporter has a Stems license. Choosing **Generate Stems** opens
Settings instead of starting a render; pre-rendered Stems play normally. This
points to processor installation detection, rather than a playback or license
failure. Physical confirmation of the fix is still pending from the reporter.

First-generation JP07/JP13/JC11 image groups include
`/usr/local/bin/stems-processor` in `rootfs-overlay`. Their release gates can
execute that path directly. Second-generation data-bundle installations keep
the stock root partition read-only: the processor wrapper is instead at
`/data/enginex/bundle/bin/stems-processor`, also reachable through the
`/data/sc5000-custom` compatibility link. Passing a first-generation gate does
not prove second-generation processor detection.

## Cause and fix

In Engine `5.3.0-sclive4`, `StemsProcessorManager::checkProcessorInstalled()`
(`0x8c0374`) and `isProcessorInstalled()` (`0x8c0d5c`) obtain the patched
`/usr/local/bin/stems-processor` path through `0x8ba000`, then call
`QFile::exists()` (`0x8c04b4`, `0x8c0e24`). The stock Qt 6.7.2 implementation
uses `statx`, which was missing from the existing access/spawn/exec fallback.
The absent rootfs path therefore looked uninstalled despite an executable
bundle wrapper.

The hook now applies the same processor fallback to `statx` and the
open/open64/openat/openat64 wrappers. It redirects only inside the Engine
process, only when the original processor path is not executable, and only to
an executable bundle processor. An installed executable stock processor keeps
priority; unrelated paths and other processes retain their original behavior.
The production bundle paths are unchanged.

## Validation scope

Sandbox/unit validation covers the three armhf Engine profiles and AZ04,
including missing/non-executable bundle processors, both bundle layouts,
statx/open/access and non-Engine pass-through. The two C tests override the
bundle paths at compile time with buffers populated under `mkdtemp`; they
remove their files and directories and never write the host's `/data` bundle.
Both committed hook copies must match a fresh source build.

A sandbox using a freshly extracted official SCLIVE4 5.3.0 image, with all
container record SHA-256 values checked, installed the actual 5.3.0.4 data
bundle onto a temporary `/data` while the rootfs stayed read-only. An armhf
probe named `Engine` returned `statx=-1 (ENOENT)`, `access=-1`, `open=-1`
without preload, and `statx=0`, `access=0`, `open>=0` with the committed hook.
The same probe named `NotEngine` still returned failure with preload. No
processor file was added to `/usr/local/bin`.

The SCLIVE4, separate PRIME4PLUS and AZ04 bootstrap sandboxes passed. This
validates processor discovery and bundle activation in a sandbox; it does not
exercise the Stems service or prove a completed render.

Physical SC LIVE 4 confirmation is required: Generate Stems must start a render
instead of opening Settings, finish successfully, and leave pre-rendered Stems
playback working. This does not claim an end-to-end render on physical hardware.
