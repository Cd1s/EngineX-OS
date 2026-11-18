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

// Whether sql is Engine's read of a track's analysis with the id bound ("… = ?"): Engine
// builds it with a placeholder and binds the id at each read (morning test 2026-11-17:
// twelve reads of the loaded row, and not one with the id in the prepared text).
static int sc_rbx_analysis_bound(const char *sql) {
    size_t len = sizeof SC_RBX_ANALYSIS_SELECT - 1;
    if (!sql || strncmp(sql, SC_RBX_ANALYSIS_SELECT, len) || sql[len] != '?') return 0;
    for (sql += len + 1; *sql == ' ' || *sql == ';'; sql++) {}
    return !*sql;
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
// and loops, each a little-endian uint32 length and its bytes, then the
// encoder's analysed flag (omitted by older builds). NULL if b is not one.
static char *sc_rbx_analysis_sql(const unsigned char *b, size_t n, long long track) {
    static const char *const columns[] = {"trackData", "overviewWaveFormData", "beatData", "quickCues", "loops"};
    size_t at = 5, len[5], start[5], total = 256;
    if (n < 5 || memcmp(b, "RBXA1", 5)) return NULL;
    for (int i = 0; i < 5; i++) {
        if (n - at < 4) return NULL;
        len[i] = (size_t)b[at] | (size_t)b[at + 1] << 8 | (size_t)b[at + 2] << 16 | (size_t)b[at + 3] << 24;
        start[i] = at + 4;
        if (len[i] > n - start[i]) return NULL;
        at = start[i] + len[i];
        total += strlen(columns[i]) + 8 + 2 * len[i];
    }
    int analysed = 1;
    if (n - at == 1 && b[at] <= 1) analysed = b[at++];
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
    // Match rb-bridge: the incoming grid is a rekordbox import, even though the
    // audio is streamed. Leave the user's beat-grid lock as Engine created it.
    sprintf(p, ", thirdPartySourceId = 1 WHERE trackId = %lld AND trackData IS NULL; UPDATE Track SET isAnalyzed = %d WHERE id = %lld AND changes() > 0", track, analysed, track);
    return sql;
}

// Where stream-sync leaves a Pro DJ Link track's analysis, and ".loading", touched when a
// deck asks it for such a track. Variables for the tests.
static const char *sc_rbx_dir = "/run/enginex-rbx-analysis-v3";
static const char *sc_rbx_drives = "/media/";
static const long sc_rbx_window = 120; // seconds a load request opens the hook for
#define SC_RBX_DOWNLOADS "/data/downloads/TIDAL/"
#define SC_RBX_DOWNLOADS_REL "../../../../downloads/TIDAL/"

// Engine uses the same 40 hex digits for the temporary and finished downloads.
static const char *sc_rbx_download_key(const char *path, const char *prefix) {
    const size_t n = strlen(prefix);
    if (!path || strncmp(path, prefix, n)) return NULL;
    const char *key = path + n;
    if (strspn(key, "0123456789abcdef") != 40) return NULL;
    const char *ext = key + 40;
    return !strcmp(ext, ".temp") || !strcmp(ext, ".flac") || !strcmp(ext, ".mp3") ||
           !strcmp(ext, ".m4a") || !strcmp(ext, ".aac") || !strcmp(ext, ".mp4") ? key : NULL;
}

static int sc_rbx_replay_path(const char *path, char *perf, size_t cap) {
    const char *key = sc_rbx_download_key(path, SC_RBX_DOWNLOADS);
    if (!key) return 0;
    int n = snprintf(perf, cap, "%s/%.40s.perf", sc_rbx_dir, key);
    return n > 0 && (size_t)n < cap && !access(perf, R_OK);
}

static void sc_rbx_link_replay(const char *key, const char *payload) {
    if (!key) return;
    char source[192], target[160];
    snprintf(source, sizeof source, "%s.perf", payload);
    snprintf(target, sizeof target, "%s/%.40s.perf", sc_rbx_dir, key);
    // The same download key always denotes the same Engine streaming track.
    (void)link(source, target);
}

struct sc_rbx_sqlite {
    int (*exec)(void *, const char *, int (*)(void *, int, char **, char **), void *, char **);
    const char *(*filename)(void *, const char *);
};

struct sc_rbx_row {
    char uri[256];
    char download[41];
    int ours;
};

static int sc_rbx_row(void *arg, int n, char **value, char **name) {
    struct sc_rbx_row *row = arg;
    (void)name;
    // Ours: a row of stream-sync's gateway (Engine's TIDAL client) that the rekordbox source
    // labelled. An Engine Link or Remote Library track has no such uri.
    if (n == 3 && value[0] && value[1] && !strncmp(value[0], "streaming://TIDAL/Track/", 24) && !strncmp(value[1], "rekordbox [", 11)) {
        snprintf(row->uri, sizeof row->uri, "%s", value[0]);
        const char *key = sc_rbx_download_key(value[2], SC_RBX_DOWNLOADS_REL);
        if (key) snprintf(row->download, sizeof row->download, "%.40s", key);
        row->ours = 1;
    }
    return 0;
}

static int sc_rbx_verbose; // with the trace flag: say why a read was left alone

// Before Engine reads track's analysis on db: when the track is a Pro DJ Link track a deck
// just asked stream-sync for, and its row is empty, fill the row from the file stream-sync
// left. Anything else is Engine's own business: with no load request in the window, or on a
// drive's database, not even a read is added. Returns what it did, or NULL when it did
// nothing; *id says the streaming id.
static const char *sc_rbx_fill_track(const struct sc_rbx_sqlite *q, void *db, long long track, long long *id) {
    char path[160], query[192];
    struct stat st;
    snprintf(path, sizeof path, "%s/.loading", sc_rbx_dir);
    if (track <= 0 || stat(path, &st) || time(NULL) - st.st_mtime > sc_rbx_window) return NULL;
    // Engine's own connection has its library attached to a main database without a file.
    const char *main = q->filename(db, "main");
    if (main && !strncmp(main, sc_rbx_drives, strlen(sc_rbx_drives))) return sc_rbx_verbose ? "skipped (a drive's database)" : NULL;
    struct sc_rbx_row row = {.ours = 0};
    snprintf(query, sizeof query, "SELECT t.uri, t.label, t.path FROM Track t JOIN PerformanceData p ON p.trackId = t.id WHERE t.id = %lld AND p.trackData IS NULL", track);
    if (q->exec(db, query, sc_rbx_row, &row, NULL) || !row.ours) return sc_rbx_verbose ? "skipped (not an empty rekordbox row)" : NULL;
    *id = sc_rbx_uri_id(row.uri);
    snprintf(path, sizeof path, "%s/%lld.bin", sc_rbx_dir, *id);
    int fd = *id ? open(path, O_RDONLY | O_CLOEXEC) : -1;
    if (fd < 0) return "no analysis yet";
    const char *result = "unreadable";
    if (!fstat(fd, &st) && st.st_size > 0 && st.st_size < (8 << 20)) {
        unsigned char *b = malloc((size_t)st.st_size);
        char *update = b && read(fd, b, (size_t)st.st_size) == st.st_size ? sc_rbx_analysis_sql(b, (size_t)st.st_size, track) : NULL;
        if (update) {
            int error = q->exec(db, update, NULL, NULL, NULL);
            result = error ? "update failed" : "filled";
            if (!error && row.download[0]) sc_rbx_link_replay(row.download, path);
        }
        free(update);
        free(b);
    }
    close(fd);
    return result;
}

// Before Engine prepares sql on db: the same, when the id is in the text.
static const char *sc_rbx_fill(const struct sc_rbx_sqlite *q, void *db, const char *sql, int n, long long *track, long long *id) {
    if (!(*track = sc_rbx_analysis_track(sql, n))) return NULL;
    return sc_rbx_fill_track(q, db, *track, id);
}

#endif
