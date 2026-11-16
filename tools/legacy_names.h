#ifndef ENGINEX_LEGACY_NAMES_H
#define ENGINEX_LEGACY_NAMES_H
// Old bundle lookup during a staged downgrade.
#define LEGACY_BUNDLE_PROCESSOR "/data/sc5000-custom/bundle/stems-processor"
// Old multi-call bundle layout.
#define LEGACY_BUNDLE_BIN_PROCESSOR "/data/sc5000-custom/bundle/bin/stems-processor"
// Old boot activator while the canonical link is being exchanged.
#define LEGACY_BOOT_ACTIVATE "/data/sc5000-custom/bundle/boot-activate.sh"
// Data directory retained by the downgrade transaction.
#define LEGACY_DATA_NAME "sc5000-custom"
// HFS cache and views mounted by an older release.
#define LEGACY_HFS_LIB_ROOT "/data/sc5000-custom/hfs-libraries"
#define LEGACY_HFS_RUN_ROOT "/run/sc5000-hfs-views"
#define LEGACY_HFS_VIEW_NAME "sc5000-hfs-view"
#define LEGACY_HFS_MOUNT "tmpfs sc5000-hfs-view "
// Older installers receive these overrides only when they fill a test/staging root.
#define LEGACY_INSTALL_ENV " SC5000_CUSTOM=\"$d\" SC5000_ALSA_OVERLAY=\"$d/alsa-conf.d\"; "
#endif

#ifdef ENGINEX_EXPORT_LEGACY_ALSA
// Installed ALSA configs enter these aliases once, then activation writes the current config.
extern __typeof__(enginex_alsa_boot_hook) sc5000_alsa_boot_hook __attribute__((alias("enginex_alsa_boot_hook"),visibility("default")));
extern __typeof__(snd_config_hook_enginex_alsa_boot_hook) snd_config_hook_sc5000_alsa_boot_hook __attribute__((alias("snd_config_hook_enginex_alsa_boot_hook"),visibility("default")));
__attribute__((used,visibility("default"))) const char _sc5000_alsa_boot_hook_dlsym_config_hook_001[]="ALSA_0.9";
__attribute__((used,visibility("default"))) const char _snd_config_hook_sc5000_alsa_boot_hook_dlsym_config_hook_001[]="ALSA_0.9";
#endif
