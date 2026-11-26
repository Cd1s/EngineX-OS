#ifndef SC_REKORDBOX_BROWSE_H
#define SC_REKORDBOX_BROWSE_H

// On-demand CDJ-style browse direct-read for rekordbox USB/SD sticks.
//
// Reads <mount>/Engine Library/Database2/.rb-bridge-browse (format "RBB1 <uuid>\n",
// sorted "T\t<id>\t<dur>\t<sr>\t<ap>\n" and "A\t<stem>\t<aw>\n" rows written by
// pkg/rbstate/browse.go). Serves:
//   1. Cover art: redirects Engine Library/Artwork/<stem>.jpg straight to
//      <mount>/<aw> (zero stick writes, zero AlbumArt BLOBs).
//   2. RGB list waveforms: intercepts Engine Library/Database2/OverviewData/<uuid>/<id>.rgb
//      read-opens, decodes PWV6 (.2EX) / PWV4 (.EXT) / PWAV (.DAT) on demand into a
//      bounded RAM LRU (256 entries, ~800 KiB .bss), and returns a memfd_create fd.
//   3. Classic 3-Band + RGB trigger (A32 SQLite column hooks): on Engine's browse worker
//      thread ONLY (dynamically checked prctl(PR_GET_NAME) == "Thread (pooled)" and
//      SCHED_OTHER), synthesizes isAnalyzed = 1 and overviewWaveFormData (from the same
//      RAM LRU) while keeping m.db isAnalyzed = 0 on disk and NEVER altering deck-load
//      threads (TrackLoad*, J:Searchingtrac, J:Fetchingperfo, J:Importingtrac, SceneGraph).

#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <pthread.h>
#include <sched.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <sys/prctl.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

#ifndef MFD_CLOEXEC
#define MFD_CLOEXEC 0x0001U
#endif

enum {
    SC_RBB_MAX_DRIVES = 8,
    SC_RBB_CACHE_CAP = 256,
    SC_RBB_RAW_LEN = 24 + 1024 * 3 + 3,      // 3099 uncompressed overview bytes
    SC_RBB_BLOB_LEN = 4 + 2 + 5 + SC_RBB_RAW_LEN + 4, // 3114 qCompress stored-block bytes
    SC_RBB_MAX_INDEX_BYTES = 16 * 1024 * 1024,
    SC_RBB_MAX_SECTION_BYTES = 4 * 1024 * 1024,
    SC_RBB_REFRESH_SECS = 2,
    SC_RBB_STMT_CACHE_CAP = 8,
    SC_RBB_TLS_BLOB_SLOTS = 4,
};

struct sc_rbb_track {
    int64_t id;
    double dur;
    uint32_t sr;
    const char *ap;
    uint8_t on_disk;
};

struct sc_rbb_art {
    const char *stem;
    const char *aw;
};

struct sc_rbb_drive {
    int used;
    int has_index;
    uint64_t gen;
    time_t last_check;
    int64_t idx_size;
    int64_t idx_mtime_sec;
    int64_t idx_mtime_nsec;
    char mount[PATH_MAX];
    size_t mount_len;
    char uuid[64];
    char *raw_buf;
    struct sc_rbb_track *tracks;
    size_t n_tracks;
    struct sc_rbb_art *arts;
    size_t n_arts;
};

struct sc_rbb_cache_entry {
    uint64_t drive_gen;
    int64_t track_id;
    uint64_t lru_tick;
    uint8_t state;   // 0 = empty, 1 = valid, 2 = negative (unreadable ANLZ)
    uint8_t is_mono; // 1 = monochrome source (PWAV/PWV2/PWV3)
    uint8_t rgb_blob[SC_RBB_BLOB_LEN];
};

// Overridable by unit tests. Default matches rekordbox_analysis.h / rekordbox_stage.h.
static const char *sc_rbb_drives = "/media/";
static int sc_rbb_active; // relaxed atomic: 1 when at least one drive has .rb-bridge-browse
static pthread_mutex_t sc_rbb_mu = PTHREAD_MUTEX_INITIALIZER;
static uint64_t sc_rbb_gen_seq;
static uint64_t sc_rbb_lru_seq;
static struct sc_rbb_drive sc_rbb_tbl[SC_RBB_MAX_DRIVES];
static struct sc_rbb_cache_entry sc_rbb_cache[SC_RBB_CACHE_CAP];
static __thread int sc_rbb_in_hook;

static int sc_rbb_is_sched_other(void) {
    return sched_getscheduler(0) == SCHED_OTHER;
}

static time_t sc_rbb_mono_sec(void) {
    struct timespec ts;
    if (clock_gettime(CLOCK_MONOTONIC, &ts) != 0) return 0;
    return ts.tv_sec;
}

static int sc_rbb_raw_open_ro(const char *path) {
    return (int)syscall(SYS_openat, AT_FDCWD, path, O_RDONLY | O_CLOEXEC);
}

static uint16_t sc_rbb_be16(const uint8_t *p) {
    return (uint16_t)((uint16_t)p[0] << 8 | (uint16_t)p[1]);
}

static uint32_t sc_rbb_be32(const uint8_t *p) {
    return ((uint32_t)p[0] << 24) | ((uint32_t)p[1] << 16) |
           ((uint32_t)p[2] << 8) | (uint32_t)p[3];
}

static void sc_rbb_put_be32(uint8_t *p, uint32_t v) {
    p[0] = (uint8_t)(v >> 24);
    p[1] = (uint8_t)(v >> 16);
    p[2] = (uint8_t)(v >> 8);
    p[3] = (uint8_t)v;
}

static void sc_rbb_put_be64(uint8_t *p, uint64_t v) {
    sc_rbb_put_be32(p, (uint32_t)(v >> 32));
    sc_rbb_put_be32(p + 4, (uint32_t)v);
}

static int sc_rbb_safe_rel(const char *p) {
    if (!p || !*p) return 0;
    const char *s = p;
    while (*s == '/') s++;
    if (!*s) return 0;
    while (*s) {
        const char *seg = s;
        while (*s && *s != '/') s++;
        size_t n = (size_t)(s - seg);
        if ((n == 2 && seg[0] == '.' && seg[1] == '.') || (n == 1 && seg[0] == '.'))
            return 0;
        if (*s == '/') s++;
    }
    return 1;
}

static int sc_rbb_join_rel(char *out, size_t cap, const char *mount, const char *rel) {
    if (!sc_rbb_safe_rel(rel)) return 0;
    while (*rel == '/') rel++;
    int n = snprintf(out, cap, "%s/%s", mount, rel);
    return n > 0 && (size_t)n < cap;
}

static int sc_rbb_parse_mount(const char *path, char mount[PATH_MAX], const char **rel_out) {
    const char *prefix = sc_rbb_drives;
    if (!path || !prefix || !*prefix) return 0;
    size_t plen = strlen(prefix);
    if (strncmp(path, prefix, plen) != 0) return 0;
    const char *vol = path + plen;
    if (!*vol || *vol == '/') return 0;
    const char *slash = strchr(vol, '/');
    if (!slash) return 0;
    size_t mlen = (size_t)(slash - path);
    if (mlen == 0 || mlen >= PATH_MAX) return 0;
    memcpy(mount, path, mlen);
    mount[mlen] = '\0';
    *rel_out = slash + 1;
    return 1;
}

