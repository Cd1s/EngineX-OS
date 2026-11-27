/* #336 S12: RecorderDsp ring buffer expansion (4.0 s -> 24.0 s) against USB write stalls.
 *
 * Build & run on ARM build host (native aarch64 and armhf):
 *   gcc -std=gnu11 -O2 -Wall -Wextra -Werror -U_FORTIFY_SOURCE tools/ui-mod/test_s12_recorder_buffer.c -ldl -lpthread -lm -o /tmp/test_s12_64
 *   /tmp/test_s12_64 /root/build/sc5000-lab/systemone-analysis/rootfs/usr/Engine/Engine
 *   arm-linux-gnueabihf-gcc -std=gnu11 -O2 -Wall -Wextra -Werror -U_FORTIFY_SOURCE tools/ui-mod/test_s12_recorder_buffer.c -ldl -lpthread -lm -o /tmp/test_s12_32
 *   /tmp/test_s12_32 /root/build/sc5000-lab/5.0.4/tree/usr/Engine/Engine \
 *                    /root/build/sc5000-lab/5.3.0/tree/usr/Engine/Engine \
 *                    /root/build/sc5000-lab/sclive4-rea61-followup/rootfs/usr/Engine/Engine
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

static char tmpdir[] = "/tmp/s12-recbuf-XXXXXX";
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

/* Exact stock RecorderDsp::start ring-buffer formula (5.3.0 0xd7dae8 / AZ04 0xe56bcc):
 *   capacity = (uint32_t)(ceilf((float)(channels * 4) * buffer_seconds * sample_rate) + 12.0f)
 * Exact stock RecorderDsp::process (0xcb5688..0xcb56bc):
 *   each audio callback pushes 12-byte header + (frames * channels * 4) payload bytes;
 *   if (capacity - used < needed) -> m_overrun = 1 -> writer thread aborts with
 *   errorNumber = 0 ("Recording was stopped because drive speed is too slow to record.").
 */
struct rec_sim_result {
    uint32_t capacity;
    uint32_t peak_used;
    int overrun;
    double overrun_at_sec;
};

static struct rec_sim_result simulate_usb_stall(float buffer_seconds, uint32_t sleep_ms,
                                                float sample_rate, int channels,
                                                int block_frames, double stall_sec) {
    float bytes_f = ceilf((float)(channels * 4) * buffer_seconds * sample_rate) + 12.0f;
    uint32_t capacity = (uint32_t)bytes_f;
    uint32_t payload = (uint32_t)(block_frames * channels * 4);
    uint32_t pkt_bytes = payload + 12u;
    double cb_dt = (double)block_frames / (double)sample_rate;
    double wake_dt = (double)sleep_ms / 1000.0;

    /* Run 2 s steady-state, then a continuous USB flash FTL GC / writeback stall of
     * `stall_sec` seconds (e.g. SLC-cache fold at ~10 min), then 3 s recovery drain. */
    double stall_start = 2.0;
    double stall_end = stall_start + stall_sec;
    double total_sec = stall_end + 3.0;

    uint32_t used = 0, peak = 0;
    double next_wake = wake_dt;
    int overrun = 0;
    double overrun_at = -1.0;

    for (double t = cb_dt; t <= total_sec; t += cb_dt) {
        /* Audio callback (RecorderDsp::process) */
        if (capacity - used < pkt_bytes) {
            overrun = 1;
            overrun_at = t - stall_start;
            break;
        }
        used += pkt_bytes;
        if (used > peak) peak = used;

        /* Writer thread (RecorderDsp::writerLoop): blocked during [stall_start, stall_end);
         * once unblocked, drains the entire queued ring buffer in one burst (0xcd6f3c..0xcd76dc). */
        if (t >= next_wake) {
            if (t < stall_start || t >= stall_end) {
                /* Writer drains full burst at conservative 10 MiB/s USB speed. */
                used = 0;
                next_wake = t + wake_dt;
            } else {
                next_wake = stall_end;
            }
        }
    }

    struct rec_sim_result r = {capacity, peak, overrun, overrun_at};
    return r;
}

/* Execute the exact machine-code instruction sequence from RecorderDsp::RecorderDsp
 * natively on the ARM CPU to decode (m_bufferSeconds, m_sleepMs). */
