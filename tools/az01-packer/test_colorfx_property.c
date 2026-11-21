/* Color FX hardware knob-path regression driver (sandbox only, never shipped).
 * Drives SweepFx/Value from QML via stock InputAssignment PropertyTarget
 * (colorfx-property-probe.qml) -> NormalizedFloatTranslator::setValue(v, 1, 0)
 * -> SweepFxStateMapAdaptor's PropertyObserver lambda, instead of calling
 * SweepFxControl::setValue directly.
 *
 * Run with:
 *   FX_DRIVER=tools/az01-packer/test_colorfx_property.c \
 *   FX_QML_PROBE=tools/az01-packer/colorfx-property-probe.qml \
 *   FX_DRIVER_TAG=CFXPROP FX_PRODUCT=NH08 \
 *   bash tools/az01-packer/test_fx_selector_sandbox.sh
 */
#define _GNU_SOURCE
#include <elf.h>
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

enum { BLOCK = 128 };
static const struct build {
    unsigned char id[20];
    uintptr_t ctrl_vt, node_vt, set_value, process, identity;
} builds[] = {
    {"\xbe\x53\x7e\x90\x81\x21\xe7\x61\x64\xdd\xbb\xf9\x73\x0d\x47\x2b\xa0\xb8\xf9\xd4",
     0x2b1e478, 0x2b22f5c, 0xd8ba3c, 0xec127c, 0},
    {"\xb9\x46\xc3\xb8\xd9\x9d\x32\x99\x8c\x81\x97\xd5\xdc\x90\x5a\x6b\xdd\xaa\x5d\x2f",
     0x2b9e0a0, 0x2ba23c8, 0xd9619c, 0xecdbd4, 0x3a6794},
};
static const struct build *eng;
#define CTRL_VT (eng->ctrl_vt)
#define NODE_VT (eng->node_vt)
#define SET_VALUE (eng->set_value)
#define PROCESS (eng->process)

typedef void (*value_fn)(void *, float);
typedef void (*process_fn)(void *, int, float **, int, float **, int, void *, void *);

static void *allocs[65536];
static unsigned count;
static uintptr_t base;

void *_Znwj(size_t n) {
    void *p = malloc(n);
    if (n == 108) {
        unsigned i = __atomic_fetch_add(&count, 1, __ATOMIC_RELAXED);
        if (i < 65536) __atomic_store_n(&allocs[i], p, __ATOMIC_RELEASE);
    }
    return p;
}

static int find_engine(struct dl_phdr_info *info, size_t size, void *data) {
    (void)size; (void)data;
    if (info->dlpi_name && info->dlpi_name[0]) return 0;
    base = info->dlpi_addr;
    for (int i = 0; i < info->dlpi_phnum; ++i) {
        const Elf32_Phdr *p = &info->dlpi_phdr[i];
        if (p->p_type != PT_NOTE) continue;
        const unsigned char *note = (const unsigned char *)(base + p->p_vaddr), *end = note + p->p_memsz;
        while (note + sizeof(Elf32_Nhdr) <= end) {
            const Elf32_Nhdr *h = (const Elf32_Nhdr *)note;
            size_t name = (h->n_namesz + 3u) & ~3u, desc = (h->n_descsz + 3u) & ~3u;
            note += sizeof *h;
            if (note + name + desc > end) break;
            if (h->n_type == NT_GNU_BUILD_ID && h->n_namesz == 4 && !memcmp(note, "GNU", 4) && h->n_descsz == 20)
                for (unsigned b = 0; b < sizeof builds / sizeof builds[0]; ++b)
                    if (!memcmp(note + name, builds[b].id, 20)) eng = &builds[b];
            note += name + desc;
        }
    }
    return 1;
}

static int failures;
#define CHECK(ok, ...) do { int pass_ = (ok); if (!pass_) ++failures; \
    fprintf(stderr, "CFXPROP %s|", pass_ ? "PASS" : "FAIL"); fprintf(stderr, __VA_ARGS__); fputc('\n', stderr); } while (0)