static void sc_rbb_recompute_active_locked(void) {
    int any = 0;
    for (int i = 0; i < SC_RBB_MAX_DRIVES; i++) {
        if (sc_rbb_tbl[i].used && sc_rbb_tbl[i].has_index) {
            any = 1;
            break;
        }
    }
    __atomic_store_n(&sc_rbb_active, any, __ATOMIC_RELAXED);
}

static void sc_rbb_clear_drive_index_locked(struct sc_rbb_drive *d) {
    free(d->raw_buf);
    free(d->tracks);
    free(d->arts);
    d->raw_buf = NULL;
    d->tracks = NULL;
    d->arts = NULL;
    d->n_tracks = 0;
    d->n_arts = 0;
    d->uuid[0] = '\0';
    d->has_index = 0;
    d->idx_size = 0;
    d->idx_mtime_sec = 0;
    d->idx_mtime_nsec = 0;
    d->gen = ++sc_rbb_gen_seq;
}

static int sc_rbb_cmp_track(const void *a, const void *b) {
    const struct sc_rbb_track *ta = (const struct sc_rbb_track *)a;
    const struct sc_rbb_track *tb = (const struct sc_rbb_track *)b;
    return (ta->id > tb->id) - (ta->id < tb->id);
}

static int sc_rbb_cmp_art(const void *a, const void *b) {
    const struct sc_rbb_art *aa = (const struct sc_rbb_art *)a;
    const struct sc_rbb_art *ab = (const struct sc_rbb_art *)b;
    return strcmp(aa->stem, ab->stem);
}

// Parse RBB1 index buffer in-place (replaces tabs/newlines with NULs).
static int sc_rbb_parse_index_buf(char *buf, size_t len, char uuid[64],
                                  struct sc_rbb_track **tracks_out, size_t *nt_out,
                                  struct sc_rbb_art **arts_out, size_t *na_out) {
    if (len < 6 || memcmp(buf, "RBB1 ", 5) != 0) return 0;
    char *nl = memchr(buf, '\n', len);
    if (!nl) return 0;
    *nl = '\0';
    char *u = buf + 5;
    size_t ulen = strlen(u);
    while (ulen > 0 && u[ulen - 1] == '\r') u[--ulen] = '\0';
    if (ulen == 0 || ulen >= 64) return 0;
    memcpy(uuid, u, ulen + 1);

    size_t nt = 0, na = 0;
    char *p = nl + 1, *end = buf + len;
    while (p < end) {
        if (*p == 'T' && p + 1 < end && p[1] == '\t') nt++;
        else if (*p == 'A' && p + 1 < end && p[1] == '\t') na++;
        char *next = memchr(p, '\n', (size_t)(end - p));
        if (!next) break;
        p = next + 1;
    }

    struct sc_rbb_track *tracks = nt ? (struct sc_rbb_track *)calloc(nt, sizeof *tracks) : NULL;
    struct sc_rbb_art *arts = na ? (struct sc_rbb_art *)calloc(na, sizeof *arts) : NULL;
    if ((nt && !tracks) || (na && !arts)) {
        free(tracks);
        free(arts);
        return 0;
    }

    size_t ti = 0, ai = 0;
    int t_sorted = 1, a_sorted = 1;
    p = nl + 1;
    while (p < end) {
        char *line = p;
        char *next = memchr(p, '\n', (size_t)(end - p));
        if (next) {
            *next = '\0';
            if (next > line && next[-1] == '\r') next[-1] = '\0';
            p = next + 1;
        } else {
            p = end;
        }
        if (line[0] == 'T' && line[1] == '\t' && ti < nt) {
            char *f1 = line + 2;
            char *f2 = strchr(f1, '\t');
            char *f3 = f2 ? strchr(f2 + 1, '\t') : NULL;
            char *f4 = f3 ? strchr(f3 + 1, '\t') : NULL;
            if (!f2 || !f3 || !f4) continue;
            *f2 = *f3 = *f4 = '\0';
            char *ep = NULL;
            long long id = strtoll(f1, &ep, 10);
            if (!ep || *ep || id <= 0) continue;
            double dur = strtod(f2 + 1, &ep);
            if (dur < 0 || dur > 86400.0 * 7) dur = 0;
            unsigned long sr = strtoul(f3 + 1, &ep, 10);
            if (sr < 8000 || sr > 384000) sr = 44100;
            const char *ap = f4 + 1;
            if (!sc_rbb_safe_rel(ap)) continue;
            if (ti > 0 && tracks[ti - 1].id > (int64_t)id) t_sorted = 0;
            tracks[ti].id = (int64_t)id;
            tracks[ti].dur = dur;
            tracks[ti].sr = (uint32_t)sr;
            tracks[ti].ap = ap;
            tracks[ti].on_disk = 0;
            ti++;
        } else if (line[0] == 'A' && line[1] == '\t' && ai < na) {
            char *f1 = line + 2;
            char *f2 = strchr(f1, '\t');
            if (!f2) continue;
            *f2 = '\0';
            const char *stem = f1;
            const char *aw = f2 + 1;
            if (!*stem || strlen(stem) >= 64 || strchr(stem, '/') || !sc_rbb_safe_rel(aw))
                continue;
            if (ai > 0 && strcmp(arts[ai - 1].stem, stem) > 0) a_sorted = 0;
            arts[ai].stem = stem;
            arts[ai].aw = aw;
            ai++;
        }
    }
    if (!t_sorted && ti > 1) qsort(tracks, ti, sizeof *tracks, sc_rbb_cmp_track);
    if (!a_sorted && ai > 1) qsort(arts, ai, sizeof *arts, sc_rbb_cmp_art);
    *tracks_out = tracks;
    *nt_out = ti;
    *arts_out = arts;
    *na_out = ai;
    return 1;
}

static char *sc_rbb_read_file_bounded(const char *path, size_t max_bytes, size_t *len_out) {
    int fd = sc_rbb_raw_open_ro(path);
    if (fd < 0) return NULL;
    size_t cap = 4096, len = 0;
    char *buf = (char *)malloc(cap + 1);
    if (!buf) {
        close(fd);
        return NULL;
    }
    for (;;) {
        if (len == cap) {
            if (cap >= max_bytes) {
                free(buf);
                close(fd);
                return NULL;
            }
            size_t next = cap * 2;
            if (next > max_bytes) next = max_bytes;
            char *nb = (char *)realloc(buf, next + 1);
            if (!nb) {
                free(buf);
                close(fd);
                return NULL;
            }
            buf = nb;
            cap = next;
        }
        ssize_t n = read(fd, buf + len, cap - len);
        if (n < 0) {
            if (errno == EINTR) continue;
            free(buf);
            close(fd);
            return NULL;
        }
        if (n == 0) break;
        len += (size_t)n;
    }
    close(fd);
    buf[len] = '\0';
    *len_out = len;
    return buf;
}

