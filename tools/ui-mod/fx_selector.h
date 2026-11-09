/* Bounded, nonblocking selector routes. Control/event writers publish only
 * atomic words; an audio callback exclusively owns its DSP bank via one CAS.
 * An absent/ambiguous/stale binding always leaves the carrier untouched. */
#ifndef SC_FX_SELECTOR_H
#define SC_FX_SELECTOR_H
#include "pioneer_fx.h"
#define SC_PFX_UNITS 6
struct sc_pfx_route {
    unsigned route, bound, busy;
    void *owner, *effect;
    void *input_a, *input_b;
    void *challenge_owner;
    unsigned challenge_a, challenge_b, challenge_route;
    void *native_owner, *mix_handler;
    unsigned native_override, restore_mix;
    unsigned mix, parameter, time;
    void *audio_effect;
    unsigned audio_epoch, render_epoch; /* audio owner only */
    struct sc_pfx_instance dsp;
};
static struct sc_pfx_route sc_pfx_routes[SC_PFX_UNITS];
static inline int sc_pfx_bind(unsigned unit, void *owner, void *effect);
static inline unsigned sc_pfx_bits(float f) { unsigned n; memcpy(&n, &f, 4); return n; }
static inline float sc_pfx_float(unsigned n) { float f; memcpy(&f, &n, 4); return f; }

static inline void sc_pfx_select(unsigned unit, unsigned mode) {
    if (unit >= SC_PFX_UNITS || mode > 5) return;
    struct sc_pfx_route *r = &sc_pfx_routes[unit];
    unsigned old = __atomic_load_n(&r->route, __ATOMIC_RELAXED);
    unsigned bound = __atomic_load_n(&r->bound, __ATOMIC_ACQUIRE);
    if ((old & 7) && mode && bound && ((bound ^ old) & ~7u) == 0) {
        __atomic_store_n(&r->route, (old & ~7u) | mode, __ATOMIC_RELEASE);
        return; /* Same proven carrier: one atomic mode change, no stock gap. */
    }
    unsigned next = ((old + 8) & ~7u) | mode;
    __atomic_store_n(&r->route, next, __ATOMIC_RELEASE);
}

static inline int sc_pfx_arm(unsigned unit, unsigned mode, unsigned a, unsigned b) {
    if (unit >= SC_PFX_UNITS) return 0;
    struct sc_pfx_route *r = &sc_pfx_routes[unit];
    unsigned old = __atomic_load_n(&r->route, __ATOMIC_ACQUIRE);
    unsigned bound = __atomic_load_n(&r->bound, __ATOMIC_ACQUIRE);
    if (mode && (old & 7) && bound && ((bound ^ old) & ~7u) == 0) {
        sc_pfx_select(unit, mode);
        return 1;
    }
    if (__atomic_load_n(&r->native_override, __ATOMIC_ACQUIRE))
        __atomic_store_n(&r->restore_mix, 1, __ATOMIC_RELEASE);
    __atomic_store_n(&r->challenge_route, 0, __ATOMIC_RELEASE);
    __atomic_store_n(&r->owner, NULL, __ATOMIC_RELEASE);
    __atomic_store_n(&r->bound, 0, __ATOMIC_RELEASE);
    __atomic_store_n(&r->challenge_a, a, __ATOMIC_RELAXED);
    __atomic_store_n(&r->challenge_b, b, __ATOMIC_RELAXED);
    __atomic_store_n(&r->challenge_owner, NULL, __ATOMIC_RELAXED);
    sc_pfx_select(unit, mode);
    if (mode) __atomic_store_n(&r->challenge_route, __atomic_load_n(&r->route, __ATOMIC_ACQUIRE), __ATOMIC_RELEASE);
    return 0;
}

/* Two exact float challenges below a MIDI detent identify a unit's native
 * ChannelDJFx owner. Both must arrive on the same owner in the same generation.
 * Consumed messages never reach stock Mix, so the handshake cannot duck audio. */
