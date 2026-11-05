/* Real-time safe smoothing and state guards for Engine's FX / Key Lock paths.
 *
 * Header-only C11. Nothing here allocates, locks or makes a system call: the
 * hooks that include it run inside the 96 kHz audio callback (SCHED_RR 49).
 * Shared state is one static block per translation unit, touched only through
 * __atomic builtins, so the UI thread can arm a transition while the audio
 * thread consumes it. Include it from a single translation unit (the hook);
 * a second one would get its own, unconnected state.
 */
#ifndef SC5000_DSP_SMOOTH_H
#define SC5000_DSP_SMOOTH_H

#include <stdint.h>
#include <stddef.h>
#include <string.h>

enum {
    SC_DSP_FX_TRANSITION_TURN_ON  = 1,
    SC_DSP_FX_TRANSITION_TURN_OFF = 2,
    SC_DSP_FX_TRANSITION_SWITCH   = 3,
    SC_DSP_FX_TRANSITION_CHANGE   = 4,
    SC_DSP_FX_TRANSITION_ENABLE   = 5,
    SC_DSP_RAMP_FRAMES            = 128  /* ~1.33ms @ 96kHz */
};

#define SC_DSP_AUTOGATE_SLOTS 8
#define SC_DSP_CHAIN_SLOTS 32
#define SC_DSP_CHAIN_STALE_GENERATIONS 8
#define SC_DSP_AUTOGATE_PHASE_OFFSET 0x1e8
/* Emergency ceiling, retaining 18 dB of headroom above normalized full scale.
 * This is an output guard, not a limiter or a reset of vendor feedback state. */
#define SC_DSP_SWEEP_SAMPLE_LIMIT 8.0f
/* Flanger makeup gain (#54): at most +6 dB, never below unity, following the
 * level over ~100 ms (9600 frames at the deck's 96 kHz). */
#define SC_DSP_MAKEUP_SLOTS 8
#define SC_DSP_MAKEUP_MAX 2.0f
#define SC_DSP_MAKEUP_TAU_FRAMES 9600.0f

enum {
    SC_DSP_SAMPLER_STOP_IMMEDIATELY = 1,
    SC_DSP_SAMPLER_STOP = 2,
    SC_DSP_SAMPLER_PLAY = 3
};

/* De-click ramp of one FX common tail (one per effect; Engine runs several on
 * different audio threads, and nothing proves one common stays on one thread).
 * Every field is accessed with __atomic builtins; a transition never writes
 * them, it only bumps the global generation. generation and ramp_remaining
 * belong to whoever holds `busy` (sc_dsp_chain_enter .. sc_dsp_chain_exit). */
struct sc_dsp_chain {
    void *self;                   /* common tail instance, NULL when free; published last */
    uint32_t generation;          /* FX generation it last synced to */
    int32_t ramp_remaining;       /* frames left in its de-click ramp */
    uint32_t busy;                /* 1 while a call owns the slot; only ever try-acquired */
};

struct sc_dsp_state {
    uint32_t generation;          /* bumped by every FX transition */
    uint32_t sampler_generation;  /* observation only; independent of FX ramps */
    uint32_t next_evict;          /* round-robin victim when the AutoGate table is full */
    struct {
        void *self;               /* AutoGate instance, NULL when free */
        uint32_t generation;      /* generation it last synced to */
    } autogate[SC_DSP_AUTOGATE_SLOTS];
    struct sc_dsp_chain chain[SC_DSP_CHAIN_SLOTS];
    uint32_t makeup_evict;        /* round-robin victim when the makeup table is full */
    struct sc_dsp_makeup {
        void *self;               /* Flanger instance, NULL when free */
        uint32_t busy;            /* try-acquired by the block that uses the slot */
        float dry, wet, gain;     /* smoothed block energies and the gain applied last */
    } makeup[SC_DSP_MAKEUP_SLOTS];
};

static inline struct sc_dsp_state *sc_dsp_state(void)
{
    static struct sc_dsp_state state;
    return &state;
}

/* 1. Finite checks on the bit pattern, so -ffast-math cannot fold them away. */
static inline int sc_dsp_finite_f32(float v)
{
    uint32_t bits;
    memcpy(&bits, &v, sizeof bits);
    return (bits & 0x7f800000u) != 0x7f800000u;
}

