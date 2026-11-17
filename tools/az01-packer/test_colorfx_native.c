/* Color FX on the channel filter knobs, sandbox only, never shipped or preloaded on a deck.
 * Run as FX_DRIVER=tools/az01-packer/test_colorfx_native.c FX_DRIVER_TAG=CFXNATIVE with
 * FX_PRODUCT=JC11|JC16|JP11|NH08|JP07 (Engine 5.3.0) or JP21|JP20|JC11S|NH08S|NH10 (Gen-2,
 * Engine 5.3.0-sclive4) (test_fx_selector_sandbox.sh). Every address below belongs to the build
 * picked by its BuildID. On Gen-2 the sandbox also needs Engine's SoC identity predicate stubbed,
 * as sclive-aux-hardware-fixture.c does: without the eFuse device Engine exits to "TestApp".
 *
 * Offline (the sandbox has no audio device): turns Engine's real SweepFxControl::setValue,
 * which the hook wraps, and renders noise through the real ChannelSweepFx nodes. Channel 1 is
 * rendered through the hooked process; channel 2 gets the same knob and input but is rendered
 * through a copy of stock's own entry, so the two are bit-identical exactly where the hook
 * leaves stock alone. Players (JP07) must have no Sweep FX control and no ready marker. */
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
static const struct build { unsigned char id[20]; uintptr_t ctrl_vt, node_vt, set_value, process, identity; } builds[] = {
    {"\xbe\x53\x7e\x90\x81\x21\xe7\x61\x64\xdd\xbb\xf9\x73\x0d\x47\x2b\xa0\xb8\xf9\xd4", 0x2b1e478, 0x2b22f5c, 0xd8ba3c, 0xec127c, 0},
    {"\xb9\x46\xc3\xb8\xd9\x9d\x32\x99\x8c\x81\x97\xd5\xdc\x90\x5a\x6b\xdd\xaa\x5d\x2f", 0x2b9e0a0, 0x2ba23c8, 0xd9619c, 0xecdbd4, 0x3a6794},
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
/* SweepFxControl lives in a 108-byte shared_ptr block (object at +12), one per mixer channel. */
void *_Znwj(size_t n) {
    void *p = malloc(n);
    if (n == 108) {
        unsigned i = __atomic_fetch_add(&count, 1, __ATOMIC_RELAXED);
        if (i < 65536) __atomic_store_n(&allocs[i], p, __ATOMIC_RELEASE);
    }
    return p;
}
/* The main program (first object): its base and, from its GNU build-id note, its build. */
static int main_base(struct dl_phdr_info *info, size_t n, void *arg) {
    (void)n; (void)arg;
    base = info->dlpi_addr;
    for (unsigned p = 0; p < info->dlpi_phnum; ++p) {
        const ElfW(Phdr) *h = info->dlpi_phdr + p;
        const unsigned char *note = (const void *)(base + h->p_vaddr), *end = note + h->p_memsz;
        while (h->p_type == PT_NOTE && note + sizeof(ElfW(Nhdr)) <= end) {
            const ElfW(Nhdr) *nh = (const void *)note;
            size_t name = (nh->n_namesz + 3u) & ~3u, desc = (nh->n_descsz + 3u) & ~3u;
            note += sizeof *nh;
            if (nh->n_type == NT_GNU_BUILD_ID && nh->n_descsz == 20)
                for (unsigned i = 0; i < sizeof builds / sizeof *builds; ++i)
                    if (!memcmp(note + name, builds[i].id, 20)) eng = &builds[i];
            note += name + desc;
        }
    }
    return 1;
}

static int failures;
#define CHECK(ok, ...) do { int pass_ = (ok); if (!pass_) ++failures; \
    fprintf(stderr, "CFXNATIVE %s|", pass_ ? "PASS" : "FAIL"); fprintf(stderr, __VA_ARGS__); fputc('\n', stderr); } while (0)

