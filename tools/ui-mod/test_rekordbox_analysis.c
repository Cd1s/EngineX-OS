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

// A library with track 16449 (uri, label) and its empty analysis row. With attach, Engine's own
// connection: a main database without a file and the library attached to it.
static sqlite3 *library_row(const char *path, const char *uri, const char *label, int attach) {
    sqlite3 *db;
    unlink(path);
    assert(sqlite3_open(attach ? ":memory:" : path, &db) == SQLITE_OK);
    char sql[768];
    snprintf(sql, sizeof sql, "%s%s%s;"
             "CREATE TABLE %sTrack (id INTEGER PRIMARY KEY, uri TEXT, label TEXT, isAnalyzed INTEGER DEFAULT 0);"
             "CREATE TABLE %sPerformanceData (trackId INTEGER PRIMARY KEY, trackData BLOB, overviewWaveFormData BLOB, beatData BLOB, quickCues BLOB, loops BLOB);"
             "INSERT INTO Track (id, uri, label) VALUES (16449, %s%s%s, '%s'); INSERT INTO PerformanceData (trackId) VALUES (16449);",
             attach ? "ATTACH '" : "SELECT 1", attach ? path : "", attach ? "' AS stm" : "", attach ? "stm." : "", attach ? "stm." : "",
             uri ? "'" : "", uri ? uri : "NULL", uri ? "'" : "", label);
    assert(sqlite3_exec(db, sql, NULL, NULL, NULL) == SQLITE_OK);
    return db;
}

static sqlite3 *library(const char *path, const char *label) { return library_row(path, "streaming://TIDAL/Track/855", label, 0); }

static int filled(sqlite3 *db) {
    sqlite3_stmt *st;
    // Filled, and the Track row says analysed (else Engine analyses it again).
    assert(sqlite3_prepare_v2(db, "SELECT hex(trackData) || '/' || hex(beatData) || '/' || isAnalyzed FROM PerformanceData JOIN Track ON id = trackId WHERE trackId = 16449", -1, &st, NULL) == SQLITE_OK);
    assert(sqlite3_step(st) == SQLITE_ROW);
    const char *v = (const char *)sqlite3_column_text(st, 0);
    int got = v && !strcmp(v, "01AB/FF/1");
    sqlite3_finalize(st);
    return got;
}

// A load through Engine's own SQL: what the hook does before Engine reads the row, with the
// id in the text (prepare) and, as Engine 5.3.0 does it, bound to "= ?" (bind_int64).
static const char *load(sqlite3 *db) {
    long long track = 0, id = 0;
    return sc_rbx_fill(&sqlite, db, SC_RBX_ANALYSIS_SELECT "16449", -1, &track, &id);
}

static const char *load_bound(sqlite3 *db) {
    long long id = 0;
    assert(sc_rbx_analysis_bound(SC_RBX_ANALYSIS_SELECT "?"));
    return sc_rbx_fill_track(&sqlite, db, 16449, &id);
}

