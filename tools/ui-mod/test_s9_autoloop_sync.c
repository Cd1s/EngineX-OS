/* #336 S9 (t/71431): Auto loops lose beat-grid sync on loop wrap in Bar/Beat Sync.
 *
 * Natively executes the real stock LoopRegion::isLengthMultipleOf and SyncModifier::onJump
 * machine code extracted from all 4 stock Engine binaries (5.0.4, 5.3.0, 5.3.0-sclive4
 * armhf; 5.3.0-az04 aarch64), before and after apply_engine_edits().
 *
 * Build & run on ARM build host (native aarch64 and armhf):
 *   gcc -std=gnu11 -O2 -Wall -Wextra -Werror -U_FORTIFY_SOURCE \
 *       tools/ui-mod/test_s9_autoloop_sync.c -ldl -lpthread -lm -o /tmp/test_s9_64
 *   /tmp/test_s9_64 /root/build/sc5000-lab/systemone-analysis/rootfs/usr/Engine/Engine
 *
 *   arm-linux-gnueabihf-gcc -std=gnu11 -O2 -Wall -Wextra -Werror -U_FORTIFY_SOURCE \
 *       -mthumb -march=armv7-a -mfpu=neon -mfloat-abi=hard \
 *       tools/ui-mod/test_s9_autoloop_sync.c -ldl -lpthread -lm -o /tmp/test_s9_32
 *   /tmp/test_s9_32 /root/build/sc5000-lab/5.0.4/tree/usr/Engine/Engine \
 *                   /root/build/sc5000-lab/5.3.0/tree/usr/Engine/Engine \
 *                   /root/build/sc5000-lab/sclive4-rea61-followup/rootfs/usr/Engine/Engine
 */
#define _GNU_SOURCE
#define ENGINE_UI_HOOK_TEST
int hook_test_engine = 1;
#include "engine_ui_hook.c"
#include <assert.h>
#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

static char tmpdir[] = "/tmp/s9-autoloop-XXXXXX";
static char board_file[sizeof tmpdir + 32];
static char conf_file[sizeof tmpdir + 32];

static void write_text(const char *path, const char *text) {
    FILE *f = fopen(path, "w");
    assert(f);
    fputs(text, f);
    fclose(f);
}

static void set_board(const char *code) {
    write_text(board_file, code);
    product_code_path = board_file;
}

static void set_disable_conf(const char *text) {
    if (text) write_text(conf_file, text);
    else unlink(conf_file);
    hf_path = conf_file;
    hf_reload();
}

static void read_exact_at(FILE *f, uint64_t off, void *dst, size_t n) {
    assert(fseeko(f, (off_t)off, SEEK_SET) == 0);
    assert(fread(dst, 1, n, f) == n);
}

static const struct engine_build *match_build_from_elf(FILE *f) {
    unsigned char hdr[8192];
    assert(fseeko(f, 0, SEEK_SET) == 0);
    size_t got = fread(hdr, 1, sizeof hdr, f);
    assert(got >= 256);
    for (size_t i = 0; i < sizeof engine_builds / sizeof *engine_builds; i++) {
        if (memmem(hdr, got, engine_builds[i].id, 20)) return &engine_builds[i];
    }
    return NULL;
}

/* Exact stock LoopRegion field offsets verified in disassembly:
 *   armhf:   +16 start (double), +24 end (double), +32 start_set (u8), +33 end_set (u8)
 *   aarch64: +24 start (double), +32 end (double), +40 start_set (u8), +41 end_set (u8) */
struct fake_loop_region {
#if defined(__aarch64__)
    uint8_t pad[24];
#else
    uint8_t pad[16];
#endif
    double start;
    double end;
    uint8_t start_set;
    uint8_t end_set;
    uint8_t pad2[6];
};

typedef uint32_t (*is_len_mult_fn)(const struct fake_loop_region *, double);
typedef void (*on_jump_call_fn)(void *, void *, double);

/* Exact stock SyncModifier::onJump fmod checks (5.3.0 0x17355a0..0x1735678 / AZ04 0x17c6dac..0x17c6e0c):
 *   Lead check:     rem4 = fmod(|delta|, 4.0); ok iff 0.0 <= rem4 && rem4 <= 1e-12;
 *                   else emit SyncEvents::ResetSyncLeadPhase!
 *   Follower check: remP = fabs(fmod(|delta|, syncPeriod));
 *                   jumpPhaseDiscontinuity = (remP > 1e-12) -> RequestSwitchToTempoSync! */
