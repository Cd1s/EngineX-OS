# Issue #140 Investigation — Denon PRIME 4 (`JC11`) Second-Boot Splash Hang

## 1. Reported Symptoms

1. First-generation Denon PRIME 4 (`JC11`) ran EngineX OS `5.3.0.2` normally.
2. Updated over Wi-Fi OTA to `5.3.0.3`; the first boot of `5.3.0.3` completed normally.
3. On the second power-on (after hard power-off), the deck remained on the `"engine dj 5.3.0"` screen for 5+ minutes with pre-Engine controller LEDs (`Filter FX` flashing, `FX 2` lit on channels 3/4), while `stream-sync` on `:8080` answered HTTP requests.
4. Flashing stock Engine OS `5.3.0` booted normally (preserving `/data`).
5. Re-flashing `5.3.0.2` or `5.3.0.3` over that `/data` hung on the first boot; a second user reproduced the hang with the internal 2.5" SATA SSD removed.

## 2. U-Boot Framebuffer Splash vs. QML Splash Proof

Inspecting the AZ01 update containers proves which stage painted the stuck screen:

- Both stock `PRIME4-5.3.0-Update.img` and `PRIME4-5.3.0.3-EngineX-OS-Denon-PRIME-4-JC11-Update.img` carry the identical 4,096,000-byte raw U-Boot `splash` partition (`SHA-1: 67551db23cde78946fd7636449a2874ba3469869`), which renders `"engine dj"` + `"5.3.0"` into the framebuffer before Linux starts.
- By contrast, `SplashScreen.png` inside `5.3.0.3`'s `/usr/share/sc5000-custom/ui-packs.tar.xz` (`12,698` bytes) renders `"engine X"` + `"5.3.0.3"` once Qt/QML paints frame 1.
- Combined with the pre-Engine MCU LED pattern and live `:8080` `stream-sync` daemon, Linux `multi-user.target` is up, but `/usr/Engine/Engine` on the affected physical `JC11` stalls or fails before committing its first QML frame to DRM/KMS.

## 3. Why Re-Flashing `5.3.0.2` After `5.3.0.3` Fails `sc5000-custom-prepare`

In `5.3.0.3`, `sc5000-custom-prepare` (`MIGRATE` in `tools/az01-packer/az01_packer.py`) renames `/data/sc5000-custom` to `/data/enginex` and creates the compatibility symlink `/data/sc5000-custom -> enginex`.

When `5.3.0.2` is subsequently flashed without erasing `/data`, `5.3.0.2`'s older `sc5000-custom-prepare` executes:

```sh
for path in /data/sc5000-custom ...; do
    [ ! -L "$path" ] || { echo "Refusing persistent SSH symlink: $path" >&2; exit 1; }
done
```

`sc5000-custom-prepare.service` exits `1` on `/data/sc5000-custom` being a symlink, so `5.3.0.2` never unpacks its `5.3.0.2` UI packs and instead boots `5.3.0.2`'s hook/rootfs against `5.3.0.3`'s `/data/enginex` directory. Stock `5.3.0` ignores `/data/enginex` and `/data/sc5000-custom` completely.

## 4. Multi-Boot + Power-Cut Sandbox Matrix

Using unpacked `JC11` (`PRODUCT=JC11`, `compatible=inmusic,jc11\0rockchip,rk3288\0`) and `JP07` (`SC5000`) root filesystems on native `armhf` with a populated internal drive (`Engine Library/Database2/m.db` schema `3.0.2` + PCM WAV tracks at `/media/az01-internal`) and hard `SIGKILL` of the entire PID/mount namespace (no `SIGTERM`, no `ExecStopPost`):

