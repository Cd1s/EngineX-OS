# Engine UI mod

`engine_ui_hook.c` is preloaded into `/usr/Engine/Engine` and swaps files in
Engine's embedded Qt resources for those in
`/data/sc5000-custom/ui/packs/<language>/`. Rootfs and the Engine binary stay
untouched. `build_packs.py` builds the packs from the QML extracted from Engine
5.0.4: language, waveform palette, separate NetEase/Spotify/Deezer sources and
removal of the improper-eject prompt.

    python3 build_packs.py --qrc <extracted qrc> --out out/ui-packs
    # unpack out/ui-packs.tar.gz into /data/sc5000-custom/ui on the deck

`--dev` adds an eval/screenshot harness; it also needs
`QML_XHR_ALLOW_FILE_READ=1` and must never ship.

`icons/` holds the service marks, rendered from Simple Icons (CC0).

Units in the rootfs `/etc`: systemd reads them before the `/etc` overlay mounts,
so a drop-in only in the overlay's upper layer is ignored at boot. On a live
deck, change that layer offline or in an image; bind-mounting `/` and editing it
while running crashed PID 1 once.

## Limits

- Translation covers text literal in QML (`i18n.tsv`). Text from Engine's C++
  stays English. Track, artist and playlist names are never translated because
  they are not literals.
- The three services share Engine's one TIDAL client, so one is active at a time.
  Tracks already loaded on a deck keep working after a switch.
- Audio for streaming tracks needs a Deemix REST bridge at 127.0.0.1:6595. None
  is installed on the deck, so browsing works and loading such a track fails.
- Spotify needs the owner's own registered Client ID. Deezer links a public
  profile, not an account login.
- Encoder navigation of the Source screen reaches only stock rows. The three
  service rows are touch-only.

## Recovery

- Three Engine starts that do not survive 90 s create
  `/data/sc5000-custom/ui/safe-mode` and the stock UI returns.
- To remove the mod: delete
  `/etc/systemd/system/engine.service.d/60-ui-hook.conf`, run
  `systemctl daemon-reload` and restart `engine.service`.