struct jump_spy_state {
    void *last_self;
    void *last_info;
    double last_delta;
    double sync_period;
    int lead_reset_fired;
    int follower_discontinuity;
    int calls;
#if defined(__aarch64__)
    void *last_got_x2;
#endif
};

static struct jump_spy_state g_spy;
#if defined(__aarch64__)
static uint64_t g_fake_canary = UINT64_C(0x5a3c91f0deadbeef);
#endif

static void record_on_jump_stock_checks(void *self, void *jump_info, double delta_beats) {
    g_spy.last_self = self;
    g_spy.last_info = jump_info;
    g_spy.last_delta = delta_beats;
    g_spy.calls++;

    double abs_d = fabs(delta_beats);
    double rem4 = fmod(abs_d, 4.0);
    int lead_ok = (rem4 >= 0.0 && rem4 <= 1e-12);
    g_spy.lead_reset_fired = !lead_ok;

    if (g_spy.sync_period > 0.0) {
        double rem_p = fabs(fmod(abs_d, g_spy.sync_period));
        g_spy.follower_discontinuity = (rem_p > 1e-12) ? 1 : 0;
    } else {
        g_spy.follower_discontinuity = 0;
    }
}

#if defined(__aarch64__)
static void on_jump_spy_a64(void *self, void *jump_info, void *got_x2, double delta_beats) {
    g_spy.last_got_x2 = got_x2;
    record_on_jump_stock_checks(self, jump_info, delta_beats);
}
#else
static void on_jump_spy_a32(void *self, void *jump_info, double delta_beats) {
    record_on_jump_stock_checks(self, jump_info, delta_beats);
}
#endif

struct build_addrs {
    const char *name;
    uint32_t fn_va;       /* LoopRegion::isLengthMultipleOf entry */
    uint32_t div_va;      /* broken vdiv/fdiv site */
    uint32_t oj_va;       /* SyncModifier::onJump entry */
    uint32_t rodata_1e5;  /* AZ04 only: .rodata 1e-5 constant */
    uint32_t got_canary;  /* AZ04 only: GOT slot loaded by adrp+ldr in onJump prologue */
};

static const struct build_addrs *lookup_addrs(const char *name) {
    static const struct build_addrs table[] = {
        {"5.0.4",         0x17759c4u, 0x1775abcu, 0x15211c0u, 0u,         0u},
        {"5.3.0",         0x18baca4u, 0x18bad9cu, 0x1735548u, 0u,         0u},
        {"5.3.0-sclive4", 0x191a8ecu, 0x191a9e4u, 0x17ab900u, 0u,         0u},
        {"5.3.0-az04",    0x1920400u, 0x19204d8u, 0x17c6d60u, 0x1dc0b90u, 0x2db8860u},
    };
    for (size_t i = 0; i < sizeof table / sizeof *table; i++) {
        if (!strcmp(table[i].name, name)) return &table[i];
    }
    return NULL;
}

static uint32_t eval_is_mult(is_len_mult_fn fn, double start, double end,
                             uint8_t sset, uint8_t eset, double period) {
    struct fake_loop_region lr;
    memset(&lr, 0, sizeof lr);
    lr.start = start;
    lr.end = end;
    lr.start_set = sset;
    lr.end_set = eset;
    return fn(&lr, period) & 0xffu;
}

