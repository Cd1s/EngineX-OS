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
- TIDAL, NetEase, Spotify and Deezer share Engine's one TIDAL client, so one is
  active at a time. TIDAL itself is passed straight to TIDAL's servers. Switching
  to it after using another service may ask for a new TIDAL login.
  Tracks already loaded on a deck keep working after a switch.
- Audio for streaming tracks comes from a Deemix REST bridge on the deck at
  127.0.0.1:6595, then the deck's NetEase login. See `docs/stream-sync.md`.
- Featured, Genres, Moods and TIDAL Rising are hidden, and My Collection too except
  on NetEase, where it opens saved albums. TIDAL itself shows them all. Encoder
  navigation can still land on those rows; the gateway answers them with empty
  listings.
- Encoder navigation of the Source screen reaches only stock rows. The three
  service rows are touch-only.
- Engine adds its Stems list row only on players with pre-rendered Stems playback
  (`Planck.hasStemsPreRenderedPlayback()` is false here), so its C++ craylist model
  never has it. The pack puts a "Stems" entry on the playlist sidebar's top bar
  instead: drop tracks or a playlist on it to render stems (the stock Stems list
  calls), tap it to sort the collection by Stems. It is not a real list.
- The deck overview is stretched only for tracks whose path has a `Contents/`
  folder (a rekordbox USB export). Streams, Stream downloads and Engine Library
  music keep stock height.
- Jog light modes only set `/Engine/Deck<N>/Color` in memory. Measured
  2026-11-03: 30 s of Psy Fast or Psy BPM Sync wrote no more to eMMC than Static
  and changed no settings, profile or database file. BPM sync follows the playing
  deck's tempo (one beat per step, four beats per colour-wheel turn), not its beat
  phase. This changes the earlier half-beat timing; the new pack needs deployment
  before a deck running the previous version changes behaviour.
- Selecting a drive that carries an Engine profile no longer asks "Load drive
  profile to device?"; the prompt's "No" is taken and the deck keeps its settings.

## Recovery

- Three Engine starts that do not survive 90 s create
  `/data/sc5000-custom/ui/safe-mode` and the stock UI returns.
- To remove the mod: delete
  `/etc/systemd/system/engine.service.d/60-ui-hook.conf`, run
  `systemctl daemon-reload` and restart `engine.service`.

## Remote Stems runtime adapter

The hook includes the four matched-byte edits from
`Engine.remote-stems-candidate.patch.json` for Engine SHA-256
`342b611bd71fd09d9fda8013f19dccfba47ee4f0f772a959058bdd19d74d5413`.
All four must match before any Stems edit is enabled. Memory protection covers
all touched pages, including spans larger than a page, and returns to RX before
Engine starts. A protection failure aborts startup rather than running a partial
Stems trampoline set. The Engine file remains unchanged.

On the current deck the hook is installed at
`/data/sc5000-custom/lib/libengine_ui_hook.so`, with the existing
`/etc/sc5000/libengine-ui-hook.so` preload path linked to it. The static ARMv7
client lives at `/data/sc5000-custom/bin/stems-processor` (0755).
`stems-runtime-prepare` is installed at `/etc/sc5000/stems-runtime-prepare` and
called by the existing `engine-ui-guard pre` entry. It overlays the read-only
`/usr/local/bin` directory, preserving vendor commands, and adds the client
symlink. Upper/work directories live under
`/data/sc5000-custom/stems-bin-overlay`.

The client additionally requires the separately provisioned private
`/etc/sc5000-stems/token` and the LAN backend. Installing the client and seeing
Engine load the hook does not establish that a track can be separated. Full
acceptance requires a real short track, successful `.stems` output, and playback
of each stem. Do not use Engine's "Remove Stems Support" button: the stock
removal path targets the processor path returned by this adapter.

Before deployment, back up the existing hook, `engine-ui-guard`, and Engine
service drop-ins to agent-backup. To roll back, stop Engine, restore those exact
files, unmount `/usr/local/bin` after stopping processes using the overlay,
remove the newly installed helper, and restart Engine. Keep user audio,
databases, and credentials intact.
