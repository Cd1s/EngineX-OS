//go:build ignore

/* Sandbox-only ALSA null + monotonic audio clock + headless eglfs_kms vsync
 * fixture for tools/shadow-twin. Derived from tools/az01-packer/sclive-aux-audio-fixture.c.
 * Never ship this library in release images. */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

typedef struct _snd_pcm snd_pcm_t;
typedef unsigned long snd_pcm_uframes_t;
typedef long snd_pcm_sframes_t;

static const char *card_name(void) {
    const char *c = getenv("SBX_CARD");
    return c && *c ? c : "JP07";
}

static unsigned env_uint(const char *k, unsigned def) {
    const char *v = getenv(k);
    return v && *v ? (unsigned)atoi(v) : def;
}

static double env_double(const char *k, double def) {
    const char *v = getenv(k);
    return v && *v ? atof(v) : def;
}

static int denied(const char *name) {
    const char *d = getenv("SBX_DENY");
    return d && *d && strstr(name, d);
}

int snd_card_next(int *c) {
    *c = *c < 0 ? 0 : -1;
    return 0;
}

int snd_ctl_open(void **ctl, const char *name, int mode) {
    (void)mode;
    if (denied(name)) return -19;
    *ctl = (void *)0x171;
    return 0;
}

int snd_ctl_close(void *ctl) {
    (void)ctl;
    return 0;
}

int snd_ctl_card_info(void *ctl, void *info) {
    (void)ctl;
    (void)info;
    return 0;
}

const char *snd_ctl_card_info_get_name(const void *info) {
    (void)info;
    return card_name();
}

const char *snd_ctl_card_info_get_components(const void *info) {
    (void)info;
    const char *c = getenv("SBX_COMPONENTS");
    return c && *c ? c : "USB15e4:0001";
}

int snd_ctl_pcm_next_device(void *ctl, int *d) {
    (void)ctl;
    *d = *d < 0 ? 0 : -1;
    return 0;
}

int snd_ctl_pcm_info(void *ctl, void *info) {
    (void)ctl;
    (void)info;
    return 0;
}

int snd_ctl_elem_info(void *ctl, void *info) {
    (void)ctl;
    (void)info;
    return -2;
}

int snd_ctl_elem_read(void *ctl, void *v) {
    (void)ctl;
    (void)v;
    return -2;
}

int snd_ctl_elem_write(void *ctl, void *v) {
    (void)ctl;
    (void)v;
    return -2;
}

const char *snd_pcm_info_get_name(const void *info) {
    (void)info;
    return card_name();
}

int snd_pcm_hw_params_any(snd_pcm_t *pcm, void *params) {
    int (*real_any)(snd_pcm_t *, void *) = dlsym(RTLD_NEXT, "snd_pcm_hw_params_any");
    int (*set_ch)(snd_pcm_t *, void *, unsigned) = dlsym(RTLD_NEXT, "snd_pcm_hw_params_set_channels");
    int (*get_stream)(snd_pcm_t *) = dlsym(RTLD_NEXT, "snd_pcm_stream");
    int rc = real_any(pcm, params);
    /* JP07 SC5000 has 4 playback channels (2 stereo pairs: Main + Layer B) and 2 capture channels. */
    unsigned count = get_stream(pcm) == 1 ? env_uint("SBX_IN", 2) : env_uint("SBX_OUT", 4);
    if (rc >= 0) rc = set_ch(pcm, params, count);
    return rc;
}

int snd_pcm_hw_params_test_format(snd_pcm_t *pcm, void *params, int format) {
    int (*real_test)(snd_pcm_t *, void *, int) = dlsym(RTLD_NEXT, "snd_pcm_hw_params_test_format");
    return format == 10 ? real_test(pcm, params, format) : -22; /* SND_PCM_FORMAT_S32_LE */
}

static unsigned hw_rate_hz = 96000;
static unsigned clock_hz = 48000;