static void prepare_elf_image(FILE *f, const struct build_addrs *a,
                              uint8_t *img, size_t page) {
    /* 1. Copy real LoopRegion::isLengthMultipleOf code and redirect only its fmod PLT call
     *    to our local libc fmod stub right after the function body. */
#if defined(__aarch64__)
    const size_t fn_copy = 0x180;
    read_exact_at(f, a->fn_va, img + a->fn_va, fn_copy);
    /* bl +0x180 from +0x34: offset = (0x180 - 0x34) / 4 = 0x53 -> 0x94000053 */
    uint32_t bl_fmod = 0x94000053u;
    memcpy(img + a->fn_va + 0x34, &bl_fmod, 4);
    uint32_t fmod_stub[4] = {0x58000050u, 0xd61f0200u, 0, 0};
    uint64_t fmod_addr = (uintptr_t)&fmod;
    memcpy(&fmod_stub[2], &fmod_addr, 8);
    memcpy(img + a->fn_va + 0x180, fmod_stub, sizeof fmod_stub);
    read_exact_at(f, a->rodata_1e5, img + a->rodata_1e5, 8);
#else
    const size_t fn_copy = 0x19c; /* includes DBL_EPSILON (+0x18c) and 1e-5 (+0x194) pool */
    read_exact_at(f, a->fn_va, img + a->fn_va, fn_copy);
    /* bl +0x1a0 from +0x3c: offset = (0x1a0 - (0x3c + 8)) / 4 = 0x57 -> 0xeb000057 */
    uint32_t bl_fmod = 0xeb000057u;
    memcpy(img + a->fn_va + 0x3c, &bl_fmod, 4);
    uint32_t fmod_stub[2] = {0xe51ff004u, (uint32_t)(uintptr_t)&fmod};
    memcpy(img + a->fn_va + 0x1a0, fmod_stub, sizeof fmod_stub);
#endif
    uintptr_t fn_page = ((uintptr_t)(img + a->fn_va)) & ~(page - 1);
    assert(mprotect((void *)fn_page, page, PROT_READ | PROT_EXEC) == 0);
    __builtin___clear_cache((char *)fn_page, (char *)fn_page + page);

    /* 2. Copy real 16-byte SyncModifier::onJump prologue, followed at +16 by a tiny
     *    stack-restore + tail-branch stub to our C spy function. */
    read_exact_at(f, a->oj_va, img + a->oj_va, 16);
#if defined(__aarch64__)
    uint64_t canary_ptr = (uintptr_t)&g_fake_canary;
    memcpy(img + a->got_canary, &canary_ptr, 8);
    uint32_t oj_tail[6] = {
        0x910283ffu, /* add sp, sp, #0xa0 */
        0xd50323bfu, /* autiasp */
        0x58000050u, /* ldr x16, [pc, #8] */
        0xd61f0200u, /* br x16 */
        0, 0
    };
    uint64_t spy_addr = (uintptr_t)&on_jump_spy_a64;
    memcpy(&oj_tail[4], &spy_addr, 8);
    memcpy(img + a->oj_va + 16, oj_tail, sizeof oj_tail);
#else
    uint32_t oj_tail[3] = {
        0xe28dd020u, /* add sp, sp, #32 */
        0xe51ff004u, /* ldr pc, [pc, #-4] */
        (uint32_t)(uintptr_t)&on_jump_spy_a32
    };
    memcpy(img + a->oj_va + 16, oj_tail, sizeof oj_tail);
#endif
    uintptr_t oj_page = ((uintptr_t)(img + a->oj_va)) & ~(page - 1);
    assert(mprotect((void *)oj_page, page, PROT_READ | PROT_EXEC) == 0);
    __builtin___clear_cache((char *)oj_page, (char *)oj_page + page);
}