int main(void) {
    assert(sc_rbx_analysis_track(SC_RBX_ANALYSIS_SELECT "16449", -1) == 16449);
    assert(sc_rbx_analysis_track(SC_RBX_ANALYSIS_SELECT "16449;", -1) == 16449);
    const char *with_tail = SC_RBX_ANALYSIS_SELECT "77 junk";
    assert(sc_rbx_analysis_track(with_tail, (int)strlen(with_tail) - 5) == 77);
    const char *bad[] = {"", SC_RBX_ANALYSIS_SELECT, SC_RBX_ANALYSIS_SELECT "x", SC_RBX_ANALYSIS_SELECT "12 OR 1",
                         "SELECT trackId FROM PerformanceData WHERE trackId = 3", SC_RBX_ANALYSIS_SELECT "123456789012345"};
    for (unsigned i = 0; i < sizeof bad / sizeof *bad; i++) assert(!sc_rbx_analysis_track(bad[i], -1));

    assert(sc_rbx_analysis_bound(SC_RBX_ANALYSIS_SELECT "?") && sc_rbx_analysis_bound(SC_RBX_ANALYSIS_SELECT "? ;"));
    const char *unbound[] = {NULL, "", SC_RBX_ANALYSIS_SELECT, SC_RBX_ANALYSIS_SELECT "16449", SC_RBX_ANALYSIS_SELECT "? OR 1",
                             SC_RBX_ANALYSIS_SELECT "?1", "SELECT trackId FROM PerformanceData WHERE trackId = ?"};
    for (unsigned i = 0; i < sizeof unbound / sizeof *unbound; i++) assert(!sc_rbx_analysis_bound(unbound[i]));
    assert(!sc_rbx_analysis_track(SC_RBX_ANALYSIS_SELECT "?", -1)); // the morning test: no id at prepare

    assert(sc_rbx_uri_id("streaming://TIDAL/Track/855") == 855);
    assert(sc_rbx_uri_id("tidal:855") == 855);
    assert(!sc_rbx_uri_id("") && !sc_rbx_uri_id("855") && !sc_rbx_uri_id("streaming://TIDAL/Track/") && !sc_rbx_uri_id("x/a855"));

    const unsigned char file[] = "RBXA1" "\2\0\0\0" "\x01\xab" "\0\0\0\0" "\1\0\0\0" "\xff" "\0\0\0\0" "\0\0\0\0";
    char *sql = sc_rbx_analysis_sql(file, sizeof file - 1, 9);
    assert(sql && !strcmp(sql, "UPDATE PerformanceData SET trackData = X'01ab', overviewWaveFormData = NULL, beatData = X'ff', quickCues = NULL, loops = NULL WHERE trackId = 9 AND trackData IS NULL; "
                              "UPDATE Track SET isAnalyzed = 1 WHERE id = 9 AND changes() > 0"));
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
    assert(!load(db) && !load_bound(db) && !statements && !filled(db));
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
    // Engine 5.3.0: the id bound at the read, on its own connection (library attached to a
    // main database without a file). Filled once.
    sqlite3 *engine = library_row(internal, "streaming://TIDAL/Track/855", "rekordbox [126 BPM 6A]", 1);
    result = load_bound(engine);
    assert(result && !strcmp(result, "filled") && filled(engine));
    statements = 0;
    assert(!load_bound(engine) && statements == 1 && filled(engine));
    sqlite3_close(engine);
    // Engine Link (a track of the other deck's USB) and Remote Library (Engine DJ desktop)
    // loads while a rekordbox load is open: one read of the row, nothing written, even with a
    // label that looks like ours and a file for the id.
    const char *theirs[][2] = {{NULL, "rekordbox [126 BPM 6A]"}, {"file:///Users/dj/Music/855.flac", "rekordbox [126 BPM 6A]"},
                               {"engine-link://{6d}/Engine Library/855", "Label"}, {"streaming://TIDAL/Track/855", "TIDAL [126 BPM]"}};
    for (unsigned i = 0; i < sizeof theirs / sizeof *theirs; i++) {
        for (int attach = 0; attach < 2; attach++) {
            sqlite3 *link = library_row(internal, theirs[i][0], theirs[i][1], attach);
            statements = 0;
            assert(!load_bound(link) && !load(link) && statements == 2 && !filled(link));
            sqlite3_close(link);
        }
    }
    // A drive's load, the id bound: stock, not one statement.
    stick = library(drive, "rekordbox [126 BPM 6A]");
    statements = 0;
    assert(!load_bound(stick) && !statements && !filled(stick));
    sqlite3_close(stick);
    // A stale request no longer opens the hook.
    struct timespec old[2] = {{.tv_sec = time(NULL) - 2 * sc_rbx_window}, {.tv_sec = time(NULL) - 2 * sc_rbx_window}};
    assert(!utimensat(AT_FDCWD, mark, old, 0));
    db = library(internal, "rekordbox [126 BPM 6A]");
    statements = 0;
    assert(!load(db) && !load_bound(db) && !statements && !filled(db));
    sqlite3_close(db);
    return 0;
}
