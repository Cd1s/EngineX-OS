/* #207, sandbox only, never shipped or preloaded on a deck: drives Engine's own Reverb
 * through the FX unit's per-block process function, offline (the sandbox has no audio
 * device), and measures its tail with the hook's decay patch in place and with the
 * original two instructions put back.
 *
 * Engine 5.3.0 armhf, PRIME 4 (JC11) only: every address below belongs to that build.
 * The unit's handlers are found as in test_fx_selector_native.c (its operator new). */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <fcntl.h>
#include <link.h>
#include <math.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>
#include "reverb_time.h"

enum { UNIT_PROCESS = 0xe90218, UNIT_VTABLE = 0x2b21eec, MIX_VTABLE = 0x2b411d4, MIX_TARGET = 0xe9437c,
       TURN_OFF = 0xe6a090, TURN_ON = 0xe6a0bc, MIX = 0xe6a0e8, PARAMETER = 0xe6a140, CHANGE = 0xe6a198,
       REVERB_SITE = 0x1725630, BLOCK = 128 };

static void *allocs[262144];
static unsigned count;
static uintptr_t base;
void *_Znwj(size_t n) {
    void *p = malloc(n);
    if (n == 24) {
        unsigned i = __atomic_fetch_add(&count, 1, __ATOMIC_RELAXED);
        if (i < 262144) __atomic_store_n(&allocs[i], p, __ATOMIC_RELEASE);
    }
    return p;
}
static int main_base(struct dl_phdr_info *info, size_t n, void *arg) {
    (void)n; (void)arg; base = info->dlpi_addr; return 1;
}

typedef void (*event_fn)(void *, const void *);
typedef void (*unit_fn)(void *, int, float **, int, float **, int);
static char *unit;
static struct { void *object; uintptr_t dispatcher; } handlers[32];
static unsigned handler_count;
static int failures;
static double rate_hz = 44100;
#define CHECK(ok, ...) do { int pass_ = (ok); if (!pass_) ++failures; \
    fprintf(stderr, "FXREVERB %s|", pass_ ? "PASS" : "FAIL"); fprintf(stderr, __VA_ARGS__); fputc('\n', stderr); } while (0)

static void *effect_named(char *owner, const char *want) {
    char *col = *(char **)(owner + 0x28);
    char **begin = *(char ***)(col + 4), **end = *(char ***)(col + 8);
    if (!begin || end < begin || end - begin > 128) return NULL;
    for (; begin < end; ++begin)
        if (*begin && *(unsigned *)(*begin + 0x2c) == strlen(want) && !memcmp(*(char **)(*begin + 0x28), want, strlen(want))) return *begin;
    return NULL;
}
/* The FX unit whose collection holds Reverb, and its handlers by dispatcher. */
static int find_unit(void) {
    unsigned limit = __atomic_load_n(&count, __ATOMIC_ACQUIRE);
    if (limit > 262144) limit = 262144;
    int fd = open("/proc/self/mem", O_RDONLY);
    for (unsigned i = 0; i < limit && !unit; ++i) {
        uint32_t w[6], vt;
        if (pread64(fd, w, sizeof w, (off64_t)(uintptr_t)allocs[i]) != sizeof w || w[0] != base + MIX_VTABLE ||
            w[4] != base + MIX_TARGET || w[5] || pread64(fd, &vt, 4, w[3]) != 4 || vt != base + UNIT_VTABLE) continue;
        if (effect_named((char *)(uintptr_t)w[3], "Reverb")) unit = (char *)(uintptr_t)w[3];
    }
    for (unsigned i = 0; unit && i < limit && handler_count < 32; ++i) {
        uint32_t w[6], slots[3];
        if (pread64(fd, w, sizeof w, (off64_t)(uintptr_t)allocs[i]) != sizeof w || w[3] != (uintptr_t)unit ||
            w[0] < base + 0x2b00000 || w[0] > base + 0x2ba0000 || pread64(fd, slots, sizeof slots, w[0]) != sizeof slots) continue;
        handlers[handler_count].object = allocs[i];
        handlers[handler_count++].dispatcher = slots[2] - base;
    }
    close(fd);
    return unit != NULL;
}
static void send(uintptr_t dispatcher, const char *name, float value) {
    unsigned char e[64] = {0};
    unsigned length = (unsigned)strlen(name);
    memcpy(e + 8, &name, 4);
    if (dispatcher == MIX) memcpy(e + 0x0c, &value, 4); else memcpy(e + 12, &length, 4);
    memcpy(e + 0x20, &value, 4);
    for (unsigned i = 0; i < handler_count; ++i)
        if (handlers[i].dispatcher == dispatcher) { ((event_fn)(base + dispatcher))(handlers[i].object, e); return; }
    CHECK(0, "no handler for dispatcher %#lx", (unsigned long)dispatcher);
}

