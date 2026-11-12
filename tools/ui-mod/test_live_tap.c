/* gcc -std=c11 -O2 -Wall -Wextra -Werror -pthread tools/ui-mod/test_live_tap.c -lm -o /tmp/test_live_tap && /tmp/test_live_tap
 * Also run once with -fsanitize=address,undefined and once with -fsanitize=thread.
 * #50 Live Stream tap: the master pair reaches the broadcaster's FIFO as 48 kHz s16le. */
#define _GNU_SOURCE
#include <assert.h>
#include <math.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/stat.h>

/* POSIX lets an O_NONBLOCK pipe write above PIPE_BUF be partial at any byte (Linux
 * happens to cut at page slots). Case 7 models the worst case: such a write stops 3
 * bytes short, inside a frame. Writes up to PIPE_BUF stay atomic, as POSIX requires. */
#include <unistd.h> /* the real write() is declared before the macro below */
#include <limits.h>
#ifndef PIPE_BUF
#define PIPE_BUF 4096
#endif
static int posix_split;
static ssize_t posix_pipe_write(int fd, const void *buf, size_t n);
#define write posix_pipe_write
#include "live_tap.h"
#undef write
static ssize_t posix_pipe_write(int fd, const void *buf, size_t n) {
    if (posix_split && n > PIPE_BUF) n -= 3;
    return write(fd, buf, n);
}

static struct sc_tap tap = {.fd = -1};
static char path[64];
static volatile int stop;
static unsigned rate_in;

static float source(unsigned rate, uint64_t i) { /* 1 kHz sine at half scale, exact phase */
    return (float)(0.5 * sin(2.0 * M_PI * (double)((1000 * i) % rate) / rate));
}

static void *producer(void *arg) { /* the audio thread: 5 ms blocks paced in real time */
    (void)arg;
    unsigned block = rate_in / 200;
    float l[480], r[480];
    uint64_t i = 0, t0 = sc_tap_now_ns();
    for (uint64_t n = 1; !__atomic_load_n(&stop, __ATOMIC_RELAXED); n++) {
        for (unsigned k = 0; k < block; k++, i++) { l[k] = source(rate_in, i); r[k] = -l[k]; }
        sc_tap_push(&tap, l, r, block);
        uint64_t due = t0 + n * 5000000u;
        struct timespec ts = {(time_t)(due / 1000000000u), (long)(due % 1000000000u)};
        clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, &ts, NULL);
    }
    return NULL;
}

static void *drainer(void *arg) { /* the drain thread, SIGPIPE blocked as in the hook */
    (void)arg;
    sigset_t set;
    sigemptyset(&set);
    sigaddset(&set, SIGPIPE);
    pthread_sigmask(SIG_BLOCK, &set, NULL);
    while (!__atomic_load_n(&stop, __ATOMIC_RELAXED)) {
        uint64_t t0 = sc_tap_now_ns();
        sc_tap_drain_once(&tap, path);
        assert(sc_tap_now_ns() - t0 < 50000000u); /* never blocks */
        usleep(10000);
    }
    return NULL;
}

/* Runs producer + drainer for `seconds` with a reading FIFO; returns the 48 kHz frames read. */
static size_t run(unsigned rate, double seconds, int16_t *got, size_t cap, int reader_reads) {
    rate_in = rate;
    stop = 0;
    int rfd = open(path, O_RDONLY | O_NONBLOCK);
    assert(rfd >= 0);
    pthread_t p, d;
    pthread_create(&p, NULL, producer, NULL);
    pthread_create(&d, NULL, drainer, NULL);
    size_t frames = 0;
    uint64_t end = sc_tap_now_ns() + (uint64_t)(seconds * 1e9);
    while (sc_tap_now_ns() < end) {
        if (reader_reads) {
            ssize_t n = read(rfd, got + frames * 2, (cap - frames) * 4);
            if (n > 0) frames += (size_t)n / 4;
        }
        usleep(5000);
    }
    __atomic_store_n(&stop, 1, __ATOMIC_RELAXED);
    pthread_join(p, NULL);
    pthread_join(d, NULL);
    close(rfd);
    return frames;
}

