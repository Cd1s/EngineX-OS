#include <assert.h>
#include <pthread.h>
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
static const struct sc_rbx_sqlite sqlite = {exec_counted, db_main, NULL};

static void check_edits(sqlite3 *db, const char *want) {
    sqlite3_stmt *st;
    assert(sqlite3_prepare_v2(db, "SELECT hex(beatData)||'/'||hex(quickCues) FROM PerformanceData", -1, &st, NULL) == SQLITE_OK);
    assert(sqlite3_step(st) == SQLITE_ROW && !strcmp((const char *)sqlite3_column_text(st, 0), want));
    sqlite3_finalize(st);
}

static void *map_thread(void *arg) {
    long long base = *(long long *)arg;
    for (int i = 0; i < 16; ++i) sc_rbx_remember_track(base + i, base + i + 1000, NULL);
    for (int pass = 0; pass < 1000; ++pass)
        for (int i = 0; i < 16; ++i) {
            long long id = 0;
            assert(sc_rbx_lookup_track(base + i, &id, NULL) && id == base + i + 1000);
        }
    return NULL;
}

// A library with track 16449 (uri, label) and its empty analysis row. With attach, Engine's own
// connection: a main database without a file and the library attached to it.
static sqlite3 *library_row(const char *path, const char *uri, const char *label, int attach) {
    sqlite3 *db;
    unlink(path);
    assert(sqlite3_open(attach ? ":memory:" : path, &db) == SQLITE_OK);
    char sql[1000];
    snprintf(sql, sizeof sql, "%s%s%s;"
             "CREATE TABLE %sTrack (id INTEGER PRIMARY KEY, uri TEXT, label TEXT, path TEXT, isAnalyzed INTEGER DEFAULT 0, isBeatGridLocked INTEGER);"
             "CREATE TABLE %sPerformanceData (trackId INTEGER PRIMARY KEY, trackData BLOB, overviewWaveFormData BLOB, beatData BLOB, quickCues BLOB, loops BLOB, thirdPartySourceId INTEGER);"
             "INSERT INTO Track (id, uri, label, path) VALUES (16449, %s%s%s, '%s', '" SC_RBX_DOWNLOADS_REL "0123456789012345678901234567890123456789.temp'); INSERT INTO PerformanceData (trackId) VALUES (16449);",
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
    char *sql = sc_rbx_analysis_sql(file, sizeof file - 1, 9, 0);
    assert(sql && !strcmp(sql, "UPDATE PerformanceData SET trackData = X'01ab', overviewWaveFormData = NULL, beatData = X'ff', quickCues = NULL, loops = NULL WHERE trackId = 9 AND trackData IS NULL; "
                              "UPDATE Track SET isAnalyzed = 1 WHERE id = 9 AND changes() > 0"));
    free(sql);
    unsigned char with_flag[sizeof file];
    memcpy(with_flag, file, sizeof file);
    sql = sc_rbx_analysis_sql(with_flag, sizeof with_flag, 16449, 0);
    assert(sql);
    sqlite3 *partial = library(":memory:", "rekordbox [126 BPM]");
    assert(sqlite3_exec(partial, sql, NULL, NULL, NULL) == SQLITE_OK);
    sqlite3_stmt *flags;
    assert(sqlite3_prepare_v2(partial, "SELECT isAnalyzed, isBeatGridLocked, hex(beatData) FROM Track JOIN PerformanceData ON id = trackId", -1, &flags, NULL) == SQLITE_OK);
    assert(sqlite3_step(flags) == SQLITE_ROW && !sqlite3_column_int(flags, 0) && sqlite3_column_type(flags, 1) == SQLITE_NULL);
    assert(!strcmp((const char *)sqlite3_column_text(flags, 2), "FF"));
    sqlite3_finalize(flags);
    sqlite3_close(partial);
    free(sql);
    assert(!sc_rbx_analysis_sql(file, sizeof file - 2, 9, 0));                      // cut short
    assert(!sc_rbx_analysis_sql((const unsigned char *)"RBXA0", 5, 9, 0));           // not ours
    const unsigned char huge[] = "RBXA1" "\xff\xff\xff\x7f";
    assert(!sc_rbx_analysis_sql(huge, sizeof huge - 1, 9, 0));                       // length past the end
    const unsigned char no_grid[] = "RBXA1" "\1\0\0\0" "\x01" "\0\0\0\0" "\0\0\0\0" "\0\0\0\0" "\0\0\0\0";
    assert(!sc_rbx_analysis_sql(no_grid, sizeof no_grid - 1, 9, 0));                 // nothing to skip analysis for

    char dir[] = "/tmp/rbx-analysis-XXXXXX", drives[64], path[160], history[64];
    assert(mkdtemp(dir));
    sc_rbx_dir = dir;
    snprintf(history, sizeof history, "%s/history", dir);
    sc_rbx_history = history;
    snprintf(drives, sizeof drives, "%s/media/", dir);
    sc_rbx_drives = drives;
    snprintf(path, sizeof path, "%s/media", dir);
    assert(!mkdir(path, 0700));
    snprintf(path, sizeof path, "%s/855.bin", dir);
    FILE *f = fopen(path, "wb");
    assert(f && fwrite(file, 1, sizeof file - 1, f) == sizeof file - 1 && !fclose(f));
    snprintf(path, sizeof path, "%s/855.bin.perf", dir);
    f = fopen(path, "wb");
    assert(f && !fclose(f));
    char replay[160];
    const char *download = "/data/downloads/TIDAL/0123456789012345678901234567890123456789.flac";
    assert(!sc_rbx_replay_path(download, replay, sizeof replay));
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
    assert(sc_rbx_replay_path(download, replay, sizeof replay));
    assert(!sc_rbx_replay_path("/data/inMusicTempFiles/0123456789012345678901234567890123456789.flac", replay, sizeof replay));
    assert(!sc_rbx_replay_path("/data/downloads/TIDAL/0123456789012345678901234567890123456789.flac.other", replay, sizeof replay));
    statements = 0;
    assert(!load(db) && statements == 1 && filled(db));
    sqlite3_close(db);
    // Engine 5.3.0: the id bound at the read, on its own connection (library attached to a
    // main database without a file). Filled once.
    sqlite3 *engine = library_row(internal, "streaming://TIDAL/Track/855", "rekordbox [126 BPM 6A]", 1);
    result = load_bound(engine);
    assert(result && !strcmp(result, "filled") && filled(engine));
    sqlite3_stmt *source;
    assert(sqlite3_prepare_v2(engine, "SELECT thirdPartySourceId FROM stm.PerformanceData WHERE trackId = 16449", -1, &source, NULL) == SQLITE_OK);
    // No import mark: Engine moves the grid, cues and loops of a marked row by its own
    // rekordbox correction when it loads the audio (an MP3 by 2292 samples), on top of ours.
    assert(sqlite3_step(source) == SQLITE_ROW && sqlite3_column_type(source, 0) == SQLITE_NULL);
    sqlite3_finalize(source);
    statements = 0;
    assert(!load_bound(engine) && statements == 1 && filled(engine));
    sqlite3_close(engine);
    db = library(internal, "rekordbox [126 BPM]");
    char alias[160];
    snprintf(alias, sizeof alias, "%s/0123456789012345678901234567890123456789.perf", dir);
    assert(!unlink(alias));
    assert(!strcmp(load_bound(db), "filled"));
    // The deck changes its cue while the source changes both fields.
    assert(sqlite3_exec(db, "UPDATE PerformanceData SET quickCues=X'99'", NULL, NULL, NULL) == SQLITE_OK);
    const unsigned char edited[] = "RBXA1" "\2\0\0\0" "\x01\xab" "\0\0\0\0" "\1\0\0\0" "\x12" "\1\0\0\0" "\x34" "\0\0\0\0";
    snprintf(path, sizeof path, "%s/855.bin", dir);
    f = fopen(path, "wb");
    assert(f && fwrite(edited, 1, sizeof edited - 1, f) == sizeof edited - 1 && !fclose(f));
    result = load_bound(db);
    assert(result && !strcmp(result, "refreshed; kept deck edits"));
    check_edits(db, "12/99");
    // A subsequent source cue edit is accepted once it matches our last
    // import; a local grid edit remains protected from refresh.
    assert(sqlite3_exec(db, "UPDATE PerformanceData SET quickCues=NULL, beatData=X'88'", NULL, NULL, NULL) == SQLITE_OK);
    unsigned char next[sizeof edited];
    memcpy(next, edited, sizeof next);
    next[19] = 0x56;
    f = fopen(path, "wb");
    assert(f && fwrite(next, 1, sizeof next - 1, f) == sizeof next - 1 && !fclose(f));
    result = load_bound(db);
    assert(result && !strcmp(result, "refreshed; kept deck edits"));
    check_edits(db, "88/34");
    sqlite3_close(db);
    // Engine Link (a track of the other deck's USB) and Remote Library (Engine DJ desktop)
    // loads while a rekordbox load is open: one read of the row, nothing written, even with a
    // label that looks like ours and a file for the id.
    const char *theirs[][2] = {{NULL, "rekordbox [126 BPM 6A]"}, {"file:///Users/dj/Music/855.flac", "rekordbox [126 BPM 6A]"},
                               {"file:///data/inMusicTempFiles/42_855.flac", "rekordbox [126 BPM 6A]"},
                               {"engine-link://{6d}/Engine Library/855", "Label"}, {"streaming://TIDAL/Track/855", "TIDAL [126 BPM]"}};
    for (unsigned i = 0; i < sizeof theirs / sizeof *theirs; i++) {
        for (int attach = 0; attach < 2; attach++) {
            sqlite3 *link = library_row(internal, theirs[i][0], theirs[i][1], attach);
            statements = 0;
            assert(!load_bound(link) && !load(link) && statements == 2 && !filled(link));
            sqlite3_close(link);
        }
    }
    // With the trace flag a skipped row says what it looked like: one more read, no write.
    sqlite3 *traced = library_row(internal, "file:///Users/dj/Music/855.flac", "Label", 1);
    snprintf(path, sizeof path, "%s/trace", dir);
    fflush(stderr);
    int saved = dup(2), log = open(path, O_WRONLY | O_CREAT | O_TRUNC, 0600);
    assert(saved >= 0 && log >= 0 && dup2(log, 2) == 2 && !close(log));
    sc_rbx_verbose = 1;
    statements = 0;
    result = load_bound(traced);
    sc_rbx_verbose = 0;
    fflush(stderr);
    assert(dup2(saved, 2) == 2 && !close(saved));
    assert(result && !strcmp(result, "skipped (not a rekordbox row)") && statements == 2 && !filled(traced));
    char said[400] = "";
    f = fopen(path, "r");
    assert(f && fgets(said, sizeof said, f) && !fclose(f) && !unlink(path));
    assert(strstr(said, "track 16449: row uri=file:///User label=Label analysed=0 data=null mark=null download=yes map="));
    sqlite3_close(traced);
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
    assert(!utimensat(AT_FDCWD, mark, NULL, 0));
    char rowmap[160];
    snprintf(rowmap, sizeof rowmap, "%s/rowmap", dir);
    assert(!access(rowmap, R_OK));
    f = fopen(rowmap, "a");
    assert(f && fputs("malformed rowmap\n1 nope\n2 3 0123456789abcdef0123456789abcdef012345678\n", f) >= 0 && !fclose(f));
    pthread_mutex_lock(&sc_rbx_track_map_mu);
    memset(sc_rbx_track_maps, 0, sizeof sc_rbx_track_maps);
    sc_rbx_track_map_next = sc_rbx_track_map_clock = 0;
    sc_rbx_track_map_loaded = 0;
    pthread_mutex_unlock(&sc_rbx_track_map_mu);
    sc_rbx_load_track_map();
    long long mapped = 0;
    assert(sc_rbx_lookup_track(16449, &mapped, NULL) && mapped == 855);
    assert(!sc_rbx_lookup_track(2, &mapped, NULL));
    pthread_t workers[8];
    long long bases[8];
    for (int i = 0; i < 8; ++i) {
        bases[i] = 600000 + i * 100;
        assert(!pthread_create(&workers[i], NULL, map_thread, &bases[i]));
    }
    for (int i = 0; i < 8; ++i) assert(!pthread_join(workers[i], NULL));
    // Engine may rewrite the Track presentation fields after the first load. A later
    // source generation must still refresh the row through the remembered mapping.
    sqlite3 *reload = library_row(internal, "streaming://TIDAL/Track/855", "rekordbox [126 BPM 6A]", 1);
    result = load_bound(reload);
    assert(result && !strcmp(result, "filled"));
    assert(sqlite3_exec(reload, "UPDATE Track SET uri='file:///data/inMusicTempFiles/935.m4a', label='Fenner' WHERE id=16449", NULL, NULL, NULL) == SQLITE_OK);
    const unsigned char refreshed[] = "RBXA1" "\2\0\0\0" "\x02" "\xbc" "\0\0\0\0" "\1\0\0\0" "\x66" "\1\0\0\0" "\x77" "\0\0\0\0";
    snprintf(path, sizeof path, "%s/855.bin", dir);
    f = fopen(path, "wb");
    assert(f && fwrite(refreshed, 1, sizeof refreshed - 1, f) == sizeof refreshed - 1 && !fclose(f));
    result = load_bound(reload);
    assert(result && !strcmp(result, "refreshed"));
    check_edits(reload, "66/77");
    sqlite3_close(reload);
    // LEFT, 2026-11-18: after a load Engine writes the row back without the import mark, uri
    // and label still ours (8 of 24 rows in its stm.db). Such a row still takes a later
    // source edit, and only where it holds what we imported.
    sqlite3 *real = library_row(internal, "streaming://TIDAL/Track/855", "rekordbox [126 BPM 6A]", 1);
    result = load_bound(real);
    assert(result && !strcmp(result, "filled"));
    check_edits(real, "66/77");
    assert(sqlite3_exec(real, "UPDATE stm.PerformanceData SET thirdPartySourceId=NULL", NULL, NULL, NULL) == SQLITE_OK);
    const unsigned char edit1[] = "RBXA1" "\2\0\0\0" "\x02" "\xbc" "\0\0\0\0" "\1\0\0\0" "\x55" "\1\0\0\0" "\x44" "\0\0\0\0";
    f = fopen(path, "wb");
    assert(f && fwrite(edit1, 1, sizeof edit1 - 1, f) == sizeof edit1 - 1 && !fclose(f));
    result = load_bound(real);
    assert(result && !strcmp(result, "refreshed"));
    check_edits(real, "55/44");
    // A grid moved on the deck stays; the cue we imported still follows the source.
    assert(sqlite3_exec(real, "UPDATE stm.PerformanceData SET thirdPartySourceId=NULL, beatData=X'99'", NULL, NULL, NULL) == SQLITE_OK);
    const unsigned char edit2[] = "RBXA1" "\2\0\0\0" "\x02" "\xbc" "\0\0\0\0" "\1\0\0\0" "\x33" "\1\0\0\0" "\x22" "\0\0\0\0";
    f = fopen(path, "wb");
    assert(f && fwrite(edit2, 1, sizeof edit2 - 1, f) == sizeof edit2 - 1 && !fclose(f));
    result = load_bound(real);
    assert(result && !strcmp(result, "refreshed; kept deck edits"));
    check_edits(real, "99/22");
    // Neighbours. A row of ours that Engine analysed itself (nothing imported to compare
    // with): untouched.
    char kept[2][200], aside[2][216];
    snprintf(kept[0], sizeof kept[0], "%s/855-16449-beatData", history);
    snprintf(kept[1], sizeof kept[1], "%s/855-16449-quickCues", history);
    for (int i = 0; i < 2; i++) {
        snprintf(aside[i], sizeof aside[i], "%s.aside", kept[i]);
        assert(!rename(kept[i], aside[i]));
    }
    assert(sqlite3_exec(real, "UPDATE stm.PerformanceData SET thirdPartySourceId=NULL, beatData=X'AA', quickCues=X'BB'", NULL, NULL, NULL) == SQLITE_OK);
    assert(!load_bound(real));
    check_edits(real, "AA/BB");
    for (int i = 0; i < 2; i++) assert(!rename(aside[i], kept[i]));
    // Another source's analysed row with the same id (uri or label not ours): one read, no write.
    assert(sqlite3_exec(real, "UPDATE stm.Track SET label='TIDAL [126 BPM]'", NULL, NULL, NULL) == SQLITE_OK);
    assert(sqlite3_exec(real, "UPDATE stm.PerformanceData SET beatData=X'99', quickCues=X'22'", NULL, NULL, NULL) == SQLITE_OK);
    statements = 0;
    assert(!load_bound(real) && statements == 1);
    check_edits(real, "99/22");
    // An unmarked row of another library whose id is in the map (a file there, its own
    // analysis): a newer source generation writes nothing into it.
    assert(sqlite3_exec(real, "UPDATE stm.Track SET uri='file:///data/music/other.mp3', label='Other'", NULL, NULL, NULL) == SQLITE_OK);
    assert(sqlite3_exec(real, "UPDATE stm.Track SET path='file:///data/music/other.mp3'", NULL, NULL, NULL) == SQLITE_OK);
    assert(sqlite3_exec(real, "UPDATE stm.PerformanceData SET beatData=X'AA', quickCues=X'BB'", NULL, NULL, NULL) == SQLITE_OK);
    f = fopen(path, "wb");
    assert(f && fwrite(edit1, 1, sizeof edit1 - 1, f) == sizeof edit1 - 1 && !fclose(f));
    statements = 0;
    load_bound(real);
    assert(statements == 1);
    check_edits(real, "AA/BB");
    // A foreign row with a different download key is also not eligible for the map.
    assert(sqlite3_exec(real, "UPDATE stm.Track SET path='../../../../downloads/TIDAL/abcdefabcdefabcdefabcdefabcdefabcdefabcdefab.mp3'", NULL, NULL, NULL) == SQLITE_OK);
    statements = 0;
    load_bound(real);
    assert(statements == 1);
    check_edits(real, "AA/BB");
    sqlite3_close(real);
    // The reload block above left 66/77 as the last import; put that back for the checks below.
    f = fopen(path, "wb");
    assert(f && fwrite(refreshed, 1, sizeof refreshed - 1, f) == sizeof refreshed - 1 && !fclose(f));

    // LEFT and RIGHT, 2026-11-19: rows a fill of before the import mark was dropped had left.
    // One that still has the mark (Engine has not loaded it, and would shift it by its own
    // rekordbox correction when it does) is all ours: filled again, without the mark.
    sqlite3 *stale = library_row(internal, "streaming://TIDAL/Track/855", "rekordbox [126 BPM 6A]", 1);
    assert(sqlite3_exec(stale, "UPDATE stm.PerformanceData SET trackData=X'0101', overviewWaveFormData=X'02', beatData=X'EE', quickCues=X'DD', thirdPartySourceId=1;"
                               "UPDATE stm.Track SET isAnalyzed=1", NULL, NULL, NULL) == SQLITE_OK);
    result = load_bound(stale);
    assert(result && !strcmp(result, "filled again"));
    check_edits(stale, "66/77");
    assert(sqlite3_prepare_v2(stale, "SELECT thirdPartySourceId IS NULL AND hex(trackData)='02BC' AND overviewWaveFormData IS NULL FROM stm.PerformanceData", -1, &source, NULL) == SQLITE_OK);
    assert(sqlite3_step(source) == SQLITE_ROW && sqlite3_column_int(source, 0) == 1);
    sqlite3_finalize(source);
    // A row Engine loaded under such a fill (LEFT's Osmosis, an MP3: path still .temp, no
    // fileType): Engine cleared the mark and saved the grid and cues it had shifted, which
    // read like edits on the deck. No record of a fill of the new kind: filled again, once.
    char unmarked[200];
    snprintf(unmarked, sizeof unmarked, "%s/855-16449-unmarked", history);
    assert(!access(unmarked, F_OK) && !unlink(unmarked));
    assert(sqlite3_exec(stale, "UPDATE stm.PerformanceData SET beatData=X'E1', quickCues=X'D1'", NULL, NULL, NULL) == SQLITE_OK);
    result = load_bound(stale);
    assert(result && !strcmp(result, "filled again"));
    check_edits(stale, "66/77");
    // Neighbour. After that, an edit on the deck stays, as on any row of the new kind.
    assert(!access(unmarked, F_OK));
    assert(sqlite3_exec(stale, "UPDATE stm.PerformanceData SET beatData=X'E2'", NULL, NULL, NULL) == SQLITE_OK);
    assert(!load_bound(stale));
    check_edits(stale, "E2/77");
    sqlite3_close(stale);

    // A staged cover is applied on Engine's own connection, including when it arrives
    // after the first analysis bind, and never replaces an existing AlbumArt blob.
    sqlite3 *cover = library_row(internal, "streaming://TIDAL/Track/855", "rekordbox [126 BPM 6A]", 1);
    assert(sqlite3_exec(cover,
        "ALTER TABLE stm.Track ADD COLUMN albumArtId INTEGER;"
        "CREATE TABLE stm.AlbumArt (id INTEGER PRIMARY KEY, hash, albumArt BLOB);"
        "INSERT INTO stm.AlbumArt VALUES (1, 'image://streaming/', NULL), (2, 'keep', X'01');"
        "UPDATE stm.Track SET albumArtId=1 WHERE id=16449", NULL, NULL, NULL) == SQLITE_OK);
    char cover_path[160];
    snprintf(cover_path, sizeof cover_path, "%s/855.cover", dir);
    unsigned char hash[20], jpeg[] = {0xff, 0xd8, 0xff, 0xe0, 0x01, 0x02};
    for (unsigned i = 0; i < sizeof hash; i++) hash[i] = (unsigned char)i;
    f = fopen(cover_path, "wb");
    assert(f && fwrite("RBXC1", 1, 5, f) == 5 && fwrite(hash, 1, sizeof hash, f) == sizeof hash &&
           fwrite(jpeg, 1, sizeof jpeg, f) == sizeof jpeg && !fclose(f));
    result = load_bound(cover);
    assert(result && !strcmp(result, "filled") && access(cover_path, F_OK));
    sqlite3_stmt *art;
    assert(sqlite3_prepare_v2(cover, "SELECT hex(hash),hex(albumArt) FROM stm.AlbumArt WHERE id=1", -1, &art, NULL) == SQLITE_OK);
    assert(sqlite3_step(art) == SQLITE_ROW && !strcmp((const char *)sqlite3_column_text(art, 0), "000102030405060708090A0B0C0D0E0F10111213") &&
           !strcmp((const char *)sqlite3_column_text(art, 1), "FFD8FFE00102"));
    sqlite3_finalize(art);
    assert(sqlite3_exec(cover, "UPDATE stm.AlbumArt SET hash='image://streaming/', albumArt=NULL WHERE id=1", NULL, NULL, NULL) == SQLITE_OK);
    f = fopen(cover_path, "wb");
    assert(f && fwrite("RBXC1", 1, 5, f) == 5 && fwrite(hash, 1, sizeof hash, f) == sizeof hash &&
           fwrite(jpeg, 1, sizeof jpeg, f) == sizeof jpeg && !fclose(f));
    assert(sqlite3_exec(cover, "UPDATE stm.Track SET albumArtId=2", NULL, NULL, NULL) == SQLITE_OK);
    assert(!load_bound(cover) && access(cover_path, F_OK));
    assert(sqlite3_prepare_v2(cover, "SELECT hash,hex(albumArt) FROM stm.AlbumArt WHERE id=2", -1, &art, NULL) == SQLITE_OK);
    assert(sqlite3_step(art) == SQLITE_ROW && !strcmp((const char *)sqlite3_column_text(art, 0), "keep") &&
           !strcmp((const char *)sqlite3_column_text(art, 1), "01"));
    sqlite3_finalize(art);

    // A cover that is already present makes the hook discard the staged copy after one
    // small read; it must not hex-encode the JPEG or issue a failing large SQL statement.
    assert(sqlite3_exec(cover, "UPDATE stm.Track SET albumArtId=2", NULL, NULL, NULL) == SQLITE_OK);
    f = fopen(cover_path, "wb");
    assert(f && fwrite("RBXC1", 1, 5, f) == 5 && fwrite(hash, 1, sizeof hash, f) == sizeof hash &&
           fwrite(jpeg, 1, sizeof jpeg, f) == sizeof jpeg && !fclose(f));
    statements = 0;
    assert(!load_bound(cover) && statements == 2 && access(cover_path, F_OK));

    // Oversized staged covers are discarded instead of retrying a statement over SQLite's
    // default SQL-length limit forever.
    size_t large_n = SC_RBX_COVER_HEADER + SC_RBX_COVER_MAX + 1;
    unsigned char *large = calloc(1, large_n);
    assert(large);
    memcpy(large, "RBXC1", 5);
    memcpy(large + SC_RBX_COVER_HEADER, jpeg, sizeof jpeg);
    f = fopen(cover_path, "wb");
    assert(f && fwrite(large, 1, large_n, f) == large_n && !fclose(f));
    free(large);
    assert(!load_bound(cover) && access(cover_path, F_OK));
    sqlite3_close(cover);
    pthread_mutex_lock(&sc_rbx_track_map_mu);
    memset(sc_rbx_track_maps, 0, sizeof sc_rbx_track_maps);
    sc_rbx_track_map_next = sc_rbx_track_map_clock = 0;
    sc_rbx_track_map_loaded = 1;
    pthread_mutex_unlock(&sc_rbx_track_map_mu);
    for (unsigned i = 0; i < SC_RBX_TRACK_MAP_CAP; ++i) sc_rbx_remember_track(700000 + i, 800000 + i, NULL);
    sc_rbx_remember_track(900000, 900001, NULL);
    assert(!sc_rbx_lookup_track(700000, &mapped, NULL));
    assert(sc_rbx_lookup_track(900000, &mapped, NULL) && mapped == 900001);
    pthread_mutex_lock(&sc_rbx_track_map_mu);
    memset(sc_rbx_track_maps, 0, sizeof sc_rbx_track_maps);
    sc_rbx_track_map_next = sc_rbx_track_map_clock = 0;
    sc_rbx_track_map_loaded = 0;
    pthread_mutex_unlock(&sc_rbx_track_map_mu);
    sc_rbx_load_track_map();
    assert(!sc_rbx_lookup_track(700000, &mapped, NULL));
    assert(sc_rbx_lookup_track(900000, &mapped, NULL) && mapped == 900001);
    snprintf(path, sizeof path, "%s/855-16449-beatData", history);
    assert(!unlink(path));
    snprintf(path, sizeof path, "%s/855-16449-quickCues", history);
    assert(!unlink(path) && !unlink(unmarked) && !rmdir(history));
    assert(!unlink(rowmap));
    snprintf(path, sizeof path, "%s/855.bin", dir);
    assert(!unlink(path));
    snprintf(path, sizeof path, "%s/855.bin.perf", dir);
    assert(!unlink(path));
    assert(!unlink(alias) && !unlink(mark) && !unlink(internal) && !unlink(drive));
    if (!access(cover_path, F_OK)) assert(!unlink(cover_path));
    snprintf(path, sizeof path, "%s/media", dir);
    assert(!rmdir(path) && !rmdir(dir));
    return 0;
}
