/* Live Stream tap (#50): the master pair that hook_master_output leaves for the DAC,
 * as 48 kHz s16le stereo on a FIFO that stream-sync's broadcaster reads.
 *
 * Audio thread (sc_tap_push): one relaxed load while nobody listens; otherwise two
 * memcpy into planar ring halves and one release store. Never a lock, an allocation,
 * a syscall or a wait. The ring overruns harmlessly: the drain thread notices and skips.
 *
 * Drain thread (sc_tap_drain_once, SCHED_OTHER): opens the FIFO with O_NONBLOCK only
 * when a reader exists (ENXIO otherwise, so the tap stays off), measures the device
 * rate from the ring's frame counter, converts to 48 kHz s16le and writes without
 * blocking. A full pipe drops audio; a closed reader (EPIPE, SIGPIPE blocked in this
 * thread) turns the tap off again. */
#ifndef SC_LIVE_TAP_H
#define SC_LIVE_TAP_H

#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <string.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

#define SC_TAP_FRAMES 65536u /* power of two; 0.68 s at 96 kHz */
#define SC_TAP_OUT_RATE 48000u
#define SC_TAP_CHUNK 2048u   /* output frames converted per write */

struct sc_tap {
    float l[SC_TAP_FRAMES], r[SC_TAP_FRAMES];
    uint32_t head;  /* frames ever pushed (wraps); audio thread writes, release */
    int on;         /* a reader is connected; drain thread writes */
    /* Drain thread only. */
    uint32_t tail;          /* published (release) so the audio thread's overwrites follow the reads */
    int fd;
    uint32_t rate;          /* snapped device rate, 0 until measured */
    uint32_t probe_head;    /* rate measurement start */
    uint64_t probe_ns;
    double phase;           /* resampler position in source frames, relative to tail */
    uint64_t dropped;       /* frames dropped (overrun or full pipe) */
};

static inline void sc_tap_push(struct sc_tap *t, const float *l, const float *r, uint32_t frames) {
    if (!__atomic_load_n(&t->on, __ATOMIC_RELAXED) || !l || !frames) return;
    if (!r) r = l;
    if (frames > SC_TAP_FRAMES) { l += frames - SC_TAP_FRAMES; r += frames - SC_TAP_FRAMES; frames = SC_TAP_FRAMES; }
    uint32_t h = __atomic_load_n(&t->head, __ATOMIC_RELAXED);
    /* Never waits on the reader: this acquire only orders its earlier reads before our overwrite. */
    (void)__atomic_load_n(&t->tail, __ATOMIC_ACQUIRE);
    uint32_t at = h & (SC_TAP_FRAMES - 1), first = SC_TAP_FRAMES - at;
    if (first > frames) first = frames;
    memcpy(t->l + at, l, first * sizeof(float));
    memcpy(t->r + at, r, first * sizeof(float));
    if (frames > first) {
        memcpy(t->l, l + first, (frames - first) * sizeof(float));
        memcpy(t->r, r + first, (frames - first) * sizeof(float));
    }
    __atomic_store_n(&t->head, h + frames, __ATOMIC_RELEASE);
}

static inline uint64_t sc_tap_now_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000000000u + (uint64_t)ts.tv_nsec;
}

/* Snap a measured rate to the device rates Engine uses; 0 while unsure. */
static inline uint32_t sc_tap_snap_rate(double measured) {
    static const uint32_t rates[] = {44100, 48000, 88200, 96000};
    for (unsigned i = 0; i < sizeof rates / sizeof *rates; i++)
    {
        double d = measured - rates[i];
        if ((d < 0 ? -d : d) <= rates[i] * 0.02) return rates[i];
    }
    return 0;
}

static inline int16_t sc_tap_s16(float x) {
    if (!(x == x)) return 0; /* NaN */
    if (x >= 1.0f) return 32767;
    if (x <= -1.0f) return -32767;
    float y = x * 32767.0f; /* round half away from zero, symmetric, no libm */
    return (int16_t)(y >= 0.0f ? y + 0.5f : y - 0.5f);
}

static inline void sc_tap_off(struct sc_tap *t) {
    __atomic_store_n(&t->on, 0, __ATOMIC_RELAXED);
    if (t->fd >= 0) close(t->fd);
    t->fd = -1;
}