static void exec_ctor_insn_pair(const uchar *code, float *out_secs, uint32_t *out_sleep_ms) {
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    uchar *stub = mmap(NULL, page, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(stub != MAP_FAILED);
#if defined(__aarch64__)
    /* AArch64 (5.3.0-az04 0x19ba0f0): mov x0, #imm16, lsl #16; movk x0, #0x64, lsl #32; ret */
    static const uint32_t ret_insn = 0xd65f03c0u;
    memcpy(stub, code, 8);
    memcpy(stub + 8, &ret_insn, 4);
    size_t sz = 12;
#elif defined(__arm__)
    /* ARMv7 (5.0.4/5.3.0/5.3.0-sclive4): mov r1, #100; mov r2, #0; movt r2, #imm16; mov r0, r2; bx lr
     * Returns uint64_t in {r0 = m_bufferSeconds bits, r1 = m_sleepMs}. */
    static const uint32_t tail[2] = {0xe1a00002u, 0xe12fff1eu};
    memcpy(stub, code, 12);
    memcpy(stub + 12, tail, 8);
    size_t sz = 20;
#else
#error "test_s12_recorder_buffer.c must run natively on armhf or aarch64"
#endif
    assert(mprotect(stub, page, PROT_READ | PROT_EXEC) == 0);
    __builtin___clear_cache((char *)stub, (char *)stub + sz);
    uint64_t (*fn)(void);
    memcpy(&fn, &stub, sizeof fn);
    uint64_t res = fn();
    munmap(stub, page);

    uint32_t bits = (uint32_t)(res & 0xffffffffu);
    memcpy(out_secs, &bits, sizeof *out_secs);
    *out_sleep_ms = (uint32_t)(res >> 32);
}

static uint32_t expected_site_for_build(const char *name) {
#if defined(__aarch64__)
    if (!strcmp(name, "5.3.0-az04")) return 0x19ba0f0u;
#else
    if (!strcmp(name, "5.0.4")) return 0x18356b0u;
    if (!strcmp(name, "5.3.0")) return 0x1987bc0u;
    if (!strcmp(name, "5.3.0-sclive4")) return 0x19ea77cu;
#endif
    return 0;
}

static uchar *map_elf_copy(const char *path, size_t *out_len) {
    FILE *f = fopen(path, "rb");
    assert(f);
    assert(fseek(f, 0, SEEK_END) == 0);
    long sz = ftell(f);
    assert(sz > 0);
    rewind(f);
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    size_t len = ((size_t)sz + page - 1) / page * page;
    uchar *img = mmap(NULL, len, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(img != MAP_FAILED);
    assert(fread(img, 1, (size_t)sz, f) == (size_t)sz);
    fclose(f);
    *out_len = len;
    return img;
}

static void select_elf_build(uchar *img) {
    ElfW(Ehdr) *hdr = (void *)img;
    struct dl_phdr_info info = {
        .dlpi_addr = (uintptr_t)img,
        .dlpi_phdr = (void *)(img + hdr->e_phoff),
        .dlpi_phnum = hdr->e_phnum,
    };
    uintptr_t found = 0;
    build = NULL;
    engine_base(&info, sizeof info, &found);
    assert(found == (uintptr_t)img && build != NULL);
}

static void verify_engine_binary(const char *path) {
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    size_t len = 0;

    /* 1. Verify stock binary executes 4.0 s / 100 ms and fails on a 6 s USB stall. */
    uchar *img = map_elf_copy(path, &len);
    select_elf_build(img);
    uint32_t site = expected_site_for_build(build->name);
    assert(site != 0);

    float secs_before = 0.0f;
    uint32_t sleep_before = 0;
    exec_ctor_insn_pair(img + site, &secs_before, &sleep_before);
    assert(secs_before == 4.0f && sleep_before == 100u);

    struct rec_sim_result sim_before = simulate_usb_stall(secs_before, sleep_before, 48000.0f, 2, 256, 6.0);
    assert(sim_before.overrun == 1);
    assert(sim_before.overrun_at_sec >= 3.8 && sim_before.overrun_at_sec <= 4.1);

    /* 2. On recording-capable console (JC11/JC11S/JP21/JP24) with dsp-guards enabled,
     *    apply_engine_edits() must expand m_bufferSeconds from 4.0 s -> 24.0 s while
     *    preserving m_sleepMs == 100 ms, and survive 6 s / 12 s / 20 s stalls. */
    set_board("JP21");
    set_disable_conf(NULL);
    apply_engine_edits((uintptr_t)img, page);

    float secs_after = 0.0f;
    uint32_t sleep_after = 0;
    exec_ctor_insn_pair(img + site, &secs_after, &sleep_after);
    assert(secs_after == 24.0f);
    assert(sleep_after == 100u);

    /* Calling apply_engine_edits() a second time is idempotent. */
    apply_engine_edits((uintptr_t)img, page);
    exec_ctor_insn_pair(img + site, &secs_after, &sleep_after);
    assert(secs_after == 24.0f && sleep_after == 100u);

    const float rates[2] = {44100.0f, 48000.0f};
    const double pass_stalls[3] = {6.0, 12.0, 20.0};
    for (int ri = 0; ri < 2; ri++) {
        for (int si = 0; si < 3; si++) {
            struct rec_sim_result r = simulate_usb_stall(secs_after, sleep_after, rates[ri], 2, 256, pass_stalls[si]);
            assert(r.overrun == 0);
        }
        /* Genuinely dead drive (> 24 s) still cleanly aborts instead of unbounded memory growth. */
        struct rec_sim_result dead = simulate_usb_stall(secs_after, sleep_after, rates[ri], 2, 256, 25.0);
        assert(dead.overrun == 1);
        assert(dead.overrun_at_sec >= 23.0 && dead.overrun_at_sec <= 24.1);
    }
    munmap(img, len);

    /* 3. Standalone player boards (JP07, JP13) have no recorder: site must stay stock 4.0 s. */
    const char *players[2] = {"JP07", "JP13"};
    for (int pi = 0; pi < 2; pi++) {
        img = map_elf_copy(path, &len);
        select_elf_build(img);
        set_board(players[pi]);
        set_disable_conf(NULL);
        apply_engine_edits((uintptr_t)img, page);
        exec_ctor_insn_pair(img + site, &secs_after, &sleep_after);
        assert(secs_after == 4.0f && sleep_after == 100u);
        munmap(img, len);
    }

    /* 4. Disabling dsp-guards in hook-disable.conf must leave the site stock 4.0 s. */
    img = map_elf_copy(path, &len);
    select_elf_build(img);
    set_board("JP21");
    set_disable_conf("dsp-guards\n");
    apply_engine_edits((uintptr_t)img, page);
    exec_ctor_insn_pair(img + site, &secs_after, &sleep_after);
    assert(secs_after == 4.0f && sleep_after == 100u);
    munmap(img, len);

    /* 5. A mismatched byte in the surrounding context must be safely refused. */
    img = map_elf_copy(path, &len);
    select_elf_build(img);
    set_board("JP21");
    set_disable_conf(NULL);
    uchar saved[12];
    memcpy(saved, img + site, sizeof saved);
    img[site + 4] ^= 0x01u;
    uchar corrupted[12];
    memcpy(corrupted, img + site, sizeof corrupted);
    apply_engine_edits((uintptr_t)img, page);
    assert(memcmp(img + site, corrupted, sizeof corrupted) == 0);
    munmap(img, len);

    printf("S12 %s (%s): stock 4.0s (overrun@%.2fs) -> patched 24.0s (6s/12s/20s PASS, 25s bounded, player/off/mismatch untouched): PASS\n",
           build->name, path, sim_before.overrun_at_sec);
}

int main(int argc, char **argv) {
    assert(mkdtemp(tmpdir) != NULL);
    snprintf(board_file, sizeof board_file, "%s/product-code", tmpdir);
    snprintf(conf_file, sizeof conf_file, "%s/hook-disable.conf", tmpdir);

    /* Always verify simulation model math even when invoked without binary args. */
    struct rec_sim_result s4 = simulate_usb_stall(4.0f, 100u, 48000.0f, 2, 256, 6.0);
    struct rec_sim_result s24 = simulate_usb_stall(24.0f, 100u, 48000.0f, 2, 256, 20.0);
    assert(s4.capacity == 1536012u && s4.overrun == 1);
    assert(s24.capacity == 9216012u && s24.overrun == 0);

    for (int i = 1; i < argc; i++) verify_engine_binary(argv[i]);

    unlink(board_file);
    unlink(conf_file);
    rmdir(tmpdir);
    puts("ALL S12 RECORDER BUFFER TESTS PASSED");
    return 0;
}
