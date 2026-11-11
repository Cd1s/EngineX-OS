/* Optional PX delay timing (#162), at Engine's 96 kHz callback rate.
 * Separate state keeps the existing #88 rings and arithmetic unchanged OFF.
 * No allocation, clearing, locks or libm on the audio thread. */
#ifndef SC_PIONEER_MS_H
#define SC_PIONEER_MS_H
#include <stdint.h>
#include <stddef.h>
#include <string.h>

#define SC_MS_RING 393216u /* 4000 ms * 96 + pitch-window headroom */
#define SC_MS_FADE 1920u  /* 20 ms */
struct sc_pfx_ms {
    float l[SC_MS_RING], r[SC_MS_RING];
    unsigned pos, valid, fade;
    float delay, next_delay, blend, head, head2, lp_l, lp_r, hp_l, hp_r;
    unsigned ready;
};

static inline int sc_ms_effect(int mode) { return mode == 1 || mode == 2 || mode == 4; }
static inline unsigned sc_ms_clamp(unsigned ms) { return ms < 10 ? 10 : (ms > 4000 ? 4000 : ms); }

/* Digits only, with overflow rejection; in particular "ms" is never numeric. */
static inline int sc_ms_integer(const char *text, unsigned *value) {
    unsigned n = 0;
    if (!text || strlen(text) > 10 || *text < '0' || *text > '9') return 0;
    do {
        unsigned digit = (unsigned)(*text++ - '0');
        if (n > (UINT32_MAX - digit) / 10u) return 0;
        n = n * 10u + digit;
    } while (*text >= '0' && *text <= '9');
    if (*text) return 0;
    *value = n;
    return 1;
}

struct sc_ms_config { unsigned ms; int enabled; };
static inline void sc_ms_setting(struct sc_ms_config *c, char *line) {
    line[strcspn(line, "\r\n")] = 0;
    if (!strncmp(line, "fx_time_mode=", 13)) c->enabled = !strcmp(line + 13, "ms");
    else if (!strncmp(line, "fx_time_ms=", 11)) {
        unsigned n;
        c->ms = sc_ms_integer(line + 11, &n) ? sc_ms_clamp(n) : 500;
    }
}

/* Polling worker only: fault in the optional ring before publishing it. */
static inline void sc_ms_prepare(struct sc_pfx_ms *s) {
    unsigned zero = 0;
    if (!__atomic_compare_exchange_n(&s->ready, &zero, 1, 0, __ATOMIC_ACQUIRE, __ATOMIC_RELAXED)) return;
    memset(s, 0, offsetof(struct sc_pfx_ms, ready));
    __atomic_store_n(&s->ready, 2, __ATOMIC_RELEASE);
}

static inline float sc_ms_clean(float x) {
    if (!(x >= -3e38f && x <= 3e38f)) return 0.0f;
    if (x > 4.0f) return 4.0f;
    if (x < -4.0f) return -4.0f;
    return x > -1e-20f && x < 1e-20f ? 0.0f : x;
}
static inline float sc_ms_sat(float x) { return x / (1.0f + (x < 0 ? -x : x)); }
static inline float sc_ms_read(const struct sc_pfx_ms *s, const float *buf, float delay) {
    unsigned d = (unsigned)delay;
    if (d > s->valid) return 0.0f;
    unsigned a = s->pos >= d ? s->pos - d : s->pos + SC_MS_RING - d;
    unsigned b = a ? a - 1 : SC_MS_RING - 1;
    float previous = d + 1 > s->valid ? 0.0f : buf[b];
    return buf[a] + (delay - (float)d) * (previous - buf[a]);
}
static inline float sc_ms_window(float delay, float maximum) {
    return delay * 0.5f < maximum ? delay * 0.5f : maximum;
}
static inline float sc_ms_shift(const struct sc_pfx_ms *s, const float *buf,
                                 float delay, float window, float a) {
    float b = a + 0.5f;
    if (b >= 1.0f) b -= 1.0f;
    float weight = 1.0f - 2.0f * (a < 0.5f ? 0.5f - a : a - 0.5f);
    return weight * sc_ms_read(s, buf, delay - a * window)
        + (1.0f - weight) * sc_ms_read(s, buf, delay - b * window);
}
static inline float sc_ms_tap(const struct sc_pfx_ms *s, const float *buf, float delay, int mode) {
    if (mode == 4) return sc_ms_read(s, buf, delay);
    if (mode == 1) return sc_ms_shift(s, buf, delay, sc_ms_window(delay, 4096.0f), s->head);
    return 0.5f * sc_ms_shift(s, buf, delay, sc_ms_window(delay, 3072.0f), s->head)
        + 0.5f * sc_ms_shift(s, buf, delay, sc_ms_window(delay, 2048.0f), s->head2);
}

