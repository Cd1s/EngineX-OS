#ifndef SC_REKORDBOX_STAGE_H
#define SC_REKORDBOX_STAGE_H

// A rekordbox stick's track loaded before the bridge's next publishing window. The background
// job has staged the track's grid, cues, loops and waveforms on the deck (pkg/rbstate's stage),
// but the stick's m.db row is still the insert's, without analysis, and Engine would analyse the
// track itself. Only a write on Engine's own connection can change a library Engine has open, so
// just before Engine reads that row the hook gives it the staged data, by one UPDATE on that
// connection. Design: docs/design/rekordbox-stage-hook.md.
//
// Engine's own library, Link and streaming rows, and a drive with nothing staged cost a compare
// and a look at the stage directory: no statement. Included once, by engine_ui_hook.c, after
// rekordbox_analysis.h (struct sc_rbx_sqlite, sc_rbx_drives, sc_rbx_read, sc_rbx_verbose).

#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <netinet/in.h>
#include <poll.h>
#include <pthread.h>
#include <sched.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <time.h>
#include <unistd.h>
#ifdef __linux__
#include <sys/prctl.h>
#endif

#include "rekordbox_analysis.h"

// What the hook shares with pkg/rbstate. test_rekordbox_stage.py holds them to the Go source.
#define SC_RBS_RULES 5                         // rbstate.Rules: a line of other rules is another build's
#define SC_RBS_INDEX "index.jsonl"             // the stage's index (rbstate stageIndex)
#define SC_RBS_SNAPSHOT ".rb-bridge-snapshot"  // beside m.db (rbstate.SnapshotName)
#define SC_RBS_SNAPSHOT_MAGIC "RBS2 "          // its first line: the magic, then the header's JSON
#define SC_RBS_JOURNAL "filled.jsonl"          // the hook's record of each fill, beside the index
#ifndef SC_RBS_ROOT
#define SC_RBS_ROOT "/data/enginex/rb-stage"   // rbstate's default stage root
#endif
#ifndef SC_RBS_ONDEMAND_SOCK
#define SC_RBS_ONDEMAND_SOCK "/run/enginex/rb-ondemand.sock" // stream-sync direct-read Unix socket
#endif
#define SC_RBS_ONDEMAND_ROUTE "/v1/enginex/rekordbox/track-data"
#define SC_RBS_ONDEMAND_MAX_BODY (4 << 20)
#define SC_RBS_ONDEMAND_TIMEOUT_DEFAULT_MS 1500
#define SC_RBS_NEG_SLOTS 64
#define SC_RBS_INFLIGHT_SLOTS 16
#define SC_RBS_NEG_TTL_PERM_NS (60LL * 1000000000LL)
#define SC_RBS_NEG_TTL_TEMP_NS (3LL * 1000000000LL)
#define SC_RBS_LINE_MAX (64 << 10)             // a longer index line is not read (a staged line is about 1 KiB)
#define SC_RBS_TEXT 1024                       // longest key, path or uuid the hook handles

static const char *sc_rbs_root = SC_RBS_ROOT;
static const char *sc_rbs_ondemand_sock = SC_RBS_ONDEMAND_SOCK;
static const char *sc_rbs_ondemand_url = NULL; // optional http://127.0.0.1:PORT/... override
static int sc_rbs_ondemand_timeout_ms = SC_RBS_ONDEMAND_TIMEOUT_DEFAULT_MS;
static int sc_rbs_ondemand_env_init;
static int (*sc_rbs_sched_override)(void);     // test hook for RT thread bailout verification

// --- SHA-1, for the stage's file names (<hex SHA-1 of the key>.bin) ---------------------------

static uint32_t sc_rbs_rol(uint32_t x, int n) { return x << n | x >> (32 - n); }

static void sc_rbs_sha1_block(uint32_t h[5], const unsigned char *p) {
    uint32_t w[80], a = h[0], b = h[1], c = h[2], d = h[3], e = h[4];
    for (int i = 0; i < 16; i++)
        w[i] = (uint32_t)p[4 * i] << 24 | (uint32_t)p[4 * i + 1] << 16 | (uint32_t)p[4 * i + 2] << 8 | p[4 * i + 3];
    for (int i = 16; i < 80; i++) w[i] = sc_rbs_rol(w[i - 3] ^ w[i - 8] ^ w[i - 14] ^ w[i - 16], 1);
    for (int i = 0; i < 80; i++) {
        uint32_t f, k;
        if (i < 20) f = (b & c) | (~b & d), k = 0x5a827999u;
        else if (i < 40) f = b ^ c ^ d, k = 0x6ed9eba1u;
        else if (i < 60) f = (b & c) | (b & d) | (c & d), k = 0x8f1bbcdcu;
        else f = b ^ c ^ d, k = 0xca62c1d6u;
        uint32_t t = sc_rbs_rol(a, 5) + f + e + k + w[i];
        e = d, d = c, c = sc_rbs_rol(b, 30), b = a, a = t;
    }
    h[0] += a, h[1] += b, h[2] += c, h[3] += d, h[4] += e;
}

// The hex SHA-1 of n bytes at s.
static void sc_rbs_sha1_hex(const char *s, size_t n, char out[41]) {
    uint32_t h[5] = {0x67452301u, 0xefcdab89u, 0x98badcfeu, 0x10325476u, 0xc3d2e1f0u};
    unsigned char block[64];
    size_t at = 0;
    for (; n - at >= 64; at += 64) sc_rbs_sha1_block(h, (const unsigned char *)s + at);
    size_t rest = n - at;
    memcpy(block, s + at, rest);
    block[rest++] = 0x80;
    if (rest > 56) {
        memset(block + rest, 0, 64 - rest);
        sc_rbs_sha1_block(h, block);
        rest = 0;
    }
    memset(block + rest, 0, 56 - rest);
    const uint64_t bits = (uint64_t)n * 8;
    for (int i = 0; i < 8; i++) block[56 + i] = (unsigned char)(bits >> (56 - 8 * i));
    sc_rbs_sha1_block(h, block);
    static const char hex[] = "0123456789abcdef";
    for (int i = 0; i < 20; i++) {
        const unsigned char byte = (unsigned char)(h[i / 4] >> (24 - 8 * (i % 4)));
        out[2 * i] = hex[byte >> 4];
        out[2 * i + 1] = hex[byte & 15];
    }
    out[40] = 0;
}

// --- keys --------------------------------------------------------------------------------------

// rbstate.FoldPath's steps that need no Unicode tables, on a row's path: '\' is '/', then one
// leading "../" and every leading '/' go. With lower, ASCII letters are made lower case, which is
// the whole fold of an ASCII path. The length, or -1: the result does not fit, or (lower) the
// path is not ASCII (folding Unicode case and NFC is the Go side's).
static int sc_rbs_fold(const char *path, char *out, size_t cap, int lower) {
    const char *p = path;
    if (p[0] == '.' && p[1] == '.' && (p[2] == '/' || p[2] == '\\')) p += 3;
    while (*p == '/' || *p == '\\') p++;
    size_t n = 0;
    for (; *p; p++) {
        unsigned char c = (unsigned char)*p;
        if (c >= 0x80 && lower) return -1;
        if (c == '\\') c = '/';
        else if (lower && c >= 'A' && c <= 'Z') c = (unsigned char)(c + ('a' - 'A'));
        if (n + 1 >= cap) return -1;
        out[n++] = (char)c;
    }
    out[n] = 0;
    return (int)n;
}

static uint64_t sc_rbs_hash(const char *s, size_t n) {
    uint64_t h = 14695981039346656037ull;
    for (size_t i = 0; i < n; i++) h = (h ^ (unsigned char)s[i]) * 1099511628211ull;
    return h ? h : 1;
}

// --- the JSON Go writes into index lines and the snapshot's header ---------------------------

static const char *sc_rbs_space(const char *p, const char *end) {
    while (p < end && (*p == ' ' || *p == '\t' || *p == '\r' || *p == '\n')) p++;
    return p;
}

static int sc_rbs_hex4(const char *p, const char *end, unsigned *v) {
    if (end - p < 4) return 0;
    *v = 0;
    for (int i = 0; i < 4; i++) {
        char c = p[i];
        unsigned d = c >= '0' && c <= '9' ? (unsigned)(c - '0') : c >= 'a' && c <= 'f' ? (unsigned)(c - 'a' + 10) :
                     c >= 'A' && c <= 'F' ? (unsigned)(c - 'A' + 10) : 16u;
        if (d > 15) return 0;
        *v = *v << 4 | d;
    }
    return 1;
}

