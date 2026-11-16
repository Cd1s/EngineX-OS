# EngineX OS 5.3.0.5 change record

## Final naming contract (#234, 2026-11-16)

The maintainer retired the compatibility HTTP namespace, data link, socket
aliases, old scale environment output, certificate domain and downgrade
restoration. Current clients use `/v1/enginex/` and `/data/enginex`. Historical
records use current terminology; only upgrade migration readers retain the
pre-5.3.0.5 literals.

The one-time migration preserves real data, rewrites managed stored values and
removes only owned compatibility links. An unrelated link or real directory
is never deleted. Old restore operations are refused with live and parked
user data untouched. Regression tests assert this contract instead of the
retired downgrade restore. Gen-1 r5 image OTA and Gen-2 r5 Data-Bundle OTA must
remain green. The Mods tree is handled by `agent/5305-mods-293`; its single
temporary guard exception must be removed at integration.

Upgrade cleanup runs after the new generation commits. Invalid input and
failed or interrupted upgrades keep the original ALSA loader, library and
existing data layout usable. Forward-upgrade rollback preserves its input
bytes; this does not restore the retired downgrade/layout operation. A cold
boot completes a committed loader-name migration without recreating a link.