/* Opens the FIFO if a reader waits. Creates it when missing; never replaces another file. */
static inline int sc_tap_connect(struct sc_tap *t, const char *path) {
    struct stat st;
    if (lstat(path, &st)) {
        if (errno != ENOENT || mkfifo(path, 0644)) return 0;
    } else if (!S_ISFIFO(st.st_mode)) {
        return 0;
    }
    int fd = open(path, O_WRONLY | O_NONBLOCK | O_CLOEXEC);
    if (fd < 0) return 0; /* ENXIO: no reader yet */
    t->fd = fd;
    t->rate = 0;
    t->phase = 0;
    t->probe_head = __atomic_load_n(&t->head, __ATOMIC_ACQUIRE);
    __atomic_store_n(&t->tail, t->probe_head, __ATOMIC_RELEASE);
    t->probe_ns = sc_tap_now_ns();
    __atomic_store_n(&t->on, 1, __ATOMIC_RELAXED);
    return 1;
}

/* One drain pass: returns the number of 48 kHz frames written. */
static inline uint32_t sc_tap_drain_once(struct sc_tap *t, const char *path) {
    if (t->fd < 0 && !sc_tap_connect(t, path)) return 0;
    uint32_t head = __atomic_load_n(&t->head, __ATOMIC_ACQUIRE);
    if (!t->rate) {
        uint64_t ns = sc_tap_now_ns() - t->probe_ns;
        if (ns < 1000000000u) return 0; /* 1 s: block-size jitter stays well under the 2% snap */
        t->rate = sc_tap_snap_rate((double)(uint32_t)(head - t->probe_head) * 1e9 / (double)ns);
        if (!t->rate) { t->probe_head = head; t->probe_ns = sc_tap_now_ns(); return 0; }
        __atomic_store_n(&t->tail, head, __ATOMIC_RELEASE); /* start live */
        t->phase = 0;
    }
    if ((uint32_t)(head - t->tail) > SC_TAP_FRAMES - 4096) { /* overrun: jump to the present */
        t->dropped += head - t->tail;
        __atomic_store_n(&t->tail, head, __ATOMIC_RELEASE);
        t->phase = 0;
    }
    double step = (double)t->rate / SC_TAP_OUT_RATE;
    int16_t out[SC_TAP_CHUNK * 2];
    uint32_t written = 0;
    for (;;) {
        uint32_t avail = head - t->tail, n = 0;
        while (n < SC_TAP_CHUNK && t->phase + 1.0 < (double)avail) {
            uint32_t i = (uint32_t)t->phase;
            float f = (float)(t->phase - i);
            uint32_t a = (t->tail + i) & (SC_TAP_FRAMES - 1), b = (t->tail + i + 1) & (SC_TAP_FRAMES - 1);
            float l, r;
            if (step == 2.0) { l = 0.5f * (t->l[a] + t->l[b]); r = 0.5f * (t->r[a] + t->r[b]); }
            else { l = t->l[a] + f * (t->l[b] - t->l[a]); r = t->r[a] + f * (t->r[b] - t->r[a]); }
            out[2 * n] = sc_tap_s16(l);
            out[2 * n + 1] = sc_tap_s16(r);
            n++;
            t->phase += step;
        }
        uint32_t used = (uint32_t)t->phase;
        /* The audio thread may have lapped what was just read: discard it. */
        if ((uint32_t)(__atomic_load_n(&t->head, __ATOMIC_ACQUIRE) - t->tail) > SC_TAP_FRAMES) {
            t->dropped += used;
            __atomic_store_n(&t->tail, __atomic_load_n(&t->head, __ATOMIC_ACQUIRE), __ATOMIC_RELEASE);
            t->phase = 0;
            return written;
        }
        __atomic_store_n(&t->tail, t->tail + used, __ATOMIC_RELEASE);
        t->phase -= used;
        if (!n) return written;
        ssize_t w = write(t->fd, out, (size_t)n * 4);
        if (w < 0) {
            if (errno == EAGAIN) { t->dropped += n; return written; } /* reader is slow: drop */
            sc_tap_off(t); /* EPIPE: reader gone */
            return written;
        }
        written += (uint32_t)w / 4; /* a partial write drops the rest of this chunk */
        if (n < SC_TAP_CHUNK) return written;
    }
}

#endif
