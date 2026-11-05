# #63 Pro DJ Link Export wire evidence — 2026-11-05

Implementation is experimental, OFF by default. This is protocol evidence, **not a CDJ-2000NXS2 or CDJ-3000 hardware acceptance claim**.

## Verified packet facts

| Traffic | Verified layout | Implementation |
| --- | --- | --- |
| UDP 50000 keepalive | 54 bytes, ASCII `Qspt1WmJOL`, type 06 at 0x0a, 20-byte model at 0x0c, number at 0x24, MAC at 0x26, IPv4 at 0x2c | Uses these offsets; probes 2.5s before announcing. Captured cadence is about 2s. |
| UDP 50002 media response | 0xc0 (192) bytes; model starts at **0x0b**; payload length at 0x22 is 0x009c; host/slot at 0x27/0x2b; UTF-16BE name at 0x2c; track/playlist counts at 0xa6/0xae; capacities at 0xb0/0xb8 | Exports USB slot 3 with Rekordbox type 1. Settings-present remains false. |
| UDP 50002 status | Extended legacy captures are **0x11c (284)** bytes, payload length 0x00f8. `0x110` is the starting offset of the last row, not the total length. Older captures also contain 0xd4. | Sends idle, USB-loaded, no-master/no-sync status every 200ms to discovered peers. Does not emulate native CDJ-3000 extensions. |
| TCP 12523 port query | `0000000f` + ASCII `RemoteDBServer\0`; reply is one BE uint16 port (capture: 0x041b / 1051) | Query and actual dbserver share TCP 12523, so reply is 0x30eb / 12523. |
| dbserver greeting/setup | Greeting `1100000001`; message magic tagged `11 872349ae`; setup transaction fffffffe, kind 0000; reply kind 4000, numeric args [0, server number] | Accepts requester 1–6. Fields and TCP fragmentation/coalescing tested. |
| dbserver menu | Availability 4000 [request type, count]; header **4001 [1,0]**; item 4101 with 12 tagged args; footer 4201 with zero args | Independent menus per DMST; root playlist ID 5 / kind 0x84 and UTF-16 label markers FFFA/FFFB match capture. |
| Get Track Information 2102 | Render returns **one** kind-0 item: file size, track ID, audio path; secondary string empty | ANLZ path is read from `export.pdb` over NFS. No invented second path item. |
| UDP 111 portmapper | ONC RPC GETPORT with UDP protocol 17 maps MOUNT v1 and NFS v2 to a bound UDP service port | dbserver is **not an ONC RPC service** and is not falsely registered here. Read-only `/C/`, UTF-16LE names. |