/* baseline is wet-only for selector instances, and full output on the master.
 * Turning OFF after ON drains a bounded crossfade; default OFF never writes state. */
static inline void sc_ms_mix(struct sc_pfx_ms *s, float dl, float dr, float *l, float *r,
                             int mode, unsigned ms, float param, int master) {
    int enabled = ms && sc_ms_effect(mode);
    if (__atomic_load_n(&s->ready, __ATOMIC_ACQUIRE) != 2 || (!enabled && s->blend == 0.0f)) return;
    dl = sc_ms_clean(dl); dr = sc_ms_clean(dr);
    *l = sc_ms_clean(*l); *r = sc_ms_clean(*r);
    if (s->blend == 0.0f) {
        s->pos = s->valid = s->fade = 0;
        s->delay = s->next_delay = (float)sc_ms_clamp(ms) * 96.0f;
        s->head = s->head2 = s->lp_l = s->lp_r = s->hp_l = s->hp_r = 0;
    }
    float target = enabled ? (float)sc_ms_clamp(ms) * 96.0f : s->next_delay;
    if (!s->fade && target != s->delay) { s->next_delay = target; s->fade = SC_MS_FADE; }
    float tl = sc_ms_tap(s, s->l, s->delay, mode), tr = sc_ms_tap(s, s->r, s->delay, mode);
    if (s->fade) {
        float a = (float)(SC_MS_FADE - s->fade) / (float)SC_MS_FADE;
        tl += a * (sc_ms_tap(s, s->l, s->next_delay, mode) - tl);
        tr += a * (sc_ms_tap(s, s->r, s->next_delay, mode) - tr);
        if (!--s->fade) s->delay = s->next_delay;
    }
    float window = sc_ms_window(s->delay, mode == 2 ? 3072.0f : 4096.0f);
    s->head += (mode == 2 ? 0.5f : (master ? 0.05f : 0.005f + param * 0.15f)) / window;
    if (s->head >= 1.0f) s->head -= 1.0f;
    s->head2 += 1.0f / sc_ms_window(s->delay, 2048.0f);
    if (s->head2 >= 1.0f) s->head2 -= 1.0f;
    if (mode == 4) {
        s->hp_l = sc_ms_clean(s->hp_l + 0.06f * (tl - s->hp_l));
        s->hp_r = sc_ms_clean(s->hp_r + 0.06f * (tr - s->hp_r));
        tl -= s->hp_l; tr -= s->hp_r;
    }
    if (mode != 1) {
        float cutoff = mode == 4 ? 0.28f : 0.40f;
        s->lp_l = sc_ms_clean(s->lp_l + cutoff * (tl - s->lp_l));
        s->lp_r = sc_ms_clean(s->lp_r + cutoff * (tr - s->lp_r));
        tl = s->lp_l; tr = s->lp_r;
    }
    float feedback = master ? (mode == 2 ? 0.65f : 0.68f) : 0.15f + param * 0.70f;
    float fb_l = mode == 4 ? sc_ms_sat(tl * 1.35f) * feedback : sc_ms_sat(tl * feedback);
    float fb_r = mode == 4 ? sc_ms_sat(tr * 1.35f) * feedback : sc_ms_sat(tr * feedback);
    s->l[s->pos] = sc_ms_clean(dl + fb_l);
    s->r[s->pos] = sc_ms_clean(dr + fb_r);
    if (++s->pos == SC_MS_RING) s->pos = 0;
    if (s->valid < SC_MS_RING) ++s->valid;
    s->blend += enabled ? 1.0f / SC_MS_FADE : -1.0f / SC_MS_FADE;
    if (s->blend > 1.0f) s->blend = 1.0f;
    if (s->blend < 0.0f) s->blend = 0.0f;
    float gain = mode == 2 ? 0.65f : 0.70f;
    float wet_l = sc_ms_clean((master ? dl : 0.0f) + tl * gain);
    float wet_r = sc_ms_clean((master ? dr : 0.0f) + tr * gain);
    if (s->blend == 1.0f) { *l = wet_l; *r = wet_r; }
    else { *l += s->blend * (wet_l - *l); *r += s->blend * (wet_r - *r); }
}
#endif