| Sequence | Platform | Boot 1 UI-Ready | Power Cut | Boot 2 UI-Ready | Notes |
| --- | --- | --- | --- | --- | --- |
| `JC11`: `stock 5.3.0 -> 5.3.0.2 -> 5.3.0.3` | `offscreen` | `16.0 s` | `SIGKILL` | `16.1 s` | `SplashScreenLoader` dismissed (`0\|1`), `.cache/qml` purged via `running-sentinel`, `boot-count=1` |
| `JC11`: `stock 5.3.0 -> 5.3.0.2 -> 5.3.0.4` | `offscreen` | `24.1 s` | `SIGKILL` | `24.0 s` | `19` ELF patches, `21` DSP guards, SQLite hooks, `:8080` + `:6595` healthy |
| `JC11`: OOB-dismissed `ConsoleMain` (`views.json` saved) | `offscreen` | `40.9 s` | `SIGKILL` | `41.7 s` | `DeckCount_4`, `ConsoleMain` restored directly on Boot 2 and across `5.3.0.4` upgrade |
| `JC11`: `stock 5.3.0 -> 5.3.0.3` (`1280x800` + `four_deck_waveform=1`, `ui_scale=85`) | `eglfs_kms` | `25.1 s` | `SIGKILL` | `25.1 s` | Full KMS/GBM scene-graph render verified across Boot 1, Boot 2, and 4-deck scaled mode |
| `JP07` (`SC5000` control): `stock 5.3.0 -> 5.3.0.2 -> 5.3.0.3` | `offscreen` | `7.2 s` | `SIGKILL` | `7.2 s` | Control passes identically |

The second-boot hang did **not** reproduce in either `offscreen` or `eglfs_kms` sandboxes.

## 5. Remaining Real-Hardware Differences on Physical `JC11`

Because both `offscreen` and `eglfs_kms` complete Boot 2 cleanly on the identical rootfs and `/data` state, any physical-deck-only hang before QML frame 1 narrows to hardware subsystems absent from the sandbox:

1. **USB MIDI/HID Control Surface & Jog Wheel Displays**: Physical `JC11` enumerates composite USB controller endpoints and platter screens (`JC11_Controller_Assignments.qml`, `JC11_Display_Assignments.qml`, `JC11_Mixer_Assignments.qml`) and performs MCU firmware/LED handshakes (explaining the stuck `Filter FX` / `FX 2` LEDs).
2. **Broadcom UART Bluetooth (`bcm43438-bt` + `bluetoothd`)**: `prepare_config` sets `hasBluetooth=1` and `hasBluetoothSendToAssignment=1`; on a real `JC11`, `Engine` queries `org.bluez` over system D-Bus against live HCI adapter state persisted in `/var/lib/bluetooth` (`/data/system/var/overlay/lib/bluetooth`).
3. **XMOS USB Audio (`xmos-update.service` / `xmosdfu` / `usb5537`)**: Runs `Before=engine.service`, toggles the `usb5537` hub, checks/flashes XMOS DFU firmware, and provides the hardware ALSA clock for `PRIME 4 Audio`.
4. **Physical RK3288 MIPI-DSI Panel (`urt,umo-p080md-t`, 90 deg rotation) & Mali-T760 (`panfrost` `ffa30000.gpu`)**: Real VOP/DSI page-flip and `panfrost` shader compilation vs. virtual DRM + `llvmpipe`.
5. **Hardware `fscrypt` (`encrypt-fs.sh`)**: Shimmed in the release gate; runs on `/data/downloads` and `/data/stems` on physical decks.
6. **Real `systemd` PID 1 Transaction & `6.18.57-az01-rt6` `PREEMPT_RT` IRQ Threads**: Real threaded IRQ priorities (`chrt -f 40` / `99`) and unit ordering under `multi-user.target`.

## 6. Release-Gate Hardening Added

To ensure no `/data` or internal-drive state regression can ever slip past single-boot testing:

- `internal/testlibrary.ExportDrive` (`SC5000_GATE_LIBRARY`) populates an `Engine Library/Database2/m.db` (`3.0.2`) + WAV fixture bind-mounted at `/media/az01-internal` in every release-gate phase.
- `tools/az01-packer/gate-ui-probe.qml` waits for `splashScreenController.viewModel.isUnlocked` before opening Control Center and emits `SCPROBE splash|<active>|<unlocked>|`, asserted as `SCPROBE splash|0|1|` on both boots.
- `tools/az01-packer/release-gate.sh` starts both new-release boots through `engine-ui-guard service` with the boot watch (`engine-guardian.service`) and requires the main UI to acknowledge the current boot attempt (UI-ready replaced the 15 s uptime window), drops `CAP_SYS_TIME` via `setpriv`, kills `--phase new` with `SIGKILL` while `Engine` and daemons are live, verifies `running-sentinel` survival and dirty `.cache/qml` purge, and runs `--phase boot2` (`second boot after power cut on the same /data`) across all three 1st-gen image groups (`JP07`, `JP13`, `JC11`).
