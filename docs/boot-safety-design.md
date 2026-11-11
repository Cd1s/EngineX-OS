# Boot recovery candidate (#140)

This is a safety net for an EngineX startup failure, not a confirmed diagnosis of
the PRIME 4 reports. It is planned for EngineX 5.3.0.4.

## Measurements before implementation

The published 5.3.0.3 PRIME 4 (JC11) image was started with its real armhf Engine
in private mount, network and PID namespaces. Each case used a new `/data`,
offscreen Qt, mocked board/IRQ files, and no GPU or physical deck. A test-only
observer in `SplashScreenLoader` recorded when the main view's stock splash
became inactive; resource registration and process uptime were not readiness.

| Stage (seconds) | Empty | Mounted 10,000-track fixture | Stock QML cache seeded |
| --- | ---: | ---: | ---: |
| etc-reset | 0.010 | 0.011 | 0.013 |
| prepare | 0.107 | 0.109 | 0.278 on update |
| runtime debloat | 0.018 | 0.019 | 0.025 |
| guard pre | 0.493 | 0.508 | 0.674 after update |
| stream-sync to HTTP ready | 0.133 | 0.126 | 0.347 |
| Engine start to main splash dismissal | 8.862 | 9.533 | 14.120 |
| Explicit data-only library heal | — | 7.287 | — |

The fixture uses the captured schema and 10,000 track rows with synthetic media
paths. Its bind mount does not reproduce SATA/USB enumeration through edisksd or
all vendor library scans. These are first application boots with fresh data,
not measured physical power-on times. The stock-cache case runs stock Engine
first with all temporary EngineX account/key state removed, then installs packs
over its cached QML. This stricter repeat was measured under higher host load.
No multi-minute EngineX step was
reproduced. Larger artwork, a slow drive, and real control-surface initialization
remain unmeasured. Library healing scales with data; the mount hook already has
a 20-second budget. Runtime inspection found that the vendor image provides
neither a timeout executable nor a BusyBox timeout applet, so the previous
conditional shell timeout did not bound pre-start reserve repair. The guardian
now enforces its own 10-second repair and 15-second report deadlines. This is
a possible source of long pre-start waits, not a confirmed cause of #140.
Reserve repair now publishes a durable backup without removing the live database
name, then atomically replaces that name. A deadline between those operations
leaves the original available to Engine.
On FAT/exFAT a backup needs a full copy because hard links are unavailable.
Repair therefore needs space for both the rebuilt file and the backup; a full
drive leaves the original unchanged. A deadline can leave partial temporary
files until the next repair attempt removes them. Libraries that exceed the
repair budget remain unchanged and may need maintenance outside startup.
Stream-sync's pre-listen cleanup only visits its download-cache directory; it
does not traverse an entire music drive.

## Controller and mixer paths

`GlobalAssignmentConfig.qml` performs one asynchronous GET every 10 seconds;
its callback parses a small response. No synchronous XHR, busy wait or repeated
tree traversal was found there or in the mixer-only startup gates. Without the
physical USB control surface the sandbox cannot establish that the separate
controller QML engine actually instantiates it. Three synchronous POSTs were
found in streaming-sort handlers, including a property-change path potentially
reachable on state restoration. They now use asynchronous requests while
retaining the existing sort actions and persistence fields. This removes a
blocking risk; it does not establish the cause of #140.

## State and fallback

The existing `engine-ui-guard` owns the state and the existing
`engine-guardian.service` runs its watchdog. Database `--watch` stays retired.
The guard wraps each invocation of the real Engine, including retries within
the vendor shell loop, preserving its exit code and quit-reason handling.

The default **T is 180 seconds without UI readiness or completed startup
progress**. This is more than 12 times the slowest measured application start
and exceeds the known 150-second profile-login wait. It leaves room for slower
hardware and bounded library preparation; it is not a hardware timing claim.
**N is three failed attempts within M = 300 seconds**. A ready UI cannot erase
recent failures and hide a crash loop. Clean requested stops clear the failure
streak; they never remove a fallback marker. Intentional USB/update handoffs
disarm their attempt before the vendor action, and stale quit reasons are
discarded before starting the next Engine.

An asynchronous main-view signal is sent only after the main display's splash
is dismissed. Its nonce must match the current attempt. Changed progress records
must also match that attempt; completed work extends the inactivity deadline,
while repeatedly publishing the same record does not. A fallback marker always
wins over readiness. Stock fallback has no Engine `LD_PRELOAD`, no QML pack and
no custom assignments. Engine's pre-start runtime tuning and later performance
tuning are skipped; the separate network preparation keeps Wi-Fi reachable.
Mods use the existing quarantine. The gen2 ALSA activator
still starts diagnostics after reboot, but without a preload its patch
constructor stays inactive and it does not restart a stock fallback Engine.

Fallback requests a service restart before collecting reports. Allow the
one-second watchdog poll and stock unit's 30-second stop timeout in addition to
T. The state survives clean stops and reboots, and is cleared only by an
authenticated explicit retry or a new version. Repacking the same version
preserves it. Recovery never calls Factory Reset, the updater, reboot or a
library writer. Reports are bounded and redacted, and copied only to eligible
mounted media identified as removable USB or SD. USB-attached fixed drives are
excluded because some models use a USB bridge for their internal SATA bay.
Publication uses atomic no-replace rename, with atomic hard-link publication
on filesystems that lack rename2 but support links; otherwise it fails without
overwriting a user file. See [the three-language FAQ](boot-recovery-faq.md).
Stock Engine versions come from the same verified GNU Build IDs as the UI hook,
read from ELF metadata without executing Engine. Unknown builds stay unknown;
the vendor's build timestamp is not treated as a version.
