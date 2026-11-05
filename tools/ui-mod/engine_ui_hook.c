// LD_PRELOAD hook for /usr/Engine/Engine.
//
// Engine registers its QML through qRegisterResourceData. This hook swaps
// individual resource files for the ones in the active UI pack, so the UI can
// be changed without touching the read-only rootfs or the Engine binary.
// Removing the pack, or creating the safe-mode file, gives the stock UI back.
//
// Only libc is used. Static init runs before Qt is usable.
#define _GNU_SOURCE
#include <dlfcn.h>
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <link.h>
#include <netdb.h>
#include <pthread.h>
#include <sched.h>
#include <spawn.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <sys/mman.h>
#include <sys/prctl.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>
#include "dsp_smooth.h"
#include "master_limiter.h"

#define UI_ROOT "/data/sc5000-custom/ui"
#define FLAG_COMPRESSED 0x01
#define FLAG_DIRECTORY 0x02
#define FLAG_ZSTD 0x04

typedef unsigned char uchar;
typedef int (*register_fn)(int, const uchar *, const uchar *, const uchar *);
static cpu_set_t engine_cpus;
static int engine_cpus_ready;

static void initialize_engine_cpus(void) {
    long online = sysconf(_SC_NPROCESSORS_ONLN);
    if (online > 1) {
        CPU_ZERO(&engine_cpus);
        for (long cpu = 0; cpu < online && cpu < CPU_SETSIZE; cpu++) CPU_SET((int)cpu, &engine_cpus);
        engine_cpus_ready = 1;
    } else {
        engine_cpus_ready = sched_getaffinity(0, sizeof engine_cpus, &engine_cpus) == 0;
    }
}

static void *next_symbol(void **cache, const char *name) {
    void *fn = __atomic_load_n(cache, __ATOMIC_ACQUIRE);
    if (!fn) {
        fn = dlsym(RTLD_NEXT, name);
        if (fn) __atomic_store_n(cache, fn, __ATOMIC_RELEASE);
    }
    return fn;
}

static uint32_t be32(const uchar *p) { return (uint32_t)p[0] << 24 | p[1] << 16 | p[2] << 8 | p[3]; }
static uint16_t be16(const uchar *p) { return (uint16_t)(p[0] << 8 | p[1]); }
static void put32(uchar *p, uint32_t v) { p[0] = v >> 24; p[1] = v >> 16; p[2] = v >> 8; p[3] = v; }

struct walk {
    uchar *tree;
    const uchar *names, *data;
    size_t node_size;
    char pack[128];
    int replaced;
};

static int is_engine(void) {
#ifdef ENGINE_UI_HOOK_TEST
    extern int hook_test_engine;
    return hook_test_engine;
#else
    static int cached = -1;
    int value = __atomic_load_n(&cached, __ATOMIC_RELAXED);
    if (value >= 0) return value;
    char exe[64];
    ssize_t n = readlink("/proc/self/exe", exe, sizeof exe - 1);
    if (n <= 0)
        return 0;
    exe[n] = 0;
    const char *base = strrchr(exe, '/');
    base = base ? base + 1 : exe;
    value = strcmp(base, "Engine") == 0;
    __atomic_store_n(&cached, value, __ATOMIC_RELAXED);
    return value;
#endif
}

static void *alsa_sw_cache, *alsa_tstamp_set_cache, *alsa_tstamp_get_cache;

int snd_pcm_sw_params_set_tstamp_type(void *pcm, void *params, int type) {
    int (*real)(void *, void *, int) = next_symbol(&alsa_tstamp_set_cache, "snd_pcm_sw_params_set_tstamp_type");
    if (!real) return -ENOSYS;
    // Engine already requests RAW in ALSADevice; never change an explicit monotonic clock.
    if (is_engine() && type == 0) type = 2; // GETTIMEOFDAY -> MONOTONIC_RAW
    return real(pcm, params, type);
}

int snd_pcm_sw_params(void *pcm, void *params) {
    int (*real)(void *, void *) = next_symbol(&alsa_sw_cache, "snd_pcm_sw_params");
    if (!real) return -ENOSYS;
    if (is_engine()) {
        int (*get)(const void *, int *) = next_symbol(&alsa_tstamp_get_cache, "snd_pcm_sw_params_get_tstamp_type");
        int type;
        if (get && !get(params, &type) && type == 0) {
            int rc = snd_pcm_sw_params_set_tstamp_type(pcm, params, 2);
            if (rc < 0) return rc;
        }
    }
    return real(pcm, params);
}

// #58: the toggle is master_limiter=1 in settings.conf, polled once a second
// off the audio thread; the limiter itself runs in hook_master_output.
static int lim_enabled, gain_enabled; // gain_enabled: #64 auto_gain=1

static void *lim_poll(void *arg) {
    (void)arg;
    for (;;) {
        int on = 0, gain = 0;
        FILE *f = fopen(UI_ROOT "/settings.conf", "r");
        if (f) {
            char line[64];
            while (fgets(line, sizeof line, f))
                if (!strncmp(line, "master_limiter=1", 16)) on = 1;
                else if (!strncmp(line, "auto_gain=1", 11)) gain = 1;
            fclose(f);
        }
        __atomic_store_n(&lim_enabled, on, __ATOMIC_RELAXED);
        __atomic_store_n(&gain_enabled, gain, __ATOMIC_RELAXED);
        sleep(1);
    }
    return NULL;
}

static void lim_start_poll(void) {
    pthread_attr_t attr;
    struct sched_param param = {.sched_priority = 0};
    pthread_t thread;
    if (pthread_attr_init(&attr)) return;
    // Never inherit a real-time policy from whichever thread starts it.
    pthread_attr_setinheritsched(&attr, PTHREAD_EXPLICIT_SCHED);
    pthread_attr_setschedpolicy(&attr, SCHED_OTHER);
    pthread_attr_setschedparam(&attr, &param);
    pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
    pthread_create(&thread, &attr, lim_poll, NULL);
    pthread_attr_destroy(&attr);
}

static int official_update_host(const char *node) {
    static const char host[] = "autoupdate.airmusictech.com";
    if (!node) return 0;
    size_t size = strlen(node);
    if (size && node[size - 1] == '.') size--;
    return size == sizeof host - 1 && !strncasecmp(node, host, size);
}

static void *getaddrinfo_cache;

int getaddrinfo(const char *node, const char *service, const struct addrinfo *hints, struct addrinfo **result) {
    // device.inmusicbrands.com also serves OAuth/unlock; only deny the firmware-update host.
    if (is_engine() && official_update_host(node)) {
        if (result) *result = NULL;
        return EAI_NONAME;
    }
    int (*real)(const char *, const char *, const struct addrinfo *, struct addrinfo **) =
        next_symbol(&getaddrinfo_cache, "getaddrinfo");
    if (real) return real(node, service, hints, result);
    errno = ENOSYS;
    return EAI_SYSTEM;
}

// Edits to Engine in memory, the same as unlock_jp07_features.py makes offline, for
// a deck whose read-only root has the older binary. Each is only made if the bytes
// are as expected, so another Engine build is left alone.
struct engine_edit {
    uint32_t offset;
    const char *expected, *replacement;
    uint32_t size;
    int group; // all sites of a group are applied together or not at all
};
enum { EDIT_ONE, EDIT_STEMS, EDIT_GATEWAY };

// Everything that differs between Engine builds. The hook only patches a build
// listed here (exact GNU BuildID), and every site is checked byte for byte first.
struct engine_build {
    const char *name;
    uchar id[20];
    uint32_t dsp[21];
    const struct engine_edit *edits;
    unsigned edit_count;
    // getConfig's common return path: site, its two literal-pool words, and the
    // config fields set there (Stems integration, Bluetooth, Bluetooth send to
    // assignment, onboard Stems rendering, disableHfsPlus).
    uint32_t config_site, config_r2, config_r3;
    uint16_t config_field[5];
    // Stems feature registration: site, feature object, native true byte, enabled path, displaced instruction.
    uint32_t feature_site, feature_object, feature_true, feature_enabled, feature_displaced;
    // sqlite3_api_routines and the functions it lists ([55] exec, [70] open,
    // [135] open_v2). opendb != 0: open and open_v2 only tail-call it, hook it once.
    uint32_t sqlite_api, sqlite_exec, sqlite_open, sqlite_open_v2, sqlite_opendb;
    const char *sqlite_prefix[3]; // exec, open (or opendb), open_v2
};
static const struct engine_build *build;

static uint32_t stems_feature_jump[2];
static uint32_t config_jump[2];

