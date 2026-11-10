/* Pioneer DJ Beat FX & Sound Color FX DSP emulations (#88).
 *
 * Header-only C11 for Engine's audio device callback: no allocation, no locks, no libm.
 * Real-time emulations of signature Pioneer DJ effects:
 *  - Spiral: pitch-shifting recirculation delay with soft saturation
 *  - Pitch Echo: dual harmonic octave/fifth pitch-shifted delay with 1st-order low-pass
 *  - Slip Roll: beat-slice circular loop repeater with 64-frame crossfade & background timeline
 *  - Dub Echo: Sound Color FX resonant bandpass + analog tape saturation echo
 *  - Space: Sound Color FX multi-tap delay + 3-stage allpass diffuser reverb
 */
#ifndef SC_PIONEER_FX_H
#define SC_PIONEER_FX_H

#include <stdint.h>
#include <stddef.h>
#include <string.h>

enum sc_pioneer_fx_mode {
    SC_PFX_OFF = 0,
    SC_PFX_SPIRAL = 1,     /* Pioneer DJM Spiral: pitch-shifting feedback delay */
    SC_PFX_PITCH_ECHO = 2, /* Pioneer Pitch Echo: harmonic pitch-shifted recirculation delay */
    SC_PFX_SLIP_ROLL = 3,  /* Pioneer Slip Roll: beat-slice circular loop repeater */
    SC_PFX_DUB_ECHO = 4,   /* Pioneer Sound Color FX Dub Echo: resonant band-pass + tape saturation echo */
    SC_PFX_SPACE = 5       /* Pioneer Sound Color FX Space: lush multi-tap allpass reverb + delay */
};

#define SC_PFX_RING_SIZE 48000u
#define SC_PFX_ROLL_FRAMES 12000u
#define SC_PFX_ROLL_XFADE 64u

struct sc_pioneer_fx {
    float buf_l[SC_PFX_RING_SIZE];
    float buf_r[SC_PFX_RING_SIZE];
    uint32_t write_pos;
    int active_mode;
    /* Zero preserves the legacy AZ04 master effect exactly. */
    int controlled;
    float time, parameter;
    int time_mode_ms;
    float time_ms;
    float delay_smooth;

    /* Spiral: variable fractional delay pitch-shifting feedback delay */
    float spiral_head_a;   /* fractional head offset [0, W) */
    float spiral_lp_l;
    float spiral_lp_r;

    /* Pitch Echo: dual harmonic pitch-shifted taps (octave + fifth) with lowpass */
    float pe_head_fifth;   /* fractional head offset for 3:2 ratio */
    float pe_head_octave;  /* fractional head offset for 2:1 ratio */
    float pe_lp_l;
    float pe_lp_r;

    /* Slip Roll: 12000-frame slice repeater with 64-frame crossfade */
    float roll_buf_l[SC_PFX_ROLL_FRAMES];
    float roll_buf_r[SC_PFX_ROLL_FRAMES];
    uint32_t roll_captured; /* number of frames captured, up to SC_PFX_ROLL_FRAMES */
    uint32_t roll_play_pos; /* playback index [0, SC_PFX_ROLL_FRAMES) */
    int roll_active;        /* 1 once slice is fully captured */

    /* Dub Echo: resonant bandpass (HPF + LPF) + tape saturation */
    float dub_hp_l, dub_hp_r;
    float dub_lp_l, dub_lp_r;

    /* Space: multi-tap delay + allpass diffusers */
    float ap1_l[149], ap1_r[149];
    uint32_t ap1_pos_l, ap1_pos_r;
    float ap2_l[373], ap2_r[373];
    uint32_t ap2_pos_l, ap2_pos_r;
    float ap3_l[541], ap3_r[541];
    uint32_t ap3_pos_l, ap3_pos_r;
    float space_lp_l, space_lp_r;
};

