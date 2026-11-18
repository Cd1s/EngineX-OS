#ifndef SC_REKORDBOX_ANALYSIS_H
#define SC_REKORDBOX_ANALYSIS_H

#include <fcntl.h>
#include <limits.h>
#include <pthread.h>
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
    return !strcmp(ext, ".temp") || !strcmp(ext, ".flac") || !strcmp(ext, ".mp3") || !strcmp(ext, ".wav") ||
           !strcmp(ext, ".aif") || !strcmp(ext, ".aiff") || !strcmp(ext, ".aifc") ||
           !strcmp(ext, ".m4a") || !strcmp(ext, ".aac") || !strcmp(ext, ".mp4") ? key : NULL;
}

static int sc_rbx_replay_path(const char *path, char *perf, size_t cap) {
    const char *key = sc_rbx_download_key(path, SC_RBX_DOWNLOADS);
    if (!key) return 0;
    int n = snprintf(perf, cap, "%s/%.40s.perf", sc_rbx_dir, key);
    return n > 0 && (size_t)n < cap && !access(perf, R_OK);
}

// A named Engine temporary copy can still identify an import when no download
// alias was installed. The small index is written by stream-sync before load.
static inline int sc_rbx_replay_named(const char *path, const char *prefix, char *perf, size_t cap) {
    size_t prefix_len = strlen(prefix);
    if (strncmp(path, prefix, prefix_len) || strchr(path + prefix_len, '/')) return 0;
    const char *name = strchr(path + prefix_len, '_');
    struct stat audio, entry;
    if (!name || !*++name || !strcmp(name, ".") || !strcmp(name, "..") ||
        stat(path, &audio) || !S_ISREG(audio.st_mode) || audio.st_size <= 0) return 0;
    char key[512];
    int n = snprintf(key, sizeof key, "%s/names/%lld/%s", sc_rbx_dir, (long long)audio.st_size, name);
    if (n < 0 || (size_t)n >= sizeof key) return 0;
    int fd = open(key, O_RDONLY | O_CLOEXEC | O_NOFOLLOW | O_NONBLOCK);
    if (fd < 0) return 0;
    char id[14];
    ssize_t bytes = !fstat(fd, &entry) && S_ISREG(entry.st_mode) && time(NULL) - entry.st_mtime <= sc_rbx_window
        ? read(fd, id, sizeof id) : -1;
    close(fd);
    if (bytes < 2 || bytes >= (ssize_t)sizeof id || id[bytes - 1] != '\n' || id[0] == '0') return 0;
    id[bytes - 1] = '\0';
    if (strspn(id, "0123456789") != (size_t)bytes - 1) return 0;
    n = snprintf(perf, cap, "%s/%s.bin.perf", sc_rbx_dir, id);
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
    int ours, imported;
};

// Engine can rewrite the Track presentation fields after the first streamed load.
// Keep the source identity we already proved for this library row so a later
// PerformanceData read can still refresh a newer .bin through Engine's connection.
#define SC_RBX_TRACK_MAP_CAP 256
struct sc_rbx_track_map {
    long long track;
    long long id;
    char download[41];
    unsigned long long age;
};
static struct sc_rbx_track_map sc_rbx_track_maps[SC_RBX_TRACK_MAP_CAP];
static unsigned sc_rbx_track_map_next;
static unsigned long long sc_rbx_track_map_clock;
static int sc_rbx_track_map_loaded;
static pthread_mutex_t sc_rbx_track_map_mu = PTHREAD_MUTEX_INITIALIZER;

static void sc_rbx_track_map_path(char path[PATH_MAX]) {
    snprintf(path, PATH_MAX, "%s/rowmap", sc_rbx_dir);
}

static int sc_rbx_write_all(int fd, const char *data, size_t size) {
    size_t at = 0;
    while (at < size) {
        ssize_t wrote = write(fd, data + at, size - at);
        if (wrote <= 0) return 0;
        at += (size_t)wrote;
    }
    return 1;
}

