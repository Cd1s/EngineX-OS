# Stock handback and USB profiles (2026-11-13)

Status: development branch `agent/5305-stock-handback`, not released. The
community report's exact model, last shutdown and original `/data` are still
unavailable. The sandbox reproduces a specific cache failure, not a confirmed
diagnosis of that user's deck.

## Stock boot failure

Tests used actual `5.3.0.4-r5` JP07/JP13 images and the corresponding official
5.3.0 SC5000, SC5000M, SC6000 and SC6000M root filesystems. Each firmware ran
in a private mount, network and PID namespace with chroot; update, reboot and
systemd commands were shims. No physical deck was used.

The 24-case handoff matrix combined four boards, media fixtures present/absent,
and clean shutdown/interrupted native Client-state write/interrupted native
GUI-state write. Every case reached stock UI. r5 had saved Player4, role24,
Paired Player, ShowLayerB and streaming-source settings. An interrupted Client
JSON could reset Player to 1 but did not prevent boot. The complete vendor
launcher and `/data` with 65536 or zero bytes free also reached UI. The media
matrix used directory fixtures, not physical USB/SD insertion events.

A separate fault injection reproduced a stock crash with just this file:

```text
/data/AppDataUser/AIR Music Technology/Engine/.cache/qml/650de302d3a1a20b034b79ab74ce016cb2479e35.qmlc
```

It is the 94184-byte cache for
`QtQuick/VirtualKeyboard/content/styles/BaseKeyboardStyle/BaseKeyboardStyle.qml`.
Keeping its length, header and timestamp, replace bytes `[512:1024]` with
`0xff`. Unpreloaded stock Engine then exits 139 before UI. Delete only that
file and the same `/data` reaches UI. Simple truncations were safely rejected
and recompiled; they did not reproduce this crash. This is injected corruption,
not evidence that an ordinary reflash necessarily corrupts bytecode.

The fix redirects EngineX bytecode to `ui/qml-cache`, outside stock's cache.
Engine overwrites `QML_DISK_CACHE_PATH` itself, so a plain exported environment
variable cannot isolate it. The hook forwards `setenv` normally except for
that key in Engine outside safe mode. Qt created the private directory without
an extra startup command. JP07 and JP14 created approximately 970 private cache
units and no shared ones; stock booted with Player4 after the same private-cache
damage. Stock fallback continues using stock resources and its shared cache.

The guard clears legacy shared bytecode at pre-start, retains healthy private
bytecode, and discards private bytecode after crashes, power cuts or fallback.
Bundle uninstall removes both QML caches and preserves profiles, libraries,
accounts and music caches.

## Recovery without SSH

For the reproduced corrupt-cache case, reflashing the matching r5 image over
the stuck stock installation recovered without changing user data: the first
r5 Engine attempt also crashed, its guard removed the bad cache, and the next
attempt reached UI. Stock then booted on the recovered data too.

For SC5000, SC5000M, SC6000 and SC6000M, the
[official recovery instructions](https://support.enginedj.com/support/solutions/articles/69000797882)
say to hold **MEDIA EJECT + SOURCE + LAYER** while powering on, then release at
Computer Update or the chip icon. A USB stick containing the matching firmware
is preferred by the updater. This enters the updater, not a data-reset mode.
No USB-only or button-only procedure that clears this cache while retaining the
installed stock rootfs has been verified.

Before restoring stock, boot an EngineX build containing this fix once, shut
down normally, then install the official image for the exact model. The fixed
Gen-2 installer/web-console uninstall also clears the QML caches. Gen-1 web
uninstall still refuses: returning to its stock rootfs requires a firmware flash.

The #234 `leave` transaction already removes the shared QML cache when installing
a bundle, including a downgrade. A stock firmware flash does not execute it.
`leave layout` only restores the directory layout for older EngineX releases;
it does not clear caches or uninstall a Gen-1 rootfs. Neither transaction needs
to rewrite Planck state or library databases for this cache fix.

## Player number, profiles and layers

r5's UI pack answered the vendor profile question with No automatically. The
fix removes that override, leaving the native Yes/No dialog and profile backend.
All three generated language packs wait for input and load only after Yes.

Native profile tests registered a folder drive with Engine's `-extraDriveDir`
interface, selected it and saved `Engine Library/user.profile`. They used
`PlayerColor4A`, a field actually serialized by the native exporter:

- Stock exported `#123456`; after changing the device to `#654321` without the
  drive, stock and fixed r5 imported `#123456`. Old r5 kept `#654321`.
- EngineX exported `#abcdef`; after changing the device to `#fedcba` without the
  drive, stock imported `#abcdef`.

Source profile files were checked unchanged after the device-only changes.
The exported profiles did not contain a Player-number field. A separate test
changed the destination through the native API without its drive: stock's
profile imported into EngineX kept device Player2; EngineX's profile imported
into stock kept device Player3. Both imports loaded the expected colors, exited
cleanly and saved those numbers in `PlanckClientState.json`. A USB profile
therefore did not restore a different player number in these tests. Automatic
numbering across four linked physical players and hardware USB/SD insertion
remain untested.

A separate native test found a Player-number loss: set Player1 to Player4 and
accept the deck menu's restart-to-apply prompt. The old SIGTERM path exits 143
and leaves Player1 on disk. After saving the EngineX option, the fixed menu calls
the vendor `Planck.quit(QuitReasons.AnalyticsChange, false)`: Engine exits cleanly
and saves Player4. The next pre-start reads 4 and resolves Auto to 24. Existing
#49 roles, odd→13/even→24 and #201 tests remain the regression boundary.
The web console's `restart=1` and retry paths still use SIGTERM and can lose
unsaved native state; this change covers the deck-menu prompt.

Finally, selecting Layer B and shutting down cleanly returned to Layer A on
both stock and r5 Paired Player/role24. Player4 remained saved. No layer-selection
or physical A/B role rule is changed.

## Regressions

`test_qml_cache.c` checks the actual hook's libc contract on armhf and arm64.
`QMLCacheCompatibilityTest`, `USBProfileCompatibilityTest`,
`NativeRestartCompatibilityTest`, the overlay guard tests, and the bundle
uninstall test cover the changed behavior. Each fix has an old-code failure;
the native firmware experiments above provide a separate runtime check.
The release gate checks that real hooked Engine writes private bytecode and
leaves no shared bytecode, and seeds both caches before its power-cut reboot.
JP07/JP14 were exercised during this investigation; the other image groups
must pass the same gate before release. Real Gen-2 cache redirection remains
unverified beyond the armhf/arm64 hook contract tests.

Final checks on the build host passed: gofmt, vet, the web UI race suite, all
27 overlay checks, 242 UI/docs/shadow/bundle tests (one existing `/data` fixture
skip), and SC LIVE 4, JC11S and AZ04 bootstrap stages. Each sandbox mount stayed
inside a private namespace and the host `/tmp` remained mode 1777. This is not
a firmware release; complete per-image release gates remain required.