static inline int sc_pioneer_fx_parse(const char *val)
{
    if (!val)
        return SC_PFX_OFF;
    while (*val == ' ' || *val == '\t')
        val++;
    if (!*val)
        return SC_PFX_OFF;

    char buf[32];
    size_t i = 0;
    while (val[i] && val[i] != ' ' && val[i] != '\t' && val[i] != '\r' && val[i] != '\n' && i < sizeof(buf) - 1) {
        buf[i] = val[i];
        i++;
    }
    buf[i] = '\0';

    if (!strcmp(buf, "spiral") || !strcmp(buf, "1"))
        return SC_PFX_SPIRAL;
    if (!strcmp(buf, "pitch_echo") || !strcmp(buf, "2"))
        return SC_PFX_PITCH_ECHO;
    if (!strcmp(buf, "slip_roll") || !strcmp(buf, "3"))
        return SC_PFX_SLIP_ROLL;
    if (!strcmp(buf, "dub_echo") || !strcmp(buf, "4"))
        return SC_PFX_DUB_ECHO;
    if (!strcmp(buf, "space") || !strcmp(buf, "5"))
        return SC_PFX_SPACE;
    return SC_PFX_OFF;
}

static inline float sc_pfx_sanitize(float x)
{
    return (x <= 3.0e38f && x >= -3.0e38f) ? x : 0.0f;
}

/* Recursive state: flush subnormals (denormal arithmetic stalls ARM FPUs) as well as inf/NaN. */
static inline float sc_pfx_state(float x)
{
    return (x > -1.0e-20f && x < 1.0e-20f) ? 0.0f : sc_pfx_sanitize(x);
}

static inline float sc_pfx_clamp_out(float x)
{
    if (!(x <= 3.0e38f && x >= -3.0e38f))
        return 0.0f;
    if (x > 4.0f)
        return 4.0f;
    if (x < -4.0f)
        return -4.0f;
    return x;
}

static inline float sc_pfx_knob_to_ms(float t)
{
    if (t < 0.0f) t = 0.0f;
    if (t > 1.0f) t = 1.0f;
    if (t <= 0.5f) {
        float f = t * 2.0f;
        return 1.0f + f * f * 199.0f;
    } else {
        float f = (t - 0.5f) * 2.0f;
        return 200.0f + f * f * 700.0f;
    }
}

/* Mild soft saturation: |x| / (1 + |x|) curve preserving sign */
static inline float sc_pfx_soft_sat(float x)
{
    float a = x < 0.0f ? -x : x;
    return x / (1.0f + a);
}

/* Linear interpolation read from ring buffer */
static inline float sc_pfx_read_ring(const float *buf, float fpos)
{
    while (fpos < 0.0f)
        fpos += (float)SC_PFX_RING_SIZE;
    while (fpos >= (float)SC_PFX_RING_SIZE)
        fpos -= (float)SC_PFX_RING_SIZE;

    uint32_t i0 = (uint32_t)fpos;
    uint32_t i1 = (i0 + 1 == SC_PFX_RING_SIZE) ? 0 : i0 + 1;
    float frac = fpos - (float)i0;
    float s0 = buf[i0];
    float s1 = buf[i1];
    return s0 + frac * (s1 - s0);
}

/* Canonical Schroeder allpass filter stage */
static inline float sc_pfx_allpass(float *buf, uint32_t *pos, uint32_t len, float in, float g)
{
    uint32_t p = *pos;
    if (p >= len)
        p = 0;
    float buf_out = buf[p];
    float out = -g * in + buf_out;
    buf[p] = sc_pfx_state(in + g * out);
    if (++p == len)
        p = 0;
    *pos = p;
    return out;
}

