#ifndef SC_REKORDBOX_ANALYSIS_H
#define SC_REKORDBOX_ANALYSIS_H

#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

// Engine 5.3.0 reads a streamed track's analysis with this statement, the id inline.
#define SC_RBX_ANALYSIS_SELECT "SELECT trackId, trackData, overviewWaveFormData, beatData, quickCues, loops, thirdPartySourceId, activeOnLoadLoops FROM PerformanceData WHERE trackId = "

// The track whose analysis sql reads (n bytes, or NUL-terminated if n < 0), or 0.
static long long sc_rbx_analysis_track(const char *sql, int n) {
    size_t len = sizeof SC_RBX_ANALYSIS_SELECT - 1;
    if (!sql || (n >= 0 && (size_t)n <= len) || strncmp(sql, SC_RBX_ANALYSIS_SELECT, len)) return 0;
    const char *p = sql + len, *end = n >= 0 ? sql + n : NULL;
    long long id = 0;
    while ((!end || p < end) && *p >= '0' && *p <= '9') {
        if (id > 100000000000LL) return 0;
        id = id * 10 + (*p++ - '0');
    }
    if (p == sql + len) return 0;
    while ((!end || p < end) && (*p == ' ' || *p == ';')) p++;
    return (end ? p == end : !*p) ? id : 0;
}

// The streaming service's track id that ends Engine's uri ("…/855"), or 0.
static long long sc_rbx_uri_id(const char *uri) {
    size_t n = uri ? strlen(uri) : 0, i = n;
    while (i > 0 && uri[i - 1] >= '0' && uri[i - 1] <= '9') i--;
    if (i == n || n - i > 12 || i == 0 || (uri[i - 1] != '/' && uri[i - 1] != ':')) return 0;
    return strtoll(uri + i, NULL, 10);
}

// The UPDATE that gives the empty PerformanceData row of track the analysis in b, the file
// stream-sync writes: "RBXA1", then trackData, overviewWaveFormData, beatData, quickCues
// and loops, each a little-endian uint32 length and its bytes. NULL if b is not one.
static char *sc_rbx_analysis_sql(const unsigned char *b, size_t n, long long track) {
    static const char *const columns[] = {"trackData", "overviewWaveFormData", "beatData", "quickCues", "loops"};
    size_t at = 5, len[5], start[5], total = 160;
    if (n < 5 || memcmp(b, "RBXA1", 5)) return NULL;
    for (int i = 0; i < 5; i++) {
        if (n - at < 4) return NULL;
        len[i] = (size_t)b[at] | (size_t)b[at + 1] << 8 | (size_t)b[at + 2] << 16 | (size_t)b[at + 3] << 24;
        start[i] = at + 4;
        if (len[i] > n - start[i]) return NULL;
        at = start[i] + len[i];
        total += strlen(columns[i]) + 8 + 2 * len[i];
    }
    if (at != n || !len[0] || !len[2]) return NULL;
    char *sql = malloc(total), *p = sql;
    if (!sql) return NULL;
    p += sprintf(p, "UPDATE PerformanceData SET ");
    for (int i = 0; i < 5; i++) {
        p += sprintf(p, "%s%s = ", i ? ", " : "", columns[i]);
        if (!len[i]) { p += sprintf(p, "NULL"); continue; }
        *p++ = 'X'; *p++ = '\'';
        for (size_t k = 0; k < len[i]; k++) p += sprintf(p, "%02x", b[start[i] + k]);
        *p++ = '\'';
    }
    sprintf(p, " WHERE trackId = %lld AND trackData IS NULL", track);
    return sql;
}

// Where stream-sync leaves a Pro DJ Link track's analysis, and ".loading", touched when a
// deck asks it for such a track. Variables for the tests.
static const char *sc_rbx_dir = "/run/enginex-rbx-analysis";
static const char *sc_rbx_drives = "/media/";
static const long sc_rbx_window = 120; // seconds a load request opens the hook for

struct sc_rbx_sqlite {
    int (*exec)(void *, const char *, int (*)(void *, int, char **, char **), void *, char **);
    const char *(*filename)(void *, const char *);
};

struct sc_rbx_row {
    char uri[256];
    int ours;
};

static int sc_rbx_row(void *arg, int n, char **value, char **name) {
    struct sc_rbx_row *row = arg;
    (void)name;
    if (n == 2 && value[0] && value[1] && !strncmp(value[1], "rekordbox [", 11)) {
        snprintf(row->uri, sizeof row->uri, "%s", value[0]);
        row->ours = 1;
    }
    return 0;
}

// Before Engine prepares sql on db: when it is Engine's read of a track's analysis, the
// track is a Pro DJ Link track a deck just asked stream-sync for, and its row is empty,
// fill the row from the file stream-sync left. Anything else is Engine's own business:
// with no load request in the window, or on a drive's database, not even a read is added.
// Returns what it did, or NULL when it did nothing; *track and *id say for what.
static const char *sc_rbx_fill(const struct sc_rbx_sqlite *q, void *db, const char *sql, int n, long long *track, long long *id) {
    char path[160], query[192];
    struct stat st;
    if (!(*track = sc_rbx_analysis_track(sql, n))) return NULL;
    snprintf(path, sizeof path, "%s/.loading", sc_rbx_dir);
    if (stat(path, &st) || time(NULL) - st.st_mtime > sc_rbx_window) return NULL;
    const char *main = q->filename(db, "main");
    if (!main || !*main || !strncmp(main, sc_rbx_drives, strlen(sc_rbx_drives))) return NULL;
    struct sc_rbx_row row = {.ours = 0};
    snprintf(query, sizeof query, "SELECT t.uri, t.label FROM Track t JOIN PerformanceData p ON p.trackId = t.id WHERE t.id = %lld AND p.trackData IS NULL", *track);
    if (q->exec(db, query, sc_rbx_row, &row, NULL) || !row.ours) return NULL;
    *id = sc_rbx_uri_id(row.uri);
    snprintf(path, sizeof path, "%s/%lld.bin", sc_rbx_dir, *id);
    int fd = *id ? open(path, O_RDONLY | O_CLOEXEC) : -1;
    if (fd < 0) return "no analysis yet";
    const char *result = "unreadable";
    if (!fstat(fd, &st) && st.st_size > 0 && st.st_size < (8 << 20)) {
        unsigned char *b = malloc((size_t)st.st_size);
        char *update = b && read(fd, b, (size_t)st.st_size) == st.st_size ? sc_rbx_analysis_sql(b, (size_t)st.st_size, *track) : NULL;
        if (update) result = q->exec(db, update, NULL, NULL, NULL) ? "update failed" : "filled";
        free(update);
        free(b);
    }
    close(fd);
    return result;
}

#endif