static inline int sc_pfx_challenge(void *owner, float value) {
    unsigned bits = sc_pfx_bits(value);
    for (unsigned i = 0; i < SC_PFX_UNITS; ++i) {
        struct sc_pfx_route *r = &sc_pfx_routes[i];
        unsigned generation = __atomic_load_n(&r->challenge_route, __ATOMIC_ACQUIRE);
        if (!generation || generation != __atomic_load_n(&r->route, __ATOMIC_ACQUIRE)) continue;
        if (bits == __atomic_load_n(&r->challenge_a, __ATOMIC_RELAXED)) {
            __atomic_store_n(&r->challenge_owner, owner, __ATOMIC_RELEASE);
            return 1;
        }
        if (bits == __atomic_load_n(&r->challenge_b, __ATOMIC_RELAXED)) {
            void *first = __atomic_load_n(&r->challenge_owner, __ATOMIC_ACQUIRE);
            if (first == owner) {
                __atomic_store_n(&r->owner, owner, __ATOMIC_RELEASE);
                __atomic_store_n(&r->challenge_route, 0, __ATOMIC_RELEASE);
            }
            return 1; /* An out-of-order challenge also must not touch stock Mix. */
        }
        if (__atomic_load_n(&r->challenge_owner, __ATOMIC_ACQUIRE) == owner)
            __atomic_store_n(&r->challenge_owner, NULL, __ATOMIC_RELEASE);
    }
    return 0;
}

static inline int sc_pfx_bind_context(void *owner, void *effect) {
    unsigned found = SC_PFX_UNITS;
    for (unsigned i = 0; i < SC_PFX_UNITS; ++i) {
        struct sc_pfx_route *r = &sc_pfx_routes[i];
        if (__atomic_load_n(&r->owner, __ATOMIC_ACQUIRE) != owner ||
            !(__atomic_load_n(&r->route, __ATOMIC_ACQUIRE) & 7)) continue;
        if (found != SC_PFX_UNITS) return 0;
        found = i;
    }
    return found != SC_PFX_UNITS && sc_pfx_bind(found, owner, effect);
}

static inline int sc_pfx_bind(unsigned unit, void *owner, void *effect) {
    if (unit >= SC_PFX_UNITS || !owner || !effect) return 0;
    struct sc_pfx_route *r = &sc_pfx_routes[unit];
    unsigned route = __atomic_load_n(&r->route, __ATOMIC_ACQUIRE);
    if (!(route & 7)) return 0;
    unsigned bound = __atomic_load_n(&r->bound, __ATOMIC_ACQUIRE);
    if (bound && !((bound ^ route) & ~7u) &&
        __atomic_load_n(&r->owner, __ATOMIC_ACQUIRE) == owner &&
        __atomic_load_n(&r->effect, __ATOMIC_ACQUIRE) == effect) return 1;
    __atomic_store_n(&r->bound, 0, __ATOMIC_RELEASE);
    __atomic_store_n(&r->owner, owner, __ATOMIC_RELAXED);
    __atomic_store_n(&r->effect, effect, __ATOMIC_RELAXED);
    __atomic_store_n(&r->bound, route, __ATOMIC_RELEASE);
    return 1;
}

static inline void sc_pfx_unbind_owner(void *owner) {
    for (unsigned i = 0; i < SC_PFX_UNITS; ++i)
        if (__atomic_load_n(&sc_pfx_routes[i].owner, __ATOMIC_ACQUIRE) == owner) {
            __atomic_store_n(&sc_pfx_routes[i].bound, 0, __ATOMIC_RELEASE);
            __atomic_store_n(&sc_pfx_routes[i].owner, NULL, __ATOMIC_RELEASE);
        }
}

