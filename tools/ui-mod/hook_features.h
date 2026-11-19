/* EngineX features that a build or the deck can switch off.
 *
 * Build:   ENGINEX_HOOK_DISABLE="stems,zoom30" sh tools/ui-mod/build.sh
 * Deck:    names in /data/enginex/hook-disable.conf (comma, space or one per line,
 *          '#' starts a comment), read once per process: change it, then restart Engine.
 * A feature is on unless the build or the deck file names it. HF_ALWAYS (0) marks
 * what is never switchable. mount-hook is handled by the bundle (boot-activate.sh,
 * build-data-bundle.py --no-edisksd-hook); this hook only knows the name.
 *
 * Included once, by engine_ui_hook.c. Only libc and raw syscalls: the hook's own
 * open/stat wrappers call hf_on(), so reading the file must not go through them. */
#ifndef ENGINEX_HOOK_FEATURES_H
#define ENGINEX_HOOK_FEATURES_H

#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>

/* X(ID, "name"): one line each, build.sh reads the names from these lines. mount-hook,
 * deemixd and internal-heal have no site in the hook: the boot script leaves
 * them out (the edisksd preload, the services) and engine-ui-guard skips the internal
 * library check. rb-bridge also gates imported-analysis replay/overview repair.
 * Naming them here keeps them known names. */
#define ENGINEX_HOOK_FEATURES(X) \
    X(GATEWAY, "gateway") \
    X(SSO, "sso") \
    X(APPLE_MUSIC, "apple-music") \
    X(STEMS, "stems") \
    X(BLUETOOTH, "bluetooth") \
    X(FILESYSTEMS, "filesystems") \
    X(READONLY_STICKS, "readonly-sticks") \
    X(LIMITER_MENU, "limiter-menu") \
    X(STEMS_EQ, "stems-eq") \
    X(SWEEP_FX, "sweep-fx") \
    X(ZOOM30, "zoom30") \
    X(REC_STREAMING, "rec-streaming") \
    X(AUX_INPUT, "aux-input") \
    X(FOUR_DECK, "four-deck") \
    X(DSP_GUARDS, "dsp-guards") \
    X(HIFI_OUTPUT, "hifi-output") \
    X(REVERB_TAIL, "reverb-tail") \
    X(SQLITE_TUNING, "sqlite-tuning") \
    X(TELEMETRY_BLOCK, "telemetry-block") \
    X(VENDOR_UPDATE_BLOCK, "vendor-update-block") \
    X(THREAD_TUNING, "thread-tuning") \
    X(ALSA_TIMESTAMP, "alsa-timestamp") \
    X(UI_PACKS, "ui-packs") \
    X(SHADOW_PAUSED_FOLLOW, "shadow-paused-follow") \
    X(MOUNT_HOOK, "mount-hook") \
    X(RB_BRIDGE, "rb-bridge") \
    X(DEEMIXD, "deemixd") \
    X(INTERNAL_HEAL, "internal-heal")

enum {
    HF_ALWAYS = 0,
#define HF_ENUM(id, name) HF_##id,
    ENGINEX_HOOK_FEATURES(HF_ENUM)
#undef HF_ENUM
    HF_COUNT
};
_Static_assert(HF_COUNT <= 32, "hook features must fit one 32-bit mask");

static const char *const hf_names[HF_COUNT] = {
    [HF_ALWAYS] = "",
#define HF_NAME(id, name) [HF_##id] = name,
    ENGINEX_HOOK_FEATURES(HF_NAME)
#undef HF_NAME
};

// Instruction tagging is checked before compilation; disabled instructions become NOPs.
#define HF_CONFIG(feature, word) (hf_on(feature) ? (uint32_t)(word) : UINT32_C(0xe320f000))
#define HF_CONFIG_COPY(feature, dst, src, size) do { if (hf_on(feature)) memcpy(dst, src, size); } while (0)
#define HF_INTERPOSE(feature) _Static_assert((feature) >= HF_ALWAYS && (feature) < HF_COUNT, "missing hook feature");

#ifndef ENGINEX_HOOK_DISABLE
#define ENGINEX_HOOK_DISABLE ""
#endif
#ifndef HOOK_DISABLE_PATH
#define HOOK_DISABLE_PATH "/data/enginex/hook-disable.conf"
#endif

static int hf_separator(char c) { return c == ',' || c == ' ' || c == '\t' || c == '\r' || c == '\n'; }