int snd_pcm_hw_params(snd_pcm_t *pcm, void *params) {
    int (*real_hw)(snd_pcm_t *, void *) = dlsym(RTLD_NEXT, "snd_pcm_hw_params");
    int (*get_rate)(const void *, unsigned *, int *) = dlsym(RTLD_NEXT, "snd_pcm_hw_params_get_rate");
    int (*get_stream)(snd_pcm_t *) = dlsym(RTLD_NEXT, "snd_pcm_stream");
    int (*get_ch)(const void *, unsigned *) = dlsym(RTLD_NEXT, "snd_pcm_hw_params_get_channels");
    int rc = real_hw(pcm, params), dir = 0;
    unsigned r = 0, ch = 0;
    if (rc >= 0 && get_rate && get_rate(params, &r, &dir) >= 0 && r) {
        hw_rate_hz = r;
    }
    clock_hz = env_uint("SBX_CLOCK_HZ", 48000);
    if (get_ch) get_ch(params, &ch);
    fprintf(stderr, "SBXA HW|pcm=%p|stream=%d|hw_rate=%u|clock_hz=%u|ch=%u|rc=%d|\n",
            (void *)pcm, get_stream ? get_stream(pcm) : -1, hw_rate_hz, clock_hz, ch, rc);
    fflush(stderr);
    return rc;
}

int snd_pcm_open(snd_pcm_t **pcm, const char *name, int stream, int mode) {
    int (*real_open)(snd_pcm_t **, const char *, int, int) = dlsym(RTLD_NEXT, "snd_pcm_open");
    int rc = denied(name) ? -19 : real_open(pcm, "null", stream, mode);
    fprintf(stderr, "SBXA OPEN|name=%s|stream=%d|rc=%d|\n", name, stream, rc);
    fflush(stderr);
    return rc;
}

int snd_pcm_start(snd_pcm_t *pcm) {
    int (*real_start)(snd_pcm_t *) = dlsym(RTLD_NEXT, "snd_pcm_start");
    int (*get_state)(snd_pcm_t *) = dlsym(RTLD_NEXT, "snd_pcm_state");
    int rc = real_start(pcm), st = get_state(pcm);
    return rc == -77 && st == 3 ? 0 : rc; /* EBADFD when null PCM already RUNNING */
}

static uint64_t mono_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000000000ull + (uint64_t)ts.tv_nsec;
}

static void sleep_until_ns(uint64_t target_ns) {
    struct timespec ts = {
        .tv_sec = (time_t)(target_ns / 1000000000ull),
        .tv_nsec = (long)(target_ns % 1000000000ull),
    };
    clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, &ts, NULL);
}

/* Engine on JP07 runs two separate RT threads: AudioOut (stream 0, writei) and
 * AudioIn (stream 1, readi), each transferring 450 frames per block. Both must
 * be paced at the same monotonic rate so AudioIn neither spins nor overflows
 * the ringbuffer shared with AudioOut. */
static uint64_t stream_next_ns[2];
static uint64_t write_start_ns;
static uint64_t write_origin_frames;
static uint64_t write_total_frames;
static uint64_t write_calls;

static void pace_stream(int stream, snd_pcm_uframes_t frames) {
    static int ppm_init = 0;
    static double rate_scale = 1.0;
    if (!ppm_init) {
        double ppm = env_double("SBX_CLOCK_PPM", 0.0);
        rate_scale = 1.0 + ppm * 1e-6;
        ppm_init = 1;
    }
    uint64_t now = mono_ns();
    double eff_rate = (double)clock_hz * rate_scale;
    uint64_t step_ns = (uint64_t)((double)frames * 1000000000.0 / eff_rate + 0.5);
    uint64_t next = stream_next_ns[stream];
    if (next == 0 || now > next + 50000000ull) {
        next = now + step_ns;
        if (stream == 0) {
            write_start_ns = now;
            write_origin_frames = write_total_frames;
        }
    } else {
        next += step_ns;
    }
    stream_next_ns[stream] = next;
    sleep_until_ns(next);

    if (stream == 0) {
        write_total_frames += frames;
        write_calls++;
        if ((write_calls & 255) == 0) {
            uint64_t t = mono_ns();
            double wall_s = (double)(t - write_start_ns) / 1e9;
            double audio_s = (double)(write_total_frames - write_origin_frames) / (double)clock_hz;
            double ppm = wall_s > 1.0 ? (audio_s / wall_s - 1.0) * 1e6 : 0.0;
            int fd = open("/tmp/alsa-clock.txt", O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC, 0644);
            if (fd >= 0) {
                char buf[256];
                int n = snprintf(buf, sizeof buf,
                    "clock_hz=%u\nhw_rate_hz=%u\nframes=%llu\nwall_s=%.6f\naudio_s=%.6f\nppm=%.2f\n",
                    clock_hz, hw_rate_hz, (unsigned long long)write_total_frames, wall_s, audio_s, ppm);
                if (n > 0) (void)!write(fd, buf, (size_t)n);
                close(fd);
            }
        }
    }
}