static char *controls[8];
static unsigned ncontrols;
static void find_controls(void) {
    unsigned limit = __atomic_load_n(&count, __ATOMIC_ACQUIRE);
    if (limit > 65536) limit = 65536;
    int fd = open("/proc/self/mem", O_RDONLY);
    for (unsigned i = 0; i < limit && ncontrols < 8; ++i) {
        uint32_t w[16], vt = 0;
        char *p = __atomic_load_n(&allocs[i], __ATOMIC_ACQUIRE);
        if (pread64(fd, w, sizeof w, (off64_t)(uintptr_t)(p + 12)) != sizeof w || w[0] != base + CTRL_VT) continue;
        if (pread64(fd, &vt, 4, (off64_t)w[14]) != 4 || vt != base + NODE_VT) continue;
        controls[ncontrols++] = p + 12;
    }
    close(fd);
}
static void *node_of(unsigned k, int spare) { return *(void **)(controls[k] + (spare ? 0x3c : 0x38)); }
static process_fn stock;
static void knob(unsigned k, float v) { ((value_fn)(base + SET_VALUE))(controls[k], v); }

static unsigned lcg = 7;
static float noise(void) { lcg = lcg * 1664525u + 1013904223u; return 0.25f * (float)((int)(lcg >> 8) % 65536 - 32768) / 65536.0f; }
struct pair { int same, exact_dry, finite; double ratio_hook, ratio_stock; };
/* The same noise through channel `a` (hooked) and channel `b` (stock's own entry). */
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
    r.ratio_hook = sqrt(h2 / in2); r.ratio_stock = sqrt(s2 / in2);
    return r;
}
static void style(const char *conf) {
    FILE *f = fopen("/data/enginex/ui/settings.conf", "w");
    if (f) { fputs(conf, f); fclose(f); }
    usleep(400000); /* the hook polls settings.conf every 80 ms */
}