static void *rx_code(const void *code, size_t size, size_t page) {
    void *stub = mmap(NULL, page, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (stub == MAP_FAILED) _exit(127);
    memcpy(stub, code, size);
    if (mprotect(stub, page, PROT_READ | PROT_EXEC)) _exit(127);
    __builtin___clear_cache(stub, (char *)stub + size);
    return stub;
}

typedef void (*dsp_event_fn)(void *, const void *);
typedef void (*dsp_process_fn)(void *, float *const *, float *const *, uint32_t);
enum {
    DSP_TURN_OFF, DSP_TURN_ON, DSP_SWITCH, DSP_CHANGE, DSP_ENABLE,
    DSP_MIX, DSP_PARAMETER, DSP_NUDGE, DSP_TEMPO, DSP_JUMP, DSP_RAMP_TIME, DSP_MOTOR_RAMP_TIME,
    DSP_SAMPLER_IMMEDIATE, DSP_SAMPLER_STOP, DSP_SAMPLER_PLAY,
    DSP_AUTOGATE, DSP_COMMON, DSP_DUB_ECHO, DSP_WASH, DSP_FLANGER, DSP_MASTER, DSP_HOOK_COUNT
};
static void *dsp_originals[DSP_HOOK_COUNT];

static void *dsp_bound_owner(void *handler) {
    // ARM MemberFunctionHandler owns a bound object and a two-word member pointer.
    // r0 at this entry is the handler, not the bound object. Keep it intact for
    // the original dispatcher, including its virtual-function/this-adjustment path.
    uint32_t owner;
    int32_t adjustment;
    memcpy(&owner, (char *)handler + 0x0c, sizeof owner);
    memcpy(&adjustment, (char *)handler + 0x14, sizeof adjustment);
    return (void *)((uintptr_t)owner + (adjustment >> 1));
}

static void dsp_transition(unsigned slot, uint32_t kind, void *handler, const void *event) {
    sc_dsp_fx_mark_transition(dsp_bound_owner(handler), kind);
    ((dsp_event_fn)dsp_originals[slot])(handler, event);
}

#define DSP_TRANSITION(name, slot, kind) \
    static void name(void *handler, const void *event) { dsp_transition(slot, kind, handler, event); }
DSP_TRANSITION(hook_fx_turn_off, DSP_TURN_OFF, SC_DSP_FX_TRANSITION_TURN_OFF)
DSP_TRANSITION(hook_fx_turn_on, DSP_TURN_ON, SC_DSP_FX_TRANSITION_TURN_ON)
DSP_TRANSITION(hook_fx_switch, DSP_SWITCH, SC_DSP_FX_TRANSITION_SWITCH)
DSP_TRANSITION(hook_fx_change, DSP_CHANGE, SC_DSP_FX_TRANSITION_CHANGE)
DSP_TRANSITION(hook_fx_enable, DSP_ENABLE, SC_DSP_FX_TRANSITION_ENABLE)
#undef DSP_TRANSITION

static void dsp_finite_event(unsigned slot, size_t offset, int is_double,
                             void *handler, const void *event) {
    if (!event) return;
    // Do not use the sanitizing sc_dsp_read_* helpers: NaN must be rejected,
    // rather than turned into a valid zero-valued control event.
    if (is_double) {
        double value;
        memcpy(&value, (const char *)event + offset, sizeof value);
        if (!sc_dsp_finite_f64(value) || (slot == DSP_TEMPO && value <= 0.0)) return;
        if ((slot == DSP_RAMP_TIME || slot == DSP_MOTOR_RAMP_TIME) && value < 0.0) return;
    } else {
        float value;
        memcpy(&value, (const char *)event + offset, sizeof value);
        if (!sc_dsp_finite_f32(value)) return;
    }
    ((dsp_event_fn)dsp_originals[slot])(handler, event);
}

static void hook_fx_mix(void *h, const void *e) { dsp_finite_event(DSP_MIX, 0x0c, 0, h, e); }
static void hook_fx_parameter(void *h, const void *e) { dsp_finite_event(DSP_PARAMETER, 0x20, 0, h, e); }
static void hook_playback_nudge(void *h, const void *e) { dsp_finite_event(DSP_NUDGE, 0x08, 1, h, e); }
static void hook_playback_tempo(void *h, const void *e) { dsp_finite_event(DSP_TEMPO, 0x08, 1, h, e); }
static void hook_time_stretch_jump(void *h, const void *e) { dsp_finite_event(DSP_JUMP, 0x08, 1, h, e); }
static void hook_ramp_time(void *h, const void *e) { dsp_finite_event(DSP_RAMP_TIME, 0x08, 1, h, e); }
static void hook_motor_ramp_time(void *h, const void *e) { dsp_finite_event(DSP_MOTOR_RAMP_TIME, 0x08, 1, h, e); }

static void dsp_sampler_event(unsigned slot, uint32_t kind, void *handler, const void *event) {
    sc_dsp_sampler_mark_event(dsp_bound_owner(handler), kind);
    ((dsp_event_fn)dsp_originals[slot])(handler, event);
}

static void hook_sampler_immediate(void *h, const void *e) {
    dsp_sampler_event(DSP_SAMPLER_IMMEDIATE, SC_DSP_SAMPLER_STOP_IMMEDIATELY, h, e);
}
static void hook_sampler_stop(void *h, const void *e) {
    dsp_sampler_event(DSP_SAMPLER_STOP, SC_DSP_SAMPLER_STOP, h, e);
}
static void hook_sampler_play(void *h, const void *e) {
    dsp_sampler_event(DSP_SAMPLER_PLAY, SC_DSP_SAMPLER_PLAY, h, e);
}

static void hook_dub_echo(void *self, float *const *in, float *const *out, uint32_t frames) {
    ((dsp_process_fn)dsp_originals[DSP_DUB_ECHO])(self, in, out, frames);
    sc_dsp_sweep_fx_post_process(self, in, frames);
}

static void hook_wash(void *self, float *const *in, float *const *out, uint32_t frames) {
    ((dsp_process_fn)dsp_originals[DSP_WASH])(self, in, out, frames);
    sc_dsp_sweep_fx_post_process(self, in, frames);
}

static void hook_autogate_process(void *self, float *const *input, float *const *output, uint32_t frames) {
    sc_dsp_autogate_pre_process(self, frames);
    ((dsp_process_fn)dsp_originals[DSP_AUTOGATE])(self, input, output, frames);
}

// Effects::Flanger::process, shared by Flanger, FlangerPositive and
// FlangerNegative (their vtables are its only references). It works in place
// on `input` and tail-calls the common chain with (input, input), so `input`
// holds the final block on return (#54).
static void hook_flanger(void *self, float *const *input, float *const *output, uint32_t frames) {
    float dry = sc_dsp_block_energy(input, frames);
    ((dsp_process_fn)dsp_originals[DSP_FLANGER])(self, input, output, frames);
    sc_dsp_makeup_apply(self, input, frames, dry);
}

// airHost's device callback (5.0.4 0x1462a6c, 5.3.0 0xc77934): it zeroes and
// then fills Engine's planar float outputs, before the resampler and the S32_LE
// conversion clip them, so this is the true master bus (#58). Every stereo pair
// gets its own linked limiter. The callback runs on the one audio thread.
typedef uint32_t (*audio_callback_fn)(void *, void *, int, const float *const *, int, float *const *, int);
static struct sc_lim master_lim[8];
static struct sc_gain master_gain[8];
// #49: in the 4-deck layout a player's 4-channel device (two stereo pairs) leaves
// decks 3 and 4 without output buffers, and Engine skips rendering them ("Mixer
// skipped ... unassigned output buffer"): their playheads stand still. All four
// render into scratch pairs (deck n into pair n-1); the deck's role (four_deck_role)
// picks which deck each of the card's pairs (RCA A, RCA B) plays. The other decks
// show and advance but stay silent here: another player or a CDJ plays them.
#define FOUR_DECK_PAD_FRAMES 8192
static float four_deck_pad[8][FOUR_DECK_PAD_FRAMES];
static int four_deck_pad_on;
static signed char four_deck_route[2] = {0, 1}; // deck (0-3) on RCA A and RCA B, -1 silent
static uint32_t hook_master_output(void *host, void *context, int in_channels, const float *const *in,
                                   int out_channels, float *const *out, int frames) {
    audio_callback_fn orig = (audio_callback_fn)dsp_originals[DSP_MASTER];
    uint32_t rc;
    if (four_deck_pad_on && out_channels == 4 && out && frames > 0 && frames <= FOUR_DECK_PAD_FRAMES) {
        float *padded[8];
        for (int ch = 0; ch < 8; ch++) padded[ch] = four_deck_pad[ch];
        rc = orig(host, context, in_channels, in, 8, padded, frames);
        for (int ch = 0; ch < 4; ch++) {
            if (!out[ch]) continue;
            int deck = four_deck_route[ch / 2];
            if (deck < 0) memset(out[ch], 0, (size_t)frames * sizeof(float));
            else memcpy(out[ch], four_deck_pad[deck * 2 + ch % 2], (size_t)frames * sizeof(float));
        }
    } else {
        rc = orig(host, context, in_channels, in, out_channels, out, frames);
    }
    int gain = __atomic_load_n(&gain_enabled, __ATOMIC_RELAXED);
    // Auto-Gain (#64) always runs into the limiter, so a boost never clips.
    if ((gain || __atomic_load_n(&lim_enabled, __ATOMIC_RELAXED)) && out && frames > 0)
        for (int ch = 0; ch < out_channels && ch < 16; ch += 2) {
            float *r = ch + 1 < out_channels ? out[ch + 1] : NULL;
            if (gain) sc_gain_planar(&master_gain[ch / 2], out[ch], r, (unsigned)frames);
            sc_lim_planar(&master_lim[ch / 2], out[ch], r, (unsigned)frames);
        }
    return rc;
}

static int dsp_buffers_overlap(const float *a, const float *b, uint32_t frames) {
    if (!a || !b) return 0;
    uintptr_t x = (uintptr_t)a, y = (uintptr_t)b;
    return (uint64_t)(x > y ? x - y : y - x) < (uint64_t)frames * sizeof(float);
}

static void hook_fx_common_chain(void *common, float *const *input, float *const *output, uint32_t frames) {
    // This runs on several RR49 render threads, one common per effect. Each has
    // its own ramp slot, owned (try-acquire only, never waiting) from enter to
    // exit; no lock, no allocation, no logging. chain == NULL (table full, or the
    // same common already in flight): the block is only sanitized.
    // The ramp is SC_DSP_RAMP_FRAMES long, so only that many dry frames are ever
    // read back: at most 1 KB of audio-thread stack at any block size.
    float dry_l[SC_DSP_RAMP_FRAMES], dry_r[SC_DSP_RAMP_FRAMES];
    float *dry_pair[2] = {dry_l, dry_r};
    struct sc_dsp_chain *chain = sc_dsp_chain_enter(common);
    int32_t ramp = chain ? __atomic_load_n(&chain->ramp_remaining, __ATOMIC_RELAXED) : 0;
    uint32_t snap = ramp <= 0 ? 0 : ((uint32_t)ramp < frames ? (uint32_t)ramp : frames);
    int aliases = input && output && frames &&
        (input == output || dsp_buffers_overlap(input[0], output[0], frames) ||
         dsp_buffers_overlap(input[0], output[1], frames) ||
         dsp_buffers_overlap(input[1], output[0], frames) ||
         dsp_buffers_overlap(input[1], output[1], frames));
    int saved_dry = 0;
    if (snap && aliases && input[0] && input[1]) {
        memcpy(dry_l, input[0], (size_t)snap * sizeof(float));
        memcpy(dry_r, input[1], (size_t)snap * sizeof(float));
        saved_dry = 1;
    }
    ((dsp_process_fn)dsp_originals[DSP_COMMON])(common, input, output, frames);
    // This is the input to the common tail; upstream FX may already have changed
    // it. An aliased block with nothing saved has no dry source.
    float *const *dry = saved_dry ? dry_pair : (aliases ? NULL : input);
    uint32_t dry_valid_frames = saved_dry ? snap : (aliases ? 0 : frames);
    sc_dsp_fx_common_post_process(chain, dry, output, frames, dry_valid_frames);
    // The helper sanitizes BEFORE interpolation. Opposite finite extremes can
    // still overflow (wet - dry), so check the at-most-128 frames it may remix.
    if (output) for (unsigned ch = 0; ch < 2; ch++) {
        if (!output[ch]) continue;
        for (uint32_t i = 0; i < frames && i < SC_DSP_RAMP_FRAMES; i++) {
            if (!sc_dsp_finite_f32(output[ch][i]))
                output[ch][i] = dry && dry[ch] && i < dry_valid_frames && sc_dsp_finite_f32(dry[ch][i]) ? dry[ch][i] : 0.0f;
        }
    }
    sc_dsp_chain_exit(chain); // NULL-safe; the slot is ours from enter to here
}

static const uint32_t dsp_event_prefix[] = {0xe1c021d0, 0xe590c00c, 0xe52db004};
static const uint32_t dsp_autogate_prefix[] = {0xe3530000, 0xe1a02001, 0x0a000035};
static const uint32_t dsp_common_prefix[] = {0xe16d42f4, 0xe1cd60f8};
static const uint32_t dsp_sweep_prefix[] = {0xe52db004, 0xe1a02001, 0xe28db000};
static const uint32_t dsp_flanger_prefix[] = {0xe16d42f4, 0xe3530000}; // strd r4,[sp,#-36]!; cmp r3,#0
// strd r4,[sp,#-36]! then mov r5,r2 (5.0.4) or strd r10,[sp,#24] (5.3.0)
static const uint32_t dsp_master_prefix[2][2] = {{0xe16d42f4, 0xe1a05002}, {0xe16d42f4, 0xe1cda1f8}};
struct dsp_hook_site {
    uint32_t offset;
    void *hook;
};
static struct dsp_hook_site dsp_hooks[DSP_HOOK_COUNT] = { // offsets: 5.0.4, replaced by select_build
    {0xd917b8, hook_fx_turn_off}, {0xd917e4, hook_fx_turn_on},
    {0xd91894, hook_fx_switch}, {0xd918c0, hook_fx_change}, {0xd918ec, hook_fx_enable},
    {0xd91810, hook_fx_mix}, {0xd91868, hook_fx_parameter},
    {0x14fff54, hook_playback_nudge}, {0x153312c, hook_playback_tempo},
    {0x1532fa0, hook_time_stretch_jump}, {0x1500950, hook_ramp_time},
    {0xd0b5bc, hook_motor_ramp_time}, {0xc5b2e0, hook_sampler_immediate},
    {0xc5b30c, hook_sampler_stop}, {0xc5b338, hook_sampler_play},
    {0xe3a598, hook_autogate_process}, {0x15fa188, hook_fx_common_chain},
    {0xe3d0a8, hook_dub_echo}, {0xe3d908, hook_wash}, {0xe3a118, hook_flanger},
    {0x1462a6c, hook_master_output}
};

static const void *dsp_expected(unsigned slot, size_t *size) {
    *size = slot == DSP_COMMON ? sizeof dsp_common_prefix : sizeof dsp_event_prefix;
    if (slot == DSP_DUB_ECHO || slot == DSP_WASH) return dsp_sweep_prefix;
    if (slot == DSP_FLANGER) { *size = sizeof dsp_flanger_prefix; return dsp_flanger_prefix; }
    if (slot == DSP_MASTER) { *size = sizeof dsp_master_prefix[0]; return dsp_master_prefix[dsp_hooks[slot].offset != 0x1462a6c]; }
    return slot == DSP_COMMON ? dsp_common_prefix :
        (slot == DSP_AUTOGATE ? dsp_autogate_prefix : dsp_event_prefix);
}

static void patch_bytes(uintptr_t entry, const void *bytes, size_t size, size_t page) {
    uintptr_t start = entry - entry % page;
    size_t span = ((entry + size + page - 1) / page * page) - start;
    // Constructor only, before Engine threads start. Never continue with a
    // partially installed group or an executable page left writable.
    if (mprotect((void *)start, span, PROT_READ | PROT_WRITE)) _exit(127);
    memcpy((void *)entry, bytes, size);
    if (mprotect((void *)start, span, PROT_READ | PROT_EXEC)) _exit(127);
    __builtin___clear_cache((char *)entry, (char *)entry + size);
}

static int install_dsp_hooks(uintptr_t base, size_t page) {
    // edit_engine has already verified the main ELF BuildID. Treat these hooks
    // as one group: if ANY original prefix differs, leave ALL DSP sites alone.
    if (!base || !page) return 0;
    for (unsigned i = 0; i < DSP_HOOK_COUNT; i++) {
        size_t size;
        const void *expected = dsp_expected(i, &size);
        if (memcmp((const void *)(base + dsp_hooks[i].offset), expected, size)) return 0;
    }
    for (unsigned i = 0; i < DSP_HOOK_COUNT; i++) {
        uintptr_t entry = base + dsp_hooks[i].offset;
        if (i == DSP_AUTOGATE) {
            // cmp r3,#0; mov r2,r1; BEQ -> absolute conditional load.
            // Both loads preserve NZCV; +12 is nonzero, +0xe4 is zero frames.
            const uint32_t code[] = {0xe3530000, 0xe1a02001, 0x059ff004, 0xe51ff004,
                                    (uint32_t)(entry + 12), (uint32_t)(entry + 0xe4)};
            dsp_originals[i] = rx_code(code, sizeof code, page);
        } else {
            uint32_t code[4];
            memcpy(code, (const void *)entry, 8); // no PC-relative instructions
            code[2] = 0xe51ff004;
            code[3] = (uint32_t)(entry + 8);
            dsp_originals[i] = rx_code(code, sizeof code, page);
        }
    }
    for (unsigned i = 0; i < DSP_HOOK_COUNT; i++) {
        uint32_t jump[] = {0xe51ff004, (uint32_t)(uintptr_t)dsp_hooks[i].hook, 0xe320f000};
        patch_bytes(base + dsp_hooks[i].offset, jump, i == DSP_AUTOGATE ? 12 : 8, page);
    }
    fprintf(stderr, "engine-ui-hook: %u DSP guards/transitions installed (shared FX state)\n", DSP_HOOK_COUNT);
    return 1;
}

// #49: four_deck_waveform=1 in settings.conf unlocks Engine's own 4-deck layout
// (Layout -> Decks / Waveforms) on a player. Engine offers it only to a non-player
// with 4 decks (5.3.0 0x1c44110); 4 decks alone crash a player's layout controller.
// The sound card keeps its 2 deck outputs (PlanckClient caps decks at 12, not at
// outputs), and engine-ui-guard names the hardware's deck group for this layout.
static const char *four_deck_settings = UI_ROOT "/settings.conf";
static const char *product_code_path = "/sys/firmware/devicetree/base/inmusic,product-code";

// The standalone players (SC5000, SC5000M, SC6000, SC6000M).
static int player_board(void) {
    char code[8] = "";
    FILE *f = fopen(product_code_path, "r");
    if (f) {
        if (!fgets(code, sizeof code, f)) code[0] = 0;
        fclose(f);
    }
    return !strncmp(code, "JP07", 4) || !strncmp(code, "JP08", 4) || !strncmp(code, "JP13", 4) || !strncmp(code, "JP14", 4);
}
static int four_deck_layout(void) {
    int v = 0;
    FILE *f = fopen(four_deck_settings, "r");
    if (f) {
        char line[64];
        while (fgets(line, sizeof line, f))
            if (!strncmp(line, "four_deck_waveform=", 19)) v = atoi(line + 19);
        fclose(f);
    }
    return v;
}

// four_deck_role: the decks this player plays on RCA A / RCA B: 13, 24, 12, or a
// single deck 1 / 2 with RCA B silent. auto (default) is what engine-ui-guard resolved
// from the Engine player number before this start; it also maps the hardware.
static const char *four_deck_resolved = "/run/sc5000-assignments/role";
static void read_role(const char *path, const char *key, char *role, size_t size) {
    FILE *f = fopen(path, "r");
    if (!f) return;
    char line[64];
    size_t n = strlen(key);
    while (fgets(line, sizeof line, f))
        if (!strncmp(line, key, n)) {
            line[strcspn(line, "\r\n")] = 0;
            snprintf(role, size, "%s", line + n);
        }
    fclose(f);
}

static void four_deck_role(void) {
    char role[8] = "auto";
    read_role(four_deck_settings, "four_deck_role=", role, sizeof role);
    if (!strcmp(role, "auto")) {
        snprintf(role, sizeof role, "13");
        read_role(four_deck_resolved, "", role, sizeof role);
    }
    static const struct { const char *name; signed char a, b; } roles[] = {
        {"12", 0, 1}, {"13", 0, 2}, {"24", 1, 3}, {"1", 0, -1}, {"2", 1, -1}};
    four_deck_route[0] = 0, four_deck_route[1] = 2;
    for (size_t i = 0; i < sizeof roles / sizeof *roles; i++)
        if (!strcmp(role, roles[i].name)) four_deck_route[0] = roles[i].a, four_deck_route[1] = roles[i].b;
}

static void prepare_config(uintptr_t base, size_t page) {
    // getConfig's common return path runs after every model's constructor.
    uint32_t code[] = {
        0xe3a02001, // mov r2, #1
        0xe5c020db, // hasStems integration
        0xe5c0224c, // hasBluetooth
        0xe5c0224e, // hasBluetoothSendToAssignment
        0xe5c02276, // remote Stems processor support
        0xe3a02000, // mov r2, #0
        0xe5c02289, // disableHfsPlus
        0xe320f000, // #49 when enabled: ldrb r2, [r0, #0x239] (isPlayer)
        0xe320f000, // cmp r2, #0: all-in-ones (not players) keep their config
        0xe320f000, // movne r2, #4
        0xe320f000, // strne r2, [r0, #0x90] (numberOfDecks)
        0xe320f000, // movne r2, #0
        0xe320f000, // strbne r2, [r0, #0x239]
        0xe59f2004, // replay the two displaced literal loads
        0xe59f3004,
        0xe59ff004,
        *(const uint32_t *)(base + build->config_r2),
        *(const uint32_t *)(base + build->config_r3),
        (uint32_t)(base + build->config_site + 8),
    };
    const unsigned at[] = {1, 2, 3, 4, 6};
    for (unsigned i = 0; i < 5; i++) code[at[i]] = (code[at[i]] & ~0xfffu) | build->config_field[i];
    if (!strcmp(build->name, "5.3.0") && four_deck_layout() > 0) {
        const uint32_t unlock[] = {0xe5d02239, 0xe3520000, 0x13a02004, 0x15802090, 0x13a02000, 0x15c02239};
        memcpy(code + 7, unlock, sizeof unlock);
        four_deck_pad_on = player_board();
        four_deck_role();
        fprintf(stderr, "engine-ui-hook: 4-deck layout unlocked (#49)\n");
    }
    config_jump[0] = 0xe51ff004;
    config_jump[1] = (uint32_t)(uintptr_t)rx_code(code, sizeof code, page);
}

static void prepare_stems_feature(uintptr_t base, size_t page) {
    // The old 0x8429b0 exit misses models configured False; intercept before that split.
    uint32_t code[] = {
        0xe59f3018, // ldr r3, [pc, #24] (Stems feature object)
        0xe1570003, // cmp r7, r3
        0x059f3014, // ldreq r3, [pc, #20] (native true byte)
        0x05873004, // streq r3, [r7, #4]
        0x059ff010, // ldreq pc, [pc, #16] (native enabled-feature registration)
        build->feature_displaced ? build->feature_displaced : 0xe51b3340, // displaced instruction
        0xe3540000, // cmp r4, #0 (displaced flags)
        0xe59ff008, // ldr pc, [pc, #8] (original store/branch)
        (uint32_t)(base + build->feature_object),
        (uint32_t)(base + build->feature_true),
        (uint32_t)(base + build->feature_enabled),
        (uint32_t)(base + build->feature_site + 8),
    };
    stems_feature_jump[0] = 0xe51ff004; // ldr pc, [pc, #-4]
    stems_feature_jump[1] = (uint32_t)(uintptr_t)rx_code(code, sizeof code, page);
}

static const struct engine_edit edits_504[] = {
    // Stems remote processor: exact Engine build from the verified patch manifest.
    {0x85ef9cu, "\xbc\x26\x9f\xe5\xbc\x36\x9f\xe5\xf4\x42\x6d\xe1\xf8\xa1\xcd\xe1\xf8\x60\xcd\xe1\xf0\x81\xcd\xe1\x20\xe0\x8d\xe5\x20\xb0\x8d\xe2\xe4\xd0\x4d\xe2\x02\x20\x8f\xe0\x3c\x10\x91\xe5\x00\x01\x0b\xe5\x94\x46\x9f\xe5\x03\x30\x92\xe7\x00\x00\x51\xe3\x04\x40\x8f",
        "\x10\x40\x2d\xe9\x00\x40\xa0\xe1\x1e\x10\xa0\xe3\x0c\x20\x8f\xe2\x04\xd5\xe3\xeb\x04\x00\xa0\xe1\x10\x80\xbd\xe8\x00\xf0\x20\xe3\x2f\x75\x73\x72\x2f\x6c\x6f\x63\x61\x6c\x2f\x62\x69\x6e\x2f\x73\x74\x65\x6d\x73\x2d\x70\x72\x6f\x63\x65\x73\x73\x6f\x72\x00", 63, EDIT_STEMS},
    {0x85e5f8u, "\x90\xc7\x9f\xe5",
        "\x1e\xff\x2f\xe1", 4, EDIT_STEMS},
    {0xf6303cu, "\xe0\x20\x9f\xe5\xd8\x30\x9f\xe5", (const char *)config_jump, 8, EDIT_STEMS},
    // TIDAL's client names a downloaded file after its codec and knows FLAC and AAC,
    // so an MP3 from the gateway would be saved with no extension and fail to load.
    // Its one "aac" string is both what it compares the codec with and the extension
    // it then uses. TIDAL's own AAC is labelled mp4a.40.x and keeps working.
    {0x1DB8E90u, "aac", "mp3", 4, EDIT_ONE},
    // Retain the pre-existing +0x24 capability patch.
    {0xF659B8u, "\x24\x30\xc4\xe5", "\x00\xf0\x20\xe3", 4, EDIT_ONE},
    {0x842644u, "\x40\x33\x1b\xe5\x00\x00\x54\xe3", (const char *)stems_feature_jump, 8, EDIT_ONE},
    // Preserve ShowStemsList eligibility in r8 across the extra device feature gate.
    {0x6D3E00u, "\x07\x00\x00\x0a", "\x07\x00\x00\xea", 4, EDIT_ONE},
    // unknown filesystem -> supported
    {0x1897e70u, "\x00\x60\xa0\xe3", "\x04\x60\xa0\xe3", 4, EDIT_ONE},
    // isSupportedFilesystem always true
    {0x1897dccu, "\x00\x40\xa0\x03", "\x01\x40\xa0\x03", 4, EDIT_ONE},
    // hfsplus not rewritten to ufsd
    {0x1678644u, "\xd1\x01\x00\x1a", "\x00\xf0\x20\xe3", 4, EDIT_ONE},
    // TidalAPI base URL -> stream-sync /v1/
    {0x336740u, "\x69\x75\x75\x71\x74\x3b\x30\x30\x62\x71\x6a\x2f\x75\x6a\x65\x62\x6d\x2f\x64\x70\x6e\x30\x77\x32", "\x69\x75\x75\x71\x3b\x30\x30\x32\x33\x38\x2f\x31\x2f\x31\x2f\x32\x3b\x39\x31\x39\x31\x30\x77\x32", 24, EDIT_GATEWAY},
    // TidalAPI auth URL -> stream-sync /au//
    {0x336758u, "\x6f\x7b\x7b\x77\x7a\x41\x36\x36\x68\x7c\x7b\x6f\x35\x7b\x70\x6b\x68\x73\x35\x6a\x76\x74\x36\x7d", "\x6f\x7b\x7b\x77\x41\x36\x36\x38\x39\x3e\x35\x37\x35\x37\x35\x38\x41\x3f\x37\x3f\x37\x36\x68\x7c", 24, EDIT_GATEWAY},
    // TidalAPI auth URL suffix 1/ -> //
    {0x33652cu, "\x38\x36\x03\xe3", "\x36\x36\x03\xe3", 4, EDIT_GATEWAY},
    // read-only volume not auto-ejected
    {0x58af34u, "\x30\x32\xd0\xe5", "\x00\x30\xa0\xe3", 4, EDIT_ONE},
    // DeviceManager::isReadOnly cache -> false
    {0x167295cu, "\x1c\x00\xd4\xe5", "\x00\x00\xa0\xe3", 4, EDIT_ONE},
    // DeviceManager::isReadOnly QFileInfo -> false
    {0x16728c0u, "\x01\x00\x24\xe2", "\x00\x00\xa0\xe3", 4, EDIT_ONE},
};

// 5.3.0 (BuildID be537e90...): sites found by RTTI/vtable slots (DSP), the QML
// property map (config fields), sqlite3_api_routines, and masked-context matches,
// each checked against the 5.0.4 disassembly.
static const struct engine_edit edits_530[] = {
    {0x8bf744u, "\xbc\x26\x9f\xe5\xbc\x36\x9f\xe5\xf4\x42\x6d\xe1\xf8\xa1\xcd\xe1\xf8\x60\xcd\xe1\xf0\x81\xcd\xe1\x20\xe0\x8d\xe5\x20\xb0\x8d\xe2\xe4\xd0\x4d\xe2\x02\x20\x8f\xe0\x3c\x10\x91\xe5\x00\x01\x0b\xe5\x94\x46\x9f\xe5\x03\x30\x92\xe7\x00\x00\x51\xe3\x04\x40\x8f",
        // As 5.0.4, with the BL re-encoded for QString::fromUtf8(QByteArrayView)@plt at 0x15f768.
        "\x10\x40\x2d\xe9\x00\x40\xa0\xe1\x1e\x10\xa0\xe3\x0c\x20\x8f\xe2\x03\x80\xe2\xeb\x04\x00\xa0\xe1\x10\x80\xbd\xe8\x00\xf0\x20\xe3\x2f\x75\x73\x72\x2f\x6c\x6f\x63\x61\x6c\x2f\x62\x69\x6e\x2f\x73\x74\x65\x6d\x73\x2d\x70\x72\x6f\x63\x65\x73\x73\x6f\x72\x00", 63, EDIT_STEMS},
    {0x8be39cu, "\x8c\xc7\x9f\xe5", "\x1e\xff\x2f\xe1", 4, EDIT_STEMS},
    {0x1537148u, "\xe4\x20\x9f\xe5\xdc\x30\x9f\xe5", (const char *)config_jump, 8, EDIT_STEMS},
    {0x1efc6bcu, "aac", "mp3", 4, EDIT_ONE},
    // Config.audio.hasLimiter (root +0x24), as the 5.0.4 +0x24 patch.
    {0x153DDB0u, "\x24\x30\xc4\xe5", "\x00\xf0\x20\xe3", 4, EDIT_ONE},
    {0x8a1eacu, "\x40\x33\x1b\xe5\x00\x00\x54\xe3", (const char *)stems_feature_jump, 8, EDIT_ONE},
    // ShowStemsList gate (eligibility is in r7 here, r8 in 5.0.4).
    {0x7398FCu, "\x07\x00\x00\x0a", "\x07\x00\x00\xea", 4, EDIT_ONE},
    {0x1a28ea8u, "\x00\x60\xa0\xe3", "\x04\x60\xa0\xe3", 4, EDIT_ONE},
    {0x1a28e04u, "\x00\x40\xa0\x03", "\x01\x40\xa0\x03", 4, EDIT_ONE},
    {0x167c6f8u, "\xfa\x01\x00\x1a", "\x00\xf0\x20\xe3", 4, EDIT_ONE},
    // TidalAPI base URL -> stream-sync /v1/ (5.3.0 XORs byte i with 0x24 + i)
    {0x3155d0u, "\x4c\x51\x52\x57\x5b\x13\x05\x04\x4d\x5d\x47\x01\x44\x58\x56\x52\x58\x1b\x55\x58\x55\x16\x4c\x0a", "\x4c\x51\x52\x57\x12\x06\x05\x1a\x1e\x1a\x00\x1f\x1e\x01\x1c\x02\x0e\x0d\x06\x0f\x08\x16\x4c\x0a", 24, EDIT_GATEWAY},
    // TidalAPI auth URL -> stream-sync /au// (5.3.0 XORs byte i with 0x57 + i)
    {0x3155e8u, "\x3f\x2c\x2d\x2a\x28\x66\x72\x71\x3e\x15\x15\x0a\x4d\x10\x0c\x02\x06\x04\x47\x09\x04\x01\x42\x18", "\x3f\x2c\x2d\x2a\x61\x73\x72\x6f\x6d\x57\x4f\x52\x4d\x54\x4b\x57\x5d\x50\x59\x52\x5b\x43\x0c\x1b", 24, EDIT_GATEWAY},
    // TidalAPI auth URL suffix 1/ -> //
    {0x3153acu, "\x5e\x3f\x05\xe3", "\x40\x3f\x05\xe3", 4, EDIT_GATEWAY},
    // The config field moved from 560 to 568.
    {0x5e28ccu, "\x38\x32\xd0\xe5", "\x00\x30\xa0\xe3", 4, EDIT_ONE},
    {0x167e32cu, "\x1c\x00\xd4\xe5", "\x00\x00\xa0\xe3", 4, EDIT_ONE},
    {0x167e290u, "\x01\x00\x24\xe2", "\x00\x00\xa0\xe3", 4, EDIT_ONE},
};

static const struct engine_edit edits_sclive[] = {
    {0x8ba000u, "\xc0\x26\x9f\xe5\xc0\x36\x9f\xe5\xf4\x42\x6d\xe1\xf8\xa1\xcd\xe1\xf8\x60\xcd\xe1\xf0\x81\xcd\xe1\x20\xe0\x8d\xe5\x20\xb0\x8d\xe2\xe4\xd0\x4d\xe2\x02\x20\x8f\xe0\x3c\x10\x91\xe5\x00\x01\x0b\xe5\x98\x46\x9f\xe5\x03\x30\x92\xe7\x00\x00\x51\xe3\x04\x40\x8f",
        "\x10\x40\x2d\xe9\x00\x40\xa0\xe1\x1e\x10\xa0\xe3\x0c\x20\x8f\xe2\x47\x9c\xe2\xeb\x04\x00\xa0\xe1\x10\x80\xbd\xe8\x00\xf0\x20\xe3\x2f\x75\x73\x72\x2f\x6c\x6f\x63\x61\x6c\x2f\x62\x69\x6e\x2f\x73\x74\x65\x6d\x73\x2d\x70\x72\x6f\x63\x65\x73\x73\x6f\x72\x00", 63, EDIT_STEMS},
    {0x8bbaa4u, "\x8c\xc7\x9f\xe5", "\x1e\xff\x2f\xe1", 4, EDIT_STEMS},
    {0x159f4a8u, "\xe4\x20\x9f\xe5\xdc\x30\x9f\xe5", (const char *)config_jump, 8, EDIT_STEMS},
    {0x1f69520u, "aac", "mp3", 4, EDIT_ONE},
    {0x15b6fb0u, "\x24\x30\xc4\xe5", "\x00\xf0\x20\xe3", 4, EDIT_ONE},
    {0x89cd64u, "\x38\x33\x1b\xe5\x00\x00\x54\xe3", (const char *)stems_feature_jump, 8, EDIT_ONE},
    {0x72c284u, "\x07\x00\x00\x0a", "\x07\x00\x00\xea", 4, EDIT_ONE},
    {0x1a8dbc0u, "\x00\x60\xa0\xe3", "\x04\x60\xa0\xe3", 4, EDIT_ONE},
    {0x1a8db1cu, "\x00\x40\xa0\x03", "\x01\x40\xa0\x03", 4, EDIT_ONE},
    {0x16d2208u, "\xfa\x01\x00\x1a", "\x00\xf0\x20\xe3", 4, EDIT_ONE},
    {0x31a880u, "\x4c\x51\x52\x57\x5b\x13\x05\x04\x4d\x5d\x47\x01\x44\x58\x56\x52\x58\x1b\x55\x58\x55\x16\x4c\x0a", "\x4c\x51\x52\x57\x12\x06\x05\x1a\x1e\x1a\x00\x1f\x1e\x01\x1c\x02\x0e\x0d\x06\x0f\x08\x16\x4c\x0a", 24, EDIT_GATEWAY},
    {0x31a898u, "\x3f\x2c\x2d\x2a\x28\x66\x72\x71\x3e\x15\x15\x0a\x4d\x10\x0c\x02\x06\x04\x47\x09\x04\x01\x42\x18", "\x3f\x2c\x2d\x2a\x61\x73\x72\x6f\x6d\x57\x4f\x52\x4d\x54\x4b\x57\x5d\x50\x59\x52\x5b\x43\x0c\x1b", 24, EDIT_GATEWAY},
    {0x31a65cu, "\x5e\x3f\x05\xe3", "\x40\x3f\x05\xe3", 4, EDIT_GATEWAY},
    {0x5d5418u, "\x38\x32\xd0\xe5", "\x00\x30\xa0\xe3", 4, EDIT_ONE},
    {0x16d3e3cu, "\x1c\x00\xd4\xe5", "\x00\x00\xa0\xe3", 4, EDIT_ONE},
    {0x16d3da0u, "\x01\x00\x24\xe2", "\x00\x00\xa0\xe3", 4, EDIT_ONE},
};

static const struct engine_build engine_builds[] = {
    {"5.0.4", "\xaf\x07\x38\x65\xa3\x31\x8d\x80\x7a\x83\x03\xf7\x16\x15\x2e\xac\x92\x3a\x4c\x6f",
     {0xd917b8, 0xd917e4, 0xd91894, 0xd918c0, 0xd918ec, 0xd91810, 0xd91868, 0x14fff54, 0x153312c,
      0x1532fa0, 0x1500950, 0xd0b5bc, 0xc5b2e0, 0xc5b30c, 0xc5b338, 0xe3a598, 0x15fa188, 0xe3d0a8, 0xe3d908, 0xe3a118, 0x1462a6c},
     edits_504, sizeof edits_504 / sizeof *edits_504,
     0xf6303c, 0xf63124, 0xf63120, {0xdb, 0x24c, 0x24e, 0x276, 0x289},
     0x842644, 0x29bc83c, 0x258e185, 0x8429b0, 0xe51b3340,
     0x29980a4, 0x1c91f74, 0x1cba858, 0x1cba874, 0,
     {"\xf4\x42\x6d\xe1\x02\x50\xa0\xe1", "\x04\xb0\x2d\xe5\x00\x30\xa0\xe3", "\x04\xb0\x2d\xe5\x00\xb0\x8d\xe2"}},
    {"5.3.0", "\xbe\x53\x7e\x90\x81\x21\xe7\x61\x64\xdd\xbb\xf9\x73\x0d\x47\x2b\xa0\xb8\xf9\xd4",
     {0xe6a090, 0xe6a0bc, 0xe6a16c, 0xe6a198, 0xe6a1c4, 0xe6a0e8, 0xe6a140, 0x170e714, 0x1710c00,
      0x1710a74, 0x170f110, 0xdde87c, 0xd265cc, 0xd265f8, 0xd26624, 0xf1afa4, 0x16b38e0, 0xf1dd20, 0xf1ede4, 0xf1ab24, 0xc77934},
     edits_530, sizeof edits_530 / sizeof *edits_530,
     0x1537148, 0x1537234, 0x1537230, {0xe3, 0x254, 0x256, 0x2a6, 0x2b9},
     0x8a1eac, 0x2c8e808, 0x21b8b98, 0x8a2218, 0xe51b3340,
     0x2b9bee8, 0x1d7289c, 0x1d47324, 0x1d7e2bc, 0x1d46a6c,
     {"\xf4\x42\x6d\xe1\xf8\x60\xcd\xe1", "\xf4\x42\x6d\xe1\x20\xe0\x8d\xe5", ""}},
    {"5.3.0-sclive4", "\xb9\x46\xc3\xb8\xd9\x9d\x32\x99\x8c\x81\x97\xd5\xdc\x90\x5a\x6b\xdd\xaa\x5d\x2f",
     {0xe774fc, 0xe77528, 0xe775d8, 0xe77604, 0xe77630, 0xe77554, 0xe775ac, 0x17597bc, 0x179b090,
      0x179af04, 0x175a1b8, 0xdeacec, 0xd31818, 0xd31844, 0xd31870, 0xf27698, 0x172e668, 0xf2a410, 0xf2b9b0, 0xf27218, 0xc7cb2c},
     edits_sclive, sizeof edits_sclive / sizeof *edits_sclive,
     0x159f4a8, 0x159f594, 0x159f590, {0xe3, 0x254, 0x256, 0x2a6, 0x2b9},
     0x89cd64, 0x2c28744, 0x222cc8d, 0x89d0d0, 0xe51b3338,
     0x2c1bea4, 0x1ddf11c, 0x1db3ba4, 0x1deab3c, 0x1db32ec,
     {"\xf4\x42\x6d\xe1\xf8\x60\xcd\xe1", "\xf4\x42\x6d\xe1\x20\xe0\x8d\xe5", ""}},
};

static void select_build(const struct engine_build *b) {
    build = b;
    for (unsigned i = 0; i < DSP_HOOK_COUNT; i++) dsp_hooks[i].offset = b->dsp[i];
}

static int engine_base(struct dl_phdr_info *info, size_t size, void *out) {
    (void)size;
    for (size_t i = 0; i < info->dlpi_phnum; i++) {
        const ElfW(Phdr) *p = &info->dlpi_phdr[i];
        if (p->p_type != PT_NOTE) continue;
        const uchar *at = (const uchar *)(info->dlpi_addr + p->p_vaddr), *end = at + p->p_memsz;
        while ((size_t)(end - at) >= sizeof(ElfW(Nhdr))) {
            const ElfW(Nhdr) *n = (const void *)at;
            size_t names = ((size_t)n->n_namesz + 3) & ~(size_t)3;
            size_t desc = ((size_t)n->n_descsz + 3) & ~(size_t)3;
            at += sizeof *n;
            if (names > (size_t)(end - at) || desc > (size_t)(end - at) - names) break;
            if (n->n_type == NT_GNU_BUILD_ID && n->n_namesz == 4 && n->n_descsz == 20 && !memcmp(at, "GNU", 4))
                for (size_t b = 0; b < sizeof engine_builds / sizeof *engine_builds; b++)
                    if (!memcmp(at + names, engine_builds[b].id, 20)) {
                        select_build(&engine_builds[b]);
                        *(uintptr_t *)out = info->dlpi_addr;
                    }
            at += names + desc;
        }
    }
    return 1; // the first object is the program itself
}

static int edit_state(uintptr_t base, const struct engine_edit *e) {
    const void *at = (const void *)(base + e->offset);
    if (!memcmp(at, e->replacement, e->size)) return 2;
    if (!memcmp(at, e->expected, e->size)) return 1;
    // Upgrade the previous processor-only getConfig branch without mixing code paths.
    if (build == &engine_builds[0] && e->offset == 0xf6303c && !memcmp(at, "\xef\xef\xe3\xea\xd8\x30\x9f\xe5", 8)) return 1;
    return 0;
}

static void install_sqlite_hooks(uintptr_t base, size_t page);

int g_hook_initialized = 0;

__attribute__((constructor)) static void edit_engine(void) {
#ifdef ENGINE_UI_HOOK_TEST
    g_hook_initialized = 1;
#else
    const char *preload = getenv("LD_PRELOAD");
    if (preload && strstr(preload, "libengine-ui-hook.so")) {
        g_hook_initialized = 1;
    }
#endif
    if (!is_engine()) return;
    if (!g_hook_initialized) return;
    initialize_engine_cpus();
    uintptr_t base = 0;
    dl_iterate_phdr(engine_base, &base);
    if (!base) return;
    long page_size = sysconf(_SC_PAGESIZE);
    if (page_size <= 0) return;
    uintptr_t page = (uintptr_t)page_size;
    fprintf(stderr, "engine-ui-hook: Engine %s\n", build->name);
    prepare_config(base, page);
    uint32_t expected_feature[2] = {build->feature_displaced ? build->feature_displaced : 0xe51b3340, 0xe3540000};
    if (build->feature_true && !memcmp((const void *)(base + build->feature_site), expected_feature, 8))
        prepare_stems_feature(base, page);
    // A group is applied only if every site of it is stock or already patched.
    int ready[3] = {1, 1, 1};
    for (unsigned i = 0; i < build->edit_count; i++)
        if (!edit_state(base, &build->edits[i])) ready[build->edits[i].group] = 0;
    for (unsigned i = 0; i < build->edit_count; i++) {
        const struct engine_edit *e = &build->edits[i];
        if (e->group != EDIT_ONE && !ready[e->group]) continue;
        // An unprepared trampoline is all zero: never write it.
        if (e->replacement == (const char *)stems_feature_jump && !stems_feature_jump[1]) continue;
        char *at = (char *)(base + e->offset);
        if (edit_state(base, e) != 1) continue;
        uintptr_t first = (uintptr_t)at - (uintptr_t)at % page;
        if (e->size > UINTPTR_MAX - (uintptr_t)at) continue;
        uintptr_t end = (uintptr_t)at + e->size;
        uintptr_t tail = end % page;
        if (tail && page - tail > UINTPTR_MAX - end) continue;
        if (tail) end += page - tail;
        char *start = (char *)first;
        size_t span = (size_t)(end - first);
        if (mprotect(start, span, PROT_READ | PROT_WRITE)) {
            // A partial Stems trampoline set must never reach Engine startup.
            _exit(127);
        }
        memcpy(at, e->replacement, e->size);
        if (mprotect(start, span, PROT_READ | PROT_EXEC)) {
            // Do not execute Engine code with an incomplete protection transition.
            static const char error[] = "engine-ui-hook: restoring RX protection failed\n";
            write(STDERR_FILENO, error, sizeof error - 1);
            _exit(127);
        }
        __builtin___clear_cache(at, at + e->size);
        fprintf(stderr, "engine-ui-hook: applied 0x%x (%u bytes)\n", e->offset, e->size);
    }
    install_sqlite_hooks(base, page);
    if (install_dsp_hooks(base, page)) lim_start_poll();
}

// Unknown or unsafe values fall back to the English pack.
static void pack_dir(char *out, size_t size) {
    char lang[16] = "en";
    FILE *f = fopen(UI_ROOT "/settings.conf", "r");
    if (f) {
        char line[64];
        while (fgets(line, sizeof line, f)) {
            if (strncmp(line, "lang=", 5))
                continue;
            line[strcspn(line, "\r\n")] = 0;
            if (!strcmp(line + 5, "zh-Hans") || !strcmp(line + 5, "zh-Hant"))
                strcpy(lang, line + 5);
        }
        fclose(f);
    }
    snprintf(out, size, UI_ROOT "/packs/%s", lang);
}

// Returns {be32 size, bytes} as rcc lays out an uncompressed file, or NULL.
static uchar *load_blob(const char *path) {
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0)
        return NULL;
    struct stat st;
    uchar *blob = NULL;
    if (fstat(fd, &st) == 0 && S_ISREG(st.st_mode) && st.st_size > 0 && st.st_size < 16 * 1024 * 1024 &&
        (blob = malloc((size_t)st.st_size + 4))) {
        put32(blob, (uint32_t)st.st_size);
        size_t got = 0;
        while (got < (size_t)st.st_size) {
            ssize_t n = read(fd, blob + 4 + got, (size_t)st.st_size - got);
            if (n <= 0) {
                free(blob);
                blob = NULL;
                break;
            }
            got += (size_t)n;
        }
    }
    close(fd);
    return blob;
}

