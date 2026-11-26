#ifndef SC_SHADOW_ANALYSIS_H
#define SC_SHADOW_ANALYSIS_H

#include <fcntl.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#define SC_SHADOW_SELECT "SELECT trackId, trackData, overviewWaveFormData, beatData, quickCues, loops, thirdPartySourceId, activeOnLoadLoops FROM PerformanceData WHERE trackId = "
#define SC_SHADOW_TRACK_READ "SELECT t.id, t.playOrder, t.length, t.bpm, t.year, t.path, "
#define SC_SHADOW_INSERT "INSERT OR REPLACE INTO PerformanceData (trackId,trackData,overviewWaveFormData,beatData,quickCues,loops,thirdPartySourceId,activeOnLoadLoops) VALUES  ( ?, ?, ?, ?, ?, ?, ?, ?);"
#define SC_SHADOW_REL "../../../../enginex/shadow-v3/"
#define SC_SHADOW_MAX (8 << 20)

static const char *sc_shadow_library = "/data/AppDataUser/AIR Music Technology/Engine/Engine Library/Database2/m.db";
static const char *sc_shadow_root = "/data/enginex/shadow-v3";

struct sc_shadow_sqlite {
    int (*exec)(void *, const char *, int (*)(void *, int, char **, char **), void *, char **);
    const char *(*filename)(void *, const char *);
};

static int sc_shadow_analysis_bound(const char *sql) {
    // Engine has already copied isAnalyzed by the time it binds PerformanceData.
    if (!sql || strncmp(sql, SC_SHADOW_TRACK_READ, sizeof SC_SHADOW_TRACK_READ - 1)) return 0;
    const char *where = strstr(sql, " WHERE t.id = ?");
    if (!where || strchr(sql, '?') != where + sizeof " WHERE t.id = " - 1) return 0;
    for (sql = where + sizeof " WHERE t.id = ?" - 1; *sql == ' ' || *sql == ';'; sql++) {}
    return !*sql;
}

static char *sc_shadow_hex(char *dst, const unsigned char *src, size_t n) {
    static const char digits[] = "0123456789abcdef";
    for (size_t i = 0; i < n; i++) {
        *dst++ = digits[src[i] >> 4];
        *dst++ = digits[src[i] & 15];
    }
    return dst;
}

static char *sc_shadow_value(char *p, const unsigned char *b, size_t n) {
    if (!n) return p + sprintf(p, "NULL");
    *p++ = 'X'; *p++ = '\'';
    p = sc_shadow_hex(p, b, n);
    *p++ = '\'';
    return p;
}

// The rekordbox RBXA1 record, with all three analysis blobs required here.
static char *sc_shadow_analysis_sql(const unsigned char *b, size_t n, long long id, const char *path) {
    static const char *const columns[] = {"trackData", "overviewWaveFormData", "beatData", "quickCues", "loops"};
    size_t at = 5, len[5], start[5], pathlen = strlen(path);
    if (n < 5 || n > SC_SHADOW_MAX || memcmp(b, "RBXA1", 5)) return NULL;
    for (int i = 0; i < 5; i++) {
        if (n - at < 4) return NULL;
        len[i] = (size_t)b[at] | (size_t)b[at + 1] << 8 | (size_t)b[at + 2] << 16 | (size_t)b[at + 3] << 24;
        start[i] = at + 4;
        if (len[i] > n - start[i] || (i < 3 && len[i] <= 4)) return NULL;
        at = start[i] + len[i];
    }
    if (at != n) return NULL;
    char *sql = malloc(1024 + 4 * (n + pathlen)), *p = sql;
    if (!sql) return NULL;
    // The row is the source's analysis whenever the copy is read, not only the first time:
    // a copy is used again for the same track, and what its row held from an earlier load
    // (another build's grid, a grid since edited in rekordbox, an overview Engine never
    // finished) drew the shadow lane apart from the source's. Written only when it differs.
    p += sprintf(p, "UPDATE main.PerformanceData SET ");
    for (int i = 0; i < 5; i++) {
        p += sprintf(p, "%s%s = ", i ? ", " : "", columns[i]);
        p = sc_shadow_value(p, b + start[i], len[i]);
    }
    p += sprintf(p, " WHERE trackId = %lld AND (trackData IS NULL", id);
    for (int i = 0; i < 5; i++) {
        p += sprintf(p, " OR %s IS NOT ", columns[i]);
        p = sc_shadow_value(p, b + start[i], len[i]);
    }
    p += sprintf(p, ") AND EXISTS "
                 "(SELECT 1 FROM main.Track WHERE id = %lld AND CAST(path AS BLOB) = X'", id);
    p = sc_shadow_hex(p, (const unsigned char *)path, pathlen);
    // Engine starts OfflineAnalyzer if its Track row is still unanalysed,
    // even when the complete native PerformanceData is already present.
    p += sprintf(p, "'); UPDATE main.Track SET isAnalyzed = 1 WHERE id = %lld AND changes() > 0 "
                    "AND CAST(path AS BLOB) = X'", id);
    p = sc_shadow_hex(p, (const unsigned char *)path, pathlen);
    memcpy(p, "'; SELECT changes()", sizeof "'; SELECT changes()");
    return sql;
}

