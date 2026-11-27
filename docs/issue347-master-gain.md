# PRIME 4+ master gain investigation (#347)

Status: investigation in progress; no production gain change is justified yet.

The reported knob positions are a reproducible owner observation, not a dB
measurement. A knob's taper and the analog stage must be measured before
converting positions to a gain difference.

## Stock and release fixtures

- Stock: PRIME4PLUS 5.3.0, board JC11S, official image SHA-256
  `bf5489d19d7ebd932b7408ab3e51e080648f557c52b78fe2c515929dc611ce7e`.
- Rootfs compressed record: 446803528 bytes, SHA-256
  `4351c2d3a351efeae4adf4e87fc2c8f9349eba61759007224a3253be189db0b4`.
  All container record hashes verified before extracting the XZ-compressed ext
  filesystem into a private scratch directory.
- The requested `az01_packer.py unpack` refuses this signed AZ0x image. The
  existing `az0x_packer.py` record parser, XZ decompression and `debugfs rdump`
  supply the read-only extraction instead. No firmware binary was modified or
  disassembled.
- Stock PRIME 4+ and SC LIVE 4 Engine files are byte-identical: SHA-256
  `68cb478d120d2793d5883876abe6eba49a3ca78b80abf041466113f93e1f0d58`.
- The published `v5.3.0.5-r1` PRIME4PLUS data bundle passed its published
  SHA256SUMS entry. Its hook equals the integration baseline's committed hook:
  SHA-256 `5077d7caf6c67836f8297026126afd9d4212ddafc56133c60c6b3953d8c89914`.
- `engine_ui_hook.c`, `dsp_smooth.h`, `master_limiter.h`, the data-bundle builder
  and `enginex-usb-audio` are unchanged between that release tag and `d97358d6`.

## First measurements

The real `hook_master_output` wrapper ran around a reference callback supplying
a stereo 1 kHz sine to all eight output channels, as native and armhf programs.
Each case ran for 12 seconds at 48 and 96 kHz; the final two seconds supplied
RMS and peak measurements. This isolates EngineX processing after the vendor
callback. It does **not** measure stock Engine decoding, the board's mixer, a
DAC or analog output.

| Input peak | EngineX controls | RMS difference | Peak difference |
| --- | --- | --- | --- |
| -12.041200 dBFS | defaults | 0.000000 dB | 0.000000 dB |
| -12.041200 dBFS | soft clipper enabled | 0.000000 dB | 0.000000 dB |
| -1.938200 dBFS | soft clipper enabled | 0.000000 dB | 0.000000 dB |
| +6.020600 dBFS | soft clipper enabled, 48 kHz armhf | -5.026151 dB | -6.125461 dB |
| -12.041200 dBFS | auto-gain enabled, 48 kHz armhf | +7.582910 dB | +7.591841 dB |

Every below-knee sample was bit-identical on every output. The overdriven
case reaches -0.104861 dBFS peak, demonstrating the intended #58 protection;
it does not establish that the reported music was overdriven. Auto-gain
boosts rather than attenuates. The pre-existing plain DSP suite passes.

## Candidates still under examination

1. ALSA: the published bundle adds a boot hook without PCM, softvol, DAC or
   mixer gain definitions. The USB routing helper does not set mixer levels.
   Stock speaker initialization includes `amixer ... Master 0dB`, but its udev
   rules target the speaker-equipped boards rather than JC11S. An effective
   ALSA configuration comparison remains to be run.
2. EngineX DSP: the measured master wrapper is unity below the knee. The
   Flanger correction clamps its gain to at least unity. Native mixer behavior
   has not yet been excluded by these measurements.
3. Settings: `master_limiter`, `auto_gain` and `hifi_output` default off, and
   Hi-Fi is forced off on consoles. EngineX also changes the native
   `Config.audio.hasLimiter` flag; its runtime effect needs separate checking.

## Remaining work

Establish a measured cause before changing production audio code. Add a
regression that fails before that change, retain #54/#58, and run the required
DSP sanitizers, native/armhf real-time tests and both bootstrap fixtures.

## Three-step owner check

1. At a safe monitor level, use one fixed 1 kHz test file and record channel
   trim, EQ, faders, crossfader, output settings and the exact master position.
2. With the same load and cable, measure RMS at the same master output on
   stock and EngineX. Record native Master Limiter and EngineX Soft Clipper,
   Auto-Gain and Hi-Fi states; do not infer dB from knob position alone.
3. Repeat with the confirmed fix, checking equal quiet-tone level and continued
   clipping protection on a deliberately hot digital mix, without raising
   speaker volume. Record master and booth separately.