int main(void) {
    snprintf(path, sizeof path, "/tmp/test-live-tap-%d.pcm", (int)getpid());
    unlink(path);

    /* 1. Nobody listens: the FIFO is created, the tap stays off, the audio thread does nothing. */
    assert(sc_tap_drain_once(&tap, path) == 0);
    struct stat st;
    assert(!lstat(path, &st) && S_ISFIFO(st.st_mode));
    assert(tap.fd < 0 && !tap.on);
    float block[256] = {0.25f};
    sc_tap_push(&tap, block, block, 256);
    assert(tap.head == 0);
    unlink(path);

    /* 2. Something else at the path is never replaced. */
    FILE *f = fopen(path, "w");
    assert(f && fputs("keep", f) >= 0 && !fclose(f));
    assert(sc_tap_drain_once(&tap, path) == 0 && tap.fd < 0);
    assert(!lstat(path, &st) && S_ISREG(st.st_mode) && st.st_size == 4);
    unlink(path);
    assert(!mkfifo(path, 0644));

    /* 3. 96 kHz device -> 48 kHz: 1 s rate probe, then live audio, L/R kept apart. */
    size_t cap = 48000 * 4;
    int16_t *got = calloc(cap * 2, sizeof *got);
    size_t frames = run(96000, 2.5, got, cap, 1);
    assert(tap.rate == 96000 && tap.on && tap.fd >= 0);
    double sum = 0;
    for (size_t i = 0; i < frames; i++) {
        sum += (double)got[2 * i] * got[2 * i];
        assert(abs(got[2 * i] + got[2 * i + 1]) <= 2); /* R == -L */
    }
    double rms = frames ? sqrt(sum / frames) / 32767.0 : 0;
    printf("96 kHz: %zu frames at 48 kHz in ~1.4 s live, RMS %.3f (sine 0.5 -> 0.354), dropped %llu\n",
           frames, rms, (unsigned long long)tap.dropped);
    assert(frames > 48000 * 1.0 && frames < 48000 * 1.8); /* about 48000 frames per second */
    assert(rms > 0.33 && rms < 0.37);
    sc_tap_off(&tap);

    /* 4. 48 kHz device: passthrough, exact sample values. */
    memset(got, 0, cap * 4);
    frames = run(48000, 2.0, got, cap, 1);
    assert(tap.rate == 48000 && frames > 48000 * 0.6);
    /* Consecutive output samples follow the 1 kHz sine at 48 kHz (phase unknown). */
    int matched = 0;
    for (size_t i = 1; i + 1 < frames && i < 4800; i++) {
        double a = got[2 * (i - 1)] / 32767.0, b = got[2 * i] / 32767.0, c = got[2 * (i + 1)] / 32767.0;
        /* sin(x-d) + sin(x+d) = 2 sin(x) cos(d) */
        if (fabs((a + c) - 2.0 * b * cos(2.0 * M_PI * 1000.0 / 48000.0)) < 2e-4) matched++;
    }
    printf("48 kHz passthrough: %zu frames, %d/4798 samples on the 1 kHz sine\n", frames, matched);
    assert(matched > 4700);
    sc_tap_off(&tap);

    /* 5. A reader that never reads: the pipe fills, audio is dropped, nothing blocks. */
    frames = run(96000, 3.0, got, cap, 0);
    assert(frames == 0 && tap.dropped > 0 && tap.on);
    printf("stalled reader: dropped %llu frames, drain never blocked\n", (unsigned long long)tap.dropped);

    /* 6. The reader goes away: EPIPE turns the tap off (SIGPIPE blocked), no crash. */
    sigset_t set;
    sigemptyset(&set);
    sigaddset(&set, SIGPIPE);
    pthread_sigmask(SIG_BLOCK, &set, NULL);
    for (int i = 0; i < 4 && tap.on; i++) sc_tap_drain_once(&tap, path);
    float more[4800];
    for (int i = 0; i < 4800; i++) more[i] = 0.1f;
    for (int i = 0; i < 20 && tap.on; i++) { sc_tap_push(&tap, more, more, 4800); sc_tap_drain_once(&tap, path); }
    assert(!tap.on && tap.fd < 0);
    puts("reader gone: EPIPE closed the tap, no SIGPIPE");

    /* 7. BH-GF3-2: a write cut inside a frame would shift the rest of the stream by
     * bytes (swapped channels, noise). Every write must be whole frames: each frame
     * read must still be L, -L. */
    unlink(path);
    assert(!mkfifo(path, 0644));
    struct sc_tap t7 = {.fd = -1};
    int r7 = open(path, O_RDONLY | O_NONBLOCK);
    assert(r7 >= 0 && sc_tap_connect(&t7, path));
    t7.rate = 48000; /* skip the 1 s probe: write as a 48 kHz device */
    posix_split = 1;
    static unsigned char seen[1 << 20];
    size_t seen_n = 0, k = 0;
    float l7[4096], r7buf[4096];
    for (int round = 0; round < 10 && t7.on; round++) {
        for (int i = 0; i < 4096; i++, k++) { l7[i] = (float)(k % 1000 + 1) / 2000.0f; r7buf[i] = -l7[i]; }
        sc_tap_push(&t7, l7, r7buf, 4096);
        sc_tap_drain_once(&t7, path);
        ssize_t n;
        while ((n = read(r7, seen + seen_n, sizeof seen - seen_n)) > 0) seen_n += (size_t)n;
    }
    posix_split = 0;
    size_t bad = 0;
    for (size_t i = 0; i + 4 <= seen_n; i += 4) {
        int16_t L, R;
        memcpy(&L, seen + i, 2); memcpy(&R, seen + i + 2, 2);
        if (L <= 0 || R != -L) bad++;
    }
    printf("whole-frame writes: %zu bytes read, %zu misaligned frames\n", seen_n, bad);
    assert(seen_n % 4 == 0 && seen_n >= 4 * 30000 && bad == 0);
    sc_tap_off(&t7);
    close(r7);

    unlink(path);
    free(got);
    puts("live tap: PASS");
    return 0;
}