// Find or allocate a drive slot for mount, refreshing .rb-bridge-browse every 2 s.
// Returns slot index [0..SC_RBB_MAX_DRIVES-1] or -1. Must be called with sc_rbb_mu unlocked.
static int sc_rbb_ensure_drive(const char *mount) {
    if (!mount || !*mount) return -1;
    size_t mlen = strlen(mount);
    if (mlen >= PATH_MAX) return -1;
    time_t now = sc_rbb_mono_sec();

    pthread_mutex_lock(&sc_rbb_mu);
    int slot = -1, empty = -1, oldest = 0;
    for (int i = 0; i < SC_RBB_MAX_DRIVES; i++) {
        if (sc_rbb_tbl[i].used && sc_rbb_tbl[i].mount_len == mlen &&
            !memcmp(sc_rbb_tbl[i].mount, mount, mlen + 1)) {
            slot = i;
            break;
        }
        if (!sc_rbb_tbl[i].used && empty < 0) empty = i;
        if (sc_rbb_tbl[i].used && sc_rbb_tbl[i].last_check < sc_rbb_tbl[oldest].last_check)
            oldest = i;
    }
    if (slot >= 0 && sc_rbb_tbl[slot].last_check != 0 &&
        now - sc_rbb_tbl[slot].last_check < SC_RBB_REFRESH_SECS) {
        int res = sc_rbb_tbl[slot].has_index ? slot : -1;
        pthread_mutex_unlock(&sc_rbb_mu);
        return res;
    }
    int64_t prev_size = (slot >= 0) ? sc_rbb_tbl[slot].idx_size : -1;
    int64_t prev_sec = (slot >= 0) ? sc_rbb_tbl[slot].idx_mtime_sec : -1;
    int64_t prev_nsec = (slot >= 0) ? sc_rbb_tbl[slot].idx_mtime_nsec : -1;
    int prev_has = (slot >= 0) ? sc_rbb_tbl[slot].has_index : 0;
    pthread_mutex_unlock(&sc_rbb_mu);

    char idx_path[PATH_MAX];
    int pn = snprintf(idx_path, sizeof idx_path, "%s/Engine Library/Database2/.rb-bridge-browse", mount);
    if (pn <= 0 || (size_t)pn >= sizeof idx_path) return -1;

    struct stat st;
    sc_rbb_in_hook++;
    int stat_rc = stat(idx_path, &st);
    sc_rbb_in_hook--;

    int unchanged = 0;
    char *raw = NULL;
    size_t raw_len = 0;
    char uuid[64] = {0};
    struct sc_rbb_track *tracks = NULL;
    struct sc_rbb_art *arts = NULL;
    size_t nt = 0, na = 0;
    int parsed = 0;

    if (stat_rc == 0 && S_ISREG(st.st_mode) && st.st_size > 0 && st.st_size <= SC_RBB_MAX_INDEX_BYTES) {
        if (prev_has && (int64_t)st.st_size == prev_size &&
            (int64_t)st.st_mtim.tv_sec == prev_sec &&
            (int64_t)st.st_mtim.tv_nsec == prev_nsec) {
            unchanged = 1;
        } else {
            sc_rbb_in_hook++;
            raw = sc_rbb_read_file_bounded(idx_path, SC_RBB_MAX_INDEX_BYTES, &raw_len);
            sc_rbb_in_hook--;
            if (raw) {
                parsed = sc_rbb_parse_index_buf(raw, raw_len, uuid, &tracks, &nt, &arts, &na);
                if (!parsed) {
                    free(raw);
                    raw = NULL;
                }
            }
        }
    }

    pthread_mutex_lock(&sc_rbb_mu);
    slot = -1;
    empty = -1;
    oldest = 0;
    for (int i = 0; i < SC_RBB_MAX_DRIVES; i++) {
        if (sc_rbb_tbl[i].used && sc_rbb_tbl[i].mount_len == mlen &&
            !memcmp(sc_rbb_tbl[i].mount, mount, mlen + 1)) {
            slot = i;
            break;
        }
        if (!sc_rbb_tbl[i].used && empty < 0) empty = i;
        if (sc_rbb_tbl[i].used && sc_rbb_tbl[i].last_check < sc_rbb_tbl[oldest].last_check)
            oldest = i;
    }
    if (slot < 0) {
        slot = (empty >= 0) ? empty : oldest;
        if (sc_rbb_tbl[slot].used) sc_rbb_clear_drive_index_locked(&sc_rbb_tbl[slot]);
        sc_rbb_tbl[slot].used = 1;
        memcpy(sc_rbb_tbl[slot].mount, mount, mlen + 1);
        sc_rbb_tbl[slot].mount_len = mlen;
    }
    struct sc_rbb_drive *d = &sc_rbb_tbl[slot];
    d->last_check = now ? now : 1;
    if (unchanged && d->has_index) {
        pthread_mutex_unlock(&sc_rbb_mu);
        return slot;
    }
    if (parsed) {
        sc_rbb_clear_drive_index_locked(d);
        d->raw_buf = raw;
        d->tracks = tracks;
        d->n_tracks = nt;
        d->arts = arts;
        d->n_arts = na;
        memcpy(d->uuid, uuid, sizeof d->uuid);
        d->has_index = 1;
        d->idx_size = (int64_t)st.st_size;
        d->idx_mtime_sec = (int64_t)st.st_mtim.tv_sec;
        d->idx_mtime_nsec = (int64_t)st.st_mtim.tv_nsec;
        sc_rbb_recompute_active_locked();
        pthread_mutex_unlock(&sc_rbb_mu);
        return slot;
    }
    free(raw);
    free(tracks);
    free(arts);
    if (d->has_index) {
        sc_rbb_clear_drive_index_locked(d);
        sc_rbb_recompute_active_locked();
    }
    pthread_mutex_unlock(&sc_rbb_mu);
    return -1;
}

static struct sc_rbb_track *sc_rbb_find_track_locked(struct sc_rbb_drive *d, int64_t id) {
    size_t lo = 0, hi = d->n_tracks;
    while (lo < hi) {
        size_t mid = lo + (hi - lo) / 2;
        if (d->tracks[mid].id < id) lo = mid + 1;
        else if (d->tracks[mid].id > id) hi = mid;
        else return &d->tracks[mid];
    }
    return NULL;
}

static const struct sc_rbb_art *sc_rbb_find_art_locked(const struct sc_rbb_drive *d, const char *stem) {
    size_t lo = 0, hi = d->n_arts;
    while (lo < hi) {
        size_t mid = lo + (hi - lo) / 2;
        int c = strcmp(d->arts[mid].stem, stem);
        if (c < 0) lo = mid + 1;
        else if (c > 0) hi = mid;
        else return &d->arts[mid];
    }
    return NULL;
}

// Pure-C RFC-1950 stored-block qCompress (zero -lz dependency, constant 3114 bytes).
static void sc_rbb_qcompress_stored(const uint8_t raw[SC_RBB_RAW_LEN], uint8_t out[SC_RBB_BLOB_LEN]) {
    sc_rbb_put_be32(out, (uint32_t)SC_RBB_RAW_LEN);
    out[4] = 0x78;
    out[5] = 0x01; // (0x78 * 256 + 0x01) % 31 == 0
    out[6] = 0x01; // BFINAL=1, BTYPE=00 (stored)
    uint16_t len = (uint16_t)SC_RBB_RAW_LEN;
    uint16_t nlen = (uint16_t)(~len);
    out[7] = (uint8_t)(len & 0xffu);
    out[8] = (uint8_t)(len >> 8);
    out[9] = (uint8_t)(nlen & 0xffu);
    out[10] = (uint8_t)(nlen >> 8);
    memcpy(out + 11, raw, SC_RBB_RAW_LEN);
    uint32_t s1 = 1, s2 = 0;
    for (size_t i = 0; i < SC_RBB_RAW_LEN; i++) {
        s1 += raw[i];
        if (s1 >= 65521u) s1 -= 65521u;
        s2 += s1;
        if (s2 >= 65521u) s2 -= 65521u;
    }
    sc_rbb_put_be32(out + 11 + SC_RBB_RAW_LEN, (s2 << 16) | s1);
}