static inline int sc_dsp_finite_f64(double v)
{
    uint64_t bits;
    memcpy(&bits, &v, sizeof bits);
    return (bits & 0x7ff0000000000000ull) != 0x7ff0000000000000ull;
}

static inline void sc_dsp_sampler_mark_event(const void *sampler_self, uint32_t kind)
{
    (void)sampler_self; /* aggregate event count, not per-voice completion state */
    if (kind < SC_DSP_SAMPLER_STOP_IMMEDIATELY || kind > SC_DSP_SAMPLER_PLAY)
        return;
    __atomic_add_fetch(&sc_dsp_state()->sampler_generation, 1u, __ATOMIC_RELEASE);
}

/* SweepFx processes input in place, even when its output argument differs.
 * No allocation, logging, floating-point classification library or shared FX
 * transition state is needed for this final stereo output guard. */
static inline void sc_dsp_sweep_fx_post_process(void *sweep_self,
                                               float *const *buffers,
                                               uint32_t frames)
{
    (void)sweep_self;
    if (!buffers || !frames)
        return;
    for (unsigned ch = 0; ch < 2; ch++) {
        float *samples = buffers[ch];
        if (!samples)
            continue;
        for (uint32_t i = 0; i < frames; i++) {
            float value = samples[i];
            if (!sc_dsp_finite_f32(value)) value = 0.0f;
            else if (value > SC_DSP_SWEEP_SAMPLE_LIMIT) value = SC_DSP_SWEEP_SAMPLE_LIMIT;
            else if (value < -SC_DSP_SWEEP_SAMPLE_LIMIT) value = -SC_DSP_SWEEP_SAMPLE_LIMIT;
            samples[i] = value;
        }
    }
}

/* Unaligned-safe reads of a control parameter inside a closed-source object.
 * A NULL base or a NaN/Inf value reads as 0, so it never reaches DSP state. */
static inline float sc_dsp_read_f32(const void *base, size_t byte_offset)
{
    float v = 0.0f;
    if (base)
        memcpy(&v, (const char *)base + byte_offset, sizeof v);
    return sc_dsp_finite_f32(v) ? v : 0.0f;
}

static inline double sc_dsp_read_f64(const void *base, size_t byte_offset)
{
    double v = 0.0;
    if (base)
        memcpy(&v, (const char *)base + byte_offset, sizeof v);
    return sc_dsp_finite_f64(v) ? v : 0.0;
}

/* 2. FX transitions. Any thread may mark one; every common tail ramps once. */
static inline void sc_dsp_fx_mark_transition(const void *fx_owner, uint32_t kind)
{
    /* shortcut: any FX event ramps every common tail once, because the owner
     * (ChannelDJFx) has no verified mapping to a common (effect+0x68) object;
     * make it precise once that mapping is reversed. */
    (void)fx_owner;
    if (kind < SC_DSP_FX_TRANSITION_TURN_ON || kind > SC_DSP_FX_TRANSITION_ENABLE)
        return;
    __atomic_add_fetch(&sc_dsp_state()->generation, 1u, __ATOMIC_RELEASE);
}

static inline int sc_dsp_chain_try(struct sc_dsp_chain *c)
{
    return __atomic_exchange_n(&c->busy, 1u, __ATOMIC_ACQUIRE) == 0;
}

static inline void sc_dsp_chain_exit(struct sc_dsp_chain *chain)
{
    if (chain)
        __atomic_store_n(&chain->busy, 0u, __ATOMIC_RELEASE);
}

/* Finds (or claims) the ramp slot of a common tail and arms its ramp when an
 * FX transition happened since it last ran. Unlike AutoGate, a common seen for
 * the first time is ramped too: a new common is usually the effect that was
 * just turned on, and skipping it would lose the turn-on de-click.
 *
 * The caller owns the slot until sc_dsp_chain_exit(), so call enter once per
 * block, before the vendor code, and exit after the last use. Ownership is a
 * per-slot busy flag that is only try-acquired: nothing here waits or spins.
 * NULL (just sanitize this block, never ramp) means: no common, the same
 * common already in flight on another thread, or no free/stale slot. A live
 * slot is never taken over: a slot whose common is not in flight is reclaimed
 * only once it is SC_DSP_CHAIN_STALE_GENERATIONS FX events behind (a live
 * common re-syncs every block), and busy keeps its owner out meanwhile.
 * A slot is initialised before `self` is published, so no other thread sees a
 * half-built one. shortcut: two threads registering the same new common at the
 * same moment can take two slots; the spare goes stale and is reclaimed. */