static void test_real_binary(const char *path) {
    FILE *f = fopen(path, "rb");
    assert(f);
    const struct engine_build *b = match_build_from_elf(f);
    assert(b);
    const struct build_addrs *a = lookup_addrs(b->name);
    assert(a);

    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    const size_t img_size = 0x3000000u; /* 48 MiB virtual; only ~4 pages faulted */
    uint8_t *img = mmap(NULL, img_size, PROT_READ | PROT_WRITE,
                        MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(img != MAP_FAILED);
    uintptr_t base = (uintptr_t)img;

    prepare_elf_image(f, a, img, page);
    is_len_mult_fn is_mult = (is_len_mult_fn)(base + a->fn_va);
    on_jump_call_fn on_jump = (on_jump_call_fn)(base + a->oj_va);

    /* --- Phase 1: Prove stock binary FAILS both bugs BEFORE our hook runs --- */
    assert(eval_is_mult(is_mult, 300.0, nextafter(332.0, 333.0), 1, 1, 4.0) == 0);
    assert(eval_is_mult(is_mult, 300.0, 332.0 + 1e-12, 1, 1, 4.0) == 0);
    assert(eval_is_mult(is_mult, 300.0, 301.0 + 1e-12, 1, 1, 1.0) == 0);
    assert(eval_is_mult(is_mult, 300.0, nextafter(332.0, 331.0), 1, 1, 4.0) == 1);

    /* Exact 8-bar Auto Loop crossing the 128.0 power-of-two boundary (100.7..132.7)
     * has length 32.0 - 2^-46 (-1 ULP): LoopRegion::wrapPosition at pos = 132.75
     * produces wrappedPos - pos = -31.999999999999986, making
     * fmod(|delta|, 4.0) = 3.999999999999986 > 1e-12 -> fires ResetSyncLeadPhase! */
    double start = 100.7, end = 132.7, pos = 132.75;
    double wrapped = start + fmod(pos - start, end - start);
    double live_wrap_delta = wrapped - pos;
    assert(fabs(live_wrap_delta + 32.0) > 1e-15 && fabs(live_wrap_delta + 32.0) < 1e-12);
    assert(fmod(fabs(live_wrap_delta), 4.0) > 3.9);

    int dummy_self = 0, dummy_info = 0;
    memset(&g_spy, 0, sizeof g_spy);
    g_spy.sync_period = 4.0;
    on_jump(&dummy_self, &dummy_info, live_wrap_delta);
    assert(g_spy.calls == 1);
    assert(g_spy.lead_reset_fired == 1);
    assert(g_spy.follower_discontinuity == 1);
#if defined(__aarch64__)
    assert(g_spy.last_got_x2 == &g_fake_canary);
#endif

    /* --- Phase 2: Verify dsp-guards=off leaves BOTH sites completely stock --- */
    select_build(b);
    set_board("AZ01");
    set_disable_conf("dsp-guards\n");
    apply_engine_edits(base, page);
    assert(eval_is_mult(is_mult, 300.0, 332.0 + 1e-12, 1, 1, 4.0) == 0);
    memset(&g_spy, 0, sizeof g_spy);
    g_spy.sync_period = 4.0;
    on_jump(&dummy_self, &dummy_info, live_wrap_delta);
    assert(g_spy.lead_reset_fired == 1);

    /* --- Phase 3: Enable dsp-guards on a PLAYER board (JP07 / SC5000) — S9 applies
     *     to all 16 units including standalone players! --- */
    set_board("JP07");
    set_disable_conf(NULL);
    apply_engine_edits(base, page);

    /* Patched Bug 1: +1 ULP, +1e-12, +1e-6, -1e-6 all pass symmetrically! */
    uint32_t m_ulp = eval_is_mult(is_mult, 300.0, nextafter(332.0, 333.0), 1, 1, 4.0);
    if (m_ulp != 1) {
        fprintf(stderr, "FAIL %s: isLengthMultipleOf(32.0 + 1 ULP, 4.0) returned %u (expected 1)\n",
                b->name, m_ulp);
        exit(1);
    }
    assert(eval_is_mult(is_mult, 300.0, 332.0 + 1e-12, 1, 1, 4.0) == 1);
    assert(eval_is_mult(is_mult, 300.0, 332.0 + 1e-6, 1, 1, 4.0) == 1);
    assert(eval_is_mult(is_mult, 300.0, 332.0 - 1e-6, 1, 1, 4.0) == 1);
    assert(eval_is_mult(is_mult, 300.0, 301.0 + 1e-12, 1, 1, 1.0) == 1);
    assert(eval_is_mult(is_mult, 300.0, 301.0 + 5e-6, 1, 1, 1.0) == 1);
    /* Genuinely non-multiple loops MUST still return 0! */
    assert(eval_is_mult(is_mult, 300.0, 332.5, 1, 1, 4.0) == 0);
    assert(eval_is_mult(is_mult, 300.0, 303.0, 1, 1, 4.0) == 0);
    assert(eval_is_mult(is_mult, 300.0, 332.0 + 1e-3, 1, 1, 4.0) == 0);
    assert(eval_is_mult(is_mult, 300.0, 301.0 + 1e-4, 1, 1, 1.0) == 0);
    /* Unset loop returns 1 (0 length, stock behavior preserved). */
    assert(eval_is_mult(is_mult, 300.0, 332.5, 0, 1, 4.0) == 1);

    /* Patched Bug 2: Live 8-bar loop wrap (-31.99999999999997) snaps to -32.0! */
    memset(&g_spy, 0, sizeof g_spy);
    g_spy.sync_period = 4.0;
    on_jump(&dummy_self, &dummy_info, live_wrap_delta);
    if (g_spy.lead_reset_fired != 0 || g_spy.follower_discontinuity != 0) {
        fprintf(stderr, "FAIL %s: SyncModifier::onJump(%.17g) fired reset=%d disc=%d\n",
                b->name, live_wrap_delta, g_spy.lead_reset_fired, g_spy.follower_discontinuity);
        exit(1);
    }
    assert(g_spy.calls == 1);
    assert(g_spy.last_self == &dummy_self);
    assert(g_spy.last_info == &dummy_info);
    assert(g_spy.last_delta == -32.0);
#if defined(__aarch64__)
    assert(g_spy.last_got_x2 == &g_fake_canary);
#endif

    /* Sample-quantized saved 8-bar loop wrap (±1 sample ≈ ±4.5e-5 beats @ 120 BPM): */
    double sample_deltas[] = {-32.000045, -31.999955, +4.00003, -16.00009, -8.0 + 1e-13};
    for (size_t i = 0; i < sizeof sample_deltas / sizeof *sample_deltas; i++) {
        memset(&g_spy, 0, sizeof g_spy);
        g_spy.sync_period = 4.0;
        on_jump(&dummy_self, &dummy_info, sample_deltas[i]);
        assert(g_spy.lead_reset_fired == 0);
        assert(g_spy.follower_discontinuity == 0);
    }

    /* 1-beat / 2-beat loop in Beat Sync (syncPeriod = 1.0): follower stays locked,
     * while Sync Lead still sees 1.0 mod 4.0 != 0 (bar phase truly shifted). */
    memset(&g_spy, 0, sizeof g_spy);
    g_spy.sync_period = 1.0;
    on_jump(&dummy_self, &dummy_info, -0.99999999999997);
    assert(g_spy.last_delta == -1.0);
    assert(g_spy.follower_discontinuity == 0);
    assert(g_spy.lead_reset_fired == 1);

    /* Genuine off-grid / fractional / zero / special jumps MUST pass through untouched! */
    double off_grid[] = {-0.5, -0.25, -32.0625, -32.005, 0.0, 0.00001, -0.49999};
    for (size_t i = 0; i < sizeof off_grid / sizeof *off_grid; i++) {
        memset(&g_spy, 0, sizeof g_spy);
        g_spy.sync_period = 4.0;
        on_jump(&dummy_self, &dummy_info, off_grid[i]);
        assert(g_spy.last_delta == off_grid[i]);
    }
    memset(&g_spy, 0, sizeof g_spy);
    g_spy.sync_period = 4.0;
    on_jump(&dummy_self, &dummy_info, NAN);
    assert(isnan(g_spy.last_delta));
    assert(g_spy.lead_reset_fired == 1);

    memset(&g_spy, 0, sizeof g_spy);
    g_spy.sync_period = 4.0;
    on_jump(&dummy_self, &dummy_info, INFINITY);
    assert(isinf(g_spy.last_delta));

    /* Idempotency: calling apply_engine_edits a second time must not double-wrap or crash. */
    apply_engine_edits(base, page);
    memset(&g_spy, 0, sizeof g_spy);
    g_spy.sync_period = 4.0;
    on_jump(&dummy_self, &dummy_info, live_wrap_delta);
    assert(g_spy.last_delta == -32.0);
    assert(g_spy.lead_reset_fired == 0);

    /* Byte-mismatch safety: corrupted stock bytes at both sites stay completely untouched. */
    uint8_t *bad = mmap(NULL, img_size, PROT_READ | PROT_WRITE,
                        MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(bad != MAP_FAILED);
    memset(bad + a->div_va, 0x55, 20);
    memset(bad + a->oj_va, 0xaa, 16);
    uint8_t expect_div[20], expect_oj[16];
    memset(expect_div, 0x55, sizeof expect_div);
    memset(expect_oj, 0xaa, sizeof expect_oj);
    apply_engine_edits((uintptr_t)bad, page);
    assert(memcmp(bad + a->div_va, expect_div, sizeof expect_div) == 0);
    assert(memcmp(bad + a->oj_va, expect_oj, sizeof expect_oj) == 0);

    munmap(bad, img_size);
    munmap(img, img_size);
    fclose(f);
    printf("PASS %s (%s): isLengthMultipleOf + SyncModifier::onJump verified\n", b->name, path);
}

int main(int argc, char **argv) {
    assert(argc >= 2);
    assert(mkdtemp(tmpdir) != NULL);
    snprintf(board_file, sizeof board_file, "%s/product", tmpdir);
    snprintf(conf_file, sizeof conf_file, "%s/hook-disable.conf", tmpdir);

    for (int i = 1; i < argc; i++) {
        test_real_binary(argv[i]);
    }

    unlink(board_file);
    unlink(conf_file);
    rmdir(tmpdir);
    return 0;
}
