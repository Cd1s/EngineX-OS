# #169 — PRIME 4+ / JC11S USB installation

The reported return to stock 5.3.0 has **not been reproduced**. The stock JC11S
5.3.0 rootfs activates the existing data bundle in the native ARM sandbox. The
confirmed defects addressed here are the installer's unverified `userdata`
fallback for JC11S and its unconditional prediction of successful activation.
This is installation validation and diagnostics, not a claim of a hardware fix.

## Released and integrated code

The reporter used v5.3.0.3 assets built from `b941ea4`. Comparing that commit with
`dfdef97` establishes the following:

| Change | Existing coverage | Remaining scope for #169 |
| --- | --- | --- |
| #129 (`f2619c9`, `a6ffa4f`, `8cb80ab`) | PRIME 4+ USB ID, Android Fastboot detection and transfer progress; already released | Does not verify a mounted/active installation |
| `066fb51` / `108865a` | Windows pnputil parsing and SC LIVE 4 guidance; missing-mke2fs diagnostic | No change to Gen-2 partition selection or activation |
| `ae45800` | First-generation USB/product fallback and rootfs flashing | Separate from JC11S data-image flashing |
| `7e7c2ff` (#164/#165 integration) | EAAS candidate discovery, Mixstream Pro+ USB ID, safe handoff to USB | No change to JC11S partition selection or bundle activation |

`build-data-bundle.py`, `host-install-userdata.py` and the bootstrap script did not
change in this range. No discovery or USB-identification implementation is
reimplemented by this patch.

## Official JC11S evidence

The originally supplied Gen-2 directory contained four other models, so a separate
PRIME4PLUS image was obtained from the official Engine DJ downloads page:

`https://public.inmusiccdn.com/Engine/5.3.0/RELEASE/d41cd81e67f225bf/PRIME4PLUS-5.3.0-Update.img`

SHA-256: `bf5489d19d7ebd932b7408ab3e51e080648f557c52b78fe2c515929dc611ce7e`.
The AZ0x parser verified the SHA-256 of every container record; this does not claim
an independent RSA-signature verification. Its rootfs record was decompressed
and extracted into a new sandbox directory. Existing laboratory material was
read-only.

The route is `15e4:d007` → `jc11s` → wait for Firmware Update Mode / fastboot →
`modeB`. The bundle is `PRIME4PLUS-…-EngineX-OS-Data-Bundle.tar.gz`; its contents
are injected into an ext4 image labelled `az01-data` with the `encrypt` feature.
The host injector writes `/enginex`, the compatibility symlink `/sc5000-custom`,
and ALSA configurations inside that image. Fastboot writes the entire selected
partition, not just `/data/enginex`.

The bootloader's GPT definition includes:

```text
name=data,size=-,type=0fc63daf-8483-4772-8e79-3d69d8477de4,uuid=931ad49d-ad59-0849-833a-9bf00af5b60e;
```

The stock rootfs contains:

```text
data.mount:
  Requires=az0x-data-mkfs.service
  After=az0x-data-mkfs.service
  What=PARTUUID=931ad49d-ad59-0849-833a-9bf00af5b60e
  Where=/data
var-lib.mount:
  What=/data/system/var/overlay/lib
  Where=/var/lib
az0x-data-mkfs.service:
  ExecStart=/sbin/az0x-data-mkfs
```

`data.mount` and `var-lib.mount` are enabled in `local-fs.target.wants`, and
`engine.service` is enabled in `multi-user.target.wants`. The formatter uses
the same PARTUUID. It calls `mkfs.ext4 -O encrypt -L az01-data` **only when
`e2label` exits 1**, not when the label text differs. Otherwise it runs
`tune2fs -O encrypt` and `fsck.ext4 -y`. The actual JC11S scripts were exercised
against both a blank loop image and the injected ext4 image; the latter's music,
version, bundle and ALSA hook survived.

The stock Engine launch script calls `encrypt-fs.sh`. That script recreates and
applies fscrypt policy to **only `downloads` and `stems`**. The sandbox ran this
script and the real stock `fscryptctl`, checked both policies, and verified that
the EngineX directory and ALSA configuration remained. The chroot exposes only
its own loop device, allocated from the host's loop pool and detached on exit;
Reporter, reset and reboot paths are blocked.

ALSA loads `/var/lib/alsa/conf.d/99-sc5000-custom.conf`, which loads the hook via
`/data/sc5000-custom/bundle/lib/libengine-ui-hook.so`. Its one-shot callback runs
`boot-activate.sh`. That writes the runtime Engine drop-in with `LD_PRELOAD` and
`Wants=enginex-stream-sync.service enginex-rb-bridge.service enginex-sc5000-deemixd.service`.
The sandbox verifies these files, the one-shot marker and the subsequent native
Engine's hook application and UI-pack loading. There is no evidence here of a
JC11S-specific missing path or destructive first-boot migration.

## Changes and validation

For JC11S only, the installer now requires a positive `partition-size:data` and
`partition-type:data: ext4`. It refuses a `userdata` fallback, even when a backup
was supplied. Official U-Boot contains size/type getvars and does not expose a
partition-UUID getvar, so this verifies the documented partition name/type, not
the live GPT UUID. Unknown responses stop before writing.

After injection, the installer finds the real installation directory (`enginex`
or the legacy `sc5000-custom` retained until first boot), reads both version markers and compares the
activator, hook library (including their legacy paths) and active ALSA config
byte-for-byte with the checked bundle. Missing or stale data causes a clear
`nothing flashed` error. This also works with an older bundle injector, which
only checks the presence of the suite and version file. Oversized images are
rejected. A USB write completion message explicitly leaves activation pending
and gives an About-screen check; it no longer says activation or the custom
Update.img path is already unlocked.

The shim tests cover the JC11S path, wrong/missing target partitions, a real
legacy directory backup, and stale readback of every checked activation artifact.
`JC11S_RELEASE_BUNDLE` also exercises the actual published archive through the Go
verifier with both fresh and legacy images. To include both SC LIVE 4 and a
separate real PRIME4PLUS rootfs in the bootstrap script, provide
`JC11S_ROOTFS=/path/to/prime4plus/rootfs`. A standalone run without this fixture
explicitly warns that JC11S was not tested; an explicitly missing path fails.
The release workflow requires the fixture. The existing AZ04-only mode is retained.

Recorded sandbox output:

```text
=== All SCLIVE4 verification checks PASSED ===
JC11S stock mkfs (blank/existing) and fscrypt first boot: PASS
Stage 1 ALSA boot activation: PASS
Stage 2 PRIME4PLUS Engine hook: PASS (19 memory edits, UI pack loaded)
=== All PRIME4PLUS verification checks PASSED ===
```

The initial JC11S run also passed before installer hardening. No change to the
bundle's activation implementation is justified by these results.

The **published v5.3.0.3 PRIME4PLUS data bundle** was then tested separately,
using the GitHub asset whose SHA-256 is
`f67d214cfbe4512d9afb37ff82baec8d3ffa814085594bdd05d8aa3ab0d0e3fd`.
Its download matched that digest. With this actual released hook and data
payload, host injection (867 files), the official JC11S blank/existing mkfs and
fscrypt checks, and Stage 1 ALSA boot activation all passed. This additional run
ended after Stage 1; the 19-edit/UI-pack Stage 2 assertion above uses the current
integration hook. The ALSA one-shot activation source between its #61/#67 and
#119 markers is byte-identical in `b941ea4` and `dfdef97` (SHA-256
`22c6052b442f00e19ad22be9709ece2898044242cb37c088c75d0effaf100941`).
Thus the released bootstrap itself also works in this sandbox; the integrated
discovery fixes must not be described as a demonstrated fix for the reporter's
post-reboot symptom.