// A copy can have one extra Contents directory for the stock overview scale.
// Nothing from the DB may name a file outside the peer cache.
static int sc_shadow_peer_path(const char *rel) {
    if (strncmp(rel, "peer-", 5)) return 0;
    size_t hash = strspn(rel + 5, "0123456789abcdef");
    if (!hash || hash > 16 || rel[5 + hash] != '/') return 0;
    const char *name = rel + 6 + hash;
    if (!strncmp(name, "Contents/", 9)) name += 9;
    return *name && strcmp(name, ".") && strcmp(name, "..") && !strchr(name, '/');
}

static int sc_shadow_analysis_staged(const char *path) {
    const size_t n = strlen(sc_shadow_root);
    if (strncmp(path, sc_shadow_root, n) || path[n] != '/' || !sc_shadow_peer_path(path + n + 1)) return 0;
    char sidecar[640];
    if ((size_t)snprintf(sidecar, sizeof sidecar, "%s.analysis", path) >= sizeof sidecar) return 0;
    struct stat st;
    return !lstat(sidecar, &st) && S_ISREG(st.st_mode) && st.st_size > 0 && st.st_size <= SC_SHADOW_MAX;
}

struct sc_shadow_row_info {
    char path[512];
    int analyzed;
    int has_blobs;
};

static int sc_shadow_copy_info(void *out, int n, char **values, char **names) {
    (void)names;
    struct sc_shadow_row_info *info = out;
    if (n >= 1 && values[0] && !strncmp(values[0], SC_SHADOW_REL, sizeof SC_SHADOW_REL - 1) &&
        sc_shadow_peer_path(values[0] + sizeof SC_SHADOW_REL - 1) && strlen(values[0]) < sizeof info->path) {
        strcpy(info->path, values[0]);
        info->analyzed = n >= 2 && values[1] && values[1][0] != '0';
        info->has_blobs = n >= 3 && values[2] && values[2][0] != '0';
    }
    return 0;
}

static int sc_shadow_written(void *out, int n, char **values, char **names) {
    (void)names;
    if (n == 1 && values[0]) *(int *)out = values[0][0] != '0';
    return 0;
}

static uint64_t sc_shadow_fnv1a(const unsigned char *b, size_t n) {
    uint64_t h = 14695981039346656037ULL;
    for (size_t i = 0; i < n; i++) {
        h ^= b[i];
        h *= 1099511628211ULL;
    }
    return h;
}

// Per-process filled cache: once a track has been filled from a given sidecar
// digest and the DB row is still analysed with complete core blobs, skip repeat
// UPDATEs triggered ~0.5-1 s later when Engine saves decoder-trimmed trackData
// or synced cues via SC_SHADOW_INSERT / Track re-read. Rollbacks, wiped rows
// (isAnalyzed=0 or missing blobs), and updated sidecars (new hash) still fill.
#define SC_SHADOW_FILLED_CAP 16
struct sc_shadow_filled_entry {
    long long id;
    uint64_t hash;
    char db[512];
    char path[512];
};
static pthread_mutex_t sc_shadow_filled_mu = PTHREAD_MUTEX_INITIALIZER;
static struct sc_shadow_filled_entry sc_shadow_filled[SC_SHADOW_FILLED_CAP];
static unsigned sc_shadow_filled_next;