static inline struct sc_dsp_chain *sc_dsp_chain_enter(void *common)
{
    struct sc_dsp_state *s = sc_dsp_state();
    uint32_t gen = __atomic_load_n(&s->generation, __ATOMIC_ACQUIRE);
    if (!common)
        return NULL;
    for (int i = 0; i < SC_DSP_CHAIN_SLOTS; i++) {
        struct sc_dsp_chain *c = &s->chain[i];
        if (__atomic_load_n(&c->self, __ATOMIC_ACQUIRE) != common)
            continue;
        if (!sc_dsp_chain_try(c))
            return NULL; /* same common overlapping itself */
        if (__atomic_load_n(&c->self, __ATOMIC_ACQUIRE) != common) { /* taken over meanwhile */
            sc_dsp_chain_exit(c);
            continue;
        }
        if (__atomic_load_n(&c->generation, __ATOMIC_RELAXED) != gen) {
            __atomic_store_n(&c->generation, gen, __ATOMIC_RELAXED);
            __atomic_store_n(&c->ramp_remaining, (int32_t)SC_DSP_RAMP_FRAMES, __ATOMIC_RELAXED);
        }
        return c;
    }
    for (int pass = 0; pass < 2; pass++) { /* 0: free slots, 1: stale slots */
        for (int i = 0; i < SC_DSP_CHAIN_SLOTS; i++) {
            struct sc_dsp_chain *c = &s->chain[i];
            void *old = __atomic_load_n(&c->self, __ATOMIC_ACQUIRE);
            if (pass == 0 ? old != NULL : (old == NULL ||
                (int32_t)(gen - __atomic_load_n(&c->generation, __ATOMIC_RELAXED)) < SC_DSP_CHAIN_STALE_GENERATIONS))
                continue;
            if (!sc_dsp_chain_try(c))
                continue;
            /* Held: re-check, the slot may have been claimed or refreshed since. */
            old = __atomic_load_n(&c->self, __ATOMIC_ACQUIRE);
            if (pass == 0 ? old != NULL : (old == NULL ||
                (int32_t)(gen - __atomic_load_n(&c->generation, __ATOMIC_RELAXED)) < SC_DSP_CHAIN_STALE_GENERATIONS)) {
                sc_dsp_chain_exit(c);
                continue;
            }
            __atomic_store_n(&c->generation, gen, __ATOMIC_RELAXED);
            __atomic_store_n(&c->ramp_remaining, (int32_t)SC_DSP_RAMP_FRAMES, __ATOMIC_RELAXED);
            __atomic_store_n(&c->self, common, __ATOMIC_RELEASE); /* publish last */
            return c;
        }
    }
    return NULL;
}

/* Clears AutoGate's phase (float at +0x1e8) once after every FX transition,
 * so a switched or re-opened gate does not resume an old phase (LA-020). An
 * instance seen for the first time only records the generation: there is no
 * transition it could have missed. With more than 8 live instances the oldest
 * slot is reused; the evicted one is treated as new when it returns. */
static inline void sc_dsp_autogate_pre_process(void *autogate_self, uint32_t frames)
{
    struct sc_dsp_state *s = sc_dsp_state();
    uint32_t gen = __atomic_load_n(&s->generation, __ATOMIC_ACQUIRE);
    int free_slot = -1;
    (void)frames;
    if (!autogate_self)
        return;
    for (int i = 0; i < SC_DSP_AUTOGATE_SLOTS; i++) {
        void *self = __atomic_load_n(&s->autogate[i].self, __ATOMIC_ACQUIRE);
        if (self == autogate_self) {
            if (__atomic_load_n(&s->autogate[i].generation, __ATOMIC_RELAXED) != gen) {
                const float zero = 0.0f;
                memcpy((char *)autogate_self + SC_DSP_AUTOGATE_PHASE_OFFSET, &zero, sizeof zero);
                __atomic_store_n(&s->autogate[i].generation, gen, __ATOMIC_RELAXED);
            }
            return;
        }
        if (!self && free_slot < 0)
            free_slot = i;
    }
    if (free_slot < 0)
        free_slot = (int)(__atomic_fetch_add(&s->next_evict, 1u, __ATOMIC_RELAXED) % SC_DSP_AUTOGATE_SLOTS);
    __atomic_store_n(&s->autogate[free_slot].generation, gen, __ATOMIC_RELAXED);
    __atomic_store_n(&s->autogate[free_slot].self, autogate_self, __ATOMIC_RELEASE);
}

