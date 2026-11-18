#ifndef SC_SHADOW_ANALYSIS_H
#define SC_SHADOW_ANALYSIS_H

#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#define SC_SHADOW_SELECT "SELECT trackId, trackData, overviewWaveFormData, beatData, quickCues, loops, thirdPartySourceId, activeOnLoadLoops FROM PerformanceData WHERE trackId = "
#define SC_SHADOW_REL "../../../../enginex/shadow-v3/"
#define SC_SHADOW_MAX (8 << 20)

static const char *sc_shadow_library = "/data/AppDataUser/AIR Music Technology/Engine/Engine Library/Database2/m.db";
static const char *sc_shadow_root = "/data/enginex/shadow-v3";

struct sc_shadow_sqlite {
    int (*exec)(void *, const char *, int (*)(void *, int, char **, char **), void *, char **);
    const char *(*filename)(void *, const char *);
};

static int sc_shadow_analysis_bound(const char *sql) {
    const size_t n = sizeof SC_SHADOW_SELECT - 1;
    if (!sql || strncmp(sql, SC_SHADOW_SELECT, n) || sql[n] != '?') return 0;
    for (sql += n + 1; *sql == ' ' || *sql == ';'; sql++) {}
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
    char *sql = malloc(512 + 2 * (n + pathlen)), *p = sql;
    if (!sql) return NULL;
    p += sprintf(p, "UPDATE main.PerformanceData SET ");
    for (int i = 0; i < 5; i++) {
        p += sprintf(p, "%s%s = ", i ? ", " : "", columns[i]);
        if (!len[i]) { p += sprintf(p, "NULL"); continue; }
        *p++ = 'X'; *p++ = '\'';
        p = sc_shadow_hex(p, b + start[i], len[i]);
        *p++ = '\'';
    }
    p += sprintf(p, " WHERE trackId = %lld AND trackData IS NULL AND EXISTS "
                 "(SELECT 1 FROM main.Track WHERE id = %lld AND CAST(path AS BLOB) = X'", id, id);
    p = sc_shadow_hex(p, (const unsigned char *)path, pathlen);
    memcpy(p, "')", 3);
    return sql;
}

// A copy can have one extra Contents directory for the stock overview scale.
// Nothing from the DB may name a file outside the peer cache.
static const char *sc_shadow_relative(const char *path) {
    if (!path || strncmp(path, SC_SHADOW_REL "peer-", sizeof SC_SHADOW_REL "peer-" - 1)) return NULL;
    const char *rel = path + sizeof SC_SHADOW_REL - 1;
    size_t hash = strspn(rel + 5, "0123456789abcdef");
    if (!hash || hash > 16 || rel[5 + hash] != '/') return NULL;
    const char *name = rel + 6 + hash;
    if (!strncmp(name, "Contents/", 9)) name += 9;
    if (!*name || !strcmp(name, ".") || !strcmp(name, "..") || strchr(name, '/')) return NULL;
    return rel;
}

static int sc_shadow_copy_path(void *out, int n, char **values, char **names) {
    (void)names;
    if (n == 1 && sc_shadow_relative(values[0]) && strlen(values[0]) < 512)
        strcpy(out, values[0]);
    return 0;
}

static int sc_shadow_analysis_fill(const struct sc_shadow_sqlite *q, void *db, long long id) {
    if (!db || id <= 0) return 0;
    const char *main = q->filename(db, "main");
    if (!main || strcmp(main, sc_shadow_library) || access(sc_shadow_root, F_OK)) return 0;
    char path[512] = "", query[384];
    snprintf(query, sizeof query,
             "SELECT t.path FROM main.Track t JOIN main.PerformanceData p ON p.trackId=t.id "
             "WHERE t.id=%lld AND p.trackData IS NULL AND t.path GLOB '" SC_SHADOW_REL "peer-*'", id);
    if (q->exec(db, query, sc_shadow_copy_path, path, NULL) || !*path) return 0;
    char sidecar[640];
    if ((size_t)snprintf(sidecar, sizeof sidecar, "%s/%s.analysis", sc_shadow_root,
                         path + sizeof SC_SHADOW_REL - 1) >= sizeof sidecar) return 0;
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
        char *sql = at == size ? sc_shadow_analysis_sql(b, size, id, path) : NULL;
        if (sql) filled = !q->exec(db, sql, NULL, NULL, NULL);
        free(sql);
        free(b);
    }
    close(fd);
    return filled;
}

#endif
