# Apple Music and Spotify on the 5.3.0.5 integration branch

Integration base: `147decfe` (`origin/agent/5305-integration`), including the
remote-access toggle and About console QR added after the initial `7dff8af7` base.
Source commits: `4a872984`, `a09c4be7`, `c1a194b5`.

The port retains the EngineX runtime paths and canonical `/v1/enginex/` routes
under the current `/v1/enginex/` namespace. All console and phone strings have
English, Simplified Chinese, Traditional Chinese and Spanish entries. The new
Apple Music helper card lives in the redesigned Music section alongside the
existing accounts, playlists, downloads and Wi-Fi upload cards.

## Spotify audio source

`GET /api/ui` exposes `spotifySource`. `POST /api/ui?spotifySource=<source>`
validates and saves the setting as `spotify_source=<source>` in
`/data/enginex/ui/settings.conf`. Both loopback API prefixes use the same
handler. Missing or invalid saved values default to `auto`; invalid API values
return 400 without changing the file. Jobs read the owner's saved selection.

- `auto`: try Deezer, NetEase, Beatport, Apple Music, then SoundCloud.
- `deezer`, `netease`, `beatport`, `applemusic`, `soundcloud`: query only that
  service; missing login, no match or unusable audio returns an English/Chinese
  error naming the selected service.
- Existing Spotify cache files are reused, including downloads made before
  direct Spotify playback was paused. A failed lossless upgrade retains the
  existing lossy file. Cache identity remains `spotify-<id>.*` when the setting
  changes.
- Apple Music searches the catalog, checks the recording with `MatchNearby`,
  downloads via `DownloadTo` under the Spotify cache name, then inspects the FLAC.
- SoundCloud searches and checks the recording, downloads via `fromSoundCloud`,
  then links it under the Spotify cache name. An unlinked public-search provider
  does not count as a logged-in SoundCloud account.

Direct Spotify audio remains paused, while login, Connect, search and browsing
stay available. The #269/#277 gateway regressions still verify manifest timing,
Range responses, preservation of real TIDAL credentials and Engine's recognized
`.mp3` download name; their old OGG fixtures now exercise cached playback with
zero Spotify audio requests.

## Validation

Validation runs on the native ARM build host after sourcing
`/etc/profile.d/buildtools.sh`.

| Check | Result |
| --- | --- |
| `gofmt -l .` | PASS; empty output |
| `go vet ./...` | PASS |
| `go test -race -timeout=30m ./...` | PASS; final integration base, no race reports |
| `bash tests/test_rootfs_overlay.sh` | PASS; 36 checks |
| `python3 -m unittest discover -s tools/az01-packer` | PASS; 454 tests, exit 0; one environment skip covered separately |
| `python3 -m unittest discover -s tools/stems` | PASS; 15 tests |
| `AZ04=1 bash tools/az01-packer/test_sclive4_bootstrap.sh` | PASS; all PRIME4G2 checks, including SYSTEMONE image application |
| `node --check` | PASS; four tracked scripts and the console inline script |
| Chromium console exercise | PASS; four languages, six options, source-change save event, no runtime exceptions; 1440px desktop and 390px mobile |

The first full Go race run reached the default 10-minute package timeout in
`pkg/bridge` while SQLite was building a snapshot, and a concurrent CLI timing
assertion measured 368 ms against its 200 ms limit. The unchanged CLI test
passed independently. The final full race run allows a 30-minute total per
package; no assertion, fixture size or race coverage is reduced.

The full packer suite skips `test_install_uses_bundle_alsa_dir` when the host has
a `/data` directory. It was separately executed and passed in a PID/mount-isolated
chroot without a `/data` directory, using the unchanged test.

The Apple Music phone pages were also rendered from the actual server in all
four languages, with a local helper fixture, and their three screenshots updated.

The browser used fixture API responses and the actual console HTML, CSS,
JavaScript and service icons. The three `24b-spotify-source-console.png` images
show that console with fixture helper status. They are not evidence of a live
subscription. Real-account entitlement, region availability and physical deck
acceptance remain external checks.

An earlier concurrent verification process ended with SIGTERM and no test
summary; that attempt is excluded. The packer rerun uses an isolated PID/mount
namespace and records the child exit status.