/* Runs on the stereo FX mix tail just before it returns: replaces NaN/Inf
 * output samples with the (finite) dry input or silence, then, while this
 * chain's transition ramp is armed, fades each frame from dry to processed so
 * an FX switch never steps. Only the first dry_valid_frames frames of input are read
 * (the ramp is 128 frames, so a caller that must snapshot an aliased input
 * needs no more than that); later frames fall back to silence. In-place
 * processing (input[0] == output[0]) has no separate dry signal and is only
 * sanitized. dry_valid_frames is clamped to frames; a NULL chain never ramps. */
static inline void sc_dsp_fx_common_post_process(struct sc_dsp_chain *chain,
                                                 float *const *input,
                                                 float *const *output,
                                                 uint32_t frames,
                                                 uint32_t dry_valid_frames)
{
    if (!output || !frames)
        return;
    if (dry_valid_frames > frames)
        dry_valid_frames = frames;
    for (int ch = 0; ch < 2; ch++) {
        float *out = output[ch];
        const float *in = input ? input[ch] : NULL;
        if (!out)
            continue;
        for (uint32_t i = 0; i < frames; i++) {
            if (!sc_dsp_finite_f32(out[i]))
                out[i] = in && i < dry_valid_frames && sc_dsp_finite_f32(in[i]) ? in[i] : 0.0f;
        }
    }
    if (!chain)
        return;
    int32_t left = __atomic_load_n(&chain->ramp_remaining, __ATOMIC_RELAXED);
    if (left <= 0 || !input || !input[0] || !input[1] || !output[0] || !output[1] || input[0] == output[0])
        return;
    for (uint32_t i = 0; i < dry_valid_frames && left > 0; i++, left--) {
        const float w = (float)(SC_DSP_RAMP_FRAMES - left) / (float)SC_DSP_RAMP_FRAMES;
        for (int ch = 0; ch < 2; ch++) {
            const float dry = sc_dsp_finite_f32(input[ch][i]) ? input[ch][i] : 0.0f;
            output[ch][i] = dry + (output[ch][i] - dry) * w;
        }
    }
    __atomic_store_n(&chain->ramp_remaining, left, __ATOMIC_RELAXED);
}

/* Mean square of a stereo block; non-finite samples count as silence. */
static inline float sc_dsp_block_energy(float *const *buf, uint32_t frames)
{
    if (!buf || !buf[0] || !buf[1] || !frames)
        return 0.0f;
    float sum = 0.0f;
    for (int ch = 0; ch < 2; ch++)
        for (uint32_t i = 0; i < frames; i++)
            if (sc_dsp_finite_f32(buf[ch][i]))
                sum += buf[ch][i] * buf[ch][i];
    return sum / (float)(2u * frames);
}

/* Engine's Flanger mixes 0.7 x dry with its delay line in place, so engaging it
 * drops the level by 3 dB and more in the comb notches (#54). This brings the
 * processed block's smoothed RMS back up to the dry input's: dry_energy is
 * sc_dsp_block_energy() of the block before the vendor process, buf the block
 * after it. The gain moves linearly across each block (no zipper noise), is
 * found by one Newton step a block (no libm in the hook), stays in
 * [1, SC_DSP_MAKEUP_MAX] and the result keeps the SweepFx output ceiling.
 * Per-instance state lives in a small table; a busy or unclaimable slot leaves
 * the block untouched. shortcut: the time constant assumes 96 kHz. */