static inline void sc_pioneer_fx_planar(struct sc_pioneer_fx *s, float *l, float *r, unsigned frames, int mode)
{
    if (!s || !l || frames == 0)
        return;

    if (mode == SC_PFX_OFF) {
        s->active_mode = SC_PFX_OFF;
        s->roll_active = 0;
        s->roll_captured = 0;
        s->delay_smooth = 0.0f;
        return;
    }

    if (mode != s->active_mode) {
        s->active_mode = mode;
        s->spiral_head_a = 0.0f;
        s->spiral_lp_l = s->spiral_lp_r = 0.0f;
        s->pe_head_fifth = s->pe_head_octave = 0.0f;
        s->pe_lp_l = s->pe_lp_r = 0.0f;
        s->roll_captured = 0;
        s->roll_play_pos = 0;
        s->roll_active = 0;
        s->dub_hp_l = s->dub_hp_r = 0.0f;
        s->dub_lp_l = s->dub_lp_r = 0.0f;
        s->space_lp_l = s->space_lp_r = 0.0f;
        s->ap1_pos_l = s->ap1_pos_r = 0;
        s->ap2_pos_l = s->ap2_pos_r = 0;
        s->ap3_pos_l = s->ap3_pos_r = 0;
        s->delay_smooth = 0.0f;
        memset(s->buf_l, 0, sizeof(s->buf_l));
        memset(s->buf_r, 0, sizeof(s->buf_r));
        memset(s->roll_buf_l, 0, sizeof(s->roll_buf_l));
        memset(s->roll_buf_r, 0, sizeof(s->roll_buf_r));
        memset(s->ap1_l, 0, sizeof(s->ap1_l));
        memset(s->ap1_r, 0, sizeof(s->ap1_r));
        memset(s->ap2_l, 0, sizeof(s->ap2_l));
        memset(s->ap2_r, 0, sizeof(s->ap2_r));
        memset(s->ap3_l, 0, sizeof(s->ap3_l));
        memset(s->ap3_r, 0, sizeof(s->ap3_r));
    }

    const float time = s->controlled ? s->time : 0.5f;
    const float param = s->controlled ? s->parameter : 0.5f;
    const float delay = s->controlled ? 4800.0f + time * 38400.0f : 24000.0f;
    const float feedback = s->controlled ? 0.15f + param * 0.70f : 0.68f;
    const uint32_t roll_frames_stock = s->controlled ? 512u + (uint32_t)(time * (SC_PFX_ROLL_FRAMES - 512u)) : SC_PFX_ROLL_FRAMES;
    float target_delay = delay;
    uint32_t roll_frames = roll_frames_stock;
    if (s->time_mode_ms != 0) {
        float base_ms = (s->time_ms >= 1.0f && s->time_ms <= 900.0f) ? s->time_ms : 500.0f;
        float ms = base_ms;
        float dt = time - 0.5f;
        if (dt < 0.0f) dt = -dt;
        if (s->controlled && dt > 0.01f) {
            ms = sc_pfx_knob_to_ms(time);
        }
        target_delay = ms * 48.0f;
        if (target_delay < 48.0f) target_delay = 48.0f;
        if (target_delay > 43200.0f) target_delay = 43200.0f;
        float rf = ms * 48.0f;
        if (rf < 512.0f) rf = 512.0f;
        if (rf > (float)SC_PFX_ROLL_FRAMES) rf = (float)SC_PFX_ROLL_FRAMES;
        roll_frames = (uint32_t)rf;
    }
    uint32_t wpos = s->write_pos;

    for (unsigned i = 0; i < frames; i++) {
        float in_l = sc_pfx_sanitize(l[i]);
        float in_r = r ? sc_pfx_sanitize(r[i]) : 0.0f;
        float out_l = in_l, out_r = in_r;

        if (s->time_mode_ms != 0) {
            s->delay_smooth = (s->delay_smooth <= 0.0f) ? target_delay : sc_pfx_state(s->delay_smooth + 0.002f * (target_delay - s->delay_smooth));
        }

        switch (mode) {
        case SC_PFX_SPIRAL: {
            /* Spiral: variable fractional delay pitch-shifting recirculation delay */
            const float D = (s->time_mode_ms != 0) ? s->delay_smooth : delay;
            const float W = 4096.0f;
            const float speed = s->controlled ? 0.005f + param * 0.15f : 0.05f; /* pitch upward shift increment per sample */

            float pA = s->spiral_head_a / W;
            float pB = pA + 0.5f;
            if (pB >= 1.0f) pB -= 1.0f;

            float wA = 1.0f - 2.0f * (pA < 0.5f ? (0.5f - pA) : (pA - 0.5f));
            float wB = 1.0f - wA;

            float posA = (float)wpos - D + pA * W;
            float posB = (float)wpos - D + pB * W;

            float dlA = sc_pfx_read_ring(s->buf_l, posA);
            float dlB = sc_pfx_read_ring(s->buf_l, posB);
            float dl = wA * dlA + wB * dlB;

            float drA = sc_pfx_read_ring(s->buf_r, posA);
            float drB = sc_pfx_read_ring(s->buf_r, posB);
            float dr = wA * drA + wB * drB;

            s->spiral_head_a += speed;
            if (s->spiral_head_a >= W)
                s->spiral_head_a -= W;

            float fb_l = sc_pfx_soft_sat(dl * feedback);
            float fb_r = sc_pfx_soft_sat(dr * feedback);

            s->buf_l[wpos] = sc_pfx_state(in_l + fb_l);
            s->buf_r[wpos] = sc_pfx_state(in_r + fb_r);

            out_l = in_l + dl * 0.70f;
            out_r = in_r + dr * 0.70f;
            break;
        }

        case SC_PFX_PITCH_ECHO: {
            /* Pitch Echo: dual harmonic octave/fifth pitch-shifted delay with 1st-order low-pass */
            const float D = (s->time_mode_ms != 0) ? s->delay_smooth : delay;
            const float W1 = 3072.0f; /* Fifth window */
            const float W2 = 2048.0f; /* Octave window */

            /* Tap 1: Fifth (pitch ratio 1.5) */
            float p1A = s->pe_head_fifth / W1;
            float p1B = p1A + 0.5f;
            if (p1B >= 1.0f) p1B -= 1.0f;
            float w1A = 1.0f - 2.0f * (p1A < 0.5f ? (0.5f - p1A) : (p1A - 0.5f));
            float w1B = 1.0f - w1A;
            float pos1A = (float)wpos - D + p1A * W1;
            float pos1B = (float)wpos - D + p1B * W1;
            float t1_l = w1A * sc_pfx_read_ring(s->buf_l, pos1A) + w1B * sc_pfx_read_ring(s->buf_l, pos1B);
            float t1_r = w1A * sc_pfx_read_ring(s->buf_r, pos1A) + w1B * sc_pfx_read_ring(s->buf_r, pos1B);

            s->pe_head_fifth += 0.5f;
            if (s->pe_head_fifth >= W1)
                s->pe_head_fifth -= W1;

            /* Tap 2: Octave (pitch ratio 2.0) */
            float p2A = s->pe_head_octave / W2;
            float p2B = p2A + 0.5f;
            if (p2B >= 1.0f) p2B -= 1.0f;
            float w2A = 1.0f - 2.0f * (p2A < 0.5f ? (0.5f - p2A) : (p2A - 0.5f));
            float w2B = 1.0f - w2A;
            float pos2A = (float)wpos - D + p2A * W2;
            float pos2B = (float)wpos - D + p2B * W2;
            float t2_l = w2A * sc_pfx_read_ring(s->buf_l, pos2A) + w2B * sc_pfx_read_ring(s->buf_l, pos2B);
            float t2_r = w2A * sc_pfx_read_ring(s->buf_r, pos2A) + w2B * sc_pfx_read_ring(s->buf_r, pos2B);

            s->pe_head_octave += 1.0f;
            if (s->pe_head_octave >= W2)
                s->pe_head_octave -= W2;

            float mix_l = 0.5f * t1_l + 0.5f * t2_l;
            float mix_r = 0.5f * t1_r + 0.5f * t2_r;

            s->pe_lp_l = sc_pfx_state(s->pe_lp_l + 0.40f * (mix_l - s->pe_lp_l));
            s->pe_lp_r = sc_pfx_state(s->pe_lp_r + 0.40f * (mix_r - s->pe_lp_r));

            float fb_l = sc_pfx_soft_sat(s->pe_lp_l * (s->controlled ? feedback : 0.65f));
            float fb_r = sc_pfx_soft_sat(s->pe_lp_r * (s->controlled ? feedback : 0.65f));

            s->buf_l[wpos] = sc_pfx_state(in_l + fb_l);
            s->buf_r[wpos] = sc_pfx_state(in_r + fb_r);

            out_l = in_l + s->pe_lp_l * 0.65f;
            out_r = in_r + s->pe_lp_r * 0.65f;
            break;
        }

        case SC_PFX_SLIP_ROLL: {
            /* Slip Roll: 12000-frame slice repeater with 64-frame crossfade & background timeline */
            float mag = (in_l < 0.0f ? -in_l : in_l) + (in_r < 0.0f ? -in_r : in_r);
            if (s->controlled && s->roll_active) {
                if (roll_frames > s->roll_captured + SC_PFX_ROLL_XFADE) {
                    // A longer slice must capture live audio, rather than play
                    // the zero-filled, never-captured part of the buffer.
                    s->roll_active = 0; s->roll_captured = 0; s->roll_play_pos = 0;
                } else if (s->roll_captured < roll_frames) {
                    s->roll_buf_l[s->roll_captured] = in_l;
                    s->roll_buf_r[s->roll_captured++] = in_r;
                }
            }
            if (!s->roll_active) {
                if (s->roll_captured > 0 || mag > 1.0e-4f) {
                    if (s->roll_captured >= roll_frames) s->roll_captured = 0;
                    s->roll_buf_l[s->roll_captured] = in_l;
                    s->roll_buf_r[s->roll_captured] = in_r;
                    s->roll_captured++;
                    if (s->roll_captured >= roll_frames) {
                        s->roll_active = 1;
                        s->roll_play_pos = 0;
                    }
                }
                out_l = in_l;
                out_r = in_r;
            } else {
                uint32_t p = s->roll_play_pos;
                if (p >= roll_frames - SC_PFX_ROLL_XFADE) p = 0;
                float rl = s->roll_buf_l[p];
                float rr = s->roll_buf_r[p];
                if (p < SC_PFX_ROLL_XFADE) {
                    float fade_in = (float)p / (float)SC_PFX_ROLL_XFADE;
                    float fade_out = 1.0f - fade_in;
                    uint32_t tail_idx = roll_frames - SC_PFX_ROLL_XFADE + p;
                    rl = fade_in * rl + fade_out * s->roll_buf_l[tail_idx];
                    rr = fade_in * rr + fade_out * s->roll_buf_r[tail_idx];
                }
                /* Wrap before the tail: its last XFADE frames are only blended into the start,
                 * so the loop stays continuous (wrapping at FRAMES jumped back XFADE-1 samples). */
                if (++p >= roll_frames - SC_PFX_ROLL_XFADE)
                    p = 0;
                s->roll_play_pos = p;
                out_l = rl;
                out_r = rr;
            }
            /* Underlying timeline advances without interruption */
            s->buf_l[wpos] = in_l;
            s->buf_r[wpos] = in_r;
            break;
        }

        case SC_PFX_DUB_ECHO: {
            /* Dub Echo: resonant bandpass (HPF + LPF) + tape saturation */
            const uint32_t D = (uint32_t)((s->time_mode_ms != 0) ? s->delay_smooth : delay);
            uint32_t rpos = (wpos >= D) ? (wpos - D) : (wpos + SC_PFX_RING_SIZE - D);

            float dl = s->buf_l[rpos];
            float dr = s->buf_r[rpos];

            /* HPF sub-bass rumble block */
            s->dub_hp_l = sc_pfx_state(s->dub_hp_l + 0.06f * (dl - s->dub_hp_l));
            s->dub_hp_r = sc_pfx_state(s->dub_hp_r + 0.06f * (dr - s->dub_hp_r));
            float hpl = dl - s->dub_hp_l;
            float hpr = dr - s->dub_hp_r;

            /* LPF warm resonant roll-off */
            s->dub_lp_l = sc_pfx_state(s->dub_lp_l + 0.28f * (hpl - s->dub_lp_l));
            s->dub_lp_r = sc_pfx_state(s->dub_lp_r + 0.28f * (hpr - s->dub_lp_r));

            /* Analog tape soft saturation */
            float sat_l = sc_pfx_soft_sat(s->dub_lp_l * 1.35f);
            float sat_r = sc_pfx_soft_sat(s->dub_lp_r * 1.35f);

            s->buf_l[wpos] = sc_pfx_state(in_l + sat_l * feedback);
            s->buf_r[wpos] = sc_pfx_state(in_r + sat_r * feedback);

            out_l = in_l + s->dub_lp_l * 0.70f;
            out_r = in_r + s->dub_lp_r * 0.70f;
            break;
        }

        case SC_PFX_SPACE: {
            /* Space: multi-tap delay + 3-stage allpass diffuser reverb */
            float d_l = sc_pfx_allpass(s->ap1_l, &s->ap1_pos_l, 149, in_l, 0.62f);
            d_l = sc_pfx_allpass(s->ap2_l, &s->ap2_pos_l, 373, d_l, 0.58f);
            d_l = sc_pfx_allpass(s->ap3_l, &s->ap3_pos_l, 541, d_l, 0.55f);

            float d_r = sc_pfx_allpass(s->ap1_r, &s->ap1_pos_r, 149, in_r, 0.62f);
            d_r = sc_pfx_allpass(s->ap2_r, &s->ap2_pos_r, 373, d_r, 0.58f);
            d_r = sc_pfx_allpass(s->ap3_r, &s->ap3_pos_r, 541, d_r, 0.55f);

            const uint32_t tap_l[4] = { 2399, 6101, 12721, 21523 };
            const uint32_t tap_r[4] = { 2801, 7219, 14389, 23747 };

            float sum_l = 0.0f, sum_r = 0.0f;
            for (int t = 0; t < 4; t++) {
                uint32_t dl = s->controlled ? (uint32_t)((float)tap_l[t] * (0.2f + 1.5f * time)) : tap_l[t];
                uint32_t tl = (wpos >= dl) ? (wpos - dl) : (wpos + SC_PFX_RING_SIZE - dl);
                uint32_t dr = s->controlled ? (uint32_t)((float)tap_r[t] * (0.2f + 1.5f * time)) : tap_r[t];
                uint32_t tr = (wpos >= dr) ? (wpos - dr) : (wpos + SC_PFX_RING_SIZE - dr);
                sum_l += s->buf_l[tl];
                sum_r += s->buf_r[tr];
            }
            sum_l *= 0.25f;
            sum_r *= 0.25f;

            s->space_lp_l = sc_pfx_state(s->space_lp_l + 0.35f * (sum_l - s->space_lp_l));
            s->space_lp_r = sc_pfx_state(s->space_lp_r + 0.35f * (sum_r - s->space_lp_r));

            float fb_l = sc_pfx_soft_sat(s->space_lp_l * (s->controlled ? feedback : 0.55f));
            float fb_r = sc_pfx_soft_sat(s->space_lp_r * (s->controlled ? feedback : 0.55f));

            s->buf_l[wpos] = sc_pfx_state(d_l + fb_l);
            s->buf_r[wpos] = sc_pfx_state(d_r + fb_r);

            out_l = in_l + s->space_lp_l * 0.70f;
            out_r = in_r + s->space_lp_r * 0.70f;
            break;
        }

        default:
            break;
        }

        if (++wpos == SC_PFX_RING_SIZE)
            wpos = 0;

        l[i] = sc_pfx_clamp_out(out_l);
        if (r)
            r[i] = sc_pfx_clamp_out(out_r);
    }

    s->write_pos = wpos;
}