static unsigned lcg = 12345;
static float noise(void) { lcg = lcg * 1664525u + 1013904223u; return 0.25f * (float)((int)(lcg >> 8) % 65536 - 32768) / 65536.0f; }
struct level { double peak, rms; int finite; };
/* `seconds` of audio through the unit: noise when `loud`, else silence. */
static struct level run(double seconds, int loud) {
    struct level v = {0, 0, 1};
    double sum = 0; unsigned n = 0, total = (unsigned)(seconds * rate_hz);
    for (; n < total; n += BLOCK) {
        float il[BLOCK], ir[BLOCK], ol[BLOCK], orr[BLOCK]; float *in[2] = {il, ir}, *out[2] = {ol, orr};
        for (unsigned i = 0; i < BLOCK; ++i) il[i] = ir[i] = loud ? noise() : 0.0f;
        ((unit_fn)(base + UNIT_PROCESS))(unit, 2, in, 2, out, BLOCK);
        for (unsigned i = 0; i < BLOCK; ++i) {
            if (!isfinite(ol[i]) || !isfinite(orr[i])) v.finite = 0;
            if (fabs(ol[i]) > v.peak) v.peak = fabs(ol[i]);
            sum += (double)ol[i] * ol[i];
        }
    }
    v.rms = sqrt(sum / (n ? n : 1));
    return v;
}
static char *current_effect(void) { return *(char **)(*(char **)(unit + 0x290) + 0x18); }
static int bypassed(void) { int *p = *(int **)(unit + 0x290); return p[1] == 0x14 && p[2] == 1; }
static int select_effect(const char *name) {
    for (int tries = 0; tries < 12; ++tries) { /* the switch waits for its fades; Engine's UI may also set a type at start */
        char *e = current_effect();
        if (*(unsigned *)(e + 0x2c) == strlen(name) && !memcmp(*(char **)(e + 0x28), name, strlen(name)) && tries) return 1;
        send(CHANGE, name, 0);
        run(0.5, 0);
    }
    return 0;
}
static void turn_on(const char *name) { send(TURN_ON, name, 0); send(MIX, name, 1.0f); run(0.25, 0); }
/* Off, then on again: what is still ringing must not come back. */
static void flush(const char *name) { send(TURN_OFF, name, 0); run(2.0, 0); turn_on(name); }

/* Half a second of noise, then silence: seconds until the output stays 60 dB below the burst. */
static double rt60(const char *name, float knob, double seconds, struct level *whole) {
    flush(name);
    send(PARAMETER, name, knob);
    struct level burst = run(0.5, 1), all = burst;
    double last = 0;
    for (double t = 0; t < seconds; t += 0.05) {
        struct level w = run(0.05, 0);
        if (w.rms > burst.rms * 0.001) last = t + 0.05;
        if (w.peak > all.peak) all.peak = w.peak;
        all.finite &= w.finite;
    }
    if (whole) *whole = all;
    return last;
}

static uint32_t patched[2];
static const uint32_t stock[2] = {0xee377a67, 0xed9f8a5f};
static void write_site(const uint32_t words[2]) {
    uintptr_t at = base + REVERB_SITE;
    mprotect((void *)(at & ~4095ul), 4096, PROT_READ | PROT_WRITE | PROT_EXEC);
    memcpy((void *)at, words, 8);
    mprotect((void *)(at & ~4095ul), 4096, PROT_READ | PROT_EXEC);
    __builtin___clear_cache((char *)at, (char *)at + 8);
}