// Node name in ASCII, or 0 for a name a pack could not use.
static int node_name(const struct walk *w, uint32_t offset, char *out, size_t size) {
    const uchar *p = w->names + offset;
    size_t len = be16(p);
    if (len == 0 || len + 1 > size)
        return 0;
    for (size_t i = 0; i < len; i++) {
        if (p[6 + i * 2] || p[7 + i * 2] < 0x20 || p[7 + i * 2] > 0x7e || p[7 + i * 2] == '/')
            return 0;
        out[i] = (char)p[7 + i * 2];
    }
    out[len] = 0;
    return 1;
}

// Engine 5.3.0 forces Qt's QML disk cache (QML_FORCE_DISK_CACHE beats QML_DISABLE_DISK_CACHE)
// into /data, and Qt reuses a .qmlc while the source timestamp is unchanged: a cache compiled
// from stock QML, an older pack or the other language would win over the pack forever.
// A timestamp from the content makes Qt recompile exactly when the text changes.
static uint64_t content_stamp(const uchar *p, size_t n) {
    uint64_t h = 0xcbf29ce484222325ULL; // FNV-1a
    while (n--) h = (h ^ *p++) * 0x100000001b3ULL;
    return 1577836800000ULL + h % 2524608000000ULL; // ms in 2020..2100, a valid QDateTime
}