static void *test(void *unused) {
    (void)unused;
    sleep(12);
    char product[8] = "";
    FILE *f = fopen("/sys/firmware/devicetree/base/inmusic,product-code", "r");
    if (f) { if (!fgets(product, sizeof product, f)) product[0] = 0; fclose(f); }
    find_controls();
    char marker[32] = "";
    f = fopen("/run/enginex-colorfx.ready", "r");
    if (f) { if (!fgets(marker, sizeof marker, f)) marker[0] = 0; fclose(f); }
    if (!strncmp(product, "JP07", 4)) {
        CHECK(ncontrols == 0 && !marker[0], "player %s: no Sweep FX control (%u) and no Color FX marker", product, ncontrols);
        fprintf(stderr, "CFXNATIVE done|%s\n", failures ? "FAIL" : "PASS");
        return NULL;
    }
    CHECK(ncontrols >= 2, "%s: %u mixer channels with a Sweep FX control", product, ncontrols);
    CHECK(!strcmp(marker, "armhf-cfx-v1\n"), "ready marker for the UI pack and pkg/webui");
    CHECK(*(uint32_t *)(base + PROCESS) == 0xe51ff004 && *(uint32_t *)(base + SET_VALUE) == 0xe51ff004, "hook installed on setValue and ChannelSweepFx::process");
    if (ncontrols < 2) { fprintf(stderr, "CFXNATIVE done|FAIL\n"); return NULL; }
    static const uint32_t entry[] = {0xe16d42f4, 0xe1cd60f8, 0xe51ff004, 0};
    uint32_t *code = mmap(NULL, 4096, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    memcpy(code, entry, sizeof entry);
    code[3] = (uint32_t)(base + PROCESS + 8);
    mprotect(code, 4096, PROT_READ | PROT_EXEC);
    __builtin___clear_cache((char *)code, (char *)code + sizeof entry);
    stock = (process_fn)code;

    /* 1. Style Off: channel 1 through the hook equals stock at every knob position. */
    style("lang=en\n");
    static const float sweep[] = {0.5f, 0.05f, 0.95f, 0.3f, 0.5f};
    int off_same = 1;
    for (unsigned i = 0; i < 5; ++i) {
        knob(0, sweep[i]); knob(1, sweep[i]);
        struct pair p = render(0, 1, 60);
        off_same &= p.same && p.finite;
        if (i == 1) CHECK(p.ratio_stock < 0.6, "stock low-pass at 0.05 (ratio %.3f)", p.ratio_stock);
    }
    CHECK(off_same, "style Off: hooked channel bit-identical to stock at 0.5, 0.05, 0.95, 0.3, 0.5");

    /* 2. A style picked, knob in the centre band: still stock, bit for bit. */
    style("lang=en\npioneer_fx=spiral\n");
    static const float centre[] = {0.5f, 0.497f, 0.503f};
    int centre_same = 1;
    for (unsigned i = 0; i < 3; ++i) {
        knob(0, centre[i]); knob(1, centre[i]);
        struct pair p = render(0, 1, 40);
        centre_same &= p.same;
    }
    CHECK(centre_same, "spiral, knob centred: bit-identical to stock");

    /* 3. Either way off centre: stock's filter is skipped and the style plays. */
    knob(0, 0.05f); knob(1, 0.05f);
    struct pair left = render(0, 1, 120);
    CHECK(!left.same && !left.exact_dry && left.finite && left.ratio_stock < 0.6,
          "spiral, knob left: style instead of stock's low-pass (ratio %.3f, stock %.3f)", left.ratio_hook, left.ratio_stock);
    knob(0, 0.95f); knob(1, 0.95f);
    struct pair right = render(0, 1, 120);
    CHECK(!right.same && !right.exact_dry && right.finite, "spiral, knob right: style instead of stock's high-pass (ratio %.3f, stock %.3f)", right.ratio_hook, right.ratio_stock);

    /* 4. Channels are independent: channel 2 back at centre settles to stock (an exact copy)
     * block by block alongside channel 1, which keeps playing the style. */
    knob(1, 0.5f);
    int other_exact = 1, still_playing = 1;
    for (unsigned n = 0; n < 240; ++n) {
        struct pair one = render(0, ncontrols, 1), two = render(1, ncontrols, 1);
        if (n >= 200) other_exact &= two.exact_dry, still_playing &= !one.exact_dry;
    }
    CHECK(other_exact && still_playing, "channel 2 centred while channel 1 plays: channel 2 stock, channel 1 still the style");

    /* 5. The second node of a playing channel is left alone, in place, as stock leaves it at centre. */
    float l[BLOCK], r[BLOCK], kl[BLOCK], kr[BLOCK], *io[2] = {l, r};
    for (unsigned i = 0; i < BLOCK; ++i) l[i] = kl[i] = r[i] = kr[i] = noise();
    uint32_t ctx[64] = {0};
    ((process_fn)(base + PROCESS))(node_of(0, 1), 2, io, 2, io, BLOCK, ctx, ctx);
    CHECK(!memcmp(l, kl, sizeof l) && !memcmp(r, kr, sizeof r), "second node untouched while the style plays");

    /* 6. Back to centre: the style rings down, then the channel is stock (an exact copy) again. */
    knob(0, 0.5f);
    render(0, ncontrols, 40);
    struct pair back = render(0, ncontrols, 40);
    CHECK(back.exact_dry, "back to centre: stock again after the ring-down");

    /* 7. Style Off with the knob left turned: stock's filter at once. */
    knob(0, 0.05f);
    render(0, ncontrols, 20);
    style("lang=en\n");
    render(0, ncontrols, 40);
    struct pair off = render(0, ncontrols, 80);
    CHECK(off.ratio_hook < 0.6 && off.finite, "style Off, knob left: stock low-pass (ratio %.3f)", off.ratio_hook);
    knob(0, 0.5f);
    fprintf(stderr, "CFXNATIVE done|%s\n", failures ? "FAIL" : "PASS");
    return NULL;
}
__attribute__((constructor)) static void start(void) {
    char exe[64];
    ssize_t n = readlink("/proc/self/exe", exe, sizeof exe - 1);
    if (n <= 0 || (exe[n] = 0, strcmp(exe, "/usr/Engine/Engine"))) return; /* Engine's helpers inherit the preload */
    dl_iterate_phdr(main_base, NULL);
    if (!eng) { fprintf(stderr, "CFXNATIVE FAIL|unknown Engine build\nCFXNATIVE done|FAIL\n"); return; }
    if (eng->identity) { /* sandbox only: the Gen-2 SoC identity predicate returns true */
        static const uint32_t expected[] = {0xe16d42f4, 0xe3031e54}, stub[] = {0xe3a00001, 0xe12fff1e};
        uint32_t *at = (uint32_t *)(base + eng->identity);
        void *page = (void *)((uintptr_t)at & ~(uintptr_t)4095);
        if (memcmp(at, expected, sizeof expected) || mprotect(page, 4096, PROT_READ | PROT_WRITE)) _exit(127);
        memcpy(at, stub, sizeof stub);
        if (mprotect(page, 4096, PROT_READ | PROT_EXEC)) _exit(127);
        __builtin___clear_cache((char *)at, (char *)at + sizeof stub);
    }
    pthread_t t;
    pthread_create(&t, NULL, test, NULL);
    pthread_detach(t);
}
