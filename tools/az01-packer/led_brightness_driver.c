/* Pad LED Brightness sandbox driver (#325). Sandbox only; see test_led_brightness_sandbox.sh.
 *
 * Preloaded after the real UI hook (LD_PRELOAD=hook:driver) into stock Engine on the build host:
 *  - it defines snd_seq_event_output_direct, the function the hook forwards to, so it records what
 *    would reach the panel, with a monotonic timestamp (the sandbox has no ALSA sequencer);
 *  - one thread plays "Engine's controller thread": it serves 127.0.0.1:8099 and, for the LED
 *    events the QML probe asks for, calls the hook's snd_seq_event_output_direct on that one thread
 *    (a stand-in for ColorOutputAssignment -> device.sendColor -> Engine's sequencer write), and it
 *    can blink one LED the way a playing deck keeps LEDs busy.
 * The QML probe drives the real settings picker action and the real controller-side QML; this file
 * measures when a RGB LED SysEx at the new brightness first leaves the hook.
 * Requests (GET): /led?ch&idx&r&g&b  /blink?on&idx&ms  /mark?l&pct  /hit?l  /result?l&max  /done */
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <dlfcn.h>
#include <errno.h>
#include <netinet/in.h>
#include <poll.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <time.h>
#include <unistd.h>

#define SEQ_SYSEX 130
struct seq_ext { uint32_t len; void *ptr; } __attribute__((packed));
struct capture { uint64_t ns; void *handle; unsigned len; unsigned char b[16]; };
enum { MAX_CAPTURE = 8192, MAX_MARK = 16 };
static struct capture captures[MAX_CAPTURE];
static unsigned ncaptured, nrgb_total;
static pthread_mutex_t cap_mu = PTHREAD_MUTEX_INITIALIZER;
static struct mark { char label[32]; int pct, expect; uint64_t t_mark, t_hit; unsigned first_index; int first_value; unsigned events_before_hit; int used; } marks[MAX_MARK];
static int failures, results;

static uint64_t now_ns(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (uint64_t)t.tv_sec * 1000000000u + (uint64_t)t.tv_nsec;
}
/* The five steps of led_scaled_sysex for a full-white pad (m = 127): 100/80/60/40/20 % */
static int expected_127(int pct) { return pct >= 100 ? 127 : pct >= 75 ? 64 : pct >= 55 ? 28 : pct >= 35 ? 12 : 4; }

/* What the hook calls as the real sequencer write (RTLD_NEXT lands here: this library follows the hook). */
int snd_seq_event_output_direct(void *handle, void *ev) {
    const unsigned char *e = ev;
    if (!e || e[0] != SEQ_SYSEX || (e[1] & 0x0c) != 0x04) return 0;
    struct seq_ext ext;
    memcpy(&ext, e + 16, sizeof ext);
    if (!ext.ptr || ext.len == 0 || ext.len > 16) return 0;
    const unsigned char *b = ext.ptr;
    uint64_t t = now_ns();
    pthread_mutex_lock(&cap_mu);
    if (ncaptured < MAX_CAPTURE) {
        struct capture *c = &captures[ncaptured++];
        c->ns = t; c->handle = handle; c->len = ext.len;
        memcpy(c->b, b, ext.len);
    }
    if (ext.len == 15 && b[8] == 0x05) { /* RGB LED SysEx */
        nrgb_total++;
        for (unsigned i = 0; i < MAX_MARK; i++) {
            struct mark *m = &marks[i];
            if (!m->used || m->t_hit) continue;
            if (b[11] == m->expect && b[11] == b[12] && b[12] == b[13]) { /* white pad at the new step */
                m->t_hit = t;
                m->first_value = b[11];
                m->events_before_hit = ncaptured - m->first_index;
            } else if (b[11] == m->expect && b[12] == 0 && b[13] == 0) { /* blinking red LED at the new step */
                m->t_hit = t;
                m->first_value = b[11];
                m->events_before_hit = ncaptured - m->first_index;
            }
        }
    }
    pthread_mutex_unlock(&cap_mu);
    return 0;
}

