/* #237: host-side budget for our DSP helpers, NOT Engine or deck certification.
 * gcc -std=c11 -O2 -Wall -Wextra -Werror -pthread tools/ui-mod/test_rt_budget.c -lm -o rt-budget
 * Also run test_dsp_smooth.c with ASan/UBSan and TSan. This harness has no worker
 * concurrency; its timings include cold/reset blocks and must not be averaged.
 * Vendor DSP, hook dispatch/routing, loader locks, faults and the tap consumer
 * need separate native/deck checks. No scheduler policy is changed here. */
#define _POSIX_C_SOURCE 200809L
#include <assert.h>
#include <math.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

static uint64_t now_ns(void)
{
    struct timespec t;
    assert(clock_gettime(CLOCK_MONOTONIC, &t) == 0);
    return (uint64_t)t.tv_sec * 1000000000u + (uint64_t)t.tv_nsec;
}

static void report(const char *path, unsigned frames, unsigned ms,
                   uint64_t budget, uint64_t maximum, unsigned misses)
{
    printf("%s frames=%u ms=%u budget_ns=%llu max_ns=%llu misses=%u\n",
           path, frames, ms, (unsigned long long)budget,
           (unsigned long long)maximum, misses);
}

static void report_switch_total(uint64_t total, unsigned blocks)
{
    printf("legacy-mode-switch-4-pairs total_ns=%llu blocks=%u\n",
           (unsigned long long)total, blocks);
}

/* live_tap.h also defines the non-RT FIFO worker. Audit it separately; only
 * sc_tap_push is exercised below. The pure DSP headers get a compile-time ban
 * on common allocation, locking, I/O and waiting APIs, including unused paths.
 * This is a source check, not interposition on libc or vendor callees. */
#include "live_tap.h"
#pragma GCC poison malloc calloc realloc free aligned_alloc posix_memalign
#pragma GCC poison pthread_mutex_lock pthread_mutex_trylock pthread_rwlock_rdlock pthread_rwlock_wrlock
#pragma GCC poison pthread_cond_wait pthread_cond_timedwait pthread_join sem_wait sem_timedwait
#pragma GCC poison open openat fopen read write close fprintf printf syscall ioctl
#pragma GCC poison mmap munmap poll select sleep usleep nanosleep clock_nanosleep clock_gettime
#include "dsp_smooth.h"
#include "master_limiter.h"
#include "echo_out.h"
#include "pioneer_master.h"
#include "pioneer_fx.h"

/* Match the hook's maxima for the optional master effects. Large buffers and
 * delay state live in BSS, never in an audio-thread stack frame. */
enum { PAIRS = 4, MAX_FRAMES = 8192, BLOCKS = 128 };
static float samples[PAIRS][2][MAX_FRAMES], dry[2][MAX_FRAMES];
static struct sc_echo echo[PAIRS];
static struct sc_master_pioneer_fx master[PAIRS];
static struct sc_pfx_ms timing[PAIRS];
static struct sc_gain gain[PAIRS];
static struct sc_lim limiter[PAIRS];
static struct sc_pfx_instance selector;
static struct sc_tap tap;
static unsigned char gate[0x300];
static char common;
static volatile float observed;

static void guards(unsigned frames)
{
    float *in[] = {dry[0], dry[1]};
    float *out[] = {samples[0][0], samples[0][1]};
    sc_dsp_fx_mark_transition(&common, SC_DSP_FX_TRANSITION_CHANGE);
    sc_dsp_sampler_mark_event(&common, SC_DSP_SAMPLER_PLAY);
    sc_dsp_autogate_pre_process(gate, frames);
    float energy = sc_dsp_block_energy(in, frames);
    sc_dsp_sweep_fx_post_process(&common, out, frames);
    sc_dsp_makeup_apply(&common, out, frames, energy);
    struct sc_dsp_chain *chain = sc_dsp_chain_enter(&common);
    sc_dsp_fx_common_post_process(chain, in, out, frames, frames);
    sc_dsp_chain_exit(chain);
}