// The JSON string whose opening quote is at p, as UTF-8 in out (out may be NULL: only skipped).
// The position after the closing quote, or NULL: cut, a bad escape, or longer than cap - 1.
static const char *sc_rbs_string(const char *p, const char *end, char *out, size_t cap, size_t *len) {
    size_t n = 0;
    if (p >= end || *p++ != '"') return NULL;
    while (p < end && *p != '"') {
        unsigned cp = (unsigned char)*p++;
        char utf[4];
        size_t k = 1;
        utf[0] = (char)cp;
        if (cp == '\\') {
            if (p >= end) return NULL;
            const char e = *p++;
            switch (e) {
            case '"': case '\\': case '/': utf[0] = e; break;
            case 'b': utf[0] = '\b'; break;
            case 'f': utf[0] = '\f'; break;
            case 'n': utf[0] = '\n'; break;
            case 'r': utf[0] = '\r'; break;
            case 't': utf[0] = '\t'; break;
            case 'u': {
                if (!sc_rbs_hex4(p, end, &cp)) return NULL;
                p += 4;
                if (cp >= 0xd800 && cp < 0xdc00) {
                    unsigned lo;
                    if (end - p < 6 || p[0] != '\\' || p[1] != 'u' || !sc_rbs_hex4(p + 2, end, &lo) || lo < 0xdc00 || lo > 0xdfff) return NULL;
                    p += 6;
                    cp = 0x10000 + ((cp - 0xd800) << 10) + (lo - 0xdc00);
                } else if (cp >= 0xdc00 && cp < 0xe000) {
                    return NULL;
                }
                if (cp < 0x80) utf[0] = (char)cp;
                else if (cp < 0x800) utf[0] = (char)(0xc0 | cp >> 6), utf[1] = (char)(0x80 | (cp & 0x3f)), k = 2;
                else if (cp < 0x10000) utf[0] = (char)(0xe0 | cp >> 12), utf[1] = (char)(0x80 | (cp >> 6 & 0x3f)), utf[2] = (char)(0x80 | (cp & 0x3f)), k = 3;
                else utf[0] = (char)(0xf0 | cp >> 18), utf[1] = (char)(0x80 | (cp >> 12 & 0x3f)), utf[2] = (char)(0x80 | (cp >> 6 & 0x3f)), utf[3] = (char)(0x80 | (cp & 0x3f)), k = 4;
                break;
            }
            default: return NULL;
            }
        } else if (cp < 0x20) {
            return NULL;
        }
        if (out) {
            if (n + k >= cap) return NULL;
            memcpy(out + n, utf, k);
        }
        n += k;
    }
    if (p >= end) return NULL;
    if (out) out[n] = 0;
    if (len) *len = n;
    return p + 1;
}

// The position after the JSON value at p, or NULL.
static const char *sc_rbs_skip(const char *p, const char *end) {
    p = sc_rbs_space(p, end);
    if (p >= end) return NULL;
    if (*p == '"') return sc_rbs_string(p, end, NULL, 0, NULL);
    if (*p == '{' || *p == '[') {
        int depth = 0;
        while (p < end) {
            if (*p == '"') {
                if (!(p = sc_rbs_string(p, end, NULL, 0, NULL))) return NULL;
                continue;
            }
            if (*p == '{' || *p == '[') depth++;
            else if ((*p == '}' || *p == ']') && --depth == 0) return p + 1;
            p++;
        }
        return NULL;
    }
    const char *start = p;
    while (p < end && *p != ',' && *p != '}' && *p != ']' && *p != ' ' && *p != '\n') p++;
    return p > start ? p : NULL;
}

// Calls field for each member of the JSON object at p; field returns the position after the
// member's value (it parses it), or NULL to have it skipped. 0 if p is not a whole object.
typedef const char *(*sc_rbs_field_fn)(void *arg, const char *name, const char *value, const char *end);

static int sc_rbs_object(const char *p, const char *end, sc_rbs_field_fn field, void *arg) {
    char name[32];
    p = sc_rbs_space(p, end);
    if (p >= end || *p++ != '{') return 0;
    p = sc_rbs_space(p, end);
    if (p < end && *p == '}') return 1;
    while (p < end) {
        size_t n = 0;
        const char *after = sc_rbs_string(p, end, name, sizeof name, &n);
        if (!after) { // a name the hook never asks for
            name[0] = 0;
            after = sc_rbs_string(p, end, NULL, 0, NULL);
            if (!after) return 0;
        }
        p = sc_rbs_space(after, end);
        if (p >= end || *p++ != ':') return 0;
        p = sc_rbs_space(p, end);
        const char *next = name[0] ? field(arg, name, p, end) : NULL;
        if (!next && !(next = sc_rbs_skip(p, end))) return 0;
        p = sc_rbs_space(next, end);
        if (p < end && *p == ',') {
            p = sc_rbs_space(p + 1, end);
            continue;
        }
        return p < end && *p == '}';
    }
    return 0;
}

static const char *sc_rbs_integer(const char *p, const char *end, long long *v) {
    int negative = p < end && *p == '-';
    const char *digits = p + negative, *q = digits;
    long long n = 0;
    while (q < end && *q >= '0' && *q <= '9') {
        if (n > 100000000000000LL) return NULL;
        n = n * 10 + (*q++ - '0');
    }
    if (q == digits) return NULL;
    *v = negative ? -n : n;
    return q;
}

static const char *sc_rbs_true(const char *p, const char *end, int *v) {
    if (end - p >= 4 && !memcmp(p, "true", 4)) { *v = 1; return p + 4; }
    if (end - p >= 5 && !memcmp(p, "false", 5)) { *v = 0; return p + 5; }
    return NULL;
}

// --- index lines ------------------------------------------------------------------------------

// One line of index.jsonl: a staged track (its key), a drop (a window applied that key), or a mark.
struct sc_rbs_line {
    int put, drop;                    // which kind it is
    char key[SC_RBS_TEXT];            // "k", or the dropped key
    char rel[SC_RBS_TEXT];            // the audio file's spelling on the volume
    char wr[64];                      // Written digest of the staged grid, cues and loops (base64)
    char wrnm[64];                    // Written digest ignoring unadjusted main-cue tail (on-demand #321)
    char row[128];                    // sig.w, the view's hash of the export fields
    long long rules, active;          // sig.v; activeOnLoadLoops
    int perf, missing, degraded, bad; // bad: a field the hook needs did not parse
};

static const char *sc_rbs_sig_field(void *arg, const char *name, const char *v, const char *end) {
    struct sc_rbs_line *l = arg;
    if (!strcmp(name, "v")) return sc_rbs_integer(v, end, &l->rules);
    if (!strcmp(name, "w")) return sc_rbs_string(v, end, l->row, sizeof l->row, NULL);
    return NULL;
}

static const char *sc_rbs_line_field(void *arg, const char *name, const char *v, const char *end) {
    struct sc_rbs_line *l = arg;
    const char *next = NULL;
    if (!strcmp(name, "k")) {
        if ((next = sc_rbs_string(v, end, l->key, sizeof l->key, NULL))) l->put = 1;
    } else if (!strcmp(name, "drop")) {
        if ((next = sc_rbs_string(v, end, l->key, sizeof l->key, NULL))) l->drop = 1;
    } else if (!strcmp(name, "rel")) {
        next = sc_rbs_string(v, end, l->rel, sizeof l->rel, NULL);
    } else if (!strcmp(name, "wr")) {
        next = sc_rbs_string(v, end, l->wr, sizeof l->wr, NULL);
    } else if (!strcmp(name, "perf")) {
        next = sc_rbs_true(v, end, &l->perf);
    } else if (!strcmp(name, "missing")) {
        next = sc_rbs_true(v, end, &l->missing);
    } else if (!strcmp(name, "deg")) {
        size_t n = 0; // any reason at all is a degraded track (Go leaves an empty one out)
        if ((next = sc_rbs_string(v, end, NULL, 0, &n))) l->degraded = n > 0;
    } else if (!strcmp(name, "active")) {
        next = sc_rbs_integer(v, end, &l->active);
    } else if (!strcmp(name, "sig")) {
        next = sc_rbs_skip(v, end);
        if (next && !sc_rbs_object(v, next, sc_rbs_sig_field, l)) next = NULL;
    } else {
        return NULL;
    }
    if (!next) l->bad = 1;
    return next;
}

// Parses one index line (without its newline). 0 if it is not a JSON object.
static int sc_rbs_parse_line(const char *line, size_t n, struct sc_rbs_line *l) {
    memset(l, 0, sizeof *l);
    l->rules = -1;
    return sc_rbs_object(line, line + n, sc_rbs_line_field, l) && !l->bad && !(l->put && l->drop);
}

// --- the stage's live keys --------------------------------------------------------------------

// The index read for the table: only a line's first four members. Go writes a staged line's "k"
// first, then optional "id", then "sig" and "rel" (Staged's first fields; indexLine embeds Staged
// first), and a drop as {"drop":…}; test_rekordbox_stage.py holds that order to the Go source.
// 1: a staged line, 2: a drop, 0: anything else. rel is filled for a staged line ("" when absent).
static int sc_rbs_line_key(const char *line, size_t n, char *key, char *rel) {
    const char *p = sc_rbs_space(line, line + n), *end = line + n;
    char name[8];
    int kind = 0;
    rel[0] = 0;
    if (p >= end || *p++ != '{') return 0;
    for (int member = 0; member < 4; member++) {
        p = sc_rbs_space(p, end);
        if (!(p = sc_rbs_string(p, end, name, sizeof name, NULL))) return kind;
        p = sc_rbs_space(p, end);
        if (p >= end || *p++ != ':') return kind;
        p = sc_rbs_space(p, end);
        if (member == 0) {
            kind = !strcmp(name, "k") ? 1 : !strcmp(name, "drop") ? 2 : 0;
            if (!kind || !(p = sc_rbs_string(p, end, key, SC_RBS_TEXT, NULL))) return 0;
            if (kind == 2) return 2;
        } else if (!strcmp(name, "rel")) {
            if (!sc_rbs_string(p, end, rel, SC_RBS_TEXT, NULL)) rel[0] = 0;
            return 1;
        } else if (!(p = sc_rbs_skip(p, end))) {
            return 1;
        }
        p = sc_rbs_space(p, end);
        if (p >= end || *p++ != ',') return 1;
    }
    return 1;
}

static int sc_rbs_ascii(const char *s) {
    for (; *s; s++)
        if ((unsigned char)*s >= 0x80) return 0;
    return 1;
}

// What the index says now: for each key, the offset of its last staged line, unless a later line
// dropped it; for each non-ASCII "rel", the offset of the last staged line naming it. Kept per
// library and read again only when the file changes; the job only appends to it.
struct sc_rbs_slot {
    uint64_t hash; // of the key (or rel); 0: free
    uint32_t off;  // of the last staged line
    uint32_t live; // keys: 0 when dropped since
};