enum {
    SC_RBB_W_NONE = 0,
    SC_RBB_W_PWV6 = 6,
    SC_RBB_W_PWV4 = 4,
    SC_RBB_W_PWV5 = 5,
    SC_RBB_W_MONO = 1, // PWAV / PWV2 / PWV3
};

static int sc_rbb_read_exact(int fd, uint8_t *dst, size_t n) {
    size_t got = 0;
    while (got < n) {
        ssize_t r = read(fd, dst + got, n - got);
        if (r < 0) {
            if (errno == EINTR) continue;
            return 0;
        }
        if (r == 0) return 0;
        got += (size_t)r;
    }
    return 1;
}

// Buffered sequential ANLZ reader that stops as soon as the target waveform section is
// read (never transferring PWV7 detail bytes across USB).
struct sc_rbb_reader {
    int fd;
    uint64_t pos;     // logical file offset
    uint64_t buf_off; // file offset of buf[0]
    size_t buf_len;
    uint8_t buf[32768];
};

static int sc_rbb_rdr_read(struct sc_rbb_reader *r, uint8_t *dst, size_t n) {
    size_t got = 0;
    while (got < n) {
        if (r->pos >= r->buf_off && r->pos < r->buf_off + r->buf_len) {
            size_t avail = (size_t)((r->buf_off + r->buf_len) - r->pos);
            size_t take = (n - got < avail) ? (n - got) : avail;
            memcpy(dst + got, r->buf + (r->pos - r->buf_off), take);
            r->pos += take;
            got += take;
            continue;
        }
        if (lseek(r->fd, (off_t)r->pos, SEEK_SET) < 0) return 0;
        // Large section reads go straight to dst without thrashing the 32 KiB buffer.
        if (n - got >= sizeof r->buf) {
            if (!sc_rbb_read_exact(r->fd, dst + got, n - got)) return 0;
            r->pos += (n - got);
            return 1;
        }
        ssize_t rd = read(r->fd, r->buf, sizeof r->buf);
        if (rd < 0) {
            if (errno == EINTR) continue;
            return 0;
        }
        if (rd == 0) return 0;
        r->buf_off = r->pos;
        r->buf_len = (size_t)rd;
    }
    return 1;
}

static void sc_rbb_encode_bins(int kind, const uint8_t *data, uint32_t width, uint32_t count,
                               double dur, uint32_t sr, int blue_tint, uint8_t out[SC_RBB_BLOB_LEN]) {
    uint8_t raw[SC_RBB_RAW_LEN];
    const uint64_t bins = 1024;
    if (sr == 0) sr = 44100;
    uint64_t samples = (dur > 0.0) ? (uint64_t)(dur * (double)sr + 0.5) : bins;
    if (samples == 0) samples = bins;
    double spb = (double)samples / (double)bins;
    uint64_t spb_bits = 0;
    memcpy(&spb_bits, &spb, sizeof spb_bits);
    sc_rbb_put_be64(raw + 0, bins);
    sc_rbb_put_be64(raw + 8, bins);
    sc_rbb_put_be64(raw + 16, spb_bits);

    uint8_t top[3] = {0, 0, 0};
    for (uint64_t i = 0; i < bins; i++) {
        uint64_t lo = i * (uint64_t)count / bins;
        uint64_t hi = (i + 1) * (uint64_t)count / bins;
        if (hi <= lo) hi = lo + 1;
        uint8_t peak[3] = {0, 0, 0};
        for (uint64_t j = lo; j < hi && j < (uint64_t)count; j++) {
            const uint8_t *p = data + j * width;
            uint8_t v[3] = {0, 0, 0};
            if (kind == SC_RBB_W_PWV6) {
                v[0] = p[0]; v[1] = p[1]; v[2] = p[2];
            } else if (kind == SC_RBB_W_PWV4) {
                v[0] = p[3]; v[1] = p[4]; v[2] = p[5];
            } else if (kind == SC_RBB_W_PWV5) {
                uint16_t w = sc_rbb_be16(p);
                uint32_t h = (w >> 2) & 31u;
                v[0] = (uint8_t)(((uint32_t)((w >> 13) & 7u) * 255u * h) / (7u * 31u));
                v[1] = (uint8_t)(((uint32_t)((w >> 10) & 7u) * 255u * h) / (7u * 31u));
                v[2] = (uint8_t)(((uint32_t)((w >> 7) & 7u) * 255u * h) / (7u * 31u));
            } else {
                uint8_t m = (uint8_t)(((uint32_t)(p[0] & 31u) * 255u) / 31u);
                if (blue_tint) {
                    v[0] = (uint8_t)(m / 4u);
                    v[1] = (uint8_t)(m / 2u);
                    v[2] = m;
                } else {
                    v[0] = m; v[1] = m; v[2] = m;
                }
            }
            for (int c = 0; c < 3; c++) {
                if (v[c] > peak[c]) peak[c] = v[c];
                if (v[c] > top[c]) top[c] = v[c];
            }
        }
        size_t off = 24 + (size_t)i * 3;
        raw[off] = peak[0];
        raw[off + 1] = peak[1];
        raw[off + 2] = peak[2];
    }
    raw[24 + 1024 * 3 + 0] = top[0];
    raw[24 + 1024 * 3 + 1] = top[1];
    raw[24 + 1024 * 3 + 2] = top[2];
    sc_rbb_qcompress_stored(raw, out);
}