static char *controls[8];
static unsigned ncontrols;
static void find_controls(void) {
    ncontrols = 0;
    unsigned limit = __atomic_load_n(&count, __ATOMIC_ACQUIRE);
    if (limit > 65536) limit = 65536;
    int fd = open("/proc/self/mem", O_RDONLY);
    if (fd < 0) return;
    for (unsigned i = 0; i < limit && ncontrols < 8; ++i) {
        uint32_t w[16], vt = 0;
        char *p = __atomic_load_n(&allocs[i], __ATOMIC_ACQUIRE);
        if (!p) continue;
        if (pread64(fd, w, sizeof w, (off64_t)(uintptr_t)(p + 12)) != sizeof w || w[0] != base + CTRL_VT) continue;
        if (pread64(fd, &vt, 4, (off64_t)w[14]) != 4 || vt != base + NODE_VT) continue;
        controls[ncontrols++] = p + 12;
    }
    close(fd);
}

static void *node_of(unsigned k, int spare) { return *(void **)(controls[k] + (spare ? 0x3c : 0x38)); }
static float knob_of(unsigned k) {
    uint32_t bits = __atomic_load_n((uint32_t *)(controls[k] + 0x40), __ATOMIC_ACQUIRE);
    float v;
    memcpy(&v, &bits, sizeof v);
    return v;
}

static process_fn stock;
static unsigned lcg = 7;
static float noise(void) {
    lcg = lcg * 1664525u + 1013904223u;
    return 0.25f * (float)((int)(lcg >> 8) % 65536 - 32768) / 65536.0f;
}

struct pair { int same, exact_dry, finite; double ratio_hook, ratio_stock; };
static struct pair render(unsigned a, unsigned b, unsigned blocks) {
    struct pair r = {1, 1, 1, 0, 0};
    double in2 = 0, h2 = 0, s2 = 0;
    for (unsigned n = 0; n < blocks; ++n) {
        float il[BLOCK], ir[BLOCK], jl[BLOCK], jr[BLOCK], hl[BLOCK], hr[BLOCK], sl[BLOCK], sr[BLOCK];
        float *hin[2] = {il, ir}, *hout[2] = {hl, hr}, *sin_[2] = {jl, jr}, *sout[2] = {sl, sr};
        for (unsigned i = 0; i < BLOCK; ++i) { il[i] = jl[i] = noise(); ir[i] = jr[i] = noise(); }
        uint32_t ctx[64] = {0};
        ((process_fn)(base + PROCESS))(node_of(a, 0), 2, hin, 2, hout, BLOCK, ctx, ctx);
        if (b < ncontrols) stock(node_of(b, 0), 2, sin_, 2, sout, BLOCK, ctx, ctx);
        if (b < ncontrols && (memcmp(hl, sl, sizeof hl) || memcmp(hr, sr, sizeof hr))) r.same = 0;
        if (memcmp(hl, jl, sizeof hl) || memcmp(hr, jr, sizeof hr)) r.exact_dry = 0;
        for (unsigned i = 0; i < BLOCK; ++i) {
            if (!isfinite(hl[i]) || !isfinite(hr[i]) || fabsf(hl[i]) > 4) r.finite = 0;
            in2 += (double)jl[i] * jl[i]; h2 += (double)hl[i] * hl[i]; s2 += (double)sl[i] * sl[i];
        }
    }
    r.ratio_hook = sqrt(h2 / in2);
    r.ratio_stock = sqrt(s2 / in2);
    return r;
}

static int wait_knob(unsigned k, float target, unsigned max_ms) {
    for (unsigned elapsed = 0; elapsed < max_ms; elapsed += 50) {
        if (fabsf(knob_of(k) - target) < 0.01f) { usleep(100000); return 1; }
        usleep(50000);
    }
    return 0;
}