struct sc_rbs_table {
    struct sc_rbs_slot *slots;
    unsigned cap, used;
};

struct sc_rbs_stage {
    char uuid[SC_RBS_TEXT];
    dev_t dev;
    ino_t ino;
    off_t size, parsed;
    struct timespec mtime;
    struct sc_rbs_table keys, rels;
    unsigned live;
    unsigned long long touched;
};

#define SC_RBS_STAGES 4
#define SC_RBS_CONNS 8
static struct sc_rbs_stage sc_rbs_stages[SC_RBS_STAGES];
static unsigned long long sc_rbs_clock;
// Guards the tables and the connection cache only: never held while Engine's connection runs a
// statement (Engine has a pool of connections per drive; one of them may hold the write lock).
static pthread_mutex_t sc_rbs_mu = PTHREAD_MUTEX_INITIALIZER;

static struct sc_rbs_slot *sc_rbs_slot(struct sc_rbs_table *t, uint64_t hash) {
    if (!t->cap) return NULL;
    for (unsigned i = (unsigned)hash & (t->cap - 1);; i = (i + 1) & (t->cap - 1))
        if (!t->slots[i].hash || t->slots[i].hash == hash) return &t->slots[i];
}

// The slot of hash, made when it is not there. NULL: out of memory.
static struct sc_rbs_slot *sc_rbs_put(struct sc_rbs_table *t, uint64_t hash) {
    if ((t->used + 1) * 2 > t->cap) {
        unsigned cap = t->cap ? t->cap * 2 : 1024;
        struct sc_rbs_slot *old = t->slots, *slots = calloc(cap, sizeof *slots);
        if (!slots) return NULL;
        const unsigned oldcap = t->cap;
        t->slots = slots;
        t->cap = cap;
        for (unsigned i = 0; i < oldcap; i++)
            if (old[i].hash) *sc_rbs_slot(t, old[i].hash) = old[i];
        free(old);
    }
    struct sc_rbs_slot *slot = sc_rbs_slot(t, hash);
    if (!slot->hash) slot->hash = hash, t->used++;
    return slot;
}

static void sc_rbs_forget(struct sc_rbs_stage *s) {
    free(s->keys.slots);
    free(s->rels.slots);
    memset(&s->keys, 0, sizeof s->keys);
    memset(&s->rels, 0, sizeof s->rels);
    s->live = 0;
    s->parsed = s->size = 0;
}

static int sc_rbs_note(struct sc_rbs_stage *s, int kind, const char *key, const char *rel, off_t off) {
    if (off > (off_t)UINT32_MAX) return 0;
    const uint64_t hash = sc_rbs_hash(key, strlen(key));
    if (kind == 2) {
        struct sc_rbs_slot *slot = sc_rbs_slot(&s->keys, hash);
        if (slot && slot->hash && slot->live) slot->live = 0, s->live--;
        return 1;
    }
    struct sc_rbs_slot *slot = sc_rbs_put(&s->keys, hash);
    if (!slot) return 0;
    if (!slot->live) s->live++;
    slot->off = (uint32_t)off;
    slot->live = 1;
    if (rel[0] && !sc_rbs_ascii(rel)) {
        struct sc_rbs_slot *by_rel = sc_rbs_put(&s->rels, sc_rbs_hash(rel, strlen(rel)));
        if (!by_rel) return 0;
        by_rel->off = (uint32_t)off;
    }
    return 1;
}

static void sc_rbs_index_path(char *path, size_t cap, const char *uuid, const char *name) {
    snprintf(path, cap, "%s/%s/%s", sc_rbs_root, uuid, name);
}