static void *test(void *unused) {
    (void)unused;
    sleep(12);
    if (!find_unit()) { CHECK(0, "the FX unit that owns Reverb"); return NULL; }
    memcpy(patched, (void *)(base + REVERB_SITE), 8);
    CHECK(patched[0] == 0xe51ff004, "the hook's jump is at the formula (%#x)", patched[0]);
    unit[0x2ad] = 1; /* the unit is assigned to a channel */
    CHECK(select_effect("Reverb"), "Reverb selected");
    char *reverb = current_effect(), *dsp = *(char **)(reverb + 0x68), *info = *(char **)(reverb + 0x64);
    turn_on("Reverb");
    CHECK(!bypassed() && ((int *)dsp)[0xe] == 0 && ((int *)dsp)[0xf] == 0, "Reverb is on and is the Classic 1 type");

    /* 1. With the original instructions: the stock decay at the centre, the knee and the top. */
    write_site(stock);
    double s50 = rt60("Reverb", 0.5f, 6, NULL), s82 = rt60("Reverb", SC_REVERB_KNEE, 10, NULL), s100 = rt60("Reverb", 1.0f, 15, NULL);
    write_site(patched);
    CHECK(s50 > 1.0 && s50 < 1.7 && s100 > 4.0 && s100 < 6.0, "stock: 50%% %.2f s, knee %.2f s, 100%% %.2f s", s50, s82, s100);

    /* 2. Patched: nothing changes up to the knee; the top is long, finite and still decays. */
    struct level top;
    double p50 = rt60("Reverb", 0.5f, 6, NULL), p82 = rt60("Reverb", SC_REVERB_KNEE, 10, NULL), p100 = rt60("Reverb", 1.0f, 60, &top);
    CHECK(fabs(p50 - s50) <= 0.1 && fabs(p82 - s82) <= 0.15, "at and below the knee as stock: 50%% %.2f s, knee %.2f s", p50, p82);
    CHECK(p100 >= 20 && p100 <= 40 && top.finite && top.peak < 1.0, "100%%: %.1f s, peak %.3f", p100, top.peak);

    /* 3. A minute of loud input at the top: the level settles, it does not climb. */
    flush("Reverb");
    send(PARAMETER, "Reverb", 1.0f);
    run(30, 1);
    struct level mid = run(10, 1), late = run(20, 1);
    CHECK(mid.finite && late.finite && late.peak < 2.0 && late.rms < mid.rms * 1.15, "60 s of input at 100%%: rms %.3f then %.3f, peak %.3f", mid.rms, late.rms, late.peak);

    /* 4. FX OFF: Engine lets the tail ring. It rings as long as the stock one, not longer,
     * and after that nothing comes back when the effect is turned on again. */
    send(TURN_OFF, "Reverb", 0);
    double off_tail = 0;
    for (double t = 0; t < 12; t += 0.05) if (run(0.05, 0).rms > late.rms * 0.001) off_tail = t + 0.05;
    send(TURN_ON, "Reverb", 0); send(MIX, "Reverb", 1.0f);
    struct level again = run(2.0, 0);
    write_site(stock);
    flush("Reverb");
    send(PARAMETER, "Reverb", 1.0f);
    run(20, 1);
    struct level stock_level = run(10, 1);
    send(TURN_OFF, "Reverb", 0);
    double stock_off = 0;
    for (double t = 0; t < 12; t += 0.05) if (run(0.05, 0).rms > stock_level.rms * 0.001) stock_off = t + 0.05;
    write_site(patched);
    turn_on("Reverb");
    CHECK(off_tail < stock_off + 1.5 && off_tail < 7.5 && again.rms < late.rms * 1e-3, "tail after FX OFF %.2f s (stock %.2f s), on again %.2g", off_tail, stock_off, again.rms);
    CHECK(late.rms < stock_level.rms * 4, "level with input at 100%%: %.3f (stock %.3f, %+.1f dB)", late.rms, stock_level.rms, 20 * log10(late.rms / stock_level.rms));

    /* 5. Changing the effect flushes it too. */
    run(1.0, 1);
    CHECK(select_effect("Echo") && select_effect("Reverb"), "to Echo and back");
    turn_on("Reverb");
    struct level back = run(2.0, 0);
    CHECK(back.rms < late.rms * 1e-3, "tail after an effect change %.2g", back.rms);

    /* 6. The deck's 48 kHz: the same top, still finite. */
    float sr = 48000; int block; memcpy(&block, info + 8, 4);
    ((void (*)(void *, int, float))*(uintptr_t *)(*(uintptr_t *)reverb + 0x1c))(reverb, block, sr);
    rate_hz = 48000;
    double p48 = rt60("Reverb", 1.0f, 60, &top);
    CHECK(p48 >= 20 && p48 <= 40 && top.finite && top.peak < 1.0, "100%% at 48 kHz: %.1f s, peak %.3f", p48, top.peak);
    sr = 44100; ((void (*)(void *, int, float))*(uintptr_t *)(*(uintptr_t *)reverb + 0x1c))(reverb, block, sr);
    rate_hz = 44100;

    /* 7. The other effects on this DSP are other types: the formula is not theirs. */
    static const char *const others[] = {"Swell Verb", "LFO Verb", "Reverb Drop", "Reverse Verb", "Reverb Rise"};
    for (unsigned i = 0; i < sizeof others / sizeof *others; ++i) {
        int ok = select_effect(others[i]);
        if (ok) turn_on(others[i]);
        CHECK(ok && ((int *)dsp)[0xf] != 0, "%s is type %d", others[i], ((int *)dsp)[0xf]);
    }
    fprintf(stderr, "FXREVERB done|%s\n", failures ? "FAIL" : "PASS");
    return NULL;
}
__attribute__((constructor)) static void start(void) {
    char exe[128] = {0}; ssize_t n = readlink("/proc/self/exe", exe, sizeof exe - 1);
    if (n <= 0 || !strstr(exe, "/Engine/Engine")) return;
    dl_iterate_phdr(main_base, NULL);
    pthread_t t; if (!pthread_create(&t, NULL, test, NULL)) pthread_detach(t);
}
