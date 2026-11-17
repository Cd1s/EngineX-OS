#include <assert.h>
#include <sqlite3.h>
#include <string.h>
#include "rekordbox_analysis.h"

// Engine's connection, played by a real SQLite database: every statement the hook runs is counted.
static int statements;
static int exec_counted(void *db, const char *sql, int (*row)(void *, int, char **, char **), void *arg, char **err) {
    statements++;
    return sqlite3_exec(db, sql, row, arg, err);
}
static const char *db_main(void *db, const char *name) { return sqlite3_db_filename(db, name); }
static const struct sc_rbx_sqlite sqlite = {exec_counted, db_main};

static sqlite3 *library(const char *path, const char *label) {
    sqlite3 *db;
    unlink(path);
    assert(sqlite3_open(path, &db) == SQLITE_OK);
    char sql[512];
    snprintf(sql, sizeof sql, "CREATE TABLE Track (id INTEGER PRIMARY KEY, uri TEXT, label TEXT);"
             "CREATE TABLE PerformanceData (trackId INTEGER PRIMARY KEY, trackData BLOB, overviewWaveFormData BLOB, beatData BLOB, quickCues BLOB, loops BLOB);"
             "INSERT INTO Track VALUES (16449, 'streaming://TIDAL/Track/855', '%s'); INSERT INTO PerformanceData (trackId) VALUES (16449);", label);
    assert(sqlite3_exec(db, sql, NULL, NULL, NULL) == SQLITE_OK);
    return db;
}

static int filled(sqlite3 *db) {
    sqlite3_stmt *st;
    assert(sqlite3_prepare_v2(db, "SELECT hex(trackData) || '/' || hex(beatData) FROM PerformanceData WHERE trackId = 16449", -1, &st, NULL) == SQLITE_OK);
    assert(sqlite3_step(st) == SQLITE_ROW);
    int got = sqlite3_column_text(st, 0) && !strcmp((const char *)sqlite3_column_text(st, 0), "01AB/FF");
    sqlite3_finalize(st);
    return got;
}

// A load through Engine's own SQL: what the hook does before Engine prepares its read.
static const char *load(sqlite3 *db) {
    long long track = 0, id = 0;
    return sc_rbx_fill(&sqlite, db, SC_RBX_ANALYSIS_SELECT "16449", -1, &track, &id);
}

int main(void) {
    assert(sc_rbx_analysis_track(SC_RBX_ANALYSIS_SELECT "16449", -1) == 16449);
    assert(sc_rbx_analysis_track(SC_RBX_ANALYSIS_SELECT "16449;", -1) == 16449);
    const char *with_tail = SC_RBX_ANALYSIS_SELECT "77 junk";
    assert(sc_rbx_analysis_track(with_tail, (int)strlen(with_tail) - 5) == 77);
    const char *bad[] = {"", SC_RBX_ANALYSIS_SELECT, SC_RBX_ANALYSIS_SELECT "x", SC_RBX_ANALYSIS_SELECT "12 OR 1",
                         "SELECT trackId FROM PerformanceData WHERE trackId = 3", SC_RBX_ANALYSIS_SELECT "123456789012345"};
    for (unsigned i = 0; i < sizeof bad / sizeof *bad; i++) assert(!sc_rbx_analysis_track(bad[i], -1));

    assert(sc_rbx_uri_id("streaming://TIDAL/Track/855") == 855);
    assert(sc_rbx_uri_id("tidal:855") == 855);
    assert(!sc_rbx_uri_id("") && !sc_rbx_uri_id("855") && !sc_rbx_uri_id("streaming://TIDAL/Track/") && !sc_rbx_uri_id("x/a855"));

    const unsigned char file[] = "RBXA1" "\2\0\0\0" "\x01\xab" "\0\0\0\0" "\1\0\0\0" "\xff" "\0\0\0\0" "\0\0\0\0";
    char *sql = sc_rbx_analysis_sql(file, sizeof file - 1, 9);
    assert(sql && !strcmp(sql, "UPDATE PerformanceData SET trackData = X'01ab', overviewWaveFormData = NULL, beatData = X'ff', quickCues = NULL, loops = NULL WHERE trackId = 9 AND trackData IS NULL"));
    free(sql);
    assert(!sc_rbx_analysis_sql(file, sizeof file - 2, 9));                      // cut short
    assert(!sc_rbx_analysis_sql((const unsigned char *)"RBXA0", 5, 9));           // not ours
    const unsigned char huge[] = "RBXA1" "\xff\xff\xff\x7f";
    assert(!sc_rbx_analysis_sql(huge, sizeof huge - 1, 9));                       // length past the end
    const unsigned char no_grid[] = "RBXA1" "\1\0\0\0" "\x01" "\0\0\0\0" "\0\0\0\0" "\0\0\0\0" "\0\0\0\0";
    assert(!sc_rbx_analysis_sql(no_grid, sizeof no_grid - 1, 9));                 // nothing to skip analysis for

    char dir[] = "/tmp/rbx-analysis-XXXXXX", drives[64], path[160];
    assert(mkdtemp(dir));
    sc_rbx_dir = dir;
    snprintf(drives, sizeof drives, "%s/media/", dir);
    sc_rbx_drives = drives;
    snprintf(path, sizeof path, "%s/media", dir);
    assert(!mkdir(path, 0700));
    snprintf(path, sizeof path, "%s/855.bin", dir);
    FILE *f = fopen(path, "wb");
    assert(f && fwrite(file, 1, sizeof file - 1, f) == sizeof file - 1 && !fclose(f));
    char internal[160], drive[160], mark[160];
    snprintf(internal, sizeof internal, "%s/stm.db", dir);
    snprintf(drive, sizeof drive, "%s/media/stm.db", dir);
    snprintf(mark, sizeof mark, "%s/.loading", dir);

    // No Pro DJ Link load asked for: stock, not one statement.
    sqlite3 *db = library(internal, "rekordbox [126 BPM 6A]");
    statements = 0;
    assert(!load(db) && !statements && !filled(db));
    // A drive's load while a Pro DJ Link load is open: stock, not one statement.
    assert((f = fopen(mark, "w")) && !fclose(f));
    sqlite3 *stick = library(drive, "rekordbox [126 BPM 6A]");
    statements = 0;
    assert(!load(stick) && !statements && !filled(stick));
    sqlite3_close(stick);
    // A track of another provider: one read, nothing written.
    sqlite3 *other = library(internal, "TIDAL [126 BPM]");
    statements = 0;
    assert(!load(other) && statements == 1 && !filled(other));
    sqlite3_close(other);
    // Ours: filled once, on Engine's own connection; a row already filled is left as it is.
    sqlite3_close(db);
    db = library(internal, "rekordbox [126 BPM 6A]");
    const char *result = load(db);
    assert(result && !strcmp(result, "filled") && filled(db));
    statements = 0;
    assert(!load(db) && statements == 1 && filled(db));
    sqlite3_close(db);
    // A stale request no longer opens the hook.
    struct timespec old[2] = {{.tv_sec = time(NULL) - 2 * sc_rbx_window}, {.tv_sec = time(NULL) - 2 * sc_rbx_window}};
    assert(!utimensat(AT_FDCWD, mark, old, 0));
    db = library(internal, "rekordbox [126 BPM 6A]");
    statements = 0;
    assert(!load(db) && !statements && !filled(db));
    sqlite3_close(db);
    return 0;
}