Required checks passed on the ARM build/test host:

```text
gofmt -l .: PASS (empty)
go vet ./...: PASS
go test -race ./...: PASS
go test -race ./cmd/enginex-installer/... ./pkg/...: PASS
Published v5.3.0.3 bundle through Go verifier (fresh + legacy images): PASS
tests/test_rootfs_overlay.sh: PASS (18 tests, mocked storage/hardware)
python3 -m unittest discover -s tools/az01-packer:
Ran 135 tests in 35.452s
OK (skipped=1)
bash syntax: PASS
```

After review remediation, all the checks above were rerun. The full bootstrap
run supplied the real JC11S fixture; a separate SC LIVE 4 run without it emitted
the expected WARN and succeeded, while an explicitly missing JC11S path failed
before building. The published-bundle Go fixture was supplied for both race
commands and both fresh/legacy subcases passed.

The first full race run caught the old SIGPIPE fixture's assumption that JC11S
could fall back to `userdata`. The fixture now uses JC11S's verified `data`
target while keeping its broken-stdout-pipe assertion; the full rerun passed.

For LAN discovery, the official JC11S and MIXSTREAMPROPLUS rootfs records differ,
but their extracted `/usr/Engine/Engine` files are byte-identical, with SHA-256
`68cb478d120d2793d5883876abe6eba49a3ca78b80abf041466113f93e1f0d58`.
The binary contains `EAAS beacon is created and Bind to Any:`. This supports
the shared-protocol investigation; it is not a JC11S packet capture. The current
listener accepts the six-byte `EAAS\x01\x00` beacon independently of model and
does not assign a board identity from it.