static uint32_t max_node(const struct walk *w, uint32_t node) {
    const uchar *e = w->tree + node * w->node_size;
    uint32_t top = node;
    if (be16(e + 4) & FLAG_DIRECTORY) {
        uint32_t count = be32(e + 6), first = be32(e + 10);
        for (uint32_t i = 0; i < count; i++) {
            uint32_t m = max_node(w, first + i);
            if (m > top)
                top = m;
        }
    }
    return top;
}

static void apply(struct walk *w, uint32_t node, const char *prefix, int depth) {
    uchar *e = w->tree + node * w->node_size;
    uint16_t flags = be16(e + 4);
    char name[128], path[512];
    if (node == 0) {
        path[0] = 0;
    } else {
        if (!node_name(w, be32(e), name, sizeof name))
            return;
        if (snprintf(path, sizeof path, "%s/%s", prefix, name) >= (int)sizeof path)
            return;
    }
    if (flags & FLAG_DIRECTORY) {
        if (depth > 32)
            return;
        uint32_t count = be32(e + 6), first = be32(e + 10);
        for (uint32_t i = 0; i < count; i++)
            apply(w, first + i, path, depth + 1);
        return;
    }
    char file[700];
    snprintf(file, sizeof file, "%s%s", w->pack, path);
    uchar *blob = load_blob(file);
    if (!blob)
        return;
    // Qt adds the offset to the data pointer. On this 32-bit target it wraps,
    // so the blob can live anywhere in the address space.
    put32(e + 10, (uint32_t)((uintptr_t)blob - (uintptr_t)w->data));
    e[4] = (uchar)((flags & ~(FLAG_COMPRESSED | FLAG_ZSTD)) >> 8);
    e[5] = (uchar)(flags & ~(FLAG_COMPRESSED | FLAG_ZSTD));
    if (w->node_size >= 22) { // rcc v2+: be64 last-modified ms at +14
        uint64_t stamp = content_stamp(blob + 4, be32(blob));
        put32(e + 14, (uint32_t)(stamp >> 32));
        put32(e + 18, (uint32_t)stamp);
    }
    w->replaced++;
}