static inline void sc_pfx_forget_owner(void *owner) {
    for (unsigned i = 0; i < SC_PFX_UNITS; ++i) {
        struct sc_pfx_route *r = &sc_pfx_routes[i];
        if (__atomic_load_n(&r->owner, __ATOMIC_ACQUIRE) == owner ||
            __atomic_load_n(&r->native_owner, __ATOMIC_ACQUIRE) == owner) {
            __atomic_store_n(&r->bound, 0, __ATOMIC_RELEASE);
            __atomic_store_n(&r->owner, NULL, __ATOMIC_RELEASE);
            __atomic_store_n(&r->native_owner, NULL, __ATOMIC_RELEASE);
            __atomic_store_n(&r->effect, NULL, __ATOMIC_RELEASE);
            __atomic_store_n(&r->input_a, NULL, __ATOMIC_RELEASE);
            __atomic_store_n(&r->input_b, NULL, __ATOMIC_RELEASE);
            __atomic_store_n(&r->mix_handler, NULL, __ATOMIC_RELEASE);
            __atomic_store_n(&r->restore_mix, 0, __ATOMIC_RELEASE);
            __atomic_store_n(&r->native_override, 0, __ATOMIC_RELEASE);
            __atomic_store_n(&r->challenge_route, 0, __ATOMIC_RELEASE);
        }
        if (__atomic_load_n(&r->challenge_owner, __ATOMIC_ACQUIRE) == owner)
            __atomic_store_n(&r->challenge_owner, NULL, __ATOMIC_RELEASE);
    }
}

static inline void sc_pfx_control(void *owner, unsigned kind, float value) {
    if (!(value >= 0 && value <= 1)) return;
    for (unsigned i = 0; i < SC_PFX_UNITS; ++i) {
        struct sc_pfx_route *r = &sc_pfx_routes[i];
        if (__atomic_load_n(&r->owner, __ATOMIC_ACQUIRE) != owner) continue;
        unsigned *p = kind == 0 ? &r->mix : (kind == 1 ? &r->parameter : &r->time);
        __atomic_store_n(p, sc_pfx_bits(value), __ATOMIC_RELEASE);
    }
}

static inline struct sc_pfx_route *sc_pfx_enter_input(void *effect, void *buffers, unsigned *mode) {
    struct sc_pfx_route *found = NULL;
    unsigned generation = 0;
    for (unsigned i = 0; i < SC_PFX_UNITS; ++i) {
        struct sc_pfx_route *r = &sc_pfx_routes[i];
        unsigned route = __atomic_load_n(&r->route, __ATOMIC_ACQUIRE);
        unsigned bound = __atomic_load_n(&r->bound, __ATOMIC_ACQUIRE);
        if (!(route & 7) || !bound || ((bound ^ route) & ~7u) ||
            __atomic_load_n(&r->effect, __ATOMIC_ACQUIRE) != effect) continue;
        if (buffers && __atomic_load_n(&r->input_a, __ATOMIC_ACQUIRE) != buffers &&
            __atomic_load_n(&r->input_b, __ATOMIC_ACQUIRE) != buffers) continue;
        if (found) return NULL; /* Never guess between duplicate bindings. */
        found = r; generation = route;
    }
    if (!found || __atomic_exchange_n(&found->busy, 1, __ATOMIC_ACQUIRE)) return NULL;
    if (__atomic_load_n(&found->route, __ATOMIC_ACQUIRE) != generation ||
        !__atomic_load_n(&found->bound, __ATOMIC_ACQUIRE) ||
        ((__atomic_load_n(&found->bound, __ATOMIC_ACQUIRE) ^ generation) & ~7u) ||
        __atomic_load_n(&found->effect, __ATOMIC_ACQUIRE) != effect ||
        (buffers && __atomic_load_n(&found->input_a, __ATOMIC_ACQUIRE) != buffers &&
         __atomic_load_n(&found->input_b, __ATOMIC_ACQUIRE) != buffers)) {
        __atomic_store_n(&found->busy, 0, __ATOMIC_RELEASE);
        return NULL;
    }
    *mode = generation & 7;
    found->render_epoch = generation & ~7u;
    return found;
}
static inline struct sc_pfx_route *sc_pfx_enter(void *effect, unsigned *mode) { return sc_pfx_enter_input(effect, NULL, mode); }
static inline void sc_pfx_leave(struct sc_pfx_route *r) { __atomic_store_n(&r->busy, 0, __ATOMIC_RELEASE); }
#endif