// Scan one ANLZ file for the best matching overview waveform.
// mode: 0 = .2EX (PWV6 only), 1 = .EXT (PWV4 > PWV5 > PWV3), 2 = .DAT (PWAV > PWV2).
static int sc_rbb_scan_anlz_file(const char *path, int mode, double dur, uint32_t sr,
                                 uint8_t out[SC_RBB_BLOB_LEN], uint8_t *is_mono_out) {
    int fd = sc_rbb_raw_open_ro(path);
    if (fd < 0) return 0;

    struct sc_rbb_reader *r = (struct sc_rbb_reader *)calloc(1, sizeof *r);
    if (!r) {
        close(fd);
        return 0;
    }
    r->fd = fd;

    uint8_t pmai[12];
    if (!sc_rbb_rdr_read(r, pmai, 12) || memcmp(pmai, "PMAI", 4) != 0) {
        close(fd);
        free(r);
        return 0;
    }
    uint32_t hdr_len = sc_rbb_be32(pmai + 4);
    uint32_t file_len = sc_rbb_be32(pmai + 8);
    if (hdr_len < 12 || (file_len != 0 && file_len < hdr_len)) {
        close(fd);
        free(r);
        return 0;
    }

    uint8_t *best_data = NULL;
    int best_kind = SC_RBB_W_NONE, best_prio = 0;
    uint32_t best_w = 0, best_n = 0;
    int stop_prio = (mode == 0) ? 5 : (mode == 1 ? 4 : 2);

    uint64_t off = hdr_len;
    for (int iter = 0; iter < 128; iter++) {
        if (file_len != 0 && off + 12 > file_len) break;
        r->pos = off;
        uint8_t shdr[24];
        if (!sc_rbb_rdr_read(r, shdr, 12)) break;
        uint32_t sh = sc_rbb_be32(shdr + 4);
        uint32_t sn = sc_rbb_be32(shdr + 8);
        if (sh < 12 || sn < sh || sn > SC_RBB_MAX_SECTION_BYTES) break;
        if (file_len != 0 && off + sn > file_len) break;

        int kind = SC_RBB_W_NONE, prio = 0;
        if (mode == 0 && !memcmp(shdr, "PWV6", 4)) { kind = SC_RBB_W_PWV6; prio = 5; }
        else if (mode == 1 && !memcmp(shdr, "PWV4", 4)) { kind = SC_RBB_W_PWV4; prio = 4; }
        else if (mode == 1 && !memcmp(shdr, "PWV5", 4)) { kind = SC_RBB_W_PWV5; prio = 3; }
        else if (mode == 1 && !memcmp(shdr, "PWV3", 4)) { kind = SC_RBB_W_MONO; prio = 2; }
        else if (mode == 2 && !memcmp(shdr, "PWAV", 4)) { kind = SC_RBB_W_MONO; prio = 2; }
        else if (mode == 2 && !memcmp(shdr, "PWV2", 4)) { kind = SC_RBB_W_MONO; prio = 1; }

        if (kind != SC_RBB_W_NONE && prio > best_prio && sn >= 20) {
            size_t need_hdr = (kind == SC_RBB_W_PWV6 || !memcmp(shdr, "PWAV", 4) || !memcmp(shdr, "PWV2", 4)) ? 20 : 24;
            if (sh >= need_hdr && sc_rbb_rdr_read(r, shdr + 12, need_hdr - 12)) {
                uint32_t w = 1, n = sc_rbb_be32(shdr + 12);
                if (kind == SC_RBB_W_PWV6 || kind == SC_RBB_W_PWV4 || kind == SC_RBB_W_PWV5 || !memcmp(shdr, "PWV3", 4)) {
                    w = sc_rbb_be32(shdr + 12);
                    n = sc_rbb_be32(shdr + 16);
                }
                if (kind == SC_RBB_W_PWV4) w = 6;
                int min_w = (kind == SC_RBB_W_PWV6) ? 3 : (kind == SC_RBB_W_PWV4 ? 6 : (kind == SC_RBB_W_PWV5 ? 2 : 1));
                uint64_t body_bytes = (uint64_t)w * (uint64_t)n;
                if (w >= (uint32_t)min_w && n > 0 && body_bytes <= (uint64_t)(sn - sh) &&
                    body_bytes <= SC_RBB_MAX_SECTION_BYTES) {
                    uint8_t *data = (uint8_t *)malloc((size_t)body_bytes);
                    if (data) {
                        r->pos = off + sh;
                        if (sc_rbb_rdr_read(r, data, (size_t)body_bytes)) {
                            free(best_data);
                            best_data = data;
                            best_kind = kind;
                            best_prio = prio;
                            best_w = w;
                            best_n = n;
                            if (best_prio >= stop_prio) break;
                        } else {
                            free(data);
                        }
                    }
                }
            }
        }
        off += sn;
    }

    close(fd);
    free(r);
    if (!best_data) return 0;
    sc_rbb_encode_bins(best_kind, best_data, best_w, best_n, dur, sr, 1, out);
    if (is_mono_out) *is_mono_out = (best_kind == SC_RBB_W_MONO) ? 1 : 0;
    free(best_data);
    return 1;
}

static int sc_rbb_build_from_anlz(const char *mount, const char *ap, double dur, uint32_t sr,
                                  uint8_t out[SC_RBB_BLOB_LEN], uint8_t *is_mono_out) {
    char base[PATH_MAX];
    if (!sc_rbb_join_rel(base, sizeof base, mount, ap)) return 0;
    size_t blen = strlen(base);
    int lower_hint = 0;
    if (blen >= 4 && base[blen - 4] == '.') {
        if (base[blen - 3] >= 'a' && base[blen - 3] <= 'z') lower_hint = 1;
        base[blen - 4] = '\0';
        blen -= 4;
    }

    char path[PATH_MAX];
    static const char *const exts_upper[3] = {".2EX", ".EXT", ".DAT"};
    static const char *const exts_lower[3] = {".2ex", ".ext", ".dat"};
    for (int mode = 0; mode < 3; mode++) {
        if (blen + 4 >= sizeof path) return 0;
        if (lower_hint) {
            snprintf(path, sizeof path, "%s%s", base, exts_lower[mode]);
            if (sc_rbb_scan_anlz_file(path, mode, dur, sr, out, is_mono_out)) return 1;
        }
        snprintf(path, sizeof path, "%s%s", base, exts_upper[mode]);
        if (sc_rbb_scan_anlz_file(path, mode, dur, sr, out, is_mono_out)) return 1;
    }
    return 0;
}

// Fetch or synthesize overview blob for (slot, expected_gen, track_id) into dst[3114].
// Releases sc_rbb_mu during stick I/O so concurrent readers/artwork never stall.
static int sc_rbb_fetch_overview(int slot, uint64_t expected_gen, int64_t track_id,
                                 uint8_t dst[SC_RBB_BLOB_LEN]) {
    if (slot < 0 || slot >= SC_RBB_MAX_DRIVES || track_id <= 0) return 0;

    char mount[PATH_MAX];
    char ap[PATH_MAX];
    double dur = 0;
    uint32_t sr = 44100;
    uint64_t gen = 0;

    pthread_mutex_lock(&sc_rbb_mu);
    struct sc_rbb_drive *d = &sc_rbb_tbl[slot];
    if (!d->used || !d->has_index || (expected_gen != 0 && d->gen != expected_gen)) {
        pthread_mutex_unlock(&sc_rbb_mu);
        return 0;
    }
    gen = d->gen;
    for (int i = 0; i < SC_RBB_CACHE_CAP; i++) {
        if (sc_rbb_cache[i].state != 0 && sc_rbb_cache[i].drive_gen == gen &&
            sc_rbb_cache[i].track_id == track_id) {
            sc_rbb_cache[i].lru_tick = ++sc_rbb_lru_seq;
            int ok = (sc_rbb_cache[i].state == 1);
            if (ok) memcpy(dst, sc_rbb_cache[i].rgb_blob, SC_RBB_BLOB_LEN);
            pthread_mutex_unlock(&sc_rbb_mu);
            return ok;
        }
    }
    struct sc_rbb_track *tr = sc_rbb_find_track_locked(d, track_id);
    if (!tr || !tr->ap || strlen(tr->ap) >= sizeof ap) {
        pthread_mutex_unlock(&sc_rbb_mu);
        return 0;
    }
    memcpy(mount, d->mount, d->mount_len + 1);
    strcpy(ap, tr->ap);
    dur = tr->dur;
    sr = tr->sr;
    pthread_mutex_unlock(&sc_rbb_mu);

    uint8_t built[SC_RBB_BLOB_LEN];
    uint8_t is_mono = 0;
    sc_rbb_in_hook++;
    int ok = sc_rbb_build_from_anlz(mount, ap, dur, sr, built, &is_mono);
    sc_rbb_in_hook--;

    pthread_mutex_lock(&sc_rbb_mu);
    int victim = 0;
    uint64_t min_tick = UINT64_MAX;
    for (int i = 0; i < SC_RBB_CACHE_CAP; i++) {
        if (sc_rbb_cache[i].state != 0 && sc_rbb_cache[i].drive_gen == gen &&
            sc_rbb_cache[i].track_id == track_id) {
            victim = i;
            break;
        }
        if (sc_rbb_cache[i].state == 0) {
            victim = i;
            break;
        }
        if (sc_rbb_cache[i].lru_tick < min_tick) {
            min_tick = sc_rbb_cache[i].lru_tick;
            victim = i;
        }
    }
    sc_rbb_cache[victim].drive_gen = gen;
    sc_rbb_cache[victim].track_id = track_id;
    sc_rbb_cache[victim].lru_tick = ++sc_rbb_lru_seq;
    sc_rbb_cache[victim].is_mono = is_mono;
    if (ok) {
        sc_rbb_cache[victim].state = 1;
        memcpy(sc_rbb_cache[victim].rgb_blob, built, SC_RBB_BLOB_LEN);
        memcpy(dst, built, SC_RBB_BLOB_LEN);
    } else {
        sc_rbb_cache[victim].state = 2;
    }
    pthread_mutex_unlock(&sc_rbb_mu);
    return ok;
}