/* Worker clears only DIRTY banks. READY -> LIVE is an acquire transfer to
 * the audio thread; LIVE -> DIRTY releases it after the last crossfade read.
 * A rapid selection waits for a prepared spare, retaining the current effect.
 * Legacy master DSP above deliberately keeps its original reset behavior. */
enum { SC_PFX_DIRTY, SC_PFX_PREPARING, SC_PFX_READY, SC_PFX_LIVE };
struct sc_pfx_instance {
    struct sc_pioneer_fx bank[2];
    unsigned bank_state[2];
    int mode, current; /* current is bank index + 1; zero means no live bank */
    unsigned fade;
    float last_l, last_r, from_l, from_r;
    float wet, time, parameter;
    int time_mode_ms;
    float time_ms;
};

static inline float sc_pfx_unit(float x) {
    return !(x >= 0.0f) ? 0.0f : (x > 1.0f ? 1.0f : x);
}

/* Ordinary polling worker only. This also faults in every delay-buffer page. */
static inline void sc_pfx_instance_prepare(struct sc_pfx_instance *s) {
    for (unsigned i = 0; i < 2; ++i) {
        unsigned expected = SC_PFX_DIRTY;
        if (!__atomic_compare_exchange_n(&s->bank_state[i], &expected, SC_PFX_PREPARING,
                                         0, __ATOMIC_ACQUIRE, __ATOMIC_RELAXED)) continue;
        memset(&s->bank[i], 0, sizeof s->bank[i]);
        __atomic_store_n(&s->bank_state[i], SC_PFX_READY, __ATOMIC_RELEASE);
    }
}

