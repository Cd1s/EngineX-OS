/* Real-time safe smoothing and state guards for Engine's FX / Key Lock paths.
 *
 * Header-only C11. Nothing here allocates, waits for a lock or makes a system call: the
 * hooks that include it run inside the 96 kHz audio callback (SCHED_RR 49).
 * Owner/event publication uses atomics. Audio history has a nonblocking
 * try-acquire guard; contention skips stateful processing. UI marks never
 * touch audio history. Include it from a single translation unit (the hook);
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
#define SC_DSP_AUTOGATE_PHASE_OFFSET 0x1e8
/* Emergency ceiling, retaining 18 dB of headroom above normalized full scale.
 * This is an output guard, not a limiter or a reset of vendor feedback state. */
#define SC_DSP_SWEEP_SAMPLE_LIMIT 8.0f

enum {
    SC_DSP_SAMPLER_STOP_IMMEDIATELY = 1,
    SC_DSP_SAMPLER_STOP = 2,
    SC_DSP_SAMPLER_PLAY = 3
};

#define SC_DSP_OWNER_SLOTS 8
#define SC_DSP_COMMON_SLOTS 16

#define SC_DSP_FX_SLOTS 64
struct sc_dsp_fx_slot {
    uintptr_t owner;              /* published once; never evict a live owner */
    uint32_t event;               /* generation in bits 3..31, kind in bits 0..2 */
    uint32_t rendering;           /* try-acquire only; audio never waits */
    uint32_t seen_event, remaining;
    unsigned history;
    float previous[2], origin[2];
    struct { void *self; uint32_t generation; } gate[SC_DSP_AUTOGATE_SLOTS];
};
struct sc_dsp_state {
    uint32_t sampler_generation;
    uint32_t slot_misses;
    struct sc_dsp_fx_slot fx[SC_DSP_FX_SLOTS];
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

/* Open addressing with immutable keys. Exhaustion falls back to stateless
 * sanitation, never to another channel's state. Channel objects live for the
 * Engine session; no phase/history pointer is read by the control thread. */
static inline struct sc_dsp_fx_slot *sc_dsp_fx_lookup(const void *owner, int create)
{
    if (!owner) return NULL;
    struct sc_dsp_state *s = sc_dsp_state();
    uintptr_t key = (uintptr_t)owner;
    unsigned first = (unsigned)((key >> 4) ^ (key >> 12)) % SC_DSP_FX_SLOTS;
    for (unsigned n = 0; n < SC_DSP_FX_SLOTS; n++) {
        struct sc_dsp_fx_slot *f = &s->fx[(first + n) % SC_DSP_FX_SLOTS];
        uintptr_t found = __atomic_load_n(&f->owner, __ATOMIC_ACQUIRE);
        if (found == key) return f;
        if (!found && create && __atomic_compare_exchange_n(&f->owner, &found, key, 0,
                                                            __ATOMIC_ACQ_REL, __ATOMIC_ACQUIRE)) return f;
        if (found == key) return f;
    }
    if (create) __atomic_add_fetch(&s->slot_misses, 1u, __ATOMIC_RELAXED);
    return NULL;
}

static inline void sc_dsp_fx_mark_transition(const void *owner, uint32_t kind)
{
    if (kind < SC_DSP_FX_TRANSITION_TURN_ON || kind > SC_DSP_FX_TRANSITION_ENABLE) return;
    struct sc_dsp_fx_slot *f = sc_dsp_fx_lookup(owner, 1);
    if (!f) return;
    uint32_t old = __atomic_load_n(&f->event, __ATOMIC_RELAXED), next;
    do { next = ((old & ~7u) + 8u) | kind; }
    while (!__atomic_compare_exchange_n(&f->event, &old, next, 0, __ATOMIC_RELEASE, __ATOMIC_RELAXED));
}

static inline struct sc_dsp_fx_slot *sc_dsp_fx_begin(const void *owner)
{
    struct sc_dsp_fx_slot *f = sc_dsp_fx_lookup(owner, 1);
    uint32_t idle = 0;
    return f && __atomic_compare_exchange_n(&f->rendering, &idle, 1u, 0,
                                            __ATOMIC_ACQUIRE, __ATOMIC_RELAXED) ? f : NULL;
}

/* Called only while this channel's render guard is held. Unknown ownership
 * deliberately leaves vendor phase alone; there is no global generation. */
static inline void sc_dsp_autogate_for_slot(struct sc_dsp_fx_slot *f, void *self, uint32_t frames)
{
    if (!f || !self || !frames) return;
    uint32_t generation = __atomic_load_n(&f->event, __ATOMIC_ACQUIRE) & ~7u;
    for (unsigned i = 0; i < SC_DSP_AUTOGATE_SLOTS; i++) {
        if (f->gate[i].self && f->gate[i].self != self) continue;
        if (!f->gate[i].self || f->gate[i].generation != generation) {
            if (generation) {
                const float zero = 0.0f;
                memcpy((char *)self + SC_DSP_AUTOGATE_PHASE_OFFSET, &zero, sizeof zero);
            }
            f->gate[i].self = self;
            f->gate[i].generation = generation;
        }
        return;
    }
    /* Full per-owner gate table: preserve phase rather than evict another gate. */
}

/* The common tail remains a stateless guard. Stateful smoothing belongs at
 * the final ChannelDJFx output, including its native bypass/TurnOff path. */
static inline void sc_dsp_fx_common_post_process(void *common, float *const *input,
                                                 float *const *output, uint32_t frames)
{
    (void)common;
    if (!output) return;
    for (unsigned ch = 0; ch < 2; ch++) {
        if (!output[ch]) continue;
        for (uint32_t i = 0; i < frames; i++)
            if (!sc_dsp_finite_f32(output[ch][i]))
                output[ch][i] = input && input[ch] && sc_dsp_finite_f32(input[ch][i]) ? input[ch][i] : 0.0f;
    }
}

static inline void sc_dsp_fx_end(struct sc_dsp_fx_slot *f, float *const *output, uint32_t frames)
{
    sc_dsp_fx_common_post_process(NULL, NULL, output, frames);
    if (!f) return;
    if (frames && output && output[0] && output[1]) {
        uint32_t event = __atomic_load_n(&f->event, __ATOMIC_ACQUIRE);
        if (event != f->seen_event) {
            for (unsigned ch = 0; ch < 2; ch++)
                f->origin[ch] = f->history & (1u << ch) ? f->previous[ch] : 0.0f;
            f->remaining = SC_DSP_RAMP_FRAMES;
            f->seen_event = event;
        }
        for (uint32_t i = 0; i < frames && f->remaining; i++, f->remaining--) {
            const double w = (double)(SC_DSP_RAMP_FRAMES - f->remaining) / SC_DSP_RAMP_FRAMES;
            for (unsigned ch = 0; ch < 2; ch++)
                output[ch][i] = (float)((1.0 - w) * (double)f->origin[ch] + w * (double)output[ch][i]);
        }
        for (unsigned ch = 0; ch < 2; ch++) f->previous[ch] = output[ch][frames - 1];
        f->history = 3;
    }
    /* UI only changes event. A new mark during this block remains pending for
     * the next block; no CAS on a re-armed counter can swallow it. */
    __atomic_store_n(&f->rendering, 0u, __ATOMIC_RELEASE);
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