/* Engine's own call into the (hooked) function. */
static int send_led(int ch, int idx, int r, int g, int b) {
    static int (*hooked)(void *, void *);
    if (!hooked) hooked = (int (*)(void *, void *))dlsym(RTLD_DEFAULT, "snd_seq_event_output_direct");
    unsigned char sysex[15] = {0xF0, 0x00, 0x02, 0x0B, 0, 0, 0x03, 0x00, 0x05, (unsigned char)(ch & 0x0f), (unsigned char)(idx & 0x7f),
                               (unsigned char)r, (unsigned char)g, (unsigned char)b, 0xF7};
    unsigned char ev[28] __attribute__((aligned(8))) = {0};
    struct seq_ext ext = {sizeof sysex, sysex};
    ev[0] = SEQ_SYSEX;
    ev[1] = 0x04; /* variable length */
    memcpy(ev + 16, &ext, sizeof ext);
    return hooked ? hooked((void *)0x1, ev) : -1;
}

static int query_int(const char *q, const char *key, int def) {
    char pat[40];
    snprintf(pat, sizeof pat, "%s=", key);
    for (const char *p = q; (p = strstr(p, pat)); p++)
        if (p == q || p[-1] == '&' || p[-1] == '?') return atoi(p + strlen(pat));
    return def;
}
static void query_str(const char *q, const char *key, char *out, size_t n) {
    char pat[40];
    out[0] = 0;
    snprintf(pat, sizeof pat, "%s=", key);
    for (const char *p = q; (p = strstr(p, pat)); p++)
        if (p == q || p[-1] == '&' || p[-1] == '?') {
            size_t i = 0;
            for (p += strlen(pat); *p && *p != '&' && *p != ' ' && i + 1 < n; p++) out[i++] = *p;
            out[i] = 0;
            return;
        }
}
static struct mark *find_mark(const char *label, int create) {
    for (unsigned i = 0; i < MAX_MARK; i++)
        if (marks[i].used && !strcmp(marks[i].label, label)) return &marks[i];
    if (!create) return NULL;
    for (unsigned i = 0; i < MAX_MARK; i++)
        if (!marks[i].used) { marks[i].used = 1; snprintf(marks[i].label, sizeof marks[i].label, "%s", label); return &marks[i]; }
    return NULL;
}

static int blink_on, blink_idx = 3, blink_ms = 100, blink_state;
static uint64_t blink_next;

