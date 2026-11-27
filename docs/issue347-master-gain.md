# PRIME 4+ master gain investigation (#347)

Status: unresolved. No cause has been demonstrated and no production audio
code has been changed. Stock PRIME 4+ and published EngineX r1 produce the
same PCM level in the measured cases. A gain fix and fail-before regression
remain blocked on a reproducible difference.

The reported master-knob positions are an owner observation, not a measured
dB difference. Knob taper and the complete output stage must be measured
before converting those positions into a gain change.

## Native stock versus EngineX measurement

The official PRIME4PLUS 5.3.0 Engine ran natively as armhf with JC11S device
identity. A 40-second stereo, 16-bit, 48 kHz WAV contains a 1 kHz sine with
peak 8192/32768 (-12.0412 dBFS). The native librarian loads it through
`/Client/Librarian/AddToDatabaseAndLoadToDeck1`; the native deck plays it.
The audio device opens 10 input and 10 output channels at 44.1 kHz, in
256-frame blocks. A fixture measures the actual interleaved S32_LE samples
passed to `snd_pcm_writei`; it does not invoke a mixer/DSP method.

The reference uses stock Engine audio code with a QRC-only diagnostic hook
whose constructor returns before any Engine patch, DSP hook or polling
worker is installed. The existing test-only SoC identity shim and ALSA null
card are identical in every run. EngineX uses the published hook, with
29 Engine edits and 21 DSP guards confirmed active. Native Master Limiter is
code 13 (stock UI: Off), channel-1 fader code is 102, and every run starts
with fresh test data and the same WAV.

After five seconds of playback warmup, the measurement takes 441 consecutive
256-frame blocks: 112896 frames, 2.56 seconds, exactly 2560 periods at 1 kHz.
This avoids partial-cycle RMS bias. The first output pair measures:

| Case | RMS dBFS | Peak dBFS | RMS difference from stock |
| --- | ---: | ---: | ---: |
| Stock audio code, QRC-only loader | -33.558864 | -30.548376 | 0.000000 dB |
| Published EngineX r1, defaults | -33.558864 | -30.548376 | 0.000000 dB |
| EngineX, `stems-eq` disabled | -33.558864 | -30.548376 | -0.000000009 dB |
| EngineX, `stems,stems-eq` disabled | -33.558864 | -30.548376 | 0.000000 dB |
| EngineX, `master_limiter=1` | -33.558864 | -30.548376 | 0.000000 dB |

Output pairs 1, 2 and 4 carry the tone. The largest RMS difference on any
active channel is 0.000000023 dB; peaks are equal. Independent five-second
windows vary by less than 0.00014 dB because their end blocks include partial
periods. These are PCM measurements, not analog voltage, loudspeaker loudness
or physical DAC-control measurements.

## Candidate results

1. **ALSA configuration and mixer defaults:** no attenuation was found in the
   shipped configuration. The published `99-enginex.conf` adds only the boot
   hook. In the stock rootfs, a native armhf libasound probe opens the null PCM
   before and after loading that file; serialized `defaults`, `pcm` and `ctl`
   subtrees are byte-identical, SHA-256
   `a41be6bef400d0351597256b6f62e14fe472f6e4b9c7facd1044060dd481a46b`.
   `enginex-usb-audio` sets no onboard PCM/DAC volume. Stock units contain the
   speaker-only `eta5805-config@` command `amixer ... Master 0dB`; its udev
   rules target NH08/NH10, not JC11S. The hook interposes ALSA rate/timestamp
   functions, with no `snd_ctl` mixer write. Physical mixer/DAC controls and
   the hardware effects of the boot-time Engine restart remain unmeasured.
2. **EngineX DSP, #58 and #54:** no fixed pre-gain or headroom attenuation was
   found. The real `hook_master_output` around a known reference callback
   preserves every below-knee sample on all eight tested channels, at
   48/96 kHz, in native and armhf programs. Input peaks -12.0412 and
   -1.9382 dBFS give exactly 0 dB RMS/peak difference with the limiter enabled.
   An isolated +6.0206 dBFS sine gives -5.02615 dB RMS reduction and a
   -0.104861 dBFS output peak, demonstrating the intended clipping protection.
   Auto-Gain boosts the quiet test by about +7.583 dB. Flanger compensation
   corrects a 0.7x wet signal (-3.098039 dB) by +3.098033 dB on armhf, leaving
   -0.000006 dB relative to dry. Its gain is never below unity.
3. **Settings and native configuration overrides:** no default master/booth
   attenuation was found. `master_limiter`, `auto_gain` and `hifi_output`
   default off; Hi-Fi is forced off on consoles. Live QML reads show JC11S
   `hasLimiter=true`, `hasBluetoothOutput=true` and `hasSoftwareLineIn=false`
   in both stock and EngineX. `hasStemsLevel` changes from false to true;
   disabling that override, or both Stems features, leaves the tone level
   unchanged. The reporter's saved profile, hardware-control state, EQ/FX
   state and actual master/booth voltage have not been supplied.

**Which candidate causes #347? None is established by this evidence.**
The tests exclude a fixed default PCM attenuation in the measured paths.
They do not establish that the owner's physical output level is unchanged.