// Reads what index.jsonl gained since the last call. 0 when it cannot be read.
static int sc_rbs_refresh(struct sc_rbs_stage *s) {
    char path[PATH_MAX];
    struct stat st;
    sc_rbs_index_path(path, sizeof path, s->uuid, SC_RBS_INDEX);
    if (stat(path, &st) || !S_ISREG(st.st_mode)) {
        sc_rbs_forget(s);
        return 0;
    }
    if (st.st_dev != s->dev || st.st_ino != s->ino || st.st_size < s->parsed) {
        sc_rbs_forget(s);
        s->dev = st.st_dev;
        s->ino = st.st_ino;
    }
    if (st.st_size == s->size && st.st_mtim.tv_sec == s->mtime.tv_sec && st.st_mtim.tv_nsec == s->mtime.tv_nsec) return 1;
    int fd = open(path, O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
    if (fd < 0) return 0;
    char *buf = malloc(SC_RBS_LINE_MAX), *key = malloc(2 * SC_RBS_TEXT), *rel = key ? key + SC_RBS_TEXT : NULL;
    int ok = buf && key;
    size_t have = 0;
    off_t at = s->parsed, base = s->parsed; // base: the file offset of buf[0]
    int skipping = 0;                       // inside a line longer than the buffer
    while (ok) {
        ssize_t got = pread(fd, buf + have, SC_RBS_LINE_MAX - have, base + (off_t)have);
        if (got < 0) { ok = 0; break; }
        if (got == 0) break;
        have += (size_t)got;
        size_t start = 0;
        for (char *nl; (nl = memchr(buf + start, '\n', have - start)); start = (size_t)(nl - buf) + 1) {
            const size_t len = (size_t)(nl - buf) - start;
            const int kind = skipping ? 0 : sc_rbs_line_key(buf + start, len, key, rel);
            if (kind && !sc_rbs_note(s, kind, key, rel, base + (off_t)start)) ok = 0;
            skipping = 0;
            at = base + (off_t)((size_t)(nl - buf) + 1);
        }
        if (start == 0 && have == SC_RBS_LINE_MAX) { // no newline in a whole buffer: drop that line
            skipping = 1;
            start = have;
        }
        memmove(buf, buf + start, have - start);
        base += (off_t)start;
        have -= start;
    }
    close(fd);
    free(buf);
    free(key);
    if (!ok) {
        sc_rbs_forget(s);
        return 0;
    }
    s->parsed = at; // a cut last line is read again once it has its newline
    s->size = st.st_size;
    s->mtime = st.st_mtim;
    return 1;
}

// The stage of uuid, read up to date; NULL when it has nothing a window has not applied.
// Called with sc_rbs_mu held.
static struct sc_rbs_stage *sc_rbs_stage_of(const char *uuid) {
    struct sc_rbs_stage *s = NULL, *oldest = &sc_rbs_stages[0];
    for (unsigned i = 0; i < SC_RBS_STAGES; i++) {
        if (!strcmp(sc_rbs_stages[i].uuid, uuid)) s = &sc_rbs_stages[i];
        if (sc_rbs_stages[i].touched < oldest->touched) oldest = &sc_rbs_stages[i];
    }
    if (!s) {
        s = oldest;
        sc_rbs_forget(s);
        memset(s, 0, sizeof *s);
        snprintf(s->uuid, sizeof s->uuid, "%s", uuid);
    }
    s->touched = ++sc_rbs_clock;
    return sc_rbs_refresh(s) && s->live ? s : NULL;
}

// The index line at off, parsed whole. 0 when it is not a staged line.
static int sc_rbs_line_at(const char *uuid, off_t off, struct sc_rbs_line *l) {
    char path[PATH_MAX];
    sc_rbs_index_path(path, sizeof path, uuid, SC_RBS_INDEX);
    int fd = open(path, O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
    if (fd < 0) return 0;
    char *buf = malloc(SC_RBS_LINE_MAX);
    ssize_t got = buf ? pread(fd, buf, SC_RBS_LINE_MAX, off) : -1;
    close(fd);
    char *nl = got > 0 ? memchr(buf, '\n', (size_t)got) : NULL;
    int ok = nl && sc_rbs_parse_line(buf, (size_t)(nl - buf), l) && l->put;
    free(buf);
    return ok;
}

// Where the live staged line of key (or, by_rel, of the non-ASCII volume spelling key) is: 1 and
// *off, or 0. Called with sc_rbs_mu held; the line itself is read without it (sc_rbs_line_at).
static int sc_rbs_find(struct sc_rbs_stage *s, const char *key, int by_rel, off_t *off) {
    struct sc_rbs_slot *slot = sc_rbs_slot(by_rel ? &s->rels : &s->keys, sc_rbs_hash(key, strlen(key)));
    if (!slot || !slot->hash || (!by_rel && !slot->live)) return 0;
    *off = slot->off;
    return 1;
}

// --- the library behind Engine's connection ---------------------------------------------------

struct sc_rbs_conn {
    void *db;
    char main[PATH_MAX];
    int known;      // 1: uuid is the snapshot's; -1: the stick had no bridge snapshot at `checked`
    time_t checked; // a -1 is looked at again after sc_rbs_recheck seconds
    char uuid[SC_RBS_TEXT];
    unsigned long long touched;
};
static long sc_rbs_recheck = 30; // a variable for the tests
static struct sc_rbs_conn sc_rbs_conns[SC_RBS_CONNS];

// A directory name for the stage, as rbstate.OpenStage takes it, and safe inside SQL quotes.
static int sc_rbs_uuid_ok(const char *uuid) {
    if (!uuid[0] || uuid[0] == '.') return 0;
    for (const char *p = uuid; *p; p++)
        if (!(*p >= '0' && *p <= '9') && !(*p >= 'a' && *p <= 'z') && !(*p >= 'A' && *p <= 'Z') && *p != '-' && *p != '_' && *p != '.') return 0;
    return 1;
}

// The library uuid of the bridge's snapshot beside main: its first line is the magic and the
// header's JSON, whose members are read in order up to "uuid" (Go writes it third). 0: none.
static int sc_rbs_snapshot_uuid(const char *main, char uuid[SC_RBS_TEXT]) {
    char path[PATH_MAX], head[4096], name[16];
    const char *slash = strrchr(main, '/');
    uuid[0] = 0;
    if (!slash || snprintf(path, sizeof path, "%.*s/%s", (int)(slash - main), main, SC_RBS_SNAPSHOT) >= (int)sizeof path) return 0;
    int fd = open(path, O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
    if (fd < 0) return 0;
    ssize_t n = read(fd, head, sizeof head);
    close(fd);
    const size_t magic = sizeof SC_RBS_SNAPSHOT_MAGIC - 1;
    if (n <= (ssize_t)magic || memcmp(head, SC_RBS_SNAPSHOT_MAGIC, magic)) return 0;
    const char *end = head + n, *nl = memchr(head, '\n', (size_t)n), *p = sc_rbs_space(head + magic, nl ? nl : end);
    if (nl) end = nl;
    if (p >= end || *p++ != '{') return 0;
    for (int member = 0; member < 8; member++) {
        p = sc_rbs_space(p, end);
        const char *value = sc_rbs_string(p, end, name, sizeof name, NULL);
        if (!value) value = sc_rbs_string(p, end, NULL, 0, NULL), name[0] = 0;
        if (!value) return 0;
        p = sc_rbs_space(value, end);
        if (p >= end || *p++ != ':') return 0;
        p = sc_rbs_space(p, end);
        if (!strcmp(name, "uuid")) return sc_rbs_string(p, end, uuid, SC_RBS_TEXT, NULL) && sc_rbs_uuid_ok(uuid);
        if (!(p = sc_rbs_skip(p, end))) return 0;
        p = sc_rbs_space(p, end);
        if (p >= end || *p++ != ',') return 0;
    }
    return 0;
}

// main when db is a drive's library, else NULL.
static const char *sc_rbs_drive(const struct sc_rbx_sqlite *q, void *db) {
    const char *main = q->filename(db, "main");
    return main && !strncmp(main, sc_rbx_drives, strlen(sc_rbx_drives)) ? main : NULL;
}

// The cached library of db. Called with sc_rbs_mu held.
static struct sc_rbs_conn *sc_rbs_conn_of(void *db, const char *main) {
    struct sc_rbs_conn *c = NULL, *oldest = &sc_rbs_conns[0];
    for (unsigned i = 0; i < SC_RBS_CONNS; i++) {
        if (sc_rbs_conns[i].db == db && !strcmp(sc_rbs_conns[i].main, main)) c = &sc_rbs_conns[i];
        if (sc_rbs_conns[i].touched < oldest->touched) oldest = &sc_rbs_conns[i];
    }
    if (!c) {
        c = oldest;
        memset(c, 0, sizeof *c);
        c->db = db;
        snprintf(c->main, sizeof c->main, "%s", main);
    }
    c->touched = ++sc_rbs_clock;
    const time_t now = time(NULL);
    if (!c->known || (c->known < 0 && now - c->checked >= sc_rbs_recheck)) {
        c->known = sc_rbs_snapshot_uuid(main, c->uuid) ? 1 : -1;
        c->checked = now;
    }
    return c;
}

// The snapshot c named is not the library's: read it again at the next load.
static void sc_rbs_conn_stale(void *db, const char *main) {
    pthread_mutex_lock(&sc_rbs_mu);
    for (unsigned i = 0; i < SC_RBS_CONNS; i++)
        if (sc_rbs_conns[i].db == db && !strcmp(sc_rbs_conns[i].main, main)) sc_rbs_conns[i].known = 0;
    pthread_mutex_unlock(&sc_rbs_mu);
}

// --- the row ------------------------------------------------------------------------------------

#define SC_RBS_TRACK_READ "SELECT t.id, t.playOrder, t.length, t.bpm, t.year, t.path, "

// Engine's deck-load and analysis-check Track read by path:
//   SELECT t.id, t.playOrder, ... FROM Track AS t ... WHERE t.path = ? [OR t.path = ?]
// Returns the number of bound path parameters (1 or 2), or 0 for any other statement.
static int sc_rbs_path_bound(const char *sql) {
    if (!sql || strncmp(sql, SC_RBS_TRACK_READ, sizeof SC_RBS_TRACK_READ - 1)) return 0;
    const char *where = strstr(sql, " WHERE t.path = ?");
    if (!where || strchr(sql, '?') != where + sizeof " WHERE t.path = " - 1) return 0;
    int count = 1;
    sql = where + sizeof " WHERE t.path = ?" - 1;
    if (!strncmp(sql, " OR t.path = ?", sizeof " OR t.path = ?" - 1)) {
        count = 2;
        sql += sizeof " OR t.path = ?" - 1;
    }
    for (; *sql == ' ' || *sql == ';'; sql++) {}
    return *sql ? 0 : count;
}

static void sc_rbs_escape_sql(const char *src, char *dst, size_t cap) {
    size_t n = 0;
    for (; *src && n + 1 + (*src == '\'') < cap; src++) {
        if (*src == '\'') dst[n++] = '\'';
        dst[n++] = *src;
    }
    dst[n] = 0;
}

struct sc_rbs_row {
    int rows;
    long long id;
    char path[SC_RBS_TEXT], uuid[SC_RBS_TEXT];
    long long import_key;
    int empty, insert_cues; // no analysis yet; cues and loops as the insert left them
    int key;                // Track.key (0..23) or -1
    double bpm;             // COALESCE(bpmAnalyzed, bpm, 0)
    long long length;       // Track.length (seconds)
};

static int sc_rbs_row_cb(void *arg, int n, char **v, char **name) {
    struct sc_rbs_row *r = arg;
    (void)name;
    if (n != 9) return 0;
    r->rows++;
    snprintf(r->path, sizeof r->path, "%s", v[0] ? v[0] : "");
    r->import_key = v[1] ? strtoll(v[1], NULL, 10) : 0;
    snprintf(r->uuid, sizeof r->uuid, "%s", v[2] ? v[2] : "");
    r->empty = v[3] && !strcmp(v[3], "1");
    r->insert_cues = v[4] && !strcmp(v[4], "1");
    r->id = v[5] ? strtoll(v[5], NULL, 10) : 0;
    r->key = v[6] ? (int)strtol(v[6], NULL, 10) : -1;
    r->bpm = v[7] ? strtod(v[7], NULL) : 0.0;
    r->length = v[8] ? strtoll(v[8], NULL, 10) : 0;
    return 0;
}

// The insert leaves a row without analysis, with either the export's cue rows and the import mark
// (thirdPartySourceId 1), or no cues, loops or mark at all. Engine clears the mark when it first
// loads the row, and the DJ edits a row only after a load, so a row in any other state is the deck's.
#define SC_RBS_UNTOUCHED(t) t "trackData IS NULL AND " t "beatData IS NULL AND " t "overviewWaveFormData IS NULL"
#define SC_RBS_INSERT_CUES(t) "(" t "thirdPartySourceId = 1 OR (" t "thirdPartySourceId IS NULL AND " t "quickCues IS NULL AND " t "loops IS NULL))"

static int sc_rbs_changes(void *arg, int n, char **v, char **names) {
    (void)names;
    if (n == 1 && v[0]) *(int *)arg = !strcmp(v[0], "1");
    return 0;
}

// The fill: the staged blobs of b (an RBXA1 file: magic, five little-endian uint32 lengths each
// followed by its bytes, one Complete byte) into the row of track, activeOnLoadLoops active, the
// import mark cleared, and the Track analysed; only while the row is as the insert left it and the
// library is uuid's. It ends with "SELECT changes()": 1 when the row was filled. NULL when b is
// not a whole, complete file with trackData and beatData (why says what is wrong).
static char *sc_rbs_fill_sql(const unsigned char *b, size_t n, long long track, long long active, const char *uuid, const char **why) {
    static const char *const columns[] = {"trackData", "overviewWaveFormData", "beatData", "quickCues", "loops"};
    size_t at = 5, len[5], start[5], total = 1088 + strlen(uuid);
    *why = "not an RBXA1 file";
    if (n < 5 || memcmp(b, "RBXA1", 5)) return NULL;
    *why = "cut short";
    for (int i = 0; i < 5; i++) {
        if (n - at < 4) return NULL;
        len[i] = (size_t)b[at] | (size_t)b[at + 1] << 8 | (size_t)b[at + 2] << 16 | (size_t)b[at + 3] << 24;
        start[i] = at + 4;
        if (len[i] > n - start[i]) return NULL;
        at = start[i] + len[i];
        total += strlen(columns[i]) + 9 + 2 * len[i];
    }
    if (n - at != 1 || b[at] > 1) return NULL;
    *why = "not complete";
    if (!b[at]) return NULL;
    *why = "no grid or track data";
    if (!len[0] || !len[2]) return NULL;
    char *sql = malloc(total), *p = sql;
    if (!sql) {
        *why = "out of memory";
        return NULL;
    }
    p += sprintf(p, "SAVEPOINT sc_rbs; UPDATE PerformanceData SET ");
    for (int i = 0; i < 5; i++) {
        p += sprintf(p, "%s = ", columns[i]);
        if (!len[i]) {
            p += sprintf(p, "NULL, ");
            continue;
        }
        *p++ = 'X', *p++ = '\'';
        for (size_t k = 0; k < len[i]; k++) p += sprintf(p, "%02x", b[start[i] + k]);
        p += sprintf(p, "', ");
    }
    sprintf(p, "activeOnLoadLoops = %lld, thirdPartySourceId = NULL WHERE trackId = %lld AND %s AND %s AND (SELECT uuid FROM Information) = '%s'; "
               "UPDATE Track SET isAnalyzed = 1 WHERE id = %lld AND changes() > 0; SELECT changes(); RELEASE sc_rbs",
            active, track, SC_RBS_UNTOUCHED(""), SC_RBS_INSERT_CUES(""), uuid, track);
    *why = NULL;
    return sql;
}

static void sc_rbs_json_text(FILE *f, const char *s) {
    fputc('"', f);
    for (; *s; s++) {
        unsigned char c = (unsigned char)*s;
        if (c == '"' || c == '\\') fprintf(f, "\\%c", c);
        else if (c < 0x20) fprintf(f, "\\u%04x", c);
        else fputc(c, f);
    }
    fputc('"', f);
}

static void sc_rbs_mkdir_p(const char *dir) {
    char buf[PATH_MAX];
    size_t n = strlen(dir);
    if (!n || n >= sizeof buf) return;
    memcpy(buf, dir, n + 1);
    for (char *p = buf + 1; *p; p++) {
        if (*p == '/') {
            *p = 0;
            (void)mkdir(buf, 0700);
            *p = '/';
        }
    }
    (void)mkdir(buf, 0700);
}

// The record beside the index: which row the hook filled with which staged or on-demand line.
// Creates <sc_rbs_root>/<uuid> when absent so on-demand fills still journal for #321 write-back.
static void sc_rbs_journal(const char *uuid, long long track, const struct sc_rbs_line *l) {
    char path[PATH_MAX], dir[PATH_MAX];
    if (snprintf(dir, sizeof dir, "%s/%s", sc_rbs_root, uuid) >= (int)sizeof dir) return;
    sc_rbs_mkdir_p(dir);
    sc_rbs_index_path(path, sizeof path, uuid, SC_RBS_JOURNAL);
    int fd = open(path, O_WRONLY | O_APPEND | O_CREAT | O_CLOEXEC | O_NOFOLLOW, 0600);
    if (fd < 0) return;
    char *line = NULL;
    size_t size = 0;
    FILE *f = open_memstream(&line, &size);
    if (f) {
        fprintf(f, "{\"id\":%lld,\"k\":", track);
        sc_rbs_json_text(f, l->key);
        fputs(",\"wr\":", f);
        sc_rbs_json_text(f, l->wr);
        if (l->wrnm[0]) {
            fputs(",\"wrnm\":", f);
            sc_rbs_json_text(f, l->wrnm);
        }
        fprintf(f, ",\"v\":%lld,\"w\":", l->rules);
        sc_rbs_json_text(f, l->row);
        fprintf(f, ",\"at\":%lld}\n", (long long)time(NULL));
        if (!fclose(f)) (void)!write(fd, line, size); // one write: a whole line, appended
    }
    free(line);
    close(fd);
}

static long long sc_rbs_mono_ns(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (long long)t.tv_sec * 1000000000LL + t.tv_nsec;
}

// --- on-demand direct-read fetch (CDJ load path, zero staging required) ------------------------

struct sc_rbs_neg_entry {
    uint64_t hash;
    long long expires_ns;
    unsigned long long touched;
};

struct sc_rbs_inflight_entry {
    uint64_t hash;
    uint64_t gen;
    int active;
    int filled;
    long long track;
};

static struct sc_rbs_neg_entry sc_rbs_neg[SC_RBS_NEG_SLOTS];
static struct sc_rbs_inflight_entry sc_rbs_inflight[SC_RBS_INFLIGHT_SLOTS];
static uint64_t sc_rbs_inflight_gen;
static pthread_cond_t sc_rbs_inflight_cond;
static int sc_rbs_inflight_cond_ready;

static void sc_rbs_ondemand_reset(void) {
    pthread_mutex_lock(&sc_rbs_mu);
    memset(sc_rbs_neg, 0, sizeof sc_rbs_neg);
    memset(sc_rbs_inflight, 0, sizeof sc_rbs_inflight);
    pthread_mutex_unlock(&sc_rbs_mu);
}

static void sc_rbs_ondemand_load_env_locked(void) {
    if (sc_rbs_ondemand_env_init) return;
    sc_rbs_ondemand_env_init = 1;
    const char *env_sock = getenv("ENGINEX_RB_ONDEMAND_SOCK");
    if (env_sock && env_sock[0]) {
        sc_rbs_ondemand_sock = (!strcmp(env_sock, "-") || !strcmp(env_sock, "off")) ? "" : env_sock;
    }
    const char *env_url = getenv("ENGINEX_RB_ONDEMAND_URL");
    if (env_url && env_url[0]) sc_rbs_ondemand_url = env_url;
    const char *env_ms = getenv("ENGINEX_RB_ONDEMAND_TIMEOUT_MS");
    if (env_ms && env_ms[0]) {
        int v = atoi(env_ms);
        if (v > 0 && v <= 15000) sc_rbs_ondemand_timeout_ms = v;
    }
}

static int sc_rbs_ondemand_available(void) {
    pthread_mutex_lock(&sc_rbs_mu);
    sc_rbs_ondemand_load_env_locked();
    const char *url = sc_rbs_ondemand_url;
    const char *sock = sc_rbs_ondemand_sock;
    pthread_mutex_unlock(&sc_rbs_mu);
    if (url && url[0]) return 1;
    return sock && sock[0] && !access(sock, F_OK);
}

// Returns 0 if safe for blocking on-demand I/O, -1 on RT audio threads, -2 on Qt GUI threads.
static int sc_rbs_thread_safe_for_ondemand(void) {
    int pol = sc_rbs_sched_override ? sc_rbs_sched_override() : sched_getscheduler(0);
    if (pol == SCHED_FIFO || pol == SCHED_RR) return -1;
#ifdef __linux__
    char comm[16];
    memset(comm, 0, sizeof comm);
    if (!prctl(PR_GET_NAME, (unsigned long)comm, 0, 0, 0)) {
        if (!strcmp(comm, "SceneGraph") || !strcmp(comm, "EMain") || !strcmp(comm, "EPlanckApp")) return -2;
    }
#endif
    return 0;
}

static uint64_t sc_rbs_ondemand_key_hash(const char *uuid, const char *path, long long track) {
    char buf[SC_RBS_TEXT * 2 + 32];
    int n = path ? snprintf(buf, sizeof buf, "%s|P|%s", uuid, path)
                 : snprintf(buf, sizeof buf, "%s|I|%lld", uuid, track);
    if (n <= 0) return 1;
    if ((size_t)n >= sizeof buf) n = (int)(sizeof buf - 1);
    return sc_rbs_hash(buf, (size_t)n);
}

static int sc_rbs_neg_hit_locked(uint64_t hash, long long now_ns) {
    for (unsigned i = 0; i < SC_RBS_NEG_SLOTS; i++) {
        if (sc_rbs_neg[i].hash == hash) {
            if (now_ns < sc_rbs_neg[i].expires_ns) {
                sc_rbs_neg[i].touched = ++sc_rbs_clock;
                return 1;
            }
            sc_rbs_neg[i].hash = 0;
            return 0;
        }
    }
    return 0;
}

static void sc_rbs_neg_put_locked(uint64_t hash, long long ttl_ns) {
    const long long exp = sc_rbs_mono_ns() + ttl_ns;
    struct sc_rbs_neg_entry *victim = &sc_rbs_neg[0];
    for (unsigned i = 0; i < SC_RBS_NEG_SLOTS; i++) {
        if (sc_rbs_neg[i].hash == hash) {
            sc_rbs_neg[i].expires_ns = exp;
            sc_rbs_neg[i].touched = ++sc_rbs_clock;
            return;
        }
        if (!sc_rbs_neg[i].hash) {
            victim = &sc_rbs_neg[i];
            break;
        }
        if (sc_rbs_neg[i].touched < victim->touched) victim = &sc_rbs_neg[i];
    }
    victim->hash = hash;
    victim->expires_ns = exp;
    victim->touched = ++sc_rbs_clock;
}

static void sc_rbs_inflight_init_locked(void) {
    if (sc_rbs_inflight_cond_ready) return;
    pthread_condattr_t attr;
    pthread_condattr_init(&attr);
    pthread_condattr_setclock(&attr, CLOCK_MONOTONIC);
    pthread_cond_init(&sc_rbs_inflight_cond, &attr);
    pthread_condattr_destroy(&attr);
    sc_rbs_inflight_cond_ready = 1;
}

// Acquires an in-flight slot for hash. If a peer thread is already filling the same track, waits
// bounded by deadline_ns until the peer finishes and commits.
// Returns: >=0 claimed slot index; -1 peer already filled (*peer_track set); -2 timed out / full.
static int sc_rbs_inflight_enter(uint64_t hash, long long deadline_ns, long long *peer_track) {
    pthread_mutex_lock(&sc_rbs_mu);
    sc_rbs_inflight_init_locked();
    for (;;) {
        int peer = -1, free_slot = -1;
        for (int i = 0; i < SC_RBS_INFLIGHT_SLOTS; i++) {
            if (sc_rbs_inflight[i].active && sc_rbs_inflight[i].hash == hash) {
                peer = i;
                break;
            }
            if (!sc_rbs_inflight[i].active && free_slot < 0) free_slot = i;
        }
        if (peer < 0) {
            if (free_slot >= 0) {
                sc_rbs_inflight[free_slot].hash = hash;
                sc_rbs_inflight[free_slot].gen = ++sc_rbs_inflight_gen;
                sc_rbs_inflight[free_slot].active = 1;
                sc_rbs_inflight[free_slot].filled = 0;
                sc_rbs_inflight[free_slot].track = 0;
            }
            pthread_mutex_unlock(&sc_rbs_mu);
            return free_slot;
        }
        uint64_t gen = sc_rbs_inflight[peer].gen;
        while (sc_rbs_inflight[peer].active && sc_rbs_inflight[peer].gen == gen) {
            long long rem = deadline_ns - sc_rbs_mono_ns();
            if (rem <= 0) {
                pthread_mutex_unlock(&sc_rbs_mu);
                return -2;
            }
            struct timespec ts;
            ts.tv_sec = (time_t)(deadline_ns / 1000000000LL);
            ts.tv_nsec = (long)(deadline_ns % 1000000000LL);
            int wrc = pthread_cond_timedwait(&sc_rbs_inflight_cond, &sc_rbs_mu, &ts);
            if (wrc == ETIMEDOUT) {
                pthread_mutex_unlock(&sc_rbs_mu);
                return -2;
            }
        }
        if (sc_rbs_inflight[peer].gen == gen && sc_rbs_inflight[peer].filled) {
            if (peer_track) *peer_track = sc_rbs_inflight[peer].track;
            pthread_mutex_unlock(&sc_rbs_mu);
            return -1;
        }
        // Peer finished without filling (e.g. negative cached or transient error): check neg cache.
        if (sc_rbs_neg_hit_locked(hash, sc_rbs_mono_ns())) {
            pthread_mutex_unlock(&sc_rbs_mu);
            return -2;
        }
    }
}

static void sc_rbs_inflight_leave(int slot, int filled, long long track) {
    if (slot < 0 || slot >= SC_RBS_INFLIGHT_SLOTS) return;
    pthread_mutex_lock(&sc_rbs_mu);
    sc_rbs_inflight[slot].filled = filled;
    sc_rbs_inflight[slot].track = track;
    sc_rbs_inflight[slot].active = 0;
    pthread_cond_broadcast(&sc_rbs_inflight_cond);
    pthread_mutex_unlock(&sc_rbs_mu);
}

static int sc_rbs_poll_fd(int fd, short events, long long deadline_ns) {
    for (;;) {
        long long rem_ns = deadline_ns - sc_rbs_mono_ns();
        if (rem_ns <= 0) return 0;
        int ms = (int)((rem_ns + 999999LL) / 1000000LL);
        if (ms < 1) ms = 1;
        struct pollfd pfd;
        pfd.fd = fd;
        pfd.events = events;
        pfd.revents = 0;
        int rc = poll(&pfd, 1, ms);
        if (rc < 0 && errno == EINTR) continue;
        return rc;
    }
}

static int sc_rbs_ascii_case_eq(const char *a, const char *b, size_t n) {
    for (size_t i = 0; i < n; i++) {
        unsigned char ca = (unsigned char)a[i], cb = (unsigned char)b[i];
        if (ca >= 'A' && ca <= 'Z') ca = (unsigned char)(ca + ('a' - 'A'));
        if (cb >= 'A' && cb <= 'Z') cb = (unsigned char)(cb + ('a' - 'A'));
        if (ca != cb) return 0;
    }
    return 1;
}

static int sc_rbs_hdr_match(const char *line, size_t len, const char *name, char *out, size_t cap) {
    const size_t nlen = strlen(name);
    if (len <= nlen || line[nlen] != ':' || !sc_rbs_ascii_case_eq(line, name, nlen)) return 0;
    const char *p = line + nlen + 1, *end = line + len;
    while (p < end && (*p == ' ' || *p == '\t')) p++;
    while (end > p && (end[-1] == ' ' || end[-1] == '\t' || end[-1] == '\r')) end--;
    size_t vlen = (size_t)(end - p);
    if (vlen >= cap) return 0;
    memcpy(out, p, vlen);
    out[vlen] = 0;
    return 1;
}

// Parses "http://127.0.0.1:<port><route>" into port and route. Returns 1 on valid loopback URL.
static int sc_rbs_parse_loopback_url(const char *url, int *port, const char **route) {
    const char *pfx = "http://127.0.0.1:";
    const size_t plen = strlen(pfx);
    if (!url || strncmp(url, pfx, plen)) return 0;
    const char *p = url + plen;
    long v = 0;
    while (*p >= '0' && *p <= '9') {
        v = v * 10 + (*p++ - '0');
        if (v > 65535) return 0;
    }
    if (v <= 0 || (*p && *p != '/')) return 0;
    *port = (int)v;
    *route = *p ? p : SC_RBS_ONDEMAND_ROUTE;
    return 1;
}

// Fetches one track's RBXA1 payload and metadata over Unix domain socket or loopback HTTP/1.1
// with a strict CLOCK_MONOTONIC deadline. Returns 0 on HTTP 200 valid RBXA1, -1 otherwise
// (*http_status holds the HTTP code when a response was parsed, 0 on transport/timeout error).
static int sc_rbs_ondemand_fetch(const char *main_db, const char *uuid, const struct sc_rbs_row *row,
                                 long long deadline_ns, struct sc_rbs_line *out_line,
                                 unsigned char **out_body, size_t *out_len, int *http_status) {
    *out_body = NULL;
    *out_len = 0;
    *http_status = 0;
    memset(out_line, 0, sizeof *out_line);
    out_line->rules = -1;

    pthread_mutex_lock(&sc_rbs_mu);
    sc_rbs_ondemand_load_env_locked();
    const char *url = sc_rbs_ondemand_url;
    const char *sock_path = sc_rbs_ondemand_sock;
    pthread_mutex_unlock(&sc_rbs_mu);

    int fd = -1;
    const char *route = SC_RBS_ONDEMAND_ROUTE;
    if (url && url[0]) {
        int port = 0;
        if (!sc_rbs_parse_loopback_url(url, &port, &route)) return -1;
        fd = socket(AF_INET, SOCK_STREAM | SOCK_NONBLOCK | SOCK_CLOEXEC, 0);
        if (fd < 0) return -1;
        struct sockaddr_in sa;
        memset(&sa, 0, sizeof sa);
        sa.sin_family = AF_INET;
        sa.sin_port = htons((uint16_t)port);
        if (inet_pton(AF_INET, "127.0.0.1", &sa.sin_addr) != 1) {
            close(fd);
            return -1;
        }
        int crc = connect(fd, (struct sockaddr *)&sa, sizeof sa);
        if (crc < 0 && errno != EINPROGRESS) {
            close(fd);
            return -1;
        }
        if (crc < 0) {
            if (sc_rbs_poll_fd(fd, POLLOUT, deadline_ns) <= 0) {
                close(fd);
                return -1;
            }
            int err = 0;
            socklen_t elen = sizeof err;
            if (getsockopt(fd, SOL_SOCKET, SO_ERROR, &err, &elen) < 0 || err != 0) {
                close(fd);
                return -1;
            }
        }
    } else if (sock_path && sock_path[0]) {
        struct sockaddr_un su;
        memset(&su, 0, sizeof su);
        if (strlen(sock_path) >= sizeof su.sun_path) return -1;
        su.sun_family = AF_UNIX;
        memcpy(su.sun_path, sock_path, strlen(sock_path) + 1);
        fd = socket(AF_UNIX, SOCK_STREAM | SOCK_NONBLOCK | SOCK_CLOEXEC, 0);
        if (fd < 0) return -1;
        int crc = connect(fd, (struct sockaddr *)&su, sizeof su);
        if (crc < 0 && errno != EINPROGRESS) {
            close(fd);
            return -1;
        }
        if (crc < 0) {
            if (sc_rbs_poll_fd(fd, POLLOUT, deadline_ns) <= 0) {
                close(fd);
                return -1;
            }
            int err = 0;
            socklen_t elen = sizeof err;
            if (getsockopt(fd, SOL_SOCKET, SO_ERROR, &err, &elen) < 0 || err != 0) {
                close(fd);
                return -1;
            }
        }
    } else {
        return -1;
    }

    char *jbody = NULL;
    size_t jlen = 0;
    FILE *jf = open_memstream(&jbody, &jlen);
    if (!jf) {
        close(fd);
        return -1;
    }
    fputs("{\"db\":", jf);
    sc_rbs_json_text(jf, main_db);
    fputs(",\"uuid\":", jf);
    sc_rbs_json_text(jf, uuid);
    fputs(",\"path\":", jf);
    sc_rbs_json_text(jf, row->path);
    fprintf(jf, ",\"id\":%lld", row->id);
    if (row->key >= 0 && row->key < 24) fprintf(jf, ",\"key\":%d", row->key);
    if (row->bpm > 0.0) fprintf(jf, ",\"bpm\":%.4f", row->bpm);
    if (row->length > 0) fprintf(jf, ",\"length\":%lld", row->length);
    fputc('}', jf);
    if (fclose(jf)) {
        free(jbody);
        close(fd);
        return -1;
    }

    char *req = NULL;
    size_t req_len = 0;
    FILE *rf = open_memstream(&req, &req_len);
    if (!rf) {
        free(jbody);
        close(fd);
        return -1;
    }
    fprintf(rf,
            "POST %s HTTP/1.1\r\n"
            "Host: 127.0.0.1\r\n"
            "Content-Type: application/json\r\n"
            "Content-Length: %zu\r\n"
            "Connection: close\r\n\r\n",
            route, jlen);
    (void)fwrite(jbody, 1, jlen, rf);
    free(jbody);
    if (fclose(rf)) {
        free(req);
        close(fd);
        return -1;
    }

    size_t sent = 0;
    while (sent < req_len) {
        ssize_t w = send(fd, req + sent, req_len - sent, MSG_NOSIGNAL);
        if (w > 0) {
            sent += (size_t)w;
            continue;
        }
        if (w < 0 && errno == EINTR) continue;
        if (w < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) {
            if (sc_rbs_poll_fd(fd, POLLOUT, deadline_ns) <= 0) break;
            continue;
        }
        break;
    }
    free(req);
    if (sent != req_len) {
        close(fd);
        return -1;
    }

    size_t cap = 32768, have = 0, hdr_end = 0;
    long long content_len = -1;
    unsigned char *buf = malloc(cap);
    if (!buf) {
        close(fd);
        return -1;
    }
    const size_t max_total = 16384 + (size_t)SC_RBS_ONDEMAND_MAX_BODY;
    for (;;) {
        if (have == cap) {
            if (cap >= max_total) {
                free(buf);
                close(fd);
                return -1;
            }
            size_t next = cap * 2;
            if (next > max_total) next = max_total;
            unsigned char *nb = realloc(buf, next);
            if (!nb) {
                free(buf);
                close(fd);
                return -1;
            }
            buf = nb;
            cap = next;
        }
        ssize_t n = recv(fd, buf + have, cap - have, 0);
        if (n > 0) {
            have += (size_t)n;
            if (!hdr_end && have >= 4) {
                for (size_t i = 0; i + 4 <= have; i++) {
                    if (buf[i] == '\r' && buf[i + 1] == '\n' && buf[i + 2] == '\r' && buf[i + 3] == '\n') {
                        hdr_end = i + 4;
                        // Peek Content-Length so we don't wait for EOF unnecessarily.
                        const char *p = (const char *)buf, *end = (const char *)buf + i;
                        while (p < end) {
                            const char *nl = memchr(p, '\n', (size_t)(end - p));
                            size_t ll = nl ? (size_t)(nl - p) : (size_t)(end - p);
                            char cl[32];
                            if (sc_rbs_hdr_match(p, ll, "Content-Length", cl, sizeof cl)) {
                                content_len = strtoll(cl, NULL, 10);
                                break;
                            }
                            if (!nl) break;
                            p = nl + 1;
                        }
                        break;
                    }
                }
            }
            if (hdr_end && content_len >= 0 && have >= hdr_end + (size_t)content_len) break;
            continue;
        }
        if (n == 0) break;
        if (errno == EINTR) continue;
        if (errno == EAGAIN || errno == EWOULDBLOCK) {
            if (sc_rbs_poll_fd(fd, POLLIN, deadline_ns) <= 0) {
                free(buf);
                close(fd);
                return -1;
            }
            continue;
        }
        free(buf);
        close(fd);
        return -1;
    }
    close(fd);

    if (!hdr_end || hdr_end < 12) {
        free(buf);
        return -1;
    }
    // Parse status line: "HTTP/1.x NNN ..."
    const char *sp = memchr(buf, ' ', hdr_end);
    if (!sp || (size_t)(sp - (const char *)buf) + 4 > hdr_end) {
        free(buf);
        return -1;
    }
    int status = atoi(sp + 1);
    *http_status = status;
    if (status != 200) {
        free(buf);
        return -1;
    }

    const char *p = (const char *)buf, *end = (const char *)buf + hdr_end - 4;
    const char *first_nl = memchr(p, '\n', (size_t)(end - p));
    if (!first_nl) {
        free(buf);
        return -1;
    }
    p = first_nl + 1;
    char tmp[64];
    while (p < end) {
        const char *nl = memchr(p, '\n', (size_t)(end - p));
        size_t ll = nl ? (size_t)(nl - p) : (size_t)(end - p);
        if (sc_rbs_hdr_match(p, ll, "X-RB-Key", out_line->key, sizeof out_line->key)) {
        } else if (sc_rbs_hdr_match(p, ll, "X-RB-Written", out_line->wr, sizeof out_line->wr)) {
        } else if (sc_rbs_hdr_match(p, ll, "X-RB-Written-NoMain", out_line->wrnm, sizeof out_line->wrnm)) {
        } else if (sc_rbs_hdr_match(p, ll, "X-RB-Row", out_line->row, sizeof out_line->row)) {
        } else if (sc_rbs_hdr_match(p, ll, "X-RB-Active-Loops", tmp, sizeof tmp)) {
            out_line->active = strtoll(tmp, NULL, 10);
        } else if (sc_rbs_hdr_match(p, ll, "X-RB-Rules", tmp, sizeof tmp)) {
            out_line->rules = strtoll(tmp, NULL, 10);
        }
        if (!nl) break;
        p = nl + 1;
    }

    size_t body_len = have - hdr_end;
    if (content_len >= 0 && body_len > (size_t)content_len) body_len = (size_t)content_len;
    if (content_len >= 0 && body_len != (size_t)content_len) {
        free(buf);
        return -1;
    }
    if (out_line->rules != SC_RBS_RULES || !out_line->key[0] || !out_line->wr[0] ||
        body_len < 5 || memcmp(buf + hdr_end, "RBXA1", 5)) {
        free(buf);
        return -1;
    }
    unsigned char *body = malloc(body_len);
    if (!body) {
        free(buf);
        return -1;
    }
    memcpy(body, buf + hdr_end, body_len);
    free(buf);
    *out_body = body;
    *out_len = body_len;
    return 0;
}

static int sc_rbs_exec_read(const struct sc_rbx_sqlite *q, void *db, const char *sql, struct sc_rbs_row *r) {
    const long long deadline = sc_rbs_mono_ns() + 200000000LL;
    for (;;) {
        memset(r, 0, sizeof *r);
        int rc = q->exec(db, sql, sc_rbs_row_cb, r, NULL);
        if ((rc & 0xff) != 5 /* SQLITE_BUSY */ || sc_rbs_mono_ns() >= deadline) return rc;
        struct timespec ts = {0, 5000000L};
        nanosleep(&ts, NULL);
    }
}

static int sc_rbs_exec_write(const struct sc_rbx_sqlite *q, void *db, const char *sql, int *filled) {
    const long long deadline = sc_rbs_mono_ns() + 200000000LL;
    for (;;) {
        *filled = 0;
        int rc = q->exec(db, sql, sc_rbs_changes, filled, NULL);
        if (!rc) return 0;
        *filled = 0;
        if (!q->exec(db, "ROLLBACK TO sc_rbs", NULL, NULL, NULL)) {
            if (q->exec(db, "RELEASE sc_rbs", NULL, NULL, NULL))
                (void)q->exec(db, "ROLLBACK", NULL, NULL, NULL);
        }
        if ((rc & 0xff) != 5 /* SQLITE_BUSY */ || sc_rbs_mono_ns() >= deadline) return rc;
        struct timespec ts = {0, 5000000L};
        nanosleep(&ts, NULL);
    }
}

// Before Engine reads a row in the library of db, a drive's (main), by Track.id (path == NULL)
// or by Track.path (path != NULL): when the stage holds the track and the row is as the insert
// left it, the row gets the staged data atomically. When allow_ondemand is set (deck load reads,
// never browse/insert scans) and the track is not staged (or no stage directory exists), the hook
// fetches the track's RBXA1 directly from stream-sync over a bounded local socket and commits it
// atomically via SAVEPOINT sc_rbs. Returns what it did, or why it left the row (only with
// sc_rbx_verbose), or NULL.
static const char *sc_rbs_fill(const struct sc_rbx_sqlite *q, void *db, const char *main,
                               long long track, const char *path, int path_len,
                               long long *out_track, int allow_ondemand) {
    static __thread char result[96];
    const char *why = NULL;
    struct timespec t0, t1;
    clock_gettime(CLOCK_MONOTONIC, &t0);
    struct stat st;
    size_t plen = 0;
    if (path) {
        plen = path_len < 0 ? strlen(path) : (size_t)path_len;
        if (!plen || plen >= SC_RBS_TEXT || memchr(path, 0, plen)) return NULL;
    } else if (track <= 0) {
        return sc_rbx_verbose ? "skipped (nothing staged on the deck)" : NULL;
    }
    const int stage_exists = !stat(sc_rbs_root, &st);
    const int ondemand_avail = allow_ondemand && sc_rbs_ondemand_available();
    if (!stage_exists && !ondemand_avail) {
        return sc_rbx_verbose ? "skipped (nothing staged on the deck)" : NULL;
    }
    struct sc_rbs_line *l = malloc(sizeof *l);
    struct sc_rbs_row *r = malloc(sizeof *r);
    char *key = malloc(SC_RBS_TEXT), *uuid = malloc(SC_RBS_TEXT), *query = malloc(SC_RBS_TEXT * 2 + 640);
    int inflight_slot = -1;
    int inflight_filled = 0;
    long long inflight_track = 0;
    if (!l || !r || !key || !uuid || !query) {
        why = "out of memory";
        goto done;
    }
    memset(r, 0, sizeof *r);
    int ascii = 0, found = 0;
    off_t off = 0;
    char raw_path[SC_RBS_TEXT];
    if (path) {
        memcpy(raw_path, path, plen);
        raw_path[plen] = 0;
    }
    // The cached library and stage, under the lock; the statements below run without it.
    pthread_mutex_lock(&sc_rbs_mu);
    struct sc_rbs_conn *c = sc_rbs_conn_of(db, main);
    const int known = c->known;
    snprintf(uuid, SC_RBS_TEXT, "%s", c->uuid);
    struct sc_rbs_stage *s = (known > 0 && stage_exists) ? sc_rbs_stage_of(uuid) : NULL;
    const int staged = s != NULL;
    if (staged && path) {
        ascii = sc_rbs_fold(raw_path, key, SC_RBS_TEXT, 1) >= 0;
        if (ascii || sc_rbs_fold(raw_path, key, SC_RBS_TEXT, 0) >= 0)
            found = sc_rbs_find(s, key, !ascii, &off);
    }
    pthread_mutex_unlock(&sc_rbs_mu);
    if (known < 0) {
        why = "skipped (no bridge snapshot)";
        goto done;
    }

    int use_ondemand = 0;
    if (!staged) {
        if (!ondemand_avail) {
            why = "skipped (nothing staged for this library)";
            goto done;
        }
        use_ondemand = 1;
    } else if (path && !found) {
        if (!ondemand_avail) {
            why = "skipped (not staged)";
            goto done;
        }
        use_ondemand = 1;
    }

    uint64_t od_hash = 0;
    long long od_deadline = 0;
    if (use_ondemand) {
        int safe = sc_rbs_thread_safe_for_ondemand();
        if (safe == -1) {
            why = "skipped (rt thread)";
            goto done;
        }
        if (safe == -2) {
            why = "skipped (gui thread)";
            goto done;
        }
        od_hash = sc_rbs_ondemand_key_hash(uuid, path ? raw_path : NULL, track);
        pthread_mutex_lock(&sc_rbs_mu);
        const int neg_hit = sc_rbs_neg_hit_locked(od_hash, sc_rbs_mono_ns());
        const int timeout_ms = sc_rbs_ondemand_timeout_ms > 0 ? sc_rbs_ondemand_timeout_ms : SC_RBS_ONDEMAND_TIMEOUT_DEFAULT_MS;
        pthread_mutex_unlock(&sc_rbs_mu);
        if (neg_hit) {
            why = "skipped (ondemand cached miss)";
            goto done;
        }
        od_deadline = sc_rbs_mono_ns() + (long long)timeout_ms * 1000000LL;
        long long peer_track = 0;
        inflight_slot = sc_rbs_inflight_enter(od_hash, od_deadline, &peer_track);
        if (inflight_slot == -1) {
            if (out_track && peer_track > 0) *out_track = peer_track;
            why = "skipped (row has analysis)";
            goto done;
        }
        if (inflight_slot == -2) {
            why = "skipped (ondemand busy)";
            goto done;
        }
    }

    // One read: the row's path, id, metadata and state, and the library's own uuid (a snapshot left
    // behind by another library can only make a miss).
    if (path) {
        char escaped[SC_RBS_TEXT * 2];
        sc_rbs_escape_sql(raw_path, escaped, sizeof escaped);
        snprintf(query, SC_RBS_TEXT * 2 + 640,
                 "SELECT t.path, t.pdbImportKey, (SELECT uuid FROM Information), " SC_RBS_UNTOUCHED("p.") ", " SC_RBS_INSERT_CUES("p.") ", t.id, COALESCE(t.key, -1), COALESCE(t.bpmAnalyzed, t.bpm, 0), COALESCE(t.length, 0)"
                 " FROM Track t JOIN PerformanceData p ON p.trackId = t.id WHERE t.path = '%s'", escaped);
    } else {
        snprintf(query, SC_RBS_TEXT * 2 + 640,
                 "SELECT t.path, t.pdbImportKey, (SELECT uuid FROM Information), " SC_RBS_UNTOUCHED("p.") ", " SC_RBS_INSERT_CUES("p.") ", t.id, COALESCE(t.key, -1), COALESCE(t.bpmAnalyzed, t.bpm, 0), COALESCE(t.length, 0)"
                 " FROM Track t JOIN PerformanceData p ON p.trackId = t.id WHERE t.id = %lld", track);
    }
    if (sc_rbs_exec_read(q, db, query, r) || r->rows != 1) {
        why = "skipped (no row)";
        goto done;
    }
    track = r->id;
    if (out_track) *out_track = track;
    if (strcmp(r->uuid, uuid)) {
        sc_rbs_conn_stale(db, main);
        why = "skipped (the snapshot names another library)";
        goto done;
    }
    if (!r->empty) {
        inflight_filled = 1;
        inflight_track = track;
        why = "skipped (row has analysis)";
        goto done;
    }
    if (!r->insert_cues) {
        why = "skipped (row has the deck's cues)";
        goto done;
    }
    if (r->import_key <= 0) {
        why = "skipped (Engine's own row)";
        goto done;
    }

    if (!use_ondemand && !path) {
        // The key: rbstate.FoldPath of an ASCII path; a non-ASCII one by the volume's spelling.
        ascii = sc_rbs_fold(r->path, key, SC_RBS_TEXT, 1) >= 0;
        if (ascii || sc_rbs_fold(r->path, key, SC_RBS_TEXT, 0) >= 0) {
            pthread_mutex_lock(&sc_rbs_mu);
            s = sc_rbs_stage_of(uuid);
            found = s && sc_rbs_find(s, key, !ascii, &off);
            pthread_mutex_unlock(&sc_rbs_mu);
        }
    }
    if (!use_ondemand) {
        found = found && sc_rbs_line_at(uuid, off, l) && (ascii ? !strcmp(l->key, key) : !strcmp(l->rel, key));
        if (found && !ascii) { // that line must still be its key's last, not dropped since
            off_t last = 0;
            pthread_mutex_lock(&sc_rbs_mu);
            s = sc_rbs_stage_of(uuid);
            found = s && sc_rbs_find(s, l->key, 0, &last) && last == off;
            pthread_mutex_unlock(&sc_rbs_mu);
        }
        if (!found) {
            if (!ondemand_avail) {
                why = "skipped (not staged)";
                goto done;
            }
            // PerformanceData load fallback: stage missed, try direct on-demand fetch.
            int safe = sc_rbs_thread_safe_for_ondemand();
            if (safe == -1) {
                why = "skipped (rt thread)";
                goto done;
            }
            if (safe == -2) {
                why = "skipped (gui thread)";
                goto done;
            }
            use_ondemand = 1;
            od_hash = sc_rbs_ondemand_key_hash(uuid, r->path[0] ? r->path : NULL, track);
            pthread_mutex_lock(&sc_rbs_mu);
            const int neg_hit = sc_rbs_neg_hit_locked(od_hash, sc_rbs_mono_ns());
            const int timeout_ms = sc_rbs_ondemand_timeout_ms > 0 ? sc_rbs_ondemand_timeout_ms : SC_RBS_ONDEMAND_TIMEOUT_DEFAULT_MS;
            pthread_mutex_unlock(&sc_rbs_mu);
            if (neg_hit) {
                why = "skipped (ondemand cached miss)";
                goto done;
            }
            od_deadline = sc_rbs_mono_ns() + (long long)timeout_ms * 1000000LL;
            long long peer_track = 0;
            inflight_slot = sc_rbs_inflight_enter(od_hash, od_deadline, &peer_track);
            if (inflight_slot == -1) {
                if (out_track && peer_track > 0) *out_track = peer_track;
                why = "skipped (row has analysis)";
                goto done;
            }
            if (inflight_slot == -2) {
                why = "skipped (ondemand busy)";
                goto done;
            }
        }
    }

    size_t size = 0;
    unsigned char *b = NULL;
    if (use_ondemand) {
        int http_status = 0;
        if (sc_rbs_ondemand_fetch(main, uuid, r, od_deadline, l, &b, &size, &http_status)) {
            if (http_status == 409) {
                sc_rbs_conn_stale(db, main);
                why = "skipped (the snapshot names another library)";
            } else if (http_status == 404 || http_status == 422) {
                pthread_mutex_lock(&sc_rbs_mu);
                sc_rbs_neg_put_locked(od_hash, SC_RBS_NEG_TTL_PERM_NS);
                pthread_mutex_unlock(&sc_rbs_mu);
                why = "skipped (ondemand degraded/missing)";
            } else {
                pthread_mutex_lock(&sc_rbs_mu);
                sc_rbs_neg_put_locked(od_hash, SC_RBS_NEG_TTL_TEMP_NS);
                pthread_mutex_unlock(&sc_rbs_mu);
                why = "ondemand fetch failed";
            }
            goto done;
        }
    } else {
        if (l->rules != SC_RBS_RULES || !l->perf || l->missing || l->degraded) {
            why = l->rules != SC_RBS_RULES ? "skipped (staged by other rules)" : "skipped (staged without analysis)";
            goto done;
        }
        char hex[41], bin[PATH_MAX];
        sc_rbs_sha1_hex(l->key, strlen(l->key), hex);
        snprintf(bin, sizeof bin, "%s/%s/%s.bin", sc_rbs_root, uuid, hex);
        b = sc_rbx_read(bin, &size);
        if (!b) {
            why = "skipped (no staged file)";
            goto done;
        }
    }
    const char *bad = NULL;
    char *update = sc_rbs_fill_sql(b, size, track, l->active, uuid, &bad);
    free(b);
    if (!update) {
        snprintf(result, sizeof result, "skipped (%s file %s)", use_ondemand ? "ondemand" : "staged", bad);
        why = result;
        goto done;
    }
    int filled = 0;
    const int error = sc_rbs_exec_write(q, db, update, &filled);
    free(update);
    if (error) {
        why = "update failed";
        goto done;
    }
    if (!filled) {
        why = "skipped (row changed)";
        goto done;
    }
    sc_rbs_journal(uuid, track, l);
    inflight_filled = 1;
    inflight_track = track;
    clock_gettime(CLOCK_MONOTONIC, &t1);
    snprintf(result, sizeof result, "%s (%.1f ms)", use_ondemand ? "filled on-demand" : "filled",
             (double)(t1.tv_sec - t0.tv_sec) * 1e3 + (double)(t1.tv_nsec - t0.tv_nsec) / 1e6);
    why = result;
done:
    if (inflight_slot >= 0) sc_rbs_inflight_leave(inflight_slot, inflight_filled, inflight_track);
    free(l);
    free(r);
    free(key);
    free(uuid);
    free(query);
    if (why && strncmp(why, "skipped", 7)) return why;
    return sc_rbx_verbose ? why : NULL;
}

static const char *sc_rbs_fill_track(const struct sc_rbx_sqlite *q, void *db, const char *main, long long track) {
    return sc_rbs_fill(q, db, main, track, NULL, 0, NULL, 0);
}

static const char *sc_rbs_fill_perf(const struct sc_rbx_sqlite *q, void *db, const char *main, long long track) {
    return sc_rbs_fill(q, db, main, track, NULL, 0, NULL, 1);
}

static const char *sc_rbs_fill_path(const struct sc_rbx_sqlite *q, void *db, const char *main,
                                    const char *path, int path_len, long long *track) {
    return sc_rbs_fill(q, db, main, 0, path, path_len, track, 1);
}

#endif