// Called with sc_rbx_track_map_mu held. The map is short-lived (/run), so a
// complete replacement keeps it bounded even after hundreds of track loads.
static void sc_rbx_compact_track_map_locked(void) {
    char path[PATH_MAX], temp[PATH_MAX], line[128];
    unsigned order[SC_RBX_TRACK_MAP_CAP];
    for (unsigned i = 0; i < sc_rbx_track_map_next; ++i) {
        order[i] = i;
        for (unsigned j = i; j && sc_rbx_track_maps[order[j]].age < sc_rbx_track_maps[order[j - 1]].age; --j) {
            unsigned swap = order[j];
            order[j] = order[j - 1];
            order[j - 1] = swap;
        }
    }
    sc_rbx_track_map_path(path);
    int n = snprintf(temp, sizeof temp, "%s.tmp", path);
    if (n <= 0 || (size_t)n >= sizeof temp) return;
    int fd = open(temp, O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC | O_NOFOLLOW, 0600);
    if (fd < 0) return;
    int ok = 1;
    for (unsigned i = 0; i < sc_rbx_track_map_next && ok; ++i) {
        const struct sc_rbx_track_map *slot = &sc_rbx_track_maps[order[i]];
        n = snprintf(line, sizeof line, "%lld %lld%s%s\n", slot->track, slot->id,
                     slot->download[0] ? " " : "", slot->download);
        ok = n > 0 && (size_t)n < sizeof line && sc_rbx_write_all(fd, line, (size_t)n);
    }
    if (close(fd) || !ok || rename(temp, path)) unlink(temp);
}

static const char *sc_rbx_map_space(const char *p) {
    while (*p == ' ' || *p == '\t') p++;
    return p;
}

static int sc_rbx_parse_track_map(const char *line, long long *track, long long *id, char download[41]) {
    char *end;
    const char *p = sc_rbx_map_space(line);
    long long parsed_track = strtoll(p, &end, 10);
    if (end == p || parsed_track <= 0) return 0;
    p = sc_rbx_map_space(end);
    long long parsed_id = strtoll(p, &end, 10);
    if (end == p || parsed_id <= 0) return 0;
    p = sc_rbx_map_space(end);
    download[0] = 0;
    if (*p == 0 || *p == '\n') goto done;
    size_t n = 0;
    while ((p[n] >= '0' && p[n] <= '9') || (p[n] >= 'a' && p[n] <= 'f')) n++;
    if (n != 40) return 0;
    memcpy(download, p, n);
    download[n] = 0;
    p = sc_rbx_map_space(p + n);
    if (*p != 0 && *p != '\n') return 0;
done:
    *track = parsed_track;
    *id = parsed_id;
    return 1;
}