int _Z21qRegisterResourceDataiPKhS0_S0_(int version, const uchar *tree, const uchar *names, const uchar *data) {
    static void *cache;
    static int active = -1;
    register_fn real = next_symbol(&cache, "_Z21qRegisterResourceDataiPKhS0_S0_");
    if (!real)
        return 0;
    if (active < 0)
        active = is_engine() && access(UI_ROOT "/safe-mode", F_OK) != 0;
    if (!active || version < 1 || version > 3 || sizeof(void *) != 4)
        return real(version, tree, names, data);

    struct walk w = {.tree = (uchar *)tree, .names = names, .data = data, .node_size = version >= 2 ? 22 : 14};
    pack_dir(w.pack, sizeof w.pack);
    size_t bytes = (size_t)(max_node(&w, 0) + 1) * w.node_size;
    uchar *copy = malloc(bytes);
    if (!copy)
        return real(version, tree, names, data);
    memcpy(copy, tree, bytes);
    w.tree = copy;
    apply(&w, 0, "", 0);
    if (!w.replaced) {
        free(copy);
        return real(version, tree, names, data);
    }
    fprintf(stderr, "engine-ui-hook: %s: %d files\n", w.pack, w.replaced);
    return real(version, copy, names, data);
}

static int thread_class(const char *name) {
    static const char *idle[] = {"Analysis Queue", "OfflineAnalyzer", "Dropbox", "Beatport",
        "J:I:Takingfreez", "Taking freeze", "Freeze Checker", "Collecting anal", "sentry", "CrashHandler"};
    for (size_t i = 0; i < sizeof idle / sizeof *idle; i++)
        if (!strncmp(name, idle[i], strlen(idle[i]))) return 2;
    return !strncmp(name, "QQuickPixmapRea", 15) || !strncmp(name, "FileInfoThread", 14);
}

