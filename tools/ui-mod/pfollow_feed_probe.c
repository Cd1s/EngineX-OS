#define _GNU_SOURCE
#include <fcntl.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

struct record {
    uint32_t seq, active, deck, reserved;
    uint64_t track_hash;
    double position_samples, rate_samples_per_sec;
    uint64_t mono_ns;
};

static void *feed_thread(void *unused) {
    (void)unused;
    const char *path = "/run/enginex/shadow-position";
    mkdir("/run/enginex", 0755);
    int fd = open(path, O_CREAT | O_RDWR | O_CLOEXEC, 0644);
    if (fd < 0) return NULL;
    if (ftruncate(fd, (off_t)(sizeof(struct record) * 4))) { close(fd); return NULL; }
    double position = 0;
    for (;;) {
        struct timespec ts;
        if (clock_gettime(CLOCK_MONOTONIC_RAW, &ts)) break;
        struct record r = {
            .seq = 1, .active = 1, .deck = 2, .track_hash = UINT64_C(0x5305feed),
            .position_samples = position, .rate_samples_per_sec = 44100.0,
            .mono_ns = (uint64_t)ts.tv_sec * UINT64_C(1000000000) + (uint64_t)ts.tv_nsec};
        if (pwrite(fd, &r.seq, sizeof r.seq, (off_t)sizeof r * 1) != (ssize_t)sizeof r.seq) break;
        if (pwrite(fd, ((char *)&r) + 4, sizeof r - 4, (off_t)sizeof r * 1 + 4) != (ssize_t)sizeof r - 4) break;
        r.seq = 2;
        if (pwrite(fd, &r.seq, sizeof r.seq, (off_t)sizeof r * 1) != (ssize_t)sizeof r.seq) break;
        fprintf(stderr, "PROBE_FEED position=%.0f rate=%.0f play=0\n", position, r.rate_samples_per_sec);
        fflush(stderr);
        position += 4410.0;
        usleep(100000);
    }
    close(fd);
    return NULL;
}

__attribute__((constructor)) static void start_feed(void) {
    pthread_t thread;
    if (!pthread_create(&thread, NULL, feed_thread, NULL)) pthread_detach(thread);
}