## 拿不准

- There is no reporter-side installer log, fastboot getvar transcript, post-boot
  marker readback or packet capture. Whether "USB" meant a computer USB cable or
  a USB stick / on-screen updater is unconfirmed. A signed stock updater and a
  fastboot data write are different paths; transfer success alone distinguishes
  neither. The custom Gen-2 Update.img wrapper requires an already active hook
  and is not a way to bootstrap stock signature verification.
- The exact original activation failure remains unknown. Sandbox success and
  pre-write checks do not prove that a particular physical deck booted the
  installation. This change does not implement post-reboot version readback.
- No physical JC11S `getvar partition-type:data` transcript is available. Static
  analysis establishes that U-Boot implements the type getvar, but not its exact
  response on the reporter's data partition. An erased/unrecognized partition
  may report `raw` or an error and will be refused. This fail-closed guard may
  require a read-only bootloader transcript before hardware acceptance; do not
  override it or write `userdata` to work around an unknown response.
- #165's six-byte EAAS listener is model-independent and can discover a JC11S
  emitting that beacon. It deliberately treats it as an unverified candidate,
  then obtains model identity from USB/fastboot. A JC11S capture is still needed
  to establish that the reporter's LAN failure was the same EAAS-only condition
  as #164; firewall, topology and device settings also remain unmeasured.
- The sandbox checks enabled stock units and follows their relevant ordering;
  it does not boot the deck's complete systemd environment. The native Engine
  run is limited to hook/patch/UI-pack assertions and can abort without physical
  peripherals. It is not a full interactive hardware acceptance test.
- Craft Anything was used to open the stock script and Engine targets. Its
  configured AI-backend list was empty, so the requested DeepSeek backend could
  not run; no credentials or tool configuration were changed.

## English draft for the reporter

Thanks for the report. We checked the PRIME 4+ (JC11S) 5.3.0 firmware and tested
its startup scripts in an isolated sandbox. We have not yet reproduced why your
unit returned to stock, so please do not factory-reset it or repeat a full data
flash just to see whether it works.

After a normal reboot, check Settings → About. It should say **EngineX OS** with
the EngineX version. Also try the EngineX page at `http://<deck-IP>:8080` from
the same LAN. Seeing stock 5.3.0 means activation has not been confirmed, even
if the file-transfer step said it succeeded.
The EngineX page may ask for the access code shown by the deck.

Once the maintainer supplies the updated installer, back up the deck's internal
music, library and settings first. Use the **PRIME4PLUS** data bundle and the
matching installer, enter Firmware Update Mode on the deck, and connect a USB
data cable directly to the computer. On Windows, follow the existing WinUSB /
WSL prerequisites. A fresh USB data-image installation replaces the entire
internal data partition; `--userdata-backup` requires a verified **raw ext4 image
of this deck's data partition**, not a folder or a file-level music backup.
That is the preserving route. Do not use a first-generation PRIME4 image or expect a custom
Gen-2 Update.img on a USB stick to unlock stock firmware.

Keep the complete installer log. The updated installer checks the JC11S target
and activation files before flashing; if it reports `nothing flashed`, stop and
send that error rather than forcing another partition. After it reboots, repeat
the About/page checks. If it still shows stock, please send the installer log,
whether USB meant a cable or a stick, and a photo or exact text of the About
screen so we can identify the remaining failure. This installer change is not
yet part of the published v5.3.0.3 assets.