static int guard_affinity(pthread_t thread, int kind) {
    static void *get_cache, *set_cache;
    int (*get)(pthread_t, size_t, cpu_set_t *) = next_symbol(&get_cache, "pthread_getaffinity_np");
    int (*set)(pthread_t, size_t, const cpu_set_t *) = next_symbol(&set_cache, "pthread_setaffinity_np");
    if (!get || !set) return ENOSYS;
    cpu_set_t allowed;
    if (engine_cpus_ready) allowed = engine_cpus;
    else {
        int rc = get(thread, sizeof allowed, &allowed);
        if (rc) return rc;
    }
    CPU_CLR(0, &allowed);
    if (!CPU_COUNT(&allowed)) return EINVAL; // A single-core target cannot avoid CPU0.
    if (kind == 2) {
        int last = CPU_SETSIZE - 1;
        while (!CPU_ISSET(last, &allowed)) last--;
        CPU_ZERO(&allowed);
        CPU_SET(last, &allowed);
    }
    return set(thread, sizeof allowed, &allowed);
}

static void lower_io_nice(pid_t tid, int nice, int idle_io) {
    setpriority(PRIO_PROCESS, (id_t)tid, nice);
    if (idle_io) syscall(SYS_ioprio_set, 1, tid, 3 << 13);
}