static void sc_rbx_load_track_map(void) {
    pthread_mutex_lock(&sc_rbx_track_map_mu);
    if (sc_rbx_track_map_loaded) { pthread_mutex_unlock(&sc_rbx_track_map_mu); return; }
    sc_rbx_track_map_loaded = 1;
    char path[PATH_MAX], line[128], download[41];
    sc_rbx_track_map_path(path);
    int fd = open(path, O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
    if (fd < 0) { pthread_mutex_unlock(&sc_rbx_track_map_mu); return; }
    FILE *file = fdopen(fd, "r");
    if (!file) { close(fd); pthread_mutex_unlock(&sc_rbx_track_map_mu); return; }
    while (sc_rbx_track_map_next < SC_RBX_TRACK_MAP_CAP && fgets(line, sizeof line, file)) {
        long long track = 0, id = 0;
        download[0] = 0;
        if (!sc_rbx_parse_track_map(line, &track, &id, download)) continue;
        int duplicate = 0;
        for (unsigned i = 0; i < sc_rbx_track_map_next; ++i)
            if (sc_rbx_track_maps[i].track == track) { duplicate = 1; break; }
        if (duplicate) continue;
        struct sc_rbx_track_map *slot = &sc_rbx_track_maps[sc_rbx_track_map_next++];
        slot->track = track;
        slot->id = id;
        slot->age = ++sc_rbx_track_map_clock;
        snprintf(slot->download, sizeof slot->download, "%s", download);
    }
    fclose(file);
    pthread_mutex_unlock(&sc_rbx_track_map_mu);
}

static void sc_rbx_remember_track(long long track, long long id, const char *download) {
    if (track <= 0 || id <= 0) return;
    pthread_mutex_lock(&sc_rbx_track_map_mu);
    for (unsigned i = 0; i < SC_RBX_TRACK_MAP_CAP; ++i) {
        if (sc_rbx_track_maps[i].track == track) {
            sc_rbx_track_maps[i].id = id;
            snprintf(sc_rbx_track_maps[i].download, sizeof sc_rbx_track_maps[i].download, "%s", download ? download : "");
            sc_rbx_track_maps[i].age = ++sc_rbx_track_map_clock;
            sc_rbx_compact_track_map_locked();
            pthread_mutex_unlock(&sc_rbx_track_map_mu);
            return;
        }
    }
    unsigned slot_index = sc_rbx_track_map_next;
    if (slot_index < SC_RBX_TRACK_MAP_CAP) {
        sc_rbx_track_map_next++;
    } else {
        slot_index = 0;
        for (unsigned i = 1; i < SC_RBX_TRACK_MAP_CAP; ++i)
            if (sc_rbx_track_maps[i].age < sc_rbx_track_maps[slot_index].age) slot_index = i;
    }
    struct sc_rbx_track_map *slot = &sc_rbx_track_maps[slot_index];
    slot->track = track;
    slot->id = id;
    snprintf(slot->download, sizeof slot->download, "%s", download ? download : "");
    slot->age = ++sc_rbx_track_map_clock;
    sc_rbx_compact_track_map_locked();
    pthread_mutex_unlock(&sc_rbx_track_map_mu);
}

static int sc_rbx_lookup_track(long long track, long long *id, char download[41]) {
    if (track <= 0) return 0;
    pthread_mutex_lock(&sc_rbx_track_map_mu);
    for (unsigned i = 0; i < SC_RBX_TRACK_MAP_CAP; ++i) {
        if (sc_rbx_track_maps[i].track != track) continue;
        if (id) *id = sc_rbx_track_maps[i].id;
        if (download) snprintf(download, 41, "%s", sc_rbx_track_maps[i].download);
        sc_rbx_track_maps[i].age = ++sc_rbx_track_map_clock;
        pthread_mutex_unlock(&sc_rbx_track_map_mu);
        return 1;
    }
    pthread_mutex_unlock(&sc_rbx_track_map_mu);
    return 0;
}

static int sc_rbx_row(void *arg, int n, char **value, char **name) {
    struct sc_rbx_row *row = arg;
    (void)name;
    row->imported = n >= 4 && value[3] && !strcmp(value[3], "1");
    // Ours: a row of stream-sync's gateway (Engine's TIDAL client) that the rekordbox source
    // labelled. An Engine Link or Remote Library track has no such uri.
    if (n >= 3 && value[0] && value[1] && !strncmp(value[0], "streaming://TIDAL/Track/", 24) && !strncmp(value[1], "rekordbox [", 11)) {
        snprintf(row->uri, sizeof row->uri, "%s", value[0]);
        const char *key = sc_rbx_download_key(value[2], SC_RBX_DOWNLOADS_REL);
        if (key) snprintf(row->download, sizeof row->download, "%.40s", key);
        row->ours = 1;
    }
    return 0;
}

static int sc_rbx_verbose; // with the trace flag: say why a read was left alone
static const char *sc_rbx_history = "/data/enginex/rekordbox-imports";

static unsigned char *sc_rbx_read(const char *path, size_t *n) {
    int fd = open(path, O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
    if (fd < 0) return NULL;
    struct stat st;
    unsigned char *b = NULL;
    if (!fstat(fd, &st) && S_ISREG(st.st_mode) && st.st_size >= 0 && st.st_size < (8 << 20)) {
        *n = (size_t)st.st_size;
        b = malloc(*n + 1);
        if (b && read(fd, b, *n) != (ssize_t)*n) { free(b); b = NULL; }
    }
    close(fd);
    return b;
}

struct sc_rbx_blob { const unsigned char *b; size_t n; };

static int sc_rbx_edits(const unsigned char *b, size_t n, struct sc_rbx_blob edits[2]) {
    if (!b || n < 5 || memcmp(b, "RBXA1", 5)) return 0;
    size_t at = 5;
    for (int i = 0; i < 5; ++i) {
        if (n - at < 4) return 0;
        size_t len = (size_t)b[at] | (size_t)b[at + 1] << 8 | (size_t)b[at + 2] << 16 | (size_t)b[at + 3] << 24;
        at += 4;
        if (len > n - at || ((i == 0 || i == 2) && !len)) return 0;
        if (i == 2 || i == 3) edits[i - 2] = (struct sc_rbx_blob){b + at, len};
        at += len;
    }
    return at == n || (n - at == 1 && b[at] <= 1);
}

static void sc_rbx_edit_path(char *path, size_t cap, long long id, long long track, const char *col) {
    snprintf(path, cap, "%s/%lld-%lld-%s", sc_rbx_history, id, track, col);
}

static void sc_rbx_save_edit(const char *path, struct sc_rbx_blob blob) {
    if (mkdir(sc_rbx_history, 0755) && access(sc_rbx_history, W_OK)) return;
    char tmp[180];
    if (snprintf(tmp, sizeof tmp, "%s.XXXXXX", path) >= (int)sizeof tmp) return;
    int fd = mkstemp(tmp);
    if (fd < 0) return;
    ssize_t written = write(fd, blob.b, blob.n);
    if (close(fd) || written != (ssize_t)blob.n || rename(tmp, path)) unlink(tmp);
}

static char *sc_rbx_blob_sql(char *p, struct sc_rbx_blob blob) {
    if (!blob.n) return p + sprintf(p, "NULL");
    *p++ = 'X'; *p++ = '\'';
    for (size_t i = 0; i < blob.n; i++) p += sprintf(p, "%02x", blob.b[i]);
    *p++ = '\'';
    return p;
}

static int sc_rbx_changes(void *arg, int n, char **v, char **names) {
    (void)names;
    if (n == 1 && v[0]) *(int *)arg = !strcmp(v[0], "1");
    return 0;
}

// The comparisons are part of the UPDATE on Engine's own connection. A
// changed cue never prevents an untouched grid from receiving a source edit.
static int sc_rbx_update_edit(const struct sc_rbx_sqlite *q, void *db, long long track,
                              const char *col, struct sc_rbx_blob before, struct sc_rbx_blob after) {
    char *sql = malloc(256 + 2 * (before.n + after.n));
    if (!sql) return -1;
    char *p = sql + sprintf(sql, "UPDATE PerformanceData SET %s = ", col);
    p = sc_rbx_blob_sql(p, after);
    p += sprintf(p, " WHERE trackId = %lld AND thirdPartySourceId = 1 AND %s IS ", track, col);
    p = sc_rbx_blob_sql(p, before);
    strcpy(p, "; SELECT changes()");
    int changed = 0;
    int err = q->exec(db, sql, sc_rbx_changes, &changed, NULL);
    free(sql);
    return err ? -1 : changed;
}

static const char *sc_rbx_refresh_edits(const struct sc_rbx_sqlite *q, void *db, const char *payload,
                                        long long track, long long id, const unsigned char *b, size_t n, int first) {
    struct sc_rbx_blob current[2], legacy[2];
    if (!sc_rbx_edits(b, n, current)) return "unreadable";
    char path[192];
    snprintf(path, sizeof path, "%s.previous", payload);
    size_t old_n = 0;
    unsigned char *previous = first ? NULL : sc_rbx_read(path, &old_n);
    int has_legacy = sc_rbx_edits(previous, old_n, legacy);
    int changed = 0, conflict = 0;
    for (int i = 0; i < 2; i++) {
        const char *col = i ? "quickCues" : "beatData";
        sc_rbx_edit_path(path, sizeof path, id, track, col);
        if (first) {
            sc_rbx_save_edit(path, current[i]);
            continue;
        }
        size_t len = 0;
        unsigned char *saved = sc_rbx_read(path, &len);
        if (!saved && !has_legacy) continue; // No known import to compare against.
        struct sc_rbx_blob old = saved ? (struct sc_rbx_blob){saved, len} : legacy[i];
        if (old.n != current[i].n || memcmp(old.b, current[i].b, old.n)) {
            int rc = sc_rbx_update_edit(q, db, track, col, old, current[i]);
            if (rc == 1) {
                sc_rbx_save_edit(path, current[i]);
                changed = 1;
            } else if (!rc) {
                conflict = 1;
            }
        }
        free(saved);
    }
    free(previous);
    if (conflict) return changed ? "refreshed; kept deck edits" : "kept deck edits";
    return changed ? "refreshed" : NULL;
}

// Before Engine reads track's analysis on db: when the track is a Pro DJ Link track a deck
// just asked stream-sync for, and its row is empty, fill the row from the file stream-sync
// left. Anything else is Engine's own business: with no load request in the window, or on a
// drive's database, not even a read is added. Returns what it did, or NULL when it did
// nothing; *id says the streaming id.
static const char *sc_rbx_fill_track(const struct sc_rbx_sqlite *q, void *db, long long track, long long *id) {
    char path[160], query[320];
    struct stat st;
    snprintf(path, sizeof path, "%s/.loading", sc_rbx_dir);
    if (track <= 0 || stat(path, &st) || time(NULL) - st.st_mtime > sc_rbx_window) return NULL;
    // Engine's own connection has its library attached to a main database without a file.
    const char *main = q->filename(db, "main");
    if (main && !strncmp(main, sc_rbx_drives, strlen(sc_rbx_drives))) return sc_rbx_verbose ? "skipped (a drive's database)" : NULL;
    struct sc_rbx_row row = {.ours = 0};
    snprintf(query, sizeof query, "SELECT t.uri,t.label,t.path,CASE WHEN p.trackData IS NOT NULL THEN p.thirdPartySourceId END FROM Track t JOIN PerformanceData p ON p.trackId=t.id WHERE t.id=%lld AND (p.trackData IS NULL OR p.thirdPartySourceId=1)", track);
    if (q->exec(db, query, sc_rbx_row, &row, NULL)) return sc_rbx_verbose ? "skipped (not a rekordbox row)" : NULL;
    if (row.ours) {
        *id = sc_rbx_uri_id(row.uri);
        sc_rbx_remember_track(track, *id, row.download);
    } else if (row.imported && sc_rbx_lookup_track(track, id, NULL)) {
        // Engine has retained our imported row but no longer exposes the original
        // gateway URI/label in this reload snapshot. The prior proof is enough to
        // refresh the same source generation; unknown rows remain untouched.
    } else {
        return sc_rbx_verbose ? "skipped (not a rekordbox row)" : NULL;
    }
    snprintf(path, sizeof path, "%s/%lld.bin", sc_rbx_dir, *id);
    size_t size = 0;
    unsigned char *b = *id ? sc_rbx_read(path, &size) : NULL;
    if (!b) return "no analysis yet";
    const char *result;
    if (row.imported) {
        result = sc_rbx_refresh_edits(q, db, path, track, *id, b, size, 0);
    } else {
        char *update = sc_rbx_analysis_sql(b, size, track);
        result = "unreadable";
        if (update) {
            int error = q->exec(db, update, NULL, NULL, NULL);
            result = error ? "update failed" : "filled";
            if (!error) {
                sc_rbx_refresh_edits(q, db, path, track, *id, b, size, 1);
                if (row.download[0]) sc_rbx_link_replay(row.download, path);
            }
        }
        free(update);
    }
    free(b);
    return result;
}

// Before Engine prepares sql on db: the same, when the id is in the text.
static const char *sc_rbx_fill(const struct sc_rbx_sqlite *q, void *db, const char *sql, int n, long long *track, long long *id) {
    if (!(*track = sc_rbx_analysis_track(sql, n))) return NULL;
    return sc_rbx_fill_track(q, db, *track, id);
}

#endif