static void *test(void *unused) {
    (void)unused;
    char product[8] = "";
    FILE *f = fopen("/sys/firmware/devicetree/base/inmusic,product-code", "r");
    if (f) { if (!fgets(product, sizeof product, f)) product[0] = 0; fclose(f); }
    product[strcspn(product, "\r\n")] = 0;

    for (unsigned ms = 0; ms < 15000; ms += 200) {
        find_controls();
        if (ncontrols >= 2) break;
        usleep(200000);
    }
    char marker[32] = "";
    for (unsigned ms = 0; ms < 5000 && !marker[0]; ms += 200) {
        f = fopen("/run/enginex-colorfx.ready", "r");
        if (f) { if (!fgets(marker, sizeof marker, f)) marker[0] = 0; fclose(f); }
        if (!marker[0]) usleep(200000);
    }

    uintptr_t vt4 = *(uintptr_t *)(base + CTRL_VT + 0x10);
    int vt4_hooked = vt4 != base + SET_VALUE;
    int inline_setval = *(uint32_t *)(base + SET_VALUE) == 0xe51ff004;
    int inline_proc = *(uint32_t *)(base + PROCESS) == 0xe51ff004;
    fprintf(stderr, "CFXPROP INFO|product=%s ncontrols=%u vt4=%p set_value=%p vt4_hooked=%d inline_setval=%d inline_proc=%d\n",
            product, ncontrols, (void *)vt4, (void *)(base + SET_VALUE), vt4_hooked, inline_setval, inline_proc);

    CHECK(ncontrols >= 2, "%s: %u mixer channels with a SweepFxControl", product, ncontrols);
    CHECK(!strcmp(marker, "armhf-cfx-v1\n"), "ready marker present (ok=%d)", !strcmp(marker, "armhf-cfx-v1\n"));
    CHECK(inline_proc, "ChannelSweepFx::process hook installed");
    if (ncontrols < 2 || !inline_proc) {
        fprintf(stderr, "CFXPROP done|FAIL\n");
        return NULL;
    }

    static const uint32_t entry[] = {0xe16d42f4, 0xe1cd60f8, 0xe51ff004, 0};
    uint32_t *code = mmap(NULL, 4096, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    memcpy(code, entry, sizeof entry);
    code[3] = (uint32_t)(base + PROCESS + 8);
    mprotect(code, 4096, PROT_READ | PROT_EXEC);
    __builtin___clear_cache((char *)code, (char *)code + sizeof entry);
    stock = (process_fn)code;

    /* Step 1: Wait for QML PropertyTarget to turn Channel 1 to 0.05 while Channel 2 stays at 0.50. */
    int ch1 = -1, ch2 = -1;
    for (unsigned elapsed = 0; elapsed < 20000 && ch1 < 0; elapsed += 50) {
        for (unsigned k = 0; k < ncontrols; ++k) {
            if (fabsf(knob_of(k) - 0.05f) < 0.01f) { ch1 = (int)k; break; }
        }
        if (ch1 < 0) usleep(50000);
    }
    if (ch1 >= 0) {
        usleep(100000);
        for (unsigned k = 0; k < ncontrols; ++k) {
            if ((int)k != ch1 && fabsf(knob_of(k) - 0.50f) < 0.01f) { ch2 = (int)k; break; }
        }
    }
    CHECK(ch1 >= 0 && ch2 >= 0, "PropertyTarget updated SweepFxControl +0x40 to 0.05 (ch1=%d ch2=%d)", ch1, ch2);
    if (ch1 < 0 || ch2 < 0) {
        fprintf(stderr, "CFXPROP done|FAIL\n");
        return NULL;
    }

    struct pair p_ch2 = render((unsigned)ch2, ncontrols, 20);
    CHECK(p_ch2.exact_dry, "channel 2 stays dry at centre while channel 1 is turned");

    /* Step 2: Wait for QML PropertyTarget to turn Channel 2 to 0.05 as the stock reference. */
    CHECK(wait_knob((unsigned)ch2, 0.05f, 15000), "PropertyTarget updated ch2 +0x40 to 0.05");
    struct pair p05 = render((unsigned)ch1, (unsigned)ch2, 80);
    CHECK(p05.ratio_stock < 0.6, "PropertyTarget 0.05 drove stock filter on ch2 (ratio %.3f)", p05.ratio_stock);
    CHECK(!p05.same && p05.finite && fabs(p05.ratio_hook - p05.ratio_stock) > 0.05,
          "PropertyTarget 0.05 with spiral plays Color FX on ch1 (same=%d hook=%.3f stock=%.3f)",
          p05.same, p05.ratio_hook, p05.ratio_stock);

    /* Step 3: Wait for QML PropertyTarget to turn both channels to 0.95 (CW). */
    CHECK(wait_knob((unsigned)ch1, 0.95f, 15000) && wait_knob((unsigned)ch2, 0.95f, 15000),
          "PropertyTarget updated ch1 & ch2 +0x40 to 0.95");
    struct pair p95 = render((unsigned)ch1, (unsigned)ch2, 80);
    CHECK(!p95.same && p95.finite,
          "PropertyTarget 0.95 with spiral plays Color FX on ch1 (same=%d hook=%.3f stock=%.3f)",
          p95.same, p95.ratio_hook, p95.ratio_stock);

    /* Step 4: Wait for QML PropertyTarget to return both channels to 0.50 (centre). */
    CHECK(wait_knob((unsigned)ch1, 0.50f, 15000) && wait_knob((unsigned)ch2, 0.50f, 15000),
          "PropertyTarget returned ch1 & ch2 +0x40 to 0.50");
    (void)render((unsigned)ch1, (unsigned)ch2, 800);
    struct pair p50 = render((unsigned)ch1, (unsigned)ch2, 20);
    CHECK(p50.same && p50.exact_dry && p50.finite,
          "PropertyTarget 0.50 rings down to bit-exact stock dry (same=%d exact_dry=%d)",
          p50.same, p50.exact_dry);

    /* Diagnostic: if PropertyTarget failed to trigger Color FX on unfixed hook, prove in the same
     * process that calling inline-patched SET_VALUE directly DOES engage spiral (refutes H2/H3/H4). */
    if (failures && inline_setval) {
        ((value_fn)(base + SET_VALUE))(controls[ch1], 0.05f);
        ((value_fn)(base + SET_VALUE))(controls[ch2], 0.05f);
        struct pair diag = render((unsigned)ch1, (unsigned)ch2, 60);
        fprintf(stderr, "CFXPROP DIAG|direct SET_VALUE(0.05): same=%d hook=%.3f stock=%.3f\n",
                diag.same, diag.ratio_hook, diag.ratio_stock);
    }

    fprintf(stderr, "CFXPROP done|%s\n", failures ? "FAIL" : "PASS");
    fflush(stderr);
    return NULL;
}

__attribute__((constructor))
static void start(void) {
    char exe[64] = "";
    ssize_t n = readlink("/proc/self/exe", exe, sizeof exe - 1);
    if (n <= 0) return;
    exe[n] = 0;
    const char *slash = strrchr(exe, '/');
    if (strcmp(slash ? slash + 1 : exe, "Engine")) return;
    dl_iterate_phdr(find_engine, NULL);
    if (!eng) return;
    if (eng->identity) {
        static const uint32_t stub[] = {0xe3a00001, 0xe12fff1e};
        uintptr_t at = base + eng->identity;
        void *page = (void *)(at & ~(uintptr_t)4095);
        if (mprotect(page, 4096, PROT_READ | PROT_WRITE | PROT_EXEC)) _exit(127);
        memcpy((void *)at, stub, sizeof stub);
        if (mprotect(page, 4096, PROT_READ | PROT_EXEC)) _exit(127);
        __builtin___clear_cache((char *)at, (char *)at + sizeof stub);
    }
    pthread_t t;
    pthread_create(&t, NULL, test, NULL);
    pthread_detach(t);
}