## Fixtures and reproducibility

- Base: `d97358d6600fa908c74139ebb836882f5346039e`; branch
  `agent/5306-issue347-master-gain`. Only investigation documentation and the
  open regression-checklist entry change; no production gain is added.
- Official PRIME4PLUS 5.3.0 image SHA-256:
  `bf5489d19d7ebd932b7408ab3e51e080648f557c52b78fe2c515929dc611ce7e`.
- Compressed rootfs record: 446803528 bytes, SHA-256
  `4351c2d3a351efeae4adf4e87fc2c8f9349eba61759007224a3253be189db0b4`.
  Container record hashes were verified before private extraction.
- The brief's `az01_packer.py unpack` command refuses signed AZ0x. The
  repository's `az0x_packer.py` record parser, XZ decompression and
  `debugfs rdump` provide the read-only extraction instead. No stock Engine
  disassembly or decompilation was performed.
- Stock PRIME 4+ and SC LIVE 4 Engine files are byte-identical, SHA-256
  `68cb478d120d2793d5883876abe6eba49a3ca78b80abf041466113f93e1f0d58`.
- The published `v5.3.0.5-r1` PRIME4PLUS bundle passed its SHA256SUMS entry.
  Published, committed and freshly rebuilt armhf hooks are byte-identical,
  SHA-256 `5077d7caf6c67836f8297026126afd9d4212ddafc56133c60c6b3953d8c89914`.
  Relevant audio, bundle and USB-helper source is unchanged from that tag
  to the integration base.
- The native render needs `SBX_CARD="PRIME 4 Audio"`, 10/10 channels and the
  existing `eglfs_kms`/llvmpipe recipe. Board codes as card names give no PCM;
  offscreen rendering is unsuitable for loading. Earlier unsuccessful
  fixture attempts are not counted as gain measurements.
- Source-only fixtures, per-block measurements, parameter snapshots and
  test summaries accompany the requested report in
  `issue347-master-gain-evidence.tar.gz`. No firmware binaries, credentials
  or raw logs are included.

## Required checks

All builds and tests ran on the designated ARM test machine. Native render,
real-time and bootstrap runs used `enginex-run exclusive`; helper/config
probes used `enginex-run sandbox`. No heavy lock was taken directly.

| Check | Result |
| --- | --- |
| `test_dsp_smooth.c`, plain | PASS |
| `test_dsp_smooth.c`, ASan/UBSan | PASS |
| `test_dsp_smooth.c`, TSan | PASS |
| `test_rt_budget.c`, native and armhf | PASS; zero budget misses in both |
| `test_rt_callback.c`, native and armhf, including guard self-tests | PASS |
| `test_sclive4_bootstrap.sh` with explicit `JC11S_ROOTFS` | PASS for SCLIVE4 and PRIME4PLUS; 18 PASS markers, no skipped JC11S warning |

There is no honest fail-before/pass-after gain regression: the released code
already passes the measured unity comparison. A gain change without a
reproduced defect would alter the tested stock-matching level. #58 and #54
remain unchanged and their existing tests pass. This report does not claim
that #347 is fixed.

## Facts needed to continue

These questions are recorded for the requested follow-up. No PM session
was contacted and no deck was accessed.

1. Which JC11S native settings or hardware mixer/DAC controls can attenuate
   master or booth after the captured PCM boundary, and what are their unity
   values? Does the Engine restart during ALSA boot activation preserve their
   gain/calibration state? Facts requiring inspection inside the stock Engine
   need the separately authorized analysis described in the brief.
2. What are the reporter's saved native Master Limiter, Output EQ, talkover,
   channel trim/EQ, faders, Stems/FX and EngineX audio settings? The issue has
   no owner level measurements or state export; its only existing reply is a
   bot comment and was not used as evidence.
3. Does a fixed-position master/booth voltage comparison show the loss on
   the same 1 kHz file, and does the digital meter or captured PCM fall by the
   same amount? This locates the difference relative to the measured boundary
   without assuming a dB value from knob positions.

## Three-step PRIME 4+ owner check

1. Record the existing profile and EngineX audio settings. Use one fixed
   1 kHz WAV, one deck and identical trim/EQ/faders/crossfader settings. Keep
   master at one marked position with the same cable/load. Record native
   Master Limiter and whether Soft Clipper or Auto-Gain is on.
2. Measure stock and EngineX RMS on the same master output with a suitable
   line-level interface or true-RMS meter. Record both voltages and compute
   `20*log10(V_EngineX/V_stock)`. Record booth separately at a fixed position.
3. Repeat with the optional EngineX audio features off, then enable only
   Soft Clipper and repeat the quiet tone. Supply the voltages, saved states
   and digital-meter readings. Check hot-mix clipping protection separately
   without raising the monitoring volume.

Skipped: production gain patch and fail-before regression, because no measured
cause is demonstrated; physical-deck and stock-binary analysis, as instructed.
Risk: #347 remains unresolved; the null-card sandbox cannot certify physical
DAC controls, the hardware mixer, analog master stage or the saved user state.