static int sc_shadow_filled_hit(long long id, const char *db, const char *path, uint64_t hash) {
    int hit = 0;
    pthread_mutex_lock(&sc_shadow_filled_mu);
    for (unsigned i = 0; i < SC_SHADOW_FILLED_CAP; i++) {
        if (sc_shadow_filled[i].id == id && sc_shadow_filled[i].hash == hash &&
            !strcmp(sc_shadow_filled[i].db, db) && !strcmp(sc_shadow_filled[i].path, path)) {
            hit = 1;
            break;
        }
    }
    pthread_mutex_unlock(&sc_shadow_filled_mu);
    return hit;
}

static void sc_shadow_filled_put(long long id, const char *db, const char *path, uint64_t hash) {
    if (strlen(db) >= sizeof sc_shadow_filled[0].db || strlen(path) >= sizeof sc_shadow_filled[0].path) return;
    pthread_mutex_lock(&sc_shadow_filled_mu);
    for (unsigned i = 0; i < SC_SHADOW_FILLED_CAP; i++) {
        if (sc_shadow_filled[i].id == id && !strcmp(sc_shadow_filled[i].db, db) && !strcmp(sc_shadow_filled[i].path, path)) {
            sc_shadow_filled[i].hash = hash;
            pthread_mutex_unlock(&sc_shadow_filled_mu);
            return;
        }
    }
    struct sc_shadow_filled_entry *e = &sc_shadow_filled[sc_shadow_filled_next++ % SC_SHADOW_FILLED_CAP];
    e->id = id;
    e->hash = hash;
    strcpy(e->db, db);
    strcpy(e->path, path);
    pthread_mutex_unlock(&sc_shadow_filled_mu);
}

// Whether the row was written.
static int sc_shadow_analysis_fill(const struct sc_shadow_sqlite *q, void *db, long long id) {
    if (!db || id <= 0) return 0;
    const char *main = q->filename(db, "main");
    if (!main || strcmp(main, sc_shadow_library) || access(sc_shadow_root, F_OK)) return 0;
    struct sc_shadow_row_info info = {"", 0, 0};
    char query[512];
    snprintf(query, sizeof query,
             "SELECT t.path, COALESCE(t.isAnalyzed,0), "
             "CASE WHEN length(p.trackData)>4 AND length(p.overviewWaveFormData)>4 AND length(p.beatData)>4 THEN 1 ELSE 0 END "
             "FROM main.Track t JOIN main.PerformanceData p ON p.trackId=t.id "
             "WHERE t.id=%lld AND t.path GLOB '" SC_SHADOW_REL "peer-*'", id);
    if (q->exec(db, query, sc_shadow_copy_info, &info, NULL) || !*info.path) return 0;
    char sidecar[640];
    if ((size_t)snprintf(sidecar, sizeof sidecar, "%s/%s.analysis", sc_shadow_root,
                         info.path + sizeof SC_SHADOW_REL - 1) >= sizeof sidecar) return 0;
    int fd = open(sidecar, O_RDONLY | O_CLOEXEC | O_NOFOLLOW | O_NONBLOCK);
    if (fd < 0) return 0;
    int filled = 0;
    struct stat st;
    if (!fstat(fd, &st) && S_ISREG(st.st_mode) && st.st_size > 0 && st.st_size <= SC_SHADOW_MAX) {
        size_t size = (size_t)st.st_size, at = 0;
        unsigned char *b = malloc(size);
        while (b && at < size) {
            ssize_t got = read(fd, b + at, size - at);
            if (got <= 0) break;
            at += (size_t)got;
        }
        if (at == size) {
            uint64_t hash = sc_shadow_fnv1a(b, size);
            if (!(info.analyzed && info.has_blobs && sc_shadow_filled_hit(id, main, info.path, hash))) {
                char *sql = sc_shadow_analysis_sql(b, size, id, info.path);
                if (sql) {
                    int rc = q->exec(db, sql, sc_shadow_written, &filled, NULL);
                    if (rc) filled = 0;
                    else if (filled || (info.analyzed && info.has_blobs)) sc_shadow_filled_put(id, main, info.path, hash);
                    free(sql);
                }
            }
        }
        free(b);
    }
    close(fd);
    return filled;
}

#endif