// Parse Engine Library/Database2/OverviewData/<uuid>/<id>.rgb from rel.
static int sc_rbb_parse_rgb_rel(const char *rel, char uuid_out[64], int64_t *id_out) {
    static const char pfx[] = "Engine Library/Database2/OverviewData/";
    size_t plen = sizeof pfx - 1;
    if (strncmp(rel, pfx, plen) != 0) return 0;
    const char *u = rel + plen;
    const char *slash = strchr(u, '/');
    if (!slash) return 0;
    size_t ulen = (size_t)(slash - u);
    if (ulen == 0 || ulen >= 64) return 0;
    const char *file = slash + 1;
    size_t flen = strlen(file);
    if (flen <= 4 || strcmp(file + flen - 4, ".rgb") != 0) return 0;
    if (strchr(file, '/')) return 0;
    char *ep = NULL;
    long long id = strtoll(file, &ep, 10);
    if (!ep || ep != file + flen - 4 || id <= 0) return 0;
    memcpy(uuid_out, u, ulen);
    uuid_out[ulen] = '\0';
    *id_out = (int64_t)id;
    return 1;
}

// Lookup a drive + track for OverviewData/<uuid>/<id>.rgb.
// If mark_on_disk is non-zero (write-open), marks the track so future reads use disk.
static int sc_rbb_lookup_rgb_track(const char *path, int mark_on_disk,
                                   int *slot_out, uint64_t *gen_out, int64_t *id_out) {
    char mount[PATH_MAX];
    const char *rel = NULL;
    if (!sc_rbb_parse_mount(path, mount, &rel)) return 0;
    char uuid[64];
    int64_t id = 0;
    if (!sc_rbb_parse_rgb_rel(rel, uuid, &id)) return 0;
    int slot = sc_rbb_ensure_drive(mount);
    if (slot < 0) return 0;

    pthread_mutex_lock(&sc_rbb_mu);
    struct sc_rbb_drive *d = &sc_rbb_tbl[slot];
    int ok = 0;
    if (d->used && d->has_index && !strcmp(d->uuid, uuid)) {
        struct sc_rbb_track *tr = sc_rbb_find_track_locked(d, id);
        if (tr) {
            if (mark_on_disk) {
                tr->on_disk = 1;
            } else if (!tr->on_disk) {
                *slot_out = slot;
                *gen_out = d->gen;
                *id_out = id;
                ok = 1;
            }
        }
    }
    pthread_mutex_unlock(&sc_rbb_mu);
    return ok;
}

// Note m.db open so sc_rbb_active becomes 1 before Engine's first browse query.
static void sc_rbb_note_path(const char *path) {
    if (!path || sc_rbb_in_hook || !sc_rbb_is_sched_other()) return;
    char mount[PATH_MAX];
    const char *rel = NULL;
    if (!sc_rbb_parse_mount(path, mount, &rel)) return;
    if (!strcmp(rel, "Engine Library/Database2/m.db")) {
        (void)sc_rbb_ensure_drive(mount);
    }
}

// Redirect <mount>/Engine Library/Artwork/<stem>.jpg -> <mount>/<aw>.
// Returns 1 if mapped into out, 0 otherwise.
static int sc_rbb_map_artwork(const char *path, char out[PATH_MAX]) {
    if (!path || sc_rbb_in_hook || !sc_rbb_is_sched_other()) return 0;
    char mount[PATH_MAX];
    const char *rel = NULL;
    if (!sc_rbb_parse_mount(path, mount, &rel)) return 0;
    static const char pfx[] = "Engine Library/Artwork/";
    size_t plen = sizeof pfx - 1;
    if (strncmp(rel, pfx, plen) != 0) return 0;
    const char *file = rel + plen;
    size_t flen = strlen(file);
    if (flen <= 4 || flen - 4 >= 64 || strcmp(file + flen - 4, ".jpg") != 0 || strchr(file, '/'))
        return 0;
    char stem[64];
    memcpy(stem, file, flen - 4);
    stem[flen - 4] = '\0';

    int slot = sc_rbb_ensure_drive(mount);
    if (slot < 0) return 0;
    int ok = 0;
    pthread_mutex_lock(&sc_rbb_mu);
    struct sc_rbb_drive *d = &sc_rbb_tbl[slot];
    if (d->used && d->has_index) {
        const struct sc_rbb_art *a = sc_rbb_find_art_locked(d, stem);
        if (a && sc_rbb_join_rel(out, PATH_MAX, d->mount, a->aw)) ok = 1;
    }
    pthread_mutex_unlock(&sc_rbb_mu);
    return ok;
}

// Intercept read-only open of OverviewData/<uuid>/<id>.rgb and return a memfd_create fd.
// Returns >= 0 fd if handled, -1 to fall through to normal open.
static int sc_rbb_open_rgb(const char *path, int flags) {
    if (!path || sc_rbb_in_hook || !sc_rbb_is_sched_other()) return -1;
    int is_write = (flags & (O_WRONLY | O_RDWR | O_CREAT | O_TRUNC)) != 0;
    int slot = -1;
    uint64_t gen = 0;
    int64_t id = 0;
    if (!sc_rbb_lookup_rgb_track(path, is_write, &slot, &gen, &id)) return -1;
    if (is_write) return -1;

    uint8_t blob[SC_RBB_BLOB_LEN];
    if (!sc_rbb_fetch_overview(slot, gen, id, blob)) return -1;

#ifdef __NR_memfd_create
    int fd = (int)syscall(__NR_memfd_create, "rb_rgb",
                          (flags & O_CLOEXEC) ? (unsigned int)MFD_CLOEXEC : 0u);
#else
    int fd = -1;
#endif
    if (fd < 0) return -1;
    size_t written = 0;
    while (written < SC_RBB_BLOB_LEN) {
        ssize_t w = write(fd, blob + written, SC_RBB_BLOB_LEN - written);
        if (w < 0) {
            if (errno == EINTR) continue;
            close(fd);
            return -1;
        }
        written += (size_t)w;
    }
    if (lseek(fd, 0, SEEK_SET) != 0) {
        close(fd);
        return -1;
    }
    return fd;
}