static inline void sc_dsp_makeup_apply(void *self, float *const *buf, uint32_t frames, float dry_energy)
{
    struct sc_dsp_state *s = sc_dsp_state();
    struct sc_dsp_makeup *m = NULL;
    if (!self || !buf || !buf[0] || !buf[1] || !frames || !sc_dsp_finite_f32(dry_energy))
        return;
    for (int i = 0; i < SC_DSP_MAKEUP_SLOTS && !m; i++)
        if (__atomic_load_n(&s->makeup[i].self, __ATOMIC_ACQUIRE) == self)
            m = &s->makeup[i];
    if (m) {
        if (__atomic_exchange_n(&m->busy, 1u, __ATOMIC_ACQUIRE))
            return;
        if (__atomic_load_n(&m->self, __ATOMIC_ACQUIRE) != self) /* evicted meanwhile */
            goto out;
    } else {
        for (int i = 0; i < SC_DSP_MAKEUP_SLOTS && !m; i++)
            if (!__atomic_load_n(&s->makeup[i].self, __ATOMIC_ACQUIRE))
                m = &s->makeup[i];
        if (!m)
            m = &s->makeup[__atomic_fetch_add(&s->makeup_evict, 1u, __ATOMIC_RELAXED) % SC_DSP_MAKEUP_SLOTS];
        if (__atomic_exchange_n(&m->busy, 1u, __ATOMIC_ACQUIRE))
            return;
        m->dry = m->wet = 0.0f;
        m->gain = 1.0f;
        __atomic_store_n(&m->self, self, __ATOMIC_RELEASE); /* publish last */
    }
    {
        float a = (float)frames / SC_DSP_MAKEUP_TAU_FRAMES;
        if (a > 1.0f) a = 1.0f;
        m->dry += a * (dry_energy - m->dry);
        m->wet += a * (sc_dsp_block_energy(buf, frames) - m->wet);
        float g0 = m->gain, target = g0;
        if (m->wet > 1e-10f && m->dry > 1e-10f) {
            float ratio = m->dry / m->wet; /* target gain squared */
            if (ratio > SC_DSP_MAKEUP_MAX * SC_DSP_MAKEUP_MAX) ratio = SC_DSP_MAKEUP_MAX * SC_DSP_MAKEUP_MAX;
            if (ratio < 1.0f) ratio = 1.0f;
            target = 0.5f * (g0 + ratio / g0);
        }
        float g1 = g0 + a * (target - g0);
        if (!sc_dsp_finite_f32(g1) || g1 < 1.0f) g1 = 1.0f;
        if (g1 > SC_DSP_MAKEUP_MAX) g1 = SC_DSP_MAKEUP_MAX;
        for (uint32_t i = 0; i < frames; i++) {
            const float g = g0 + (g1 - g0) * (float)(i + 1) / (float)frames;
            for (int ch = 0; ch < 2; ch++) {
                float v = buf[ch][i] * g;
                if (!sc_dsp_finite_f32(v)) v = 0.0f;
                else if (v > SC_DSP_SWEEP_SAMPLE_LIMIT) v = SC_DSP_SWEEP_SAMPLE_LIMIT;
                else if (v < -SC_DSP_SWEEP_SAMPLE_LIMIT) v = -SC_DSP_SWEEP_SAMPLE_LIMIT;
                buf[ch][i] = v;
            }
        }
        m->gain = g1;
    }
out:
    __atomic_store_n(&m->busy, 0u, __ATOMIC_RELEASE);
}

/* Standalone crossfade helper; not installed as an Engine Fader hook. The
 * verified native ElastiqueFader already advances its weight per sample frame.
 * This ramps from w_start to w_end over interleaved L/R pairs (an odd last
 * sample is a frame of its own); both channels share one weight. Invalid arguments
 * leave out untouched. out may alias either input. */
static inline void sc_dsp_fader_smooth_block(const float *buf_from,
                                             const float *buf_to,
                                             float *out,
                                             uint32_t float_count,
                                             double w_start,
                                             double w_end)
{
    if (!buf_from || !buf_to || !out || float_count < 2)
        return;
    if (!sc_dsp_finite_f64(w_start) || !sc_dsp_finite_f64(w_end) || w_start < 0.0 || w_start >= 1.0 || w_end <= w_start)
        return;
    const uint32_t num_frames = (float_count + 1u) / 2u;
    for (uint32_t i = 0; i < float_count; i++) {
        const double t = (double)(i / 2u + 1u) / (double)num_frames;
        double w = w_start + (w_end - w_start) * t;
        w = w < 0.0 ? 0.0 : (w > 1.0 ? 1.0 : w);
        out[i] = (float)((1.0 - w) * (double)buf_from[i] + w * (double)buf_to[i]);
    }
}

#endif
