# Remote Load LAN API

Loads a track from a drive mounted under `/media` (USB stick, SD card, and on SC6000 / PRIME 4 a drive in the SATA bay) onto an idle deck over the local network. Not yet released.

## Enabling

The feature is off by default. Switch it on at the deck: `Settings -> Services -> Remote Load (LAN API)` (`Off` / `On`). The switch is stored as `remote_load=1` in `settings.conf` and cannot be changed from the web console (`POST /api/ui` with `remoteLoad` returns `403`).

While the switch is off, `POST` returns `403 remote load is switched off`. A `pending` request becomes `rejected` (`switched off`); a request already `loading` ends as `failed` (`no confirmation`) 8 s after the deck last reported it. The deck polls its own state every 500 ms only while the switch is on.

## Authentication and network

All routes are on the web console port (`:8080`) and need the 8-character access code shown on the deck under `Settings -> About / Update`:

```
Authorization: Bearer XXXX-XXXX
```

Requests that a tunnel edge relayed (headers `Cf-Connecting-Ip`, `Cf-Ray`, `Cdn-Loop` or `Tailscale-User-Login`, the Cloudflare tunnel and Tailscale Serve) are refused with `403 remote load is not available through the tunnel`. Use the LAN address. The switch cannot be changed through a tunnel either: the Quick Tunnel and the Tailscale console forward reach a console listener of their own (`127.0.0.1:8081`) that has no `/v1/*` routes, and `/v1/*` on `:8080` refuses relayed requests.

Tailscale: with the Tailscale VPN on, tailnet peers reach the web console through that forward and are treated like LAN clients: `/api/remote-load` works with the access code, and the switch and the deck's poll route are out of their reach.

## Routes

| Method and path | Purpose |
| --- | --- |
| `GET /api/remote-load` | Switch state, board, deck states and the last 20 requests (newest first) |
| `POST /api/remote-load` | Submit a load request |
| `GET /api/remote-load/{id}` | One request by id |

### GET /api/remote-load

```sh
curl -H "Authorization: Bearer XXXX-XXXX" http://<deck-ip>:8080/api/remote-load
```

```json
{
  "enabled": true,
  "board": "SCLIVE4",
  "stateAgeMs": 310,
  "lockPlayingDeck": true,
  "decks": [
    {"deck": 1, "side": "left",  "local": true, "loaded": true,  "playing": false, "title": "Track", "artist": "Artist"},
    {"deck": 2, "side": "right", "local": true, "loaded": false, "playing": false}
  ],
  "requests": []
}
```

- `local`: the unit owns the deck. SC LIVE 4: decks 1-4; 2-deck consoles: decks 1-2; standalone players: their own decks. Remote (linked) decks are `local: false`.
- `playing`: the deck is busy: Engine plays it, or it is audible without playing: its position moved in the last 1.5 s (CUE held for preview, scratching, searching) or its jog wheel / platter is touched. Measured on a SC LIVE 4: Engine's `PlayState` stays false in all three cases.
- `side`: only set on the SC LIVE 4 (`board` `SCLIVE4`): decks 1 and 3 are `left`, decks 2 and 4 are `right`. Empty on other boards.
- `stateAgeMs`: age of the deck report. A request is only accepted while the report is at most 2000 ms old.
- `lockPlayingDeck`: Engine's own setting `Lock Playing Deck` as the deck last read it: `true`, `false`, or `null` when it is unknown (no report yet, Engine does not report it, or reports a value other than On/Off). While it is not `true` the deck refuses every load (`rejected`, `lock playing deck is off` or `lock playing deck unknown`); see [Playing decks](#playing-decks).
- `decks` is empty until the deck has reported once.

### POST /api/remote-load

```sh
curl -X POST -H "Authorization: Bearer XXXX-XXXX" -H "Content-Type: application/json" \
  -d '{"deck": 2, "path": "Music/Set/track.flac"}' \
  http://<deck-ip>:8080/api/remote-load
```

Body fields:

| Field | Meaning |
| --- | --- |
| `deck` | Deck number 1-4 (required) |
| `path` | Relative path of the audio file on the drive, `/` separated, no leading `/`, no `..` (required). Spell it exactly as on the drive, including upper and lower case: FAT and exFAT sticks also find the file in another case, but Engine may then add it to its library a second time, without the cues and beat grid of the existing entry |
| `drive` | Optional drive name (the folder name under `/media`) to pick one drive when the path exists on several |

The deck checks (`deck`, then fresh deck state, `deck locked`, `deck playing`, open request) run before the file lookup, so `{"deck": 9, "path": "missing.mp3"}` answers `400 deck must be 1-4` and a playing deck answers `409 deck playing` even when the file does not exist.

Only drives mounted under `/media` are searched: USB sticks, SD cards and, on SC6000 and PRIME 4, a drive in the SATA bay. The internal storage (`/data`) and network shares are never used. The file must be a regular audio file; symlinks that leave the drive are ignored.

Success is `202 Accepted`:

```json
{"id": "9f3a1c52b07d4e11", "status": "pending"}
```

If the path exists on more than one drive the answer is `409` with the drive names; repeat the request with `"drive"` set:

```json
{"error": "file exists on several drives", "candidates": ["USB-A", "SD-1"]}
```

### GET /api/remote-load/{id}

```sh
curl -H "Authorization: Bearer XXXX-XXXX" http://<deck-ip>:8080/api/remote-load/9f3a1c52b07d4e11
```