static int sc_rbb_access_rgb(const char *path, int mode) {
    if (!path || sc_rbb_in_hook || (mode & W_OK) || !sc_rbb_is_sched_other()) return 0;
    int slot = -1;
    uint64_t gen = 0;
    int64_t id = 0;
    return sc_rbb_lookup_rgb_track(path, 0, &slot, &gen, &id);
}

#ifdef STATX_BASIC_STATS
static int sc_rbb_statx_rgb(const char *path, struct statx *buf) {
    if (!path || !buf || sc_rbb_in_hook || !sc_rbb_is_sched_other()) return 0;
    int slot = -1;
    uint64_t gen = 0;
    int64_t id = 0;
    if (!sc_rbb_lookup_rgb_track(path, 0, &slot, &gen, &id)) return 0;
    memset(buf, 0, sizeof *buf);
    buf->stx_mask = STATX_TYPE | STATX_MODE | STATX_NLINK | STATX_SIZE | STATX_INO | STATX_BLOCKS;
    buf->stx_blksize = 4096;
    buf->stx_nlink = 1;
    buf->stx_mode = (uint16_t)(S_IFREG | 0444);
    buf->stx_ino = (uint64_t)id;
    buf->stx_size = SC_RBB_BLOB_LEN;
    buf->stx_blocks = (SC_RBB_BLOB_LEN + 511) / 512;
    return 1;
}
#endif

static int sc_rbb_stat_rgb(const char *path, struct stat *buf) {
    if (!path || !buf || sc_rbb_in_hook || !sc_rbb_is_sched_other()) return 0;
    int slot = -1;
    uint64_t gen = 0;
    int64_t id = 0;
    if (!sc_rbb_lookup_rgb_track(path, 0, &slot, &gen, &id)) return 0;
    memset(buf, 0, sizeof *buf);
    buf->st_mode = S_IFREG | 0444;
    buf->st_nlink = 1;
    buf->st_ino = (ino_t)id;
    buf->st_size = SC_RBB_BLOB_LEN;
    buf->st_blksize = 4096;
    buf->st_blocks = (SC_RBB_BLOB_LEN + 511) / 512;
    return 1;
}

// ============================================================================
// SQLite browse column synthesis (A1: isAnalyzed=1 & overviewWaveFormData)
// ============================================================================

struct sc_rbb_sqlite_api {
    const void *(*col_blob)(void *stmt, int col);
    int (*col_bytes)(void *stmt, int col);
    int (*col_int)(void *stmt, int col);
    long long (*col_int64)(void *stmt, int col);
    int (*col_type)(void *stmt, int col);
    int (*col_count)(void *stmt);
    const char *(*col_name)(void *stmt, int col);
    void *(*db_handle)(void *stmt);
    const char *(*sql)(void *stmt);
    const char *(*db_filename)(void *db, const char *name);
};

static struct sc_rbb_sqlite_api sc_rbb_sq;

struct sc_rbb_stmt_meta {
    const void *stmt;
    const char *sql_ptr;
    int col_count;
    int relevant;
    int drive_slot;
    uint64_t drive_gen;
    int col_id;
    int col_analyzed;
    int col_overview;
    int64_t inline_id;
};

static __thread struct sc_rbb_stmt_meta sc_rbb_stmts[SC_RBB_STMT_CACHE_CAP];
static __thread unsigned sc_rbb_stmt_next;
static __thread uint8_t sc_rbb_tls_blobs[SC_RBB_TLS_BLOB_SLOTS][SC_RBB_BLOB_LEN];
static __thread struct {
    const void *stmt;
    uint64_t drive_gen;
    int64_t track_id;
    unsigned slot;
    int valid;
} sc_rbb_tls_cur;

static int sc_rbb_is_browse_thread(void) {
    if (!__atomic_load_n(&sc_rbb_active, __ATOMIC_RELAXED)) return 0;
    if (sc_rbb_in_hook) return 0;
    if (!sc_rbb_is_sched_other()) return 0;
    char comm[16];
    memset(comm, 0, sizeof comm);
    if (prctl(PR_GET_NAME, comm, 0, 0, 0) != 0) return 0;
    return strcmp(comm, "Thread (pooled)") == 0;
}

static int sc_rbb_sql_has_path_filter(const char *sql) {
    for (const char *p = sql; *p; p++) {
        if ((p[0] == 'p' || p[0] == 'P') && !strncasecmp(p, "path", 4)) {
            const char *q = p + 4;
            while (*q == ' ' || *q == '\t') q++;
            if (*q == '=') return 1;
        }
    }
    return 0;
}

static int64_t sc_rbb_sql_inline_id(const char *sql) {
    for (const char *p = sql; *p; p++) {
        if ((p[0] == 'w' || p[0] == 'W') && !strncasecmp(p, "where", 5)) {
            const char *q = p + 5;
            while (*q) {
                if ((!strncasecmp(q, "trackId", 7) && (q[7] == ' ' || q[7] == '=')) ||
                    (!strncasecmp(q, "id", 2) && (q == p + 5 || q[-1] == ' ' || q[-1] == '.' || q[-1] == '(') &&
                     (q[2] == ' ' || q[2] == '='))) {
                    const char *eq = strchr(q, '=');
                    if (!eq) break;
                    eq++;
                    while (*eq == ' ' || *eq == '\t') eq++;
                    if (*eq >= '1' && *eq <= '9') {
                        char *ep = NULL;
                        long long id = strtoll(eq, &ep, 10);
                        if (id > 0 && ep && (*ep == '\0' || *ep == ' ' || *ep == ';' || *ep == ')'))
                            return (int64_t)id;
                    }
                }
                q++;
            }
            break;
        }
    }
    return 0;
}