static int hf_lookup(const char *name, size_t size) {
    for (int f = 1; f < HF_COUNT; f++)
        if (strlen(hf_names[f]) == size && !memcmp(hf_names[f], name, size)) return f;
    return HF_ALWAYS;
}

/* The mask of the named features; *unknown counts names that are not features. */
static uint32_t hf_parse(const char *text, size_t size, unsigned *unknown) {
    uint32_t mask = 0;
    size_t i = 0;
    while (i < size && text[i]) {
        if (text[i] == '#') {
            while (i < size && text[i] && text[i] != '\n') i++;
        } else if (hf_separator(text[i])) {
            i++;
        } else {
            size_t start = i;
            while (i < size && text[i] && !hf_separator(text[i]) && text[i] != '#') i++;
            int f = hf_lookup(text + start, i - start);
            if (f) mask |= 1u << f;
            else if (unknown) (*unknown)++;
        }
    }
    return mask;
}

static void hf_log(const char *text);

/* Only the first reader gets here (hf_state), so one static buffer is enough. A file
 * larger than it would lose names at its end: refuse it whole and say so. */
static char hf_text[64 * 1024];
static uint32_t hf_read(const char *path, unsigned *unknown) {
    long fd = syscall(SYS_openat, AT_FDCWD, path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return 0;
    size_t got = 0;
    long n = 1;
    while (got < sizeof hf_text && (n = syscall(SYS_read, (int)fd, hf_text + got, sizeof hf_text - got)) > 0)
        got += (size_t)n;
    char more;
    int too_large = got == sizeof hf_text && syscall(SYS_read, (int)fd, &more, 1) > 0;
    syscall(SYS_close, (int)fd);
    if (too_large) {
        hf_log("engine-ui-hook: hook-disable.conf is larger than 64 KiB; ignored\n");
        return 0;
    }
    return hf_parse(hf_text, got, unknown);
}

static const char *hf_path = HOOK_DISABLE_PATH; /* tests point it elsewhere */
static uint32_t hf_mask;
static int hf_state; /* 0 not read, 1 being read, 2 read */

static void hf_log(const char *text) {
    if (text[0]) syscall(SYS_write, STDERR_FILENO, text, strlen(text));
}

static uint32_t hf_with_needs(uint32_t mask) {
    if (mask & (1u << HF_SSO)) mask |= 1u << HF_GATEWAY;
    if (mask & (1u << HF_DSP_GUARDS)) mask |= 1u << HF_HIFI_OUTPUT;
    return mask;
}

/* Build and deck mask, read once. A caller racing the first read gets the build mask. */
static uint32_t hf_disabled(void) {
    if (__atomic_load_n(&hf_state, __ATOMIC_ACQUIRE) == 2) return hf_mask;
    unsigned unknown = 0;
    uint32_t mask = hf_with_needs(hf_parse(ENGINEX_HOOK_DISABLE, sizeof ENGINEX_HOOK_DISABLE - 1, NULL));
    int expected = 0;
    if (!__atomic_compare_exchange_n(&hf_state, &expected, 1, 0, __ATOMIC_ACQ_REL, __ATOMIC_ACQUIRE))
        return expected == 2 ? __atomic_load_n(&hf_mask, __ATOMIC_ACQUIRE) : mask;
    mask = hf_with_needs(mask | hf_read(hf_path, &unknown));
    __atomic_store_n(&hf_mask, mask, __ATOMIC_RELEASE);
    __atomic_store_n(&hf_state, 2, __ATOMIC_RELEASE);
    if (unknown) hf_log("engine-ui-hook: hook-disable.conf names an unknown feature; ignored\n");
    return mask;
}

/* Forget the cached mask (tests, after changing hf_path or the file). */
static uint32_t hf_unavailable, hf_unproven;
__attribute__((unused)) static void hf_reload(void) {
    __atomic_store_n(&hf_unavailable, 0, __ATOMIC_RELEASE);
    __atomic_store_n(&hf_unproven, 0, __ATOMIC_RELEASE);
    __atomic_store_n(&hf_mask, 0, __ATOMIC_RELEASE);
    __atomic_store_n(&hf_state, 0, __ATOMIC_RELEASE);
}

static int hf_on(int feature) {
    return feature <= HF_ALWAYS || feature >= HF_COUNT || !(hf_disabled() & (1u << feature));
}

/* A feature this process turned out not to have (an Engine build without a verified
 * site): off from here on, and hf_publish() lists it with what the build left out, so
 * the console shows it as not available instead of asking for an Engine restart. */
__attribute__((unused)) static void hf_runtime_off(int feature) {
    if (feature <= HF_ALWAYS || feature >= HF_COUNT) return;
    hf_disabled();
    __atomic_or_fetch(&hf_unavailable, 1u << feature, __ATOMIC_RELEASE);
    __atomic_or_fetch(&hf_mask, 1u << feature, __ATOMIC_RELEASE);
}

/* "gateway,stems" for a mask; "" when it names nothing. */
static void hf_names_of(uint32_t mask, char *out, size_t size) {
    size_t used = 0;
    if (size) out[0] = 0;
    for (int f = 1; f < HF_COUNT && size; f++) {
        if (!(mask & (1u << f))) continue;
        size_t n = strlen(hf_names[f]);
        if (used + (used ? 1 : 0) + n + 1 > size) break;
        if (used) out[used++] = ',';
        memcpy(out + used, hf_names[f], n);
        used += n;
        out[used] = 0;
    }
}

/* The features this process runs without, for the start-up log. */
static void hf_describe(char *out, size_t size) { hf_names_of(hf_disabled(), out, size); }

#ifndef HOOK_FEATURES_STATE_PATH
#define HOOK_FEATURES_STATE_PATH "/run/enginex/hook-features"
#endif
static const char *hf_state_path = HOOK_FEATURES_STATE_PATH; /* tests point it elsewhere */

/* Tells stream-sync what the running Engine was started with: the web console's feature
 * menu and the QML rows of switched-off features read it. hook-disable.conf alone is not
 * enough: it may have changed since Engine started, and the build list is not in it.
 *   pid=<Engine pid>\nbuild=<names>\noff=<names>\n[on=shadow-paused-follow\n]
 * on= names what this hook does and a hook of before it did not: not being in off= cannot
 * say that, and a UI pack that left a shadow deck paused under a hook that does not move
 * its line would show a waveform that stands still.
 * Written to a temporary file and renamed, so a reader never sees half of it. */
static void hf_publish(void) {
    char build_off[512], off[512], text[1100], tmp[256], dir[256];
    hf_names_of(hf_with_needs(hf_parse(ENGINEX_HOOK_DISABLE, sizeof ENGINEX_HOOK_DISABLE - 1, NULL)) | hf_unavailable, build_off, sizeof build_off);
    hf_names_of(hf_disabled(), off, sizeof off);
    int n = snprintf(text, sizeof text, "pid=%d\nbuild=%s\noff=%s\n%s", (int)getpid(), build_off, off,
                     hf_on(HF_SHADOW_PAUSED_FOLLOW) && !(__atomic_load_n(&hf_unproven, __ATOMIC_ACQUIRE) & (1u << HF_SHADOW_PAUSED_FOLLOW)) ? "on=shadow-paused-follow\n" : "");
    if (n < 0 || (size_t)n >= sizeof text) return;
    if ((size_t)snprintf(dir, sizeof dir, "%s", hf_state_path) >= sizeof dir) return;
    char *slash = strrchr(dir, '/');
    if (slash && slash != dir) {
        *slash = 0;
        mkdir(dir, 0755);
    }
    if ((size_t)snprintf(tmp, sizeof tmp, "%s.tmp", hf_state_path) >= sizeof tmp) return;
    long fd = syscall(SYS_openat, AT_FDCWD, tmp, O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC, 0644);
    if (fd < 0) return;
    long w = syscall(SYS_write, (int)fd, text, (size_t)n);
    syscall(SYS_close, (int)fd);
    if (w != n || rename(tmp, hf_state_path) != 0) unlink(tmp);
}

/* A feature that is installed but depends on something Engine has yet to do in this
 * process: hf_publish() does not list it under on= between hf_await() and hf_proven().
 * hf_proven() publishes once, from the thread that saw it work. */
__attribute__((unused)) static void hf_await(int feature) {
    if (feature > HF_ALWAYS && feature < HF_COUNT) __atomic_or_fetch(&hf_unproven, 1u << feature, __ATOMIC_RELEASE);
}
__attribute__((unused)) static void hf_proven(int feature) {
    uint32_t bit = feature > HF_ALWAYS && feature < HF_COUNT ? 1u << feature : 0;
    if (!(__atomic_load_n(&hf_unproven, __ATOMIC_ACQUIRE) & bit)) return;
    if (__atomic_fetch_and(&hf_unproven, ~bit, __ATOMIC_ACQ_REL) & bit) hf_publish();
}

#endif