Sources: [Deep Symmetry media](https://djl-analysis.deepsymmetry.org/djl-analysis/media.html), [status](https://djl-analysis.deepsymmetry.org/djl-analysis/vcdj.html), [dbserver](https://djl-analysis.deepsymmetry.org/djl-analysis/track_metadata.html), [menus](https://djl-analysis.deepsymmetry.org/djl-analysis/menus.html), [Beat Link MediaDetails](https://github.com/Deep-Symmetry/beat-link/blob/main/src/main/java/org/deepsymmetry/beatlink/MediaDetails.java), [CdjStatus](https://github.com/Deep-Symmetry/beat-link/blob/main/src/main/java/org/deepsymmetry/beatlink/CdjStatus.java).

## Capture provenance and scope

Packet excerpts in `pkg/prolink/testdata/captured-wire.json` are pinned to Deep-Symmetry/dysentery commit `7f61b1c1276c27c80643632c24ee57d13b0db572`. The JSON records raw bytes, source URLs and complete PCAP SHA256 values:

- S05-link-browse/run.pcapng: `4ae48d19172c10c07180a80e6d488ae8c0d7f437149d8428cf17ad060adc8852`
- S06-load-and-play/run.pcapng: `c69d0ea093991dce8181eb286031faed8940aa806ed9dc5737e51919e0bb8bf2`
- LinkInfo.pcapng: `e87e12b109cb2b5993d4755b122d2f7af1a4c95821acfb2e233d67ee37697559`

S05/S06 identify **CDJ-2000nexus firmware 1.44**, not NXS2 or 3000. Oracle analysis reassembled TCP streams using sequence numbers and checked IPv4 header checksums. Tests compare exact menu/path bytes and keepalive bytes (changing only model name), replay all selected dbserver fields, and check framing/offsets of variable media/status payloads. Whole status/media packets are not asserted identical: actual track, volume, firmware, settings and capacity fields differ.

Target-specific primary evidence exists but does not close the raw-capture gap:

- [dysentery PR 60](https://github.com/Deep-Symmetry/dysentery/pull/60) reports tests on CDJ-2000NXS2; [issue 2](https://github.com/Deep-Symmetry/dysentery/issues/2) discusses its larger status packets.
- [PR 76](https://github.com/Deep-Symmetry/dysentery/pull/76) reports real CDJ-3000 firmware 3.20 tests, including dbserver requester numbers 1–6. This supports accepting six requester numbers, not a claim that our export has played on a 3000.
- [PR 73](https://github.com/Deep-Symmetry/dysentery/pull/73) describes later CDJ-3000 captures; [issue 78](https://github.com/Deep-Symmetry/dysentery/issues/78) requests their publication. No public target-specific raw PCAP was located in this pass. Do not relabel the nexus fixtures.

## Official firmware attempt and remaining acceptance

The [official NXS2 firmware page](https://www.pioneerdj.com/en-us/support/software/player/cdj-2000nxs2/) and [CDJ-3000 firmware page](https://www.pioneerdj.com/en-us/support/software/player/cdj-3000/), regional/support alternatives and attempted official ZIP links returned HTTP 403 from Oracle. The old official CDN hostname did not resolve. Official [NXS2 update instructions](https://www.pioneerdj.com/-/media/pioneerdj/downloads/firmwares/players/cdj-2000nxs2/cdj-2000nxs2_updateguide-en.pdf) and [CDJ-3000 instructions](https://www.pioneerdj.com/-/media/pioneerdj/downloads/firmwares/players/cdj-3000/cdj-3000_updateguide-en.pdf) describe UPD installation, not dbserver wire specifications. No official firmware was downloaded or disassembled; no firmware-derived packet claim is made.

Before calling this compatible with either target, capture each real model browsing folders/playlists, loading an audio file and reading its DAT/EXT/2EX through NFS. Include simultaneous startup and number collision, USB removal, and ON/OFF tests. Full Pioneer claim-stage negotiation, native 3000 status extensions, waveform/artwork binary queries and complete sorting are not implemented. The passive highest-unused selection and immediate keepalive-based yield meet the requested experimental numbering subset; they do not prove all simultaneous-boot races are avoided.

## Virtual CDJ simulator

`pkg/prolink/sim_cdj3000_test.go` independently constructs 54-byte keepalives, 96-byte beats and the documented **512-byte (0x200)** CDJ-3000 status envelope. Only documented fields required by this implementation are populated; unknown firmware fields are not reverse engineered. A separate parser tolerance test includes the requested 288-byte (0x120) length as explicitly synthetic input.

The server-to-Denon test transmits over actual UDP discovery/status/beat ports on isolated 127.63.0.x loopback addresses, serves the fixture USB using NFS, verifies all DAT/EXT/2EX files were fetched, checks three-band waveform columns, BPM/pitch/status and beatgrid-derived position, then runs Stage and rb-bridge and queries the resulting Engine database for the CDJ folder, playlists and five playable staged tracks. The reverse test uses an independent dbserver client (including requester 6), queries USB media, browses nested playlists/tracks, fetches audio and all analysis formats over UTF-16LE NFS, and asserts Player 4→3 yield within 50ms. Service port overrides isolate the optional export test from live LAN traffic.

These tests verify application data supplied to the display snapshot and the actual Engine database. They do not exercise native QML rendering, firmware execution, real CDJ filesystem behavior, waveform hardware display or physical audio playback. The simulator exposed and fixed preexisting status flag/pitch offsets (0x89/0x8c), misuse of 0xb4 as a position, and beat notifications clearing status flags.

The final bridge regression also covers a receiving USB with no local `PIONEER` directory/export, an empty `PIONEER` directory, and an existing native Engine database. Staged-only imports retain native Engine tracks. A drive with neither a local export nor staged exports still bypasses without writes. `enginedb.Sync` accepts the validated staged library; the bypass decision belongs to `bridge.Convert`.

Exporter status recipients are limited to six IPs on the selected interface subnet. Per-client menus retain at most four locations; the title menu is sorted once and shared. Incoming status is enriched through the existing MediaCache when sharing is enabled, and a coexistence test keeps the passive listener running while the exporter receives unicast, ensuring the waveform reaches its observer. Invalid player numbers are rejected before forwarding.

For a staged-only import, the bridge updates only the staged source ID namespace (`>= 1<<24`), retaining earlier local bridge-owned tracks/playlists as well as native Engine rows. Only the private “no local export candidate” sentinel enables this mode; an I/O/parse failure opening an existing candidate does not. Regression tests rename an earlier local export, prune a missing staged audio file, and verify the local IDs/memberships remain intact, including Smart Match updates.

Incoming waveform enrichment now runs on a bounded queue in a separate worker, created only while export is ON. Its context cancels in-flight NFS reads on OFF and is checked between PDB page reads. A blackhole-NFS test proves media queries still reply within 200ms and OFF completes within 200ms while a fetch is blocked. This tests software/network cancellation; it is not a bound on uninterruptible filesystem or device I/O.

Known namespace limit: the staged ID scheme reserves IDs from `1<<24` upward. A previously imported local source with IDs in that reserved range cannot be reliably distinguished in staged-only mode. Keep/restore its original local export and avoid staged-only conversion for such a library; this boundary is not covered by the passing low-ID fixtures. This is separate from native Engine rows, which are preserved.