static void guard_named_thread(pthread_t thread, const char *name) {
    if (!is_engine()) return;
    int kind = thread_class(name);
    if (!kind) return;
    int saved_errno = errno;
    struct sched_param param = {.sched_priority = 0};
    if (kind == 2) pthread_setschedparam(thread, SCHED_IDLE, &param);
    guard_affinity(thread, kind);
    if (pthread_equal(thread, pthread_self())) {
        lower_io_nice((pid_t)syscall(SYS_gettid), kind == 2 ? 19 : 10, kind == 2);
    } else {
        // Naming another pthread must not lower the caller (often Engine's main thread).
        // Equal kernel names have the same policy, so duplicate worker names are safe.
        DIR *tasks = opendir("/proc/self/task");
        struct dirent *entry;
        while (tasks && (entry = readdir(tasks))) {
            char *end;
            long tid = strtol(entry->d_name, &end, 10);
            if (*end || tid <= 0) continue;
            char path[80], comm[32];
            snprintf(path, sizeof path, "/proc/self/task/%ld/comm", tid);
            FILE *f = fopen(path, "r");
            if (!f) continue;
            int match = fgets(comm, sizeof comm, f) &&
                (comm[strcspn(comm, "\n")] = 0, !strncmp(comm, name, 15));
            fclose(f);
            if (match) lower_io_nice((pid_t)tid, kind == 2 ? 19 : 10, kind == 2);
        }
        if (tasks) closedir(tasks);
    }
    errno = saved_errno;
}

// #55: Engine analyses in OfflineAnalyzer child processes, which inherit this
// preload. Analysing a whole drive in the background must never compete with
// playback: idle CPU class, nice 19 and idle I/O, off CPU 0; threads inherit all.
__attribute__((constructor)) static void lower_offline_analyzer(void) {
    char exe[64];
    ssize_t n = readlink("/proc/self/exe", exe, sizeof exe - 1);
    if (n <= 0) return;
    exe[n] = 0;
    const char *name = strrchr(exe, '/');
    if (strcmp(name ? name + 1 : exe, "OfflineAnalyzer")) return;
    struct sched_param param = {.sched_priority = 0};
    sched_setscheduler(0, SCHED_IDLE, &param);
    lower_io_nice(0, 19, 1);
    cpu_set_t cpus;
    if (!sched_getaffinity(0, sizeof cpus, &cpus) && CPU_ISSET(0, &cpus) && CPU_COUNT(&cpus) > 1) {
        CPU_CLR(0, &cpus);
        sched_setaffinity(0, sizeof cpus, &cpus);
    }
}

// Engine itself raises its analyzers to nice -5 / SCHED_OTHER once they connect
// (seen on 5.3.0): keep them idle (#55).
static int offline_analyzer(pid_t pid) {
    char path[48], comm[32] = "";
    if (pid <= 0) return 0;
    snprintf(path, sizeof path, "/proc/%d/comm", (int)pid);
    FILE *f = fopen(path, "r");
    if (!f) return 0;
    int match = fgets(comm, sizeof comm, f) && !strncmp(comm, "OfflineAnalyzer", 15);
    fclose(f);
    return match;
}

int setpriority(__priority_which_t which, id_t who, int prio) {
    static void *cache;
    int (*real)(__priority_which_t, id_t, int) = next_symbol(&cache, "setpriority");
    if (!real) { errno = ENOSYS; return -1; }
    if (which == PRIO_PROCESS && is_engine() && offline_analyzer((pid_t)who)) prio = 19;
    return real(which, who, prio);
}

int sched_setscheduler(pid_t pid, int policy, const struct sched_param *param) {
    static void *cache;
    int (*real)(pid_t, int, const struct sched_param *) = next_symbol(&cache, "sched_setscheduler");
    if (!real) { errno = ENOSYS; return -1; }
    static const struct sched_param idle = {.sched_priority = 0};
    if (is_engine() && offline_analyzer(pid)) policy = SCHED_IDLE, param = &idle;
    return real(pid, policy, param);
}

int pthread_setname_np(pthread_t thread, const char *name) {
    static void *cache;
    int (*real)(pthread_t, const char *) = next_symbol(&cache, "pthread_setname_np");
    if (!real) return ENOSYS;
    int rc = real(thread, name);
    if (!rc) guard_named_thread(thread, name);
    return rc;
}

static int named_prctl(void **cache, const char *symbol, int option, va_list ap) {
    int (*real)(int, ...) = next_symbol(cache, symbol);
    if (!real) { errno = ENOSYS; return -1; }
    // prctl's libc wrapper forwards four machine-word arguments to the kernel.
    unsigned long a2 = va_arg(ap, unsigned long), a3 = va_arg(ap, unsigned long);
    unsigned long a4 = va_arg(ap, unsigned long), a5 = va_arg(ap, unsigned long);
    int rc = real(option, a2, a3, a4, a5);
    if (!rc && option == PR_SET_NAME) guard_named_thread(pthread_self(), (const char *)a2);
    return rc;
}

int hook_prctl(int, ...) __asm__("prctl");
int hook_prctl_time64(int, ...) __asm__("__prctl_time64");
#define PRCTL_WRAPPER(fn, symbol) \
int fn(int option, ...) { \
    static void *cache; va_list ap; va_start(ap, option); \
    int rc = named_prctl(&cache, symbol, option, ap); va_end(ap); return rc; \
}
PRCTL_WRAPPER(hook_prctl, "prctl")
PRCTL_WRAPPER(hook_prctl_time64, "__prctl_time64")

int pthread_setschedparam(pthread_t thread, int policy, const struct sched_param *param) {
    static void *cache;
    int (*real)(pthread_t, int, const struct sched_param *) = next_symbol(&cache, "pthread_setschedparam");
    if (!real) return ENOSYS;
    char name[16];
    struct sched_param idle = {.sched_priority = 0};
    if (is_engine() && !pthread_getname_np(thread, name, sizeof name) && thread_class(name) == 2) {
        policy = SCHED_IDLE;
        param = &idle;
    }
    return real(thread, policy, param);
}

static int analytics_db(const char *filename) {
    if (!filename) return 0;
    int uri = !strncmp(filename, "file:", 5);
    const char *start = filename + (uri ? 5 : 0), *end = start + strlen(start);
    if (uri) { const char *query = strchr(start, '?'); if (query) end = query; }
    const char *base = start;
    for (const char *p = start; p < end; p++) if (*p == '/') base = p + 1;
    size_t size = (size_t)(end - base);
    return (size == 10 && !memcmp(base, "posthog.db", 10)) ||
           (size == 9 && !memcmp(base, "events.db", 9));
}

typedef int (*sqlite_open_v2_fn)(const char *, void **, int, const char *);
typedef int (*sqlite_open_fn)(const char *, void **);
typedef int (*sqlite_exec_fn)(void *, const char *, int (*)(void *, int, char **, char **), void *, char **);
static sqlite_open_v2_fn sqlite_open_v2_original;
static sqlite_open_fn sqlite_open_original;
static sqlite_exec_fn sqlite_exec_original;

static void tune_database(int rc, const char *filename, void **db) {
    if (rc || !db || !*db || (filename && !strcmp(filename, ":memory:"))) return;
    sqlite_exec_original(*db, "PRAGMA synchronous = NORMAL; PRAGMA temp_store = MEMORY; PRAGMA cache_size = -8000;",
                         NULL, NULL, NULL);
}

static int __attribute__((used)) tuned_sqlite_open_v2(const char *filename, void **db, int flags, const char *vfs) {
    const char *target = analytics_db(filename) ? ":memory:" : filename;
    int rc = sqlite_open_v2_original(target, db, flags, vfs);
    tune_database(rc, target, db);
    return rc;
}

static int __attribute__((used)) tuned_sqlite_open(const char *filename, void **db) {
    const char *target = analytics_db(filename) ? ":memory:" : filename;
    int rc = sqlite_open_original(target, db);
    tune_database(rc, target, db);
    return rc;
}

// Target of an unconditional ARM B at "at", or 0.
static uintptr_t arm_branch_target(uintptr_t at) {
    uint32_t insn = *(const uint32_t *)at;
    if ((insn & 0xff000000u) != 0xea000000u) return 0;
    int32_t offset = (int32_t)(insn << 8) >> 6;
    return at + 8 + (uintptr_t)offset;
}

static void *function_trampoline(uintptr_t entry, size_t page) {
    uint32_t code[4];
    memcpy(code, (const void *)entry, 8);
    code[2] = 0xe51ff004;
    code[3] = (uint32_t)(entry + 8);
    return rx_code(code, sizeof code, page);
}

static void redirect_function(uintptr_t entry, const void *handler, size_t page) {
    uint32_t jump[] = {0xe51ff004, (uint32_t)(uintptr_t)handler};
    uintptr_t first = entry - entry % page;
    size_t span = ((entry + sizeof jump + page - 1) / page * page) - first;
    if (mprotect((void *)first, span, PROT_READ | PROT_WRITE)) _exit(127);
    memcpy((void *)entry, jump, sizeof jump);
    if (mprotect((void *)first, span, PROT_READ | PROT_EXEC)) _exit(127);
    __builtin___clear_cache((char *)entry, (char *)entry + sizeof jump);
}

static void install_sqlite_hooks(uintptr_t base, size_t page) {
    // Identified through sqlite3_api_routines slots 55/70/135 and ARM disassembly.
    const struct engine_build *b = build;
    const uint32_t *api = (const void *)(base + b->sqlite_api);
    if (api[55] != base + b->sqlite_exec || api[70] != base + b->sqlite_open || api[135] != base + b->sqlite_open_v2) return;
    if (memcmp((const void *)(base + b->sqlite_exec), b->sqlite_prefix[0], 8)) return;
    sqlite_exec_original = (sqlite_exec_fn)(base + b->sqlite_exec);
    if (b->sqlite_opendb) {
        // open = "mov r3,#0; mov r2,#6; b opendb", open_v2 = "b opendb": opendb has
        // open_v2's signature and is the only way in, so hooking it covers both.
        const uint32_t *open = (const void *)(base + b->sqlite_open), *v2 = (const void *)(base + b->sqlite_open_v2);
        if (open[0] != 0xe3a03000 || open[1] != 0xe3a02006 || arm_branch_target((uintptr_t)&open[2]) != base + b->sqlite_opendb ||
            arm_branch_target((uintptr_t)v2) != base + b->sqlite_opendb ||
            memcmp((const void *)(base + b->sqlite_opendb), b->sqlite_prefix[1], 8)) return;
        sqlite_open_v2_original = (sqlite_open_v2_fn)function_trampoline(base + b->sqlite_opendb, page);
        redirect_function(base + b->sqlite_opendb, tuned_sqlite_open_v2, page);
    } else {
        if (memcmp((const void *)(base + b->sqlite_open), b->sqlite_prefix[1], 8) ||
            memcmp((const void *)(base + b->sqlite_open_v2), b->sqlite_prefix[2], 8)) return;
        sqlite_open_original = (sqlite_open_fn)function_trampoline(base + b->sqlite_open, page);
        sqlite_open_v2_original = (sqlite_open_v2_fn)function_trampoline(base + b->sqlite_open_v2, page);
        redirect_function(base + b->sqlite_open, tuned_sqlite_open, page);
        redirect_function(base + b->sqlite_open_v2, tuned_sqlite_open_v2, page);
    }
    fprintf(stderr, "engine-ui-hook: SQLite open/open_v2 hooks installed\n");
}

