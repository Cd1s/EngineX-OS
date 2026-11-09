/* Sandbox-only offline driver, never shipped/preloaded on a deck.
 * The sandbox has no audio device. Capture real ChannelDJFx handlers allocated
 * by Engine, then replay ABI event fixtures and feed the real carrier pointer
 * through the installed production process hook. This is NOT an audio-device
 * or hardware-routing test. No production heap scanning is used. */
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
#include <unistd.h>

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
typedef void (*effect_fn)(void *, float *const *, int, unsigned);
struct owner { void *self, *effect, *mix, *parameter, *change, *collection; };

static void *carrier(void *owner) {
    void *col = *(void **)((char *)owner + 0x28);
    void **begin = *(void ***)((char *)col + 4), **end = *(void ***)((char *)col + 8);
    if (!begin || end < begin || end - begin > 128) return NULL;
    for (; begin < end; ++begin) {
        char *e = *begin;
        if (!e || *(uintptr_t *)(*(uintptr_t *)e + 8) != base + 0xf1ab24) continue;
        if (*(unsigned *)(e + 0x2c) == 7 && !memcmp(*(char **)(e + 0x28), "Flanger", 7)) return e;
    }
    return NULL;
}
static int owners(struct owner out[2]) {
    unsigned found = 0, limit = __atomic_load_n(&count, __ATOMIC_ACQUIRE);
    if (limit > 262144) limit = 262144;
    int fd = open("/proc/self/mem", O_RDONLY);
    for (unsigned i = 0; i < limit; ++i) {
        void *p = __atomic_load_n(&allocs[i], __ATOMIC_ACQUIRE);
        uint32_t w[6], vt;
        if (pread64(fd, w, sizeof w, (off64_t)(uintptr_t)p) != sizeof w ||
            w[0] != base + 0x2b411d4 || w[4] != base + 0xe9437c || w[5]) continue;
        if (pread64(fd, &vt, 4, w[3]) != 4 || vt != base + 0x2b21eec) continue;
        void *o = (void *)(uintptr_t)w[3];
        if (found && o == out[0].self) continue;
        void *e = carrier(o);
        if (!e) continue;
        void *col = *(void **)((char *)o + 0x28);
        if (found && (e == out[0].effect || col == out[0].collection)) continue;
        out[found++] = (struct owner){.self=o, .effect=e, .mix=p, .collection=col};
        if (found == 2) break;
    }
    if (found == 2) for (unsigned i = 0; i < limit; ++i) {
        void *p = __atomic_load_n(&allocs[i], __ATOMIC_ACQUIRE);
        uint32_t w[6];
        if (pread64(fd, w, sizeof w, (off64_t)(uintptr_t)p) != sizeof w) continue;
        for (unsigned u = 0; u < 2; ++u) if (w[3] == (uintptr_t)out[u].self) {
            if (w[0] == base + 0x2b411ac && w[4] == base + 0xe942c4) out[u].parameter = p;
            if (w[0] == base + 0x2b41184 && w[4] == base + 0xe8f36c) out[u].change = p;
        }
    }
    close(fd);
    return found == 2 && out[0].parameter && out[1].parameter && out[0].change && out[1].change;
}
static void named_event(unsigned char *e) {
    const char *name = "Flanger"; unsigned length = 7;
    memcpy(e + 8, &name, 4); memcpy(e + 12, &length, 4);
}
static void mix(struct owner *o, float value) {
    unsigned char e[16] = {0}; memcpy(e + 12, &value, 4);
    ((event_fn)(base + 0xe6a0e8))(o->mix, e);
}
static void parameter(struct owner *o, float value) {
    unsigned char e[36] = {0}; named_event(e); memcpy(e + 32, &value, 4);
    ((event_fn)(base + 0xe6a140))(o->parameter, e);
}
static int command(unsigned unit, unsigned mode, unsigned *a, unsigned *b) {
    FILE *f = fopen("/data/sc5000-custom/ui/fx-selector.request", "r");
    unsigned u, m; unsigned long long seq, expires;
    int n = f ? fscanf(f, "%u %u %llu %u %u %llu", &u, &m, &seq, a, b, &expires) : 0;
    if (f) fclose(f);
    if (n != 6 || u != unit || m != mode) return 0;
    unsigned au, am, aa, ab, reused; unsigned long long as, ae;
    f = fopen("/data/sc5000-custom/ui/fx-selector.ack", "r");
    n = f ? fscanf(f, "%u %u %llu %u %u %llu %u", &au, &am, &as, &aa, &ab, &ae, &reused) : 0;
    if (f) fclose(f);
    return n == 7 && ae == expires && au == u && am == m && as == seq && aa == *a && ab == *b;
}
static double render(struct owner *o, float wet, float param, int *ok) {
    mix(o, wet); parameter(o, param);
    double energy = 0;
    for (unsigned block = 0; block < 750; ++block) {
        float l[128], r[128], dry[128];
        float **p = (float **)((char *)o->self + 0x30 + 0xf8);
        float *saved[2] = {p[0], p[1]}; p[0] = l; p[1] = r;
        for (unsigned i = 0; i < 128; ++i) dry[i] = l[i] = r[i] = 0.2f * sinf((float)(block * 128 + i) * 0.027f);
        ((effect_fn)(base + 0xf1ab24))(o->effect, p, 2, 128);
        p[0] = saved[0]; p[1] = saved[1];
        if (wet == 0 && block * 128 >= 4096 && (memcmp(l, dry, sizeof l) || memcmp(r, dry, sizeof r))) *ok = 0;
        for (unsigned i = 0; i < 128; ++i) {
            if (!isfinite(l[i]) || fabsf(l[i]) > 4) *ok = 0;
            energy += (double)l[i] * l[i];
        }
    }
    return energy;
}
static void marker(const char *path) { FILE *f = fopen(path, "w"); if (f) { fputs("ok\n", f); fclose(f); } }
static void *run(void *unused) {
    (void)unused;
    sleep(12);
    struct owner o[2] = {0};
    if (!owners(o)) { fprintf(stderr, "FXNATIVE FAIL real-owner-discovery\n"); return NULL; }
    int ok = 1;
    fprintf(stderr, "FXNATIVE distinct-collections|true\n");
    for (unsigned u = 0; u < 2; ++u) {
        unsigned a = 0, b = 0; int ready = 0;
        for (unsigned tries = 0; tries < 100; ++tries) {
            if (command(u + 1, u + 1, &a, &b)) { ready = 1; break; }
            usleep(100000);
        }
        if (!ready) { fprintf(stderr, "FXNATIVE FAIL QML-command-unit%u\n", u + 1); return NULL; }
        /* Replay fixtures into real native handlers: the device callback is
         * absent, so Engine's pending DSP queue is deliberately not claimed
         * as tested by this offline driver. */
        unsigned char change[32] = {0}; named_event(change);
        ((event_fn)(base + 0xe6a198))(o[u].change, change);
        float fa, fb; memcpy(&fa, &a, 4); memcpy(&fb, &b, 4);
        mix(&o[u], fa); mix(&o[u], fb);
        double ea = render(&o[u], 0.7f, 0.15f, &ok);
        double eb = render(&o[u], 0.7f, 0.85f, &ok);
        render(&o[u], 0, 0.5f, &ok);
        if (fabs(ea - eb) < 0.001 || ea <= 0 || eb <= 0) ok = 0;
        if (u) render(&o[0], 0, 0.5f, &ok); /* unit 2 changes preserve unit 1's literal bypass */
        fprintf(stderr, "FXNATIVE unit=%u finite+param+wet0|%s\n", u + 1, ok ? "PASS" : "FAIL");
        marker(u ? "/tmp/fx-native-stage2" : "/tmp/fx-native-stage1");
    }
    unsigned a, b; int released = 0;
    for (unsigned tries = 0; tries < 100; ++tries) {
        if (command(1, 0, &a, &b)) { released = 1; break; }
        usleep(100000);
    }
    if (!released) ok = 0;
    if (released) {
        float l[1] = {0}, r[1] = {0}; float *p[] = {l, r};
        ((effect_fn)(base + 0xf1ab24))(o[0].effect, p, 2, 0);
        float restored; memcpy(&restored, (char *)o[0].self + 0x2b8, 4);
        if (restored != 0) ok = 0;
        fprintf(stderr, "FXNATIVE stock-mix-restored|%s\n", restored == 0 ? "PASS" : "FAIL");
    }
    fprintf(stderr, "FXNATIVE offline-driver|%s\n", ok ? "PASS" : "FAIL");
    if (ok) marker("/tmp/fx-native-done");
    return NULL;
}
__attribute__((constructor)) static void start(void) {
    char exe[128] = {0}; ssize_t n = readlink("/proc/self/exe", exe, sizeof exe - 1);
    if (n <= 0 || !strstr(exe, "/Engine/Engine")) return;
    dl_iterate_phdr(main_base, NULL);
    pthread_t t; if (!pthread_create(&t, NULL, run, NULL)) pthread_detach(t);
}