static void handle(const char *path, char *body, size_t n) {
    const char *q = strchr(path, '?');
    q = q ? q + 1 : "";
    snprintf(body, n, "ok");
    if (!strncmp(path, "/led", 4)) {
        send_led(query_int(q, "ch", 0), query_int(q, "idx", 0), query_int(q, "r", 0), query_int(q, "g", 0), query_int(q, "b", 0));
    } else if (!strncmp(path, "/blink", 6)) {
        blink_on = query_int(q, "on", 0);
        blink_idx = query_int(q, "idx", 3);
        blink_ms = query_int(q, "ms", 100);
        blink_state = 0;
        blink_next = now_ns();
    } else if (!strncmp(path, "/mark", 5)) {
        char label[32];
        query_str(q, "l", label, sizeof label);
        pthread_mutex_lock(&cap_mu);
        struct mark *m = find_mark(label, 1);
        if (m) {
            m->pct = query_int(q, "pct", 100);
            m->expect = expected_127(m->pct);
            m->t_mark = now_ns();
            m->t_hit = 0;
            m->first_index = ncaptured;
        }
        pthread_mutex_unlock(&cap_mu);
    } else if (!strncmp(path, "/hit", 4)) {
        char label[32];
        query_str(q, "l", label, sizeof label);
        pthread_mutex_lock(&cap_mu);
        struct mark *m = find_mark(label, 0);
        snprintf(body, n, "%d", m && m->t_hit ? 1 : 0);
        pthread_mutex_unlock(&cap_mu);
    } else if (!strncmp(path, "/result", 7)) {
        char label[32];
        query_str(q, "l", label, sizeof label);
        int max_ms = query_int(q, "max", 1000);
        pthread_mutex_lock(&cap_mu);
        struct mark *m = find_mark(label, 0);
        results++;
        if (m && m->t_hit) {
            int ms = (int)((m->t_hit - m->t_mark) / 1000000u);
            int ok = ms <= max_ms;
            if (!ok) failures++;
            fprintf(stderr, "LEDB %s|%s: pad SysEx at %d%% (first byte %d) left the hook %d ms after the pick (limit %d ms), %u events since the pick\n",
                    ok ? "PASS" : "FAIL", label, m->pct, m->first_value, ms, max_ms, m->events_before_hit);
        } else {
            failures++;
            fprintf(stderr, "LEDB FAIL|%s: no pad SysEx at the new level (%d%%, first byte %d) within the wait; %u events captured since the pick\n",
                    label, m ? m->pct : -1, m ? m->expect : -1, m ? ncaptured - m->first_index : 0);
        }
        pthread_mutex_unlock(&cap_mu);
    } else if (!strncmp(path, "/done", 5)) {
        fprintf(stderr, "LEDB done|%s\n", failures || !results ? "FAIL" : "PASS");
    } else {
        snprintf(body, n, "unknown");
    }
}

static void *engine_thread(void *arg) {
    (void)arg;
    int fd = socket(AF_INET, SOCK_STREAM, 0);
    int one = 1;
    setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
    struct sockaddr_in a = {.sin_family = AF_INET, .sin_port = htons(8099)};
    a.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    if (bind(fd, (struct sockaddr *)&a, sizeof a) || listen(fd, 8)) {
        fprintf(stderr, "LEDB driver: cannot listen on 8099: %s\n", strerror(errno));
        return NULL;
    }
    fprintf(stderr, "LEDB driver: listening\n");
    for (;;) {
        struct pollfd p = {fd, POLLIN, 0};
        int r = poll(&p, 1, 10);
        if (blink_on && now_ns() >= blink_next) {
            blink_state ^= 1;
            send_led(0, blink_idx, blink_state ? 127 : 0, 0, 0);
            blink_next += (uint64_t)blink_ms * 1000000u;
        }
        if (r <= 0) continue;
        int c = accept(fd, NULL, NULL);
        if (c < 0) continue;
        char req[2048];
        ssize_t got = recv(c, req, sizeof req - 1, 0);
        if (got > 0) {
            req[got] = 0;
            char *sp = strchr(req, ' ');
            char *end = sp ? strchr(sp + 1, ' ') : NULL;
            char body[64] = "bad";
            if (sp && end) {
                *end = 0;
                handle(sp + 1, body, sizeof body);
            }
            char out[256];
            int len = snprintf(out, sizeof out, "HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\nAccess-Control-Allow-Origin: *\r\nContent-Length: %zu\r\nConnection: close\r\n\r\n%s",
                               strlen(body), body);
            if (send(c, out, (size_t)len, MSG_NOSIGNAL) < 0) {}
        }
        close(c);
    }
    return NULL;
}

__attribute__((constructor)) static void start(void) {
    char exe[256] = "";
    ssize_t n = readlink("/proc/self/exe", exe, sizeof exe - 1);
    if (n < 0) return;
    exe[n] = 0;
    const char *base = strrchr(exe, '/');
    if (!base || strcmp(base + 1, "Engine")) return; /* Engine only, not its OfflineAnalyzer children */
    pthread_t t;
    if (pthread_create(&t, NULL, engine_thread, NULL) == 0) pthread_detach(t);
}