snd_pcm_sframes_t snd_pcm_readi(snd_pcm_t *pcm, void *buf, snd_pcm_uframes_t frames) {
    static snd_pcm_sframes_t (*real_readi)(snd_pcm_t *, void *, snd_pcm_uframes_t);
    static unsigned long n;
    if (!real_readi) real_readi = dlsym(RTLD_NEXT, "snd_pcm_readi");
    pace_stream(1, frames);
    if (++n == 1) {
        fprintf(stderr, "SBXA READI|pcm=%p|frames=%lu|clock_hz=%u|\n", (void *)pcm, (unsigned long)frames, clock_hz);
        fflush(stderr);
    }
    return real_readi(pcm, buf, frames);
}

snd_pcm_sframes_t snd_pcm_writei(snd_pcm_t *pcm, const void *buf, snd_pcm_uframes_t frames) {
    static snd_pcm_sframes_t (*real_writei)(snd_pcm_t *, const void *, snd_pcm_uframes_t);
    static unsigned long n;
    if (!real_writei) real_writei = dlsym(RTLD_NEXT, "snd_pcm_writei");
    pace_stream(0, frames);
    if (++n == 1) {
        fprintf(stderr, "SBXA WRITEI|pcm=%p|frames=%lu|clock_hz=%u|\n", (void *)pcm, (unsigned long)frames, clock_hz);
        fflush(stderr);
    }
    return real_writei(pcm, buf, frames);
}

/* When ScreenConfiguration.json uses "headless": "1280x800", Qt's
 * QEglFSKmsGbmScreen::waitForFlip() returns immediately without waiting for KMS
 * vblank. Pace QEGLPlatformContext::swapBuffers at 60 Hz when SBX_HEADLESS_VSYNC=1
 * so both instances A and B can render full OpenGL simultaneously at 60 FPS. */
static uint64_t vsync_next_ns;

int _ZN19QEGLPlatformContext11swapBuffersEP16QPlatformSurface(void *ctx, void *surface) {
    static int (*real_swap)(void *, void *);
    static int vsync_mode = -1;
    if (!real_swap) real_swap = dlsym(RTLD_NEXT, "_ZN19QEGLPlatformContext11swapBuffersEP16QPlatformSurface");
    if (vsync_mode < 0) {
        const char *v = getenv("SBX_HEADLESS_VSYNC");
        vsync_mode = (v && !strcmp(v, "1")) ? 1 : 0;
    }
    int rc = real_swap(ctx, surface);
    if (vsync_mode) {
        const uint64_t frame_ns = 16666667ull; /* 60.00 Hz */
        uint64_t now = mono_ns();
        if (vsync_next_ns == 0 || now > vsync_next_ns + 50000000ull) {
            vsync_next_ns = now + frame_ns;
        } else {
            vsync_next_ns += frame_ns;
        }
        sleep_until_ns(vsync_next_ns);
    }
    return rc;
}

/* Engine pins both EMain and SceneGraph (plus llvmpipe software GL) to CPU 1,
 * and pins TrackLoad1 + AudioIn to CPU 2. In a dual-instance software-GL
 * sandbox on a 4-core host, letting the kernel scheduler balance across all 4
 * cores avoids single-core contention between EMain and llvmpipe. */
int sched_setaffinity(int pid, size_t cpusetsize, const void *mask) {
    (void)pid;
    (void)cpusetsize;
    (void)mask;
    return 0;
}

int pthread_setaffinity_np(unsigned long thread, size_t cpusetsize, const void *cpuset) {
    (void)thread;
    (void)cpusetsize;
    (void)cpuset;
    return 0;
}