static void master_block(unsigned frames, int mode, unsigned ms)
{
    for (unsigned p = 0; p < PAIRS; ++p) {
        float *l = samples[p][0], *r = samples[p][1];
        sc_echo_planar(&echo[p], l, r, frames, 1);
        if ((ms && sc_ms_effect(mode)) || timing[p].blend > 0.0f) {
            for (unsigned i = 0; i < frames; ++i) {
                float dl = l[i], dr = r[i];
                sc_master_pioneer_fx_planar(&master[p], &l[i], &r[i], 1, mode);
                sc_ms_mix(&timing[p], dl, dr, &l[i], &r[i], mode, ms, 0.5f, 1);
            }
        } else {
            sc_master_pioneer_fx_planar(&master[p], l, r, frames, mode);
        }
        sc_gain_planar(&gain[p], l, r, frames);
        sc_lim_planar(&limiter[p], l, r, frames);
    }
    sc_tap_push(&tap, samples[0][0], samples[0][1], frames);
}

int main(void)
{
    const unsigned sizes[] = {64, 128, 256, 512, 1024, 4096, MAX_FRAMES};
    unsigned failures = 0;
    /* Worker preparation stays outside the measured audio blocks. */
    for (unsigned p = 0; p < PAIRS; ++p) sc_ms_prepare(&timing[p]);
    tap.on = 1;
    for (unsigned n = 0; n < sizeof sizes / sizeof *sizes; ++n) {
        unsigned frames = sizes[n];
        uint64_t budget = (uint64_t)frames * 1000000000u / 96000u;
        for (unsigned path = 0; path < 3; ++path) {
            for (unsigned ms_case = 0; ms_case < (path ? 3u : 1u); ++ms_case) {
                unsigned ms = ms_case == 0 ? 0 : (ms_case == 1 ? 10 : 4000);
                uint64_t maximum = 0;
                unsigned misses = 0;
                for (unsigned block = 0; block < BLOCKS; ++block) {
                    /* Fresh finite input; setup and worker clears are not timed. */
                    for (unsigned i = 0; i < frames; ++i) {
                        float x = 0.2f * sinf((float)(block * frames + i) * 0.065f);
                        dry[0][i] = x; dry[1][i] = -x;
                        for (unsigned p = 0; p < PAIRS; ++p) {
                            samples[p][0][i] = x; samples[p][1][i] = -x;
                        }
                    }
                    #ifndef T4_BASELINE
                    if (path == 1) for (unsigned p = 0; p < PAIRS; ++p)
                        sc_master_pioneer_fx_prepare(&master[p]);
#endif
                    int mode = (int)((block / 8) % 6); /* OFF, all modes, transitions */
                    if (path == 2) {
                        selector.time_ms = ms;
                        sc_pfx_instance_prepare(&selector);
                        sc_ms_prepare(&selector.ms);
                    }
                    uint64_t start = now_ns();
                    if (path == 0) guards(frames);
                    else if (path == 1) master_block(frames, mode, ms);
                    else (void)sc_pfx_process(&selector, samples[0][0], samples[0][1],
                                             frames, mode, 0.5f, 0.5f, 1.0f);
                    uint64_t elapsed = now_ns() - start;
                    if (elapsed > maximum) maximum = elapsed;
                    if (elapsed > budget) ++misses;
                    /* Keep the computation observable; inspect outside the budget. */
                    for (unsigned i = 0; i < frames; ++i) {
                        assert(sc_dsp_finite_f32(samples[0][0][i]));
                        assert(sc_dsp_finite_f32(samples[0][1][i]));
                    }
                    observed = samples[0][0][frames - 1];
                }
                report(path == 0 ? "guards" : (path == 1 ? "master-4-pairs" : "selector-1-pair"),
                       frames, ms, budget, maximum, misses);
                failures += misses;
            }
        }
    }
    uint64_t switch_max = 0, switch_total = 0;
    unsigned switch_misses = 0;
    for (unsigned block = 0; block < BLOCKS; ++block) {
#ifndef T4_BASELINE
        for (unsigned p = 0; p < PAIRS; ++p) sc_master_pioneer_fx_prepare(&master[p]);
#endif
        uint64_t start = now_ns();
        for (unsigned p = 0; p < PAIRS; ++p)
            sc_master_pioneer_fx_planar(&master[p], samples[p][0], samples[p][1], 1,
                                        1 + block % 5);
        uint64_t elapsed = now_ns() - start;
        switch_total += elapsed;
        if (elapsed > switch_max) switch_max = elapsed;
        if (elapsed > 1000000000u / 96000u) ++switch_misses;
    }
    report("legacy-mode-switch-4-pairs", 1, 0, 1000000000u / 96000u, switch_max, switch_misses);
    /* Total supplements the maximum when other host tasks preempt a sample. */
    report_switch_total(switch_total, BLOCKS);
    return failures ? EXIT_FAILURE : EXIT_SUCCESS;
}