static const char *volatile_path(int dirfd, const char *path, char out[PATH_MAX]) {
    if (!path || !is_engine()) return path;
    char full[PATH_MAX];
    if (*path == '/') {
        if (strlen(path) >= sizeof full) { errno = ENAMETOOLONG; return NULL; }
        strcpy(full, path);
    } else {
        char dir[PATH_MAX];
        if (dirfd == AT_FDCWD) {
            if (!getcwd(dir, sizeof dir)) return path;
        } else {
            char fdpath[64];
            snprintf(fdpath, sizeof fdpath, "/proc/self/fd/%d", dirfd);
            ssize_t n = readlink(fdpath, dir, sizeof dir - 1);
            if (n < 0) return path;
            dir[n] = 0;
        }
        if (snprintf(full, sizeof full, "%s/%s", dir, path) >= (int)sizeof full) {
            errno = ENAMETOOLONG; return NULL;
        }
    }
    if (!strstr(full, "/analytics/posthog.db") && !strstr(full, "/analytics/events.db") &&
        !strstr(full, "/CrashReportDB/")) return path;
    if (strstr(full, "/../") || !strcmp(full + strlen(full) - 3, "/..")) {
        errno = EINVAL; return NULL;
    }
    char root[80];
    snprintf(root, sizeof root, "/tmp/sc5000-engine-volatile-%u", (unsigned)geteuid());
    if (!strncmp(full, root, strlen(root)) && full[strlen(root)] == '/') return path;
    if (mkdir(root, 0700) && errno != EEXIST) return NULL;
    struct stat st;
    if (lstat(root, &st) || !S_ISDIR(st.st_mode) || st.st_uid != geteuid() || (st.st_mode & 077)) {
        errno = EACCES; return NULL;
    }
    if (snprintf(out, PATH_MAX, "%s%s", root, full) >= PATH_MAX) {
        errno = ENAMETOOLONG; return NULL;
    }
    for (char *p = out + strlen(root) + 1; *p; p++) {
        if (*p != '/') continue;
        *p = 0;
        int failed = mkdir(out, 0700) && errno != EEXIST;
        *p = '/';
        if (failed) return NULL;
    }
    return out;
}

static int redirected_open(int dirfd, const char *path, int flags, mode_t mode) {
    static void *cache;
    int (*real)(int, const char *, int, ...) = next_symbol(&cache, "openat");
    char mapped[PATH_MAX];
    const char *target = volatile_path(dirfd, path, mapped);
    if (!target) return -1;
    if (target == mapped) dirfd = AT_FDCWD;
    return real ? real(dirfd, target, flags, mode) : (int)syscall(SYS_openat, dirfd, target, flags, mode);
}

#define OPEN_MODE(flags, ap) (((flags) & O_CREAT) || (((flags) & O_TMPFILE) == O_TMPFILE) ? va_arg(ap, mode_t) : 0)
#define OPEN_WRAPPER(fn, large) \
int fn(const char *path, int flags, ...) { \
    va_list ap; va_start(ap, flags); mode_t mode = OPEN_MODE(flags, ap); va_end(ap); \
    return redirected_open(AT_FDCWD, path, flags | (large), mode); \
}
#define OPENAT_WRAPPER(fn, large) \
int fn(int dirfd, const char *path, int flags, ...) { \
    va_list ap; va_start(ap, flags); mode_t mode = OPEN_MODE(flags, ap); va_end(ap); \
    return redirected_open(dirfd, path, flags | (large), mode); \
}
// Explicit assembler names also export both variants on Debian's default time64 ABI.
int hook_open(const char *, int, ...) __asm__("open");
int hook_open64(const char *, int, ...) __asm__("open64");
int hook_openat(int, const char *, int, ...) __asm__("openat");
int hook_openat64(int, const char *, int, ...) __asm__("openat64");
OPEN_WRAPPER(hook_open, 0)
OPEN_WRAPPER(hook_open64, O_LARGEFILE)
OPENAT_WRAPPER(hook_openat, 0)
OPENAT_WRAPPER(hook_openat64, O_LARGEFILE)

static int crashpad(const char *file) {
    const char *base = file ? strrchr(file, '/') : NULL;
    return file && !strcmp(base ? base + 1 : file, "crashpad_handler");
}

static void *access_cache;
static int raw_access(const char *path, int mode) {
    int (*real)(const char *, int) = next_symbol(&access_cache, "access");
    return real ? real(path, mode) : -1;
}

static const char *stems_processor_path(const char *path) {
    if (path && !strcmp(path, "/usr/local/bin/stems-processor")) {
        if (raw_access(path, X_OK) != 0) {
            if (raw_access("/data/sc5000-custom/bundle/stems-processor", X_OK) == 0)
                return "/data/sc5000-custom/bundle/stems-processor";
            if (raw_access("/data/sc5000-custom/bundle/bin/stems-processor", X_OK) == 0)
                return "/data/sc5000-custom/bundle/bin/stems-processor";
        }
    }
    return path;
}

int access(const char *path, int mode) {
    int (*real)(const char *, int) = next_symbol(&access_cache, "access");
    if (!real) { errno = ENOSYS; return -1; }
    if (is_engine()) path = stems_processor_path(path);
    return real(path, mode);
}

int posix_spawn(pid_t *pid, const char *path, const posix_spawn_file_actions_t *actions,
                const posix_spawnattr_t *attr, char *const argv[], char *const envp[]) {
    static void *cache;
    if (is_engine()) {
        if (crashpad(path)) return ENOENT; // Never return success with a fabricated PID.
        path = stems_processor_path(path);
    }
    int (*real)(pid_t *, const char *, const posix_spawn_file_actions_t *,
                const posix_spawnattr_t *, char *const [], char *const []) = next_symbol(&cache, "posix_spawn");
    return real ? real(pid, path, actions, attr, argv, envp) : ENOSYS;
}

int execve(const char *path, char *const argv[], char *const envp[]) {
    static void *cache;
    if (is_engine()) {
        if (crashpad(path)) { errno = ENOENT; return -1; }
        path = stems_processor_path(path);
    }
    int (*real)(const char *, char *const [], char *const []) = next_symbol(&cache, "execve");
    if (real) return real(path, argv, envp);
    errno = ENOSYS; return -1;
}

int execv(const char *path, char *const argv[]) {
    extern char **environ;
    return execve(path, argv, environ);
}

// Engine's RenderStemsJob sets thread affinity to CPU cores that exist on 6/8-core models
// (e.g. CPU 4/5), but on the 4-core SC5000 (cores 0..3) pthread_setaffinity_np fails with
// EINVAL, causing Engine to call QMessageLogger::fatal("Failed setting CPU Affinity") and abort.
int pthread_setaffinity_np(pthread_t thread, size_t cpusetsize, const cpu_set_t *cpuset) {
    static void *cache;
    int (*real_fn)(pthread_t, size_t, const cpu_set_t *) = next_symbol(&cache, "pthread_setaffinity_np");
    if (!real_fn) return ENOSYS;
    int res = real_fn(thread, cpusetsize, cpuset);
    if (!is_engine()) return res;
    if (res == EINVAL) {
        cpu_set_t fallback;
        if (engine_cpus_ready) fallback = engine_cpus;
        else if (sched_getaffinity(0, sizeof fallback, &fallback)) return res;
        res = real_fn(thread, sizeof fallback, &fallback);
    }
    char name[16];
    if (!res && !pthread_getname_np(thread, name, sizeof name)) {
        int kind = thread_class(name);
        if (kind) res = guard_affinity(thread, kind);
    }
    return res;
}

int pthread_getaffinity_np(pthread_t thread, size_t cpusetsize, cpu_set_t *cpuset) {
    static void *cache;
    int (*real_fn)(pthread_t, size_t, cpu_set_t *) = next_symbol(&cache, "pthread_getaffinity_np");
    if (!real_fn) return ENOSYS;
    int res = real_fn(thread, cpusetsize, cpuset);
    return res;
}

// #61 / #67: One-shot ALSA boot loader for SC LIVE 4 (inmusic,jp21) and devices
// with read-only root filesystems where LD_PRELOAD cannot be placed in rootfs.
__attribute__((visibility("default")))
int sc5000_alsa_boot_hook(void *root, void *config, void **dst, void *private_data) {
    (void)root;
    (void)config;
    (void)private_data;
    if (dst) *dst = NULL;
    if (g_hook_initialized) return 0;
    if (!is_engine()) return 0;
    if (access("/run/sc5000-alsa-reexec.once", F_OK) == 0) return 0;

    int fd = open("/run/sc5000-alsa-reexec.once", O_CREAT | O_WRONLY | O_TRUNC, 0644);
    if (fd >= 0) close(fd);

    fprintf(stderr, "engine-ui-hook: ALSA one-shot boot hook triggered; activating EngineX OS\n");
    pid_t pid = fork();
    if (pid == 0) {
        setsid();
        int devnull = open("/dev/null", O_RDWR);
        if (devnull >= 0) {
            dup2(devnull, 0);
            dup2(devnull, 1);
            dup2(devnull, 2);
            if (devnull > 2) close(devnull);
        }
        char *const argv[] = {"/bin/sh", "/data/sc5000-custom/bundle/boot-activate.sh", NULL};
        execv("/bin/sh", argv);
        _exit(1);
    }
    return 0;
}

__attribute__((visibility("default")))
int snd_config_hook_sc5000_alsa_boot_hook(void *root, void *config, void **dst, void *private_data) {
    return sc5000_alsa_boot_hook(root, config, dst, private_data);
}

__attribute__((used, visibility("default")))
const char _sc5000_alsa_boot_hook_dlsym_config_hook_001[] = "ALSA_0.9";
__attribute__((used, visibility("default")))
const char _snd_config_hook_sc5000_alsa_boot_hook_dlsym_config_hook_001[] = "ALSA_0.9";

