"""Legacy build inputs and readers for upgrades from released images."""
import os
from pathlib import Path

LAB = os.environ.get('ENGINEX_LAB', '/root/build/sc5000-lab')  # Existing read-only firmware fixture.
DATA_NAME = 'sc5000-custom'  # Data layout in releases before the rename.
ALSA_FILE = '99-sc5000-custom.conf'  # Installed ALSA configs, accepted during migration.
SUITE = 'sc5000-suite'  # Older bundle executable.
IMAGE_DIR = '/etc/sc5000'  # Version and release-tag reader.


def managed_paths(managed):
    """Clear upper-layer files left by old released images before installing ours."""
    managed += sorted({p.replace('enginex', 'sc5000') for p in managed} - set(managed))
    # default/*-perf is retired (5.3.0.5, #298): no image ships it, so name it to drop old copies.
    return sorted(set(managed) | {'sc5000', 'sc5000/stems-runtime-prepare', 'enginex',
                                  'dnsmasq.d/sc5000-telemetry.conf', 'default/sc5000-perf', 'default/enginex-perf'})
DATA_DIR = '/data/sc5000-custom'  # Legacy migration fixture or reader.
CUSTOM_ENV = 'SC5000_CUSTOM'  # Legacy migration fixture or reader.
ALSA_ENV = 'SC5000_ALSA_OVERLAY'  # Legacy migration fixture or reader.
DEEMIX_UNIT = 'enginex-sc5000-deemixd.service'  # Legacy migration fixture or reader.
DEEMIX = 'sc5000-deemixd'  # Legacy migration fixture or reader.
DROPIN = '99-sc5000.conf'  # Legacy migration fixture or reader.
ENGINE_SCRIPT = 'sc5000-engine-script'  # Legacy migration fixture or reader.
ASSIGNMENTS = 'sc5000-assignments'  # Legacy migration fixture or reader.
BOOT_CONVERTED = 'sc5000-boot-converted'  # Legacy migration fixture or reader.
TZ_NEWER = 'sc5000-tz-newer'  # Legacy migration fixture or reader.
PROJECT = 'sc5000'  # Legacy migration fixture or reader.
UI_HOOK_ENV = 'SC5000_UI_HOOK'  # Environment carried by an older runtime unit.