```json
{"id": "9f3a1c52b07d4e11", "deck": 2, "path": "Music/Set/track.flac", "drive": "USB-A", "status": "loaded"}
```

`reason` is added when there is one.

## Request status

| Status | Meaning |
| --- | --- |
| `pending` | Accepted, waiting for the deck to pick it up. The deck loads one track at a time, so a request for another deck waits while one is `loading`. Becomes `expired` after 10 s without pickup |
| `loading` | The deck sent the load command and waits for Engine to show the track (the deck allows 5 s, and while Engine shows it is still loading up to 60 s). Each poll of the deck names the request it waits for; 8 s after the last such report a request still `loading` becomes `failed` (`no confirmation`) |
| `loaded` | Engine freshly loaded the requested file on the deck: the deck's track ends with the file's path on the drive, and Engine loaded it after the request (the track changed, or Engine showed the deck loading). If the deck already held that very file and Engine shows no new load, the request ends as `failed` (`timeout`), never as `loaded` |
| `rejected` | The deck refused it: `deck playing`, `deck locked`, `lock playing deck is off` (Engine's `Lock Playing Deck` setting is off, see [Playing decks](#playing-decks)), `lock playing deck unknown` (the deck could not read that setting as On or Off), `busy` (a safety net: the deck was still handling another request) or `switched off` (a `pending` request when the switch is turned off) |
| `failed` | The deck did not confirm the load: `timeout` (Engine did not show the track within 5 s and was not loading), `other track loaded`, `bad request`, `deck unavailable` or `no confirmation` (the deck stopped reporting the request without a result for 8 s, for example after the UI restarted) |
| `expired` | Nobody picked the request up within 10 s |

After `failed` with `timeout` or `no confirmation` the track may still arrive on the deck a moment later: Engine may still be loading it. Read the deck state (`GET /api/remote-load`) before you try again, because a new request makes Engine start the load over.

Poll `GET /api/remote-load/{id}` until the status is `loaded`, `rejected`, `failed` or `expired`. At most one `pending` or `loading` request exists per deck; the last 20 requests are kept in memory and are lost on restart.

## Playing decks

EngineX refuses a deck that is playing when the request arrives (`409 deck playing`) and checks again right before it gives Engine the load command; a deck that started playing in between ends as `rejected` (`deck playing`). "Playing" here means busy as `playing` above: `PlayState`, a position that moved in the last 1.5 s (a deck that just loaded counts as moved at its first position), or a touched jog wheel. Without the position check a load replaced a track during a held CUE preview on a real SC LIVE 4 (Engine's `Lock Playing Deck` did not stop it). Engine then loads the track on its own, which takes a moment. If the DJ presses play on that deck during this short window, only Engine's own setting `Lock Playing Deck` (Profile -> Safety) stops the new track from replacing the one that now plays. Turn that setting on. A load goes ahead only while the deck reads it as On; otherwise it ends as `rejected` (`lock playing deck is off`, or `lock playing deck unknown` when the deck cannot read it); `GET /api/remote-load` shows the value as `lockPlayingDeck`.

## Tested on a real deck

SC LIVE 4 (board `SCLIVE4`), Engine 5.3.0 with EngineX 5.3.0.4 built from this change, a FLAC track on a USB stick, 2026-11-14:

| Case | Result |
| --- | --- |
| Request for a playing deck | `409 deck playing`, track not replaced |
| CUE held for preview on a paused deck | deck reported `playing: true`, `409 deck playing`, track not replaced |
| Scratching / searching on a paused deck | deck reported `playing: true`, `409 deck playing`, track not replaced |
| The same deck left alone for 2 s | `loaded` after about 1 s |
| Idle, empty deck | `loaded` after about 1 s |
| The requested file already on that deck | `failed` (`timeout`), as described under `loaded` |
| `Lock Playing Deck` off | `lockPlayingDeck: false`, request `rejected` (`lock playing deck is off`), nothing loaded |
| Switch off | `POST` answers `403 remote load is switched off`; `GET /api/remote-load` shows `enabled: false` |

Not yet tested on a real deck: pressing play between submit and pickup or while `loading`, a long track from a slow drive, linked remote decks, and the other models.

## Error codes

| Code | Error text | Cause |
| --- | --- | --- |
| 400 | `invalid JSON` | Body is not valid JSON |
| 400 | `deck must be 1-4` | `deck` outside 1-4 |
| 400 | `path must be relative`, `path must not contain ..`, `path must name a file` | Bad `path` |
| 401 | `console login required` | Missing or wrong access code |
| 429 | `too many wrong access codes, try again in a minute` | Too many wrong access codes from this address |
| 403 | `remote load is switched off` | Switch is `Off` on the deck |
| 403 | `remote load is not available through the tunnel` | Request came through the Cloudflare tunnel |
| 404 | `file not found on any drive` | No eligible drive holds that file |
| 404 | `unknown request` | `GET` by id: id unknown or already dropped from the last 20 |
| 409 | `file exists on several drives` | Ambiguous path; response has `candidates`, repeat with `drive` |
| 409 | `deck locked` | The deck is not local (linked remote deck) |
| 409 | `deck playing` | The deck is playing or audible (see `playing`) |
| 409 | `deck has an open request` | Another request for that deck is `pending` or `loading` |
| 503 | `deck state unavailable` | No fresh deck report (older than 2 s), or the deck does not exist on this unit |
