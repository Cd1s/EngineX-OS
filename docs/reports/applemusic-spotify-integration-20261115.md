# Apple Music deck integration and Spotify matched playback

Historical source-branch report at `c1a194b5`, based on `a09c4be7` (PR #294).
The validation below describes that source revision. For the 5.3.0.5 port and
Spotify source setting, see `5305-applemusic-spotify-integration.md`.

## Result

Spotify login, Device Auth, PKCE, Connect pairing/logout, public links, playlists,
liked songs, search and 429 handling remain available. Deck acquisition no longer
calls Spotify audio download. It matches only configured/logged-in Deezer,
NetEase, Beatport and Apple Music, in that order, and keeps `spotify-<id>.*`
cache names. SoundCloud shares the same matcher. Existing lossy Spotify files
remain playable when a lossless upgrade fails. FLAC, MP3 and OGG fixtures cover
matching, analysis, waveform/beatgrid generation and Engine `m.db` import.

Apple Music helper configuration persists separately at
`<state-dir>/applemusic-helper.json` with mode `0600`. Environment settings remain
fallback defaults; an explicit token-clear marker prevents a cleared token from
returning after restart. Browsing credentials and helper audio login remain
separate. The console and loopback APIs expose helper status, login, 2FA and QR.
The flow-token-protected LAN phone page exposes login/2FA/QR without allowing
helper URL/token changes.

Status JSON deliberately returns a protected local `loginPageUrl` redirect,
rather than embedding the helper token in JSON. The redirect and QR use the
actual helper login URL. Helper responses/errors cannot echo credentials, and
helper download credentials are never forwarded to a different origin or across
redirects. Helper URLs accept HTTP/HTTPS without userinfo, query or fragment.

Library songs retain their original ID plus a catalog ID hint, falling back to
Apple's authenticated library-song catalog endpoint when necessary. Downloads
use a five-minute default client timeout, a temporary file on the cache volume,
flush/close, compatible permissions and atomic rename. Quality comes from the
actual FLAC headers. The cache index, cold lookup and eviction recognize Apple
Music and the additional Spotify formats.

## Touch UI inspection

`build_packs.py` already provides the `applemusic` source ID, icon, service
switch, shared browse gateway and QR modal. Its original stock-row filters did
not include `Apple Music`, so Gen-2's stock row could duplicate the EngineX row
in connected/available sources and Streaming settings. The stock display name
is now filtered consistently in all three places and Browse encoder navigation;
a stale stock-row activation opens EngineX `applemusic`. Existing visible stock
Beatport/other services keep their original navigation behavior. Behavioral Node
tests and generated trilingual packs cover both paths. Physical Gen-2 acceptance
remains necessary to confirm the runtime model and hardware encoder behavior.

The three new phone-page screenshots were rendered from the actual page code
with a local helper-status fixture; they are not captures from a physical deck.
The three READMEs and website translations describe helper-based ALAC-to-FLAC
playback, desktop/non-Docker and Gen-2 helper options, and Spotify matching.

## Verification

| Command | Result |
| --- | --- |
| `. /etc/profile.d/buildtools.sh; gofmt -l .` | PASS: 0 unformatted files |
| `go vet ./...` | PASS: exit 0, no diagnostics |
| `go test -race ./...` (JSON captured with `-json`) | PASS: 1992 passed results including subtests, 6 skipped, 0 failed; 1278 top-level tests passed; 32 packages passed, 6 packages have no tests |
| `bash tests/test_rootfs_overlay.sh` | PASS: 27/27 |
| `python3 -m unittest discover -s tools/az01-packer` | PASS: 340 run, 339 passed, 1 skipped, 0 failed; exit 0 |
| `python3 -m unittest discover -s tools/stems` | PASS: 14/14 |
| `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o out/arm64/enginex-suite ./cmd/enginex-suite` | PASS: ELF64 AArch64, 33560984 bytes |
| `python3 tools/ui-mod/build_packs.py --qrc <firmware-lab>/5.3.0/qrc --out out/packs_test` | PASS: en 414 files, zh-Hans 419 files, zh-Hant 419 files |
| `node --check` for extracted console JS and `docs/site/content.js` | PASS: 2/2 |

The final full Go run and the final streaming-focused race run both exited 0.
The full Go tests used a workspace-local temporary directory; the Python packer
suite used its normal temporary directory because its output-containment tests
intentionally reject binary output anywhere inside a Git worktree. An initial
packer run reported OK but its shell exited 143; a complete rerun with an
explicitly recorded child exit code confirmed 339 passes, one skip and exit 0.

Go skips: `TestHelperDeemixd`, `TestModeBJC11SPublishedBundle`, `TestGateExport`, `TestGateLibrary`, `TestKilledHealChild`, `TestLinkMasterTakenBackWhenNobodyHoldsIt`.
The published PRIME4PLUS bundle/release-gate fixtures were not supplied; two
helper/child tests only run under their parent tests. The Pro DJ Link takeover
integration test skipped because its UDP request received no answer (the test
reports a possibly occupied beat port). No test was removed to suppress a failure.

Build SHA-256: `d7991bf02d3d5088d14379f434e2d906571a89ce464ebb2a6f86786a3eb8320e`.
Local validation logs are under ignored `out/verification/`.


## QA follow-up

- Use a real `enginex-amd` account to exercise signed-out, logging-in, 2FA,
  ready, expired/invalid helper credentials and helper-offline states. Refresh
  the phone page or console status while an asynchronous login is in progress.
- Test a long ALAC-to-FLAC conversion on a real FAT/exFAT cache volume with a
  constrained system temporary filesystem; interrupt the transfer and confirm
  the finished filename is not published. Confirm cached playback after reboot.
- Test both library songs with catalog twins and personal uploads without a
  catalog recording; uploads without a twin should fail catalog resolution
  cleanly. Browsing/library authorization is still required separately.
- On Gen-1 and Gen-2, verify the source, QR modal, Streaming service toggle,
  browse categories, encoder navigation and native LOAD behavior. Gen-2 should
  show one EngineX Apple Music row and no duplicate stock row.
- With real subscriptions, verify each Spotify matching service independently,
  no-service bilingual error, lossy-cache fallback and matched-file quality.
  Catalog availability, regional differences and subscription entitlements may
  prevent a match; direct Spotify audio intentionally remains disabled.

## Changed files

- `README.md`
- `README.zh-CN.md`
- `README.zh-TW.md`
- `docs/REGRESSION-CHECKLIST.md`
- `docs/assets/screenshots/en/24-applemusic-helper-login.png`
- `docs/assets/screenshots/zh-CN/24-applemusic-helper-login.png`
- `docs/assets/screenshots/zh-TW/24-applemusic-helper-login.png`
- `docs/index.html`
- `docs/reports/applemusic-spotify-integration-20261115.md`
- `docs/site/content.js`
- `pkg/deemixclient/client.go`
- `pkg/deemixclient/fetch.go`
- `pkg/deemixclient/fetch_test.go`
- `pkg/providers/applemusic.go`
- `pkg/providers/applemusic_helper.go`
- `pkg/providers/applemusic_helper_test.go`
- `pkg/providers/provider.go`
- `pkg/webui/applemusic_helper.go`
- `pkg/webui/applemusic_helper_test.go`
- `pkg/webui/index.html`
- `pkg/webui/jobs.go`
- `pkg/webui/jobs_acquire_test.go`
- `pkg/webui/jobs_cold_test.go`
- `pkg/webui/phone_pages.go`
- `pkg/webui/server.go`
- `pkg/webui/spotify_native_test.go`
- `pkg/webui/testdata/README.md`
- `pkg/webui/testdata/spotify-matched-44100.mp3`
- `pkg/webui/tidal_gateway.go`
- `pkg/webui/ui_login.go`
- `pkg/webui/ui_login_test.go`
- `tools/az01-packer/test_ui_mod.py`
- `tools/ui-mod/build_packs.py`