static const struct sc_rbb_stmt_meta *sc_rbb_inspect_stmt(void *stmt) {
    if (!stmt || !sc_rbb_sq.sql || !sc_rbb_sq.col_count || !sc_rbb_sq.col_name ||
        !sc_rbb_sq.db_handle || !sc_rbb_sq.db_filename)
        return NULL;
    const char *sql = sc_rbb_sq.sql(stmt);
    int n = sc_rbb_sq.col_count(stmt);
    for (int i = 0; i < SC_RBB_STMT_CACHE_CAP; i++) {
        if (sc_rbb_stmts[i].stmt == stmt && sc_rbb_stmts[i].sql_ptr == sql &&
            sc_rbb_stmts[i].col_count == n) {
            return sc_rbb_stmts[i].relevant ? &sc_rbb_stmts[i] : NULL;
        }
    }

    struct sc_rbb_stmt_meta m;
    memset(&m, 0, sizeof m);
    m.stmt = stmt;
    m.sql_ptr = sql;
    m.col_count = n;
    m.drive_slot = -1;
    m.col_id = -1;
    m.col_analyzed = -1;
    m.col_overview = -1;

    if (sql && n > 0 && !strncasecmp(sql, "SELECT ", 7) && !sc_rbb_sql_has_path_filter(sql)) {
        void *db = sc_rbb_sq.db_handle(stmt);
        const char *fname = db ? sc_rbb_sq.db_filename(db, "main") : NULL;
        char mount[PATH_MAX];
        const char *rel = NULL;
        if (fname && sc_rbb_parse_mount(fname, mount, &rel) &&
            !strcmp(rel, "Engine Library/Database2/m.db")) {
            int slot = sc_rbb_ensure_drive(mount);
            if (slot >= 0) {
                for (int i = 0; i < n; i++) {
                    const char *cn = sc_rbb_sq.col_name(stmt, i);
                    if (!cn) continue;
                    if (m.col_id < 0 && (!strcasecmp(cn, "id") || !strcasecmp(cn, "trackId")))
                        m.col_id = i;
                    else if (m.col_analyzed < 0 && !strcasecmp(cn, "isAnalyzed"))
                        m.col_analyzed = i;
                    else if (m.col_overview < 0 && !strcasecmp(cn, "overviewWaveFormData"))
                        m.col_overview = i;
                }
                if (m.col_analyzed >= 0 || m.col_overview >= 0) {
                    if (m.col_id < 0) m.inline_id = sc_rbb_sql_inline_id(sql);
                    if (m.col_id >= 0 || m.inline_id > 0) {
                        pthread_mutex_lock(&sc_rbb_mu);
                        if (sc_rbb_tbl[slot].used && sc_rbb_tbl[slot].has_index) {
                            m.drive_slot = slot;
                            m.drive_gen = sc_rbb_tbl[slot].gen;
                            m.relevant = 1;
                        }
                        pthread_mutex_unlock(&sc_rbb_mu);
                    }
                }
            }
        }
    }

    unsigned idx = sc_rbb_stmt_next++ % SC_RBB_STMT_CACHE_CAP;
    sc_rbb_stmts[idx] = m;
    return sc_rbb_stmts[idx].relevant ? &sc_rbb_stmts[idx] : NULL;
}

static int64_t sc_rbb_stmt_track_id(void *stmt, const struct sc_rbb_stmt_meta *m) {
    if (m->col_id >= 0) return (int64_t)sc_rbb_sq.col_int64(stmt, m->col_id);
    return m->inline_id;
}

static int sc_rbb_drive_has_track(int slot, uint64_t gen, int64_t tid) {
    if (slot < 0 || slot >= SC_RBB_MAX_DRIVES || tid <= 0) return 0;
    pthread_mutex_lock(&sc_rbb_mu);
    struct sc_rbb_drive *d = &sc_rbb_tbl[slot];
    int ok = d->used && d->has_index && d->gen == gen && sc_rbb_find_track_locked(d, tid) != NULL;
    pthread_mutex_unlock(&sc_rbb_mu);
    return ok;
}

static const uint8_t *sc_rbb_get_tls_overview(void *stmt, const struct sc_rbb_stmt_meta *m, int64_t tid) {
    if (sc_rbb_tls_cur.valid && sc_rbb_tls_cur.stmt == stmt &&
        sc_rbb_tls_cur.drive_gen == m->drive_gen && sc_rbb_tls_cur.track_id == tid) {
        return sc_rbb_tls_blobs[sc_rbb_tls_cur.slot];
    }
    unsigned slot = (sc_rbb_tls_cur.slot + 1u) % SC_RBB_TLS_BLOB_SLOTS;
    if (!sc_rbb_fetch_overview(m->drive_slot, m->drive_gen, tid, sc_rbb_tls_blobs[slot]))
        return NULL;
    sc_rbb_tls_cur.stmt = stmt;
    sc_rbb_tls_cur.drive_gen = m->drive_gen;
    sc_rbb_tls_cur.track_id = tid;
    sc_rbb_tls_cur.slot = slot;
    sc_rbb_tls_cur.valid = 1;
    return sc_rbb_tls_blobs[slot];
}

__attribute__((unused)) static int sc_rbb_column_int_hook(void *stmt, int col) {
    int val = sc_rbb_sq.col_int(stmt, col);
    if (val != 0 || !sc_rbb_is_browse_thread()) return val;
    sc_rbb_in_hook++;
    const struct sc_rbb_stmt_meta *m = sc_rbb_inspect_stmt(stmt);
    if (m && col == m->col_analyzed) {
        int64_t tid = sc_rbb_stmt_track_id(stmt, m);
        if (sc_rbb_drive_has_track(m->drive_slot, m->drive_gen, tid)) val = 1;
    }
    sc_rbb_in_hook--;
    return val;
}

__attribute__((unused)) static long long sc_rbb_column_int64_hook(void *stmt, int col) {
    long long val = sc_rbb_sq.col_int64(stmt, col);
    if (val != 0 || !sc_rbb_is_browse_thread()) return val;
    sc_rbb_in_hook++;
    const struct sc_rbb_stmt_meta *m = sc_rbb_inspect_stmt(stmt);
    if (m && col == m->col_analyzed) {
        int64_t tid = sc_rbb_stmt_track_id(stmt, m);
        if (sc_rbb_drive_has_track(m->drive_slot, m->drive_gen, tid)) val = 1;
    }
    sc_rbb_in_hook--;
    return val;
}

__attribute__((unused)) static int sc_rbb_column_type_hook(void *stmt, int col) {
    int ty = sc_rbb_sq.col_type(stmt, col);
    // SQLITE_NULL == 5, SQLITE_INTEGER == 1, SQLITE_BLOB == 4
    if (ty != 5 || !sc_rbb_is_browse_thread()) return ty;
    sc_rbb_in_hook++;
    const struct sc_rbb_stmt_meta *m = sc_rbb_inspect_stmt(stmt);
    if (m) {
        if (col == m->col_analyzed) {
            int64_t tid = sc_rbb_stmt_track_id(stmt, m);
            if (sc_rbb_drive_has_track(m->drive_slot, m->drive_gen, tid)) ty = 1;
        } else if (col == m->col_overview) {
            int64_t tid = sc_rbb_stmt_track_id(stmt, m);
            if (tid > 0 && sc_rbb_get_tls_overview(stmt, m, tid) != NULL) ty = 4;
        }
    }
    sc_rbb_in_hook--;
    return ty;
}

__attribute__((unused)) static int sc_rbb_column_bytes_hook(void *stmt, int col) {
    int n = sc_rbb_sq.col_bytes(stmt, col);
    if (n > 0 || !sc_rbb_is_browse_thread()) return n;
    sc_rbb_in_hook++;
    const struct sc_rbb_stmt_meta *m = sc_rbb_inspect_stmt(stmt);
    if (m && col == m->col_overview) {
        int64_t tid = sc_rbb_stmt_track_id(stmt, m);
        if (tid > 0 && sc_rbb_get_tls_overview(stmt, m, tid) != NULL) n = SC_RBB_BLOB_LEN;
    }
    sc_rbb_in_hook--;
    return n;
}

__attribute__((unused)) static const void *sc_rbb_column_blob_hook(void *stmt, int col) {
    const void *p = sc_rbb_sq.col_blob(stmt, col);
    if (p != NULL || !sc_rbb_is_browse_thread()) return p;
    sc_rbb_in_hook++;
    const struct sc_rbb_stmt_meta *m = sc_rbb_inspect_stmt(stmt);
    if (m && col == m->col_overview) {
        int64_t tid = sc_rbb_stmt_track_id(stmt, m);
        if (tid > 0) p = sc_rbb_get_tls_overview(stmt, m, tid);
    }
    sc_rbb_in_hook--;
    return p;
}

#endif