/* Audio owner only: invalidate live banks without touching their buffers. */
static inline void sc_pfx_instance_reset(struct sc_pfx_instance *s) {
    for (unsigned i = 0; i < 2; ++i)
        if (__atomic_load_n(&s->bank_state[i], __ATOMIC_RELAXED) == SC_PFX_LIVE)
            __atomic_store_n(&s->bank_state[i], SC_PFX_DIRTY, __ATOMIC_RELEASE);
    s->mode = SC_PFX_OFF; s->current = 0; s->fade = 0;
    s->last_l = s->last_r = s->from_l = s->from_r = 0;
    s->wet = s->time = s->parameter = 0;
}

/* Zero means no prepared bank: input is unchanged; override callers keep it dry. */
static inline int sc_pfx_process(struct sc_pfx_instance *s, float *l, float *r,
                                unsigned frames, int mode, float time, float param, float wet) {
    if (!s || !l || !r) return 0;
    time = sc_pfx_unit(time); param = sc_pfx_unit(param); wet = sc_pfx_unit(wet);
    if (mode < SC_PFX_OFF || mode > SC_PFX_SPACE) mode = SC_PFX_OFF;
    if (mode == SC_PFX_OFF) wet = 0.0f;
    if ((mode != s->mode || !s->current) && !s->fade) {
        for (unsigned i = 0; i < 2; ++i) {
            unsigned expected = SC_PFX_READY;
            if (!__atomic_compare_exchange_n(&s->bank_state[i], &expected, SC_PFX_LIVE,
                                             0, __ATOMIC_ACQUIRE, __ATOMIC_RELAXED)) continue;
            s->bank[i].active_mode = mode; /* all other state was cleared by worker */
            s->bank[i].controlled = 1;
            s->current = (int)i + 1; s->mode = mode; s->fade = 256;
            s->from_l = s->last_l; s->from_r = s->last_r;
            break;
        }
    }
    if (!s->current) return 0;
    /* A pending selection must not reset the current bank in planar(). */
    mode = s->mode;
    for (unsigned i = 0; i < frames; ++i) {
        float dl = l[i], dr = r[i], nl = dl, nr = dr, ol = dl, or_ = dr;
        s->time = sc_pfx_state(s->time + (time - s->time) * (1.0f / 256.0f));
        s->parameter = sc_pfx_state(s->parameter + (param - s->parameter) * (1.0f / 256.0f));
        s->wet = sc_pfx_state(s->wet + (wet - s->wet) * (1.0f / 256.0f));
        unsigned current = (unsigned)s->current - 1, previous = current ^ 1;
        struct sc_pioneer_fx *n = &s->bank[current], *o = &s->bank[previous];
        n->time = s->time; n->parameter = s->parameter;
        n->time_mode_ms = s->time_mode_ms; n->time_ms = s->time_ms;
        sc_pioneer_fx_planar(n, &nl, &nr, 1, mode);
        if (mode != SC_PFX_SLIP_ROLL) { nl -= dl; nr -= dr; }
        if (!mode) nl = nr = 0.0f;
        if (s->fade) {
            int old_mode = __atomic_load_n(&s->bank_state[previous], __ATOMIC_RELAXED) == SC_PFX_LIVE ? o->active_mode : 0;
            if (old_mode) {
                o->time_mode_ms = s->time_mode_ms; o->time_ms = s->time_ms;
                sc_pioneer_fx_planar(o, &ol, &or_, 1, old_mode);
                if (old_mode != SC_PFX_SLIP_ROLL) { ol -= dl; or_ -= dr; }
            } else ol = or_ = 0.0f;
            float a = (float)(256 - s->fade) / 256.0f;
            float old_l = old_mode ? s->from_l + a * (ol - s->from_l) : dl;
            float old_r = old_mode ? s->from_r + a * (or_ - s->from_r) : dr;
            nl = old_l + a * (nl - old_l); nr = old_r + a * (nr - old_r);
            if (!--s->fade && __atomic_load_n(&s->bank_state[previous], __ATOMIC_RELAXED) == SC_PFX_LIVE)
                __atomic_store_n(&s->bank_state[previous], SC_PFX_DIRTY, __ATOMIC_RELEASE);
        }
        s->last_l = sc_pfx_clamp_out(nl); s->last_r = sc_pfx_clamp_out(nr);
        if (wet == 0.0f && s->wet < 1.0e-6f) { s->wet = 0.0f; continue; }
        l[i] = sc_pfx_clamp_out(dl * (1.0f - s->wet) + s->last_l * s->wet);
        r[i] = sc_pfx_clamp_out(dr * (1.0f - s->wet) + s->last_r * s->wet);
    }
    return 1;
}

#endif
