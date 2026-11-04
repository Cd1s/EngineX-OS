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
#define SC_DSP_AUTOGATE_PHASE_OFFSET 0x1e8

struct sc_dsp_state {
    uint32_t generation;          /* bumped by every FX transition */
    int32_t ramp_remaining;       /* frames left in the de-click ramp */
    uint32_t next_evict;          /* round-robin victim when the table is full */
    struct {
        void *self;               /* AutoGate instance, NULL when free */
        uint32_t generation;      /* generation it last synced to */
    } autogate[SC_DSP_AUTOGATE_SLOTS];
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

/* 2. FX transitions. Any thread may mark one; the audio thread consumes it. */
static inline void sc_dsp_fx_mark_transition(const void *fx_owner, uint32_t kind)
{
    struct sc_dsp_state *s = sc_dsp_state();
    (void)fx_owner; /* one global ramp: the common mix tail does not know its owner */
    if (kind < SC_DSP_FX_TRANSITION_TURN_ON || kind > SC_DSP_FX_TRANSITION_ENABLE)
        return;
    __atomic_add_fetch(&s->generation, 1u, __ATOMIC_RELEASE);
    __atomic_store_n(&s->ramp_remaining, (int32_t)SC_DSP_RAMP_FRAMES, __ATOMIC_RELEASE);
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
 * output samples with the (finite) dry input or silence, then, while a
 * transition ramp is armed, fades each frame from dry to processed so an
 * FX switch never steps. In-place processing (input[0] == output[0]) has no
 * separate dry signal and is only sanitized. */
static inline void sc_dsp_fx_common_post_process(void *common_self,
                                                 float *const *input,
                                                 float *const *output,
                                                 uint32_t frames)
{
    struct sc_dsp_state *s = sc_dsp_state();
    (void)common_self;
    if (!output || !frames)
        return;
    for (int ch = 0; ch < 2; ch++) {
        float *out = output[ch];
        const float *in = input ? input[ch] : NULL;
        if (!out)
            continue;
        for (uint32_t i = 0; i < frames; i++) {
            if (!sc_dsp_finite_f32(out[i]))
                out[i] = in && sc_dsp_finite_f32(in[i]) ? in[i] : 0.0f;
        }
    }
    int32_t remaining = __atomic_load_n(&s->ramp_remaining, __ATOMIC_ACQUIRE);
    if (remaining <= 0 || !input || !input[0] || !input[1] || !output[0] || !output[1] || input[0] == output[0])
        return;
    int32_t left = remaining;
    for (uint32_t i = 0; i < frames && left > 0; i++, left--) {
        const float w = (float)(SC_DSP_RAMP_FRAMES - left) / (float)SC_DSP_RAMP_FRAMES;
        for (int ch = 0; ch < 2; ch++) {
            const float dry = sc_dsp_finite_f32(input[ch][i]) ? input[ch][i] : 0.0f;
            output[ch][i] = dry + (output[ch][i] - dry) * w;
        }
    }
    /* A transition marked meanwhile re-armed the ramp: keep that, not our count. */
    __atomic_compare_exchange_n(&s->ramp_remaining, &remaining, left, 0, __ATOMIC_RELEASE, __ATOMIC_RELAXED);
}

/* 3. Key Lock fader: the stock ElastiqueFader holds one weight per audio
 * period, a step every 450 frames. This ramps it per frame from w_start to
 * w_end over interleaved L/R pairs (an odd last sample is a frame of its
 * own); both channels of a frame share one weight. Out of range arguments
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
