# EngineX naming (#234, 5.3.0.5)

Commands, units, environment variables, host names and API callers use
`enginex` / `ENGINEX_*`. The local API is `/v1/enginex/`; the retired namespace
returns 404. Relay and phone clients use the current routes.

## Upgrade contract

A deck upgrading from 5.3.0.4 or earlier needs no manual data cleanup. The
one-time migration moves its pre-5.3.0.5 data directory to `/data/enginex`,
preserves settings, identity, media and conflicting files, and rewrites managed
configuration values to the current paths and names. Existing compatibility
links are removed only when they are our relative or absolute link to
`/data/enginex`. Real directories and unrelated links are preserved.

No compatibility data link, old socket alias, legacy scale environment output
or old certificate domain is created. A stored certificate containing the
retired domain is replaced once before it is served. Current certificates and
private settings otherwise keep their existing behavior.

Name cleanup follows the upgrade commit. Before it, a refusal or interrupted
upgrade preserves the original loader and data layout; a committed update
finishes the loader rename at its next boot.

Downgrade restoration and the manual layout handoff are retired. Older bundle
installs are refused without modifying the live generation. An existing
retired transaction journal is left intact and reported for manual recovery;
its parked data is never deleted automatically. Activation and current bundle
installation remain serialized by the same blocking lock.

## Migration readers and repository guard

Necessary upgrade literals live in one file per language:
`internal/migration/legacy.go`,
`rootfs-overlay/usr/libexec/enginex-migration`,
`tools/az01-packer/legacy_names.py` and `tools/legacy_names.h`.
Installed ALSA configurations can enter the migration ABI once, then use the
current `99-enginex.conf` / `enginex_alsa_boot_hook` configuration.

`tools/az01-packer/test_rename234_names.py` checks paths and all current and
historical text. Its explicit allow-list contains only migration files and a
temporary Mods exception, to be removed when `agent/5305-mods-293` merges.
Denon hardware model names and official image prefixes remain unchanged.

## History

The initial implementation kept compatibility routes, data links and a
transactional downgrade restore. The maintainer retired those contracts on
2026-11-16 in favor of upgrading to the latest release. Historical records now
use current names or describe the pre-5.3.0.5 paths without spelling them out.
The r5-to-current Gen-1 image OTA and Gen-2 bundle OTA remain required gates.
