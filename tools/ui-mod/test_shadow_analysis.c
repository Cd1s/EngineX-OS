// cc -O2 -Wall -Wextra -Werror -fsanitize=address,undefined test_shadow_analysis.c -lsqlite3 -o test_shadow_analysis
#define _GNU_SOURCE
#include <assert.h>
#include <sqlite3.h>
#include "shadow_analysis.h"

static int exec_sql(void *db, const char *sql, int (*row)(void *, int, char **, char **), void *arg, char **err) {
    return sqlite3_exec(db, sql, row, arg, err);
}

static const char *filename(void *db, const char *name) {
    return sqlite3_db_filename(db, name);
}

static sqlite3 *library(const char *file) {
    sqlite3 *db = NULL;
    assert(sqlite3_open(file, &db) == SQLITE_OK);
    assert(sqlite3_exec(db,
        "CREATE TABLE Track(id INTEGER PRIMARY KEY, path TEXT, isAnalyzed INTEGER DEFAULT 0,"
        " isBeatGridLocked INTEGER DEFAULT 0);"
        "CREATE TABLE PerformanceData(trackId INTEGER PRIMARY KEY, trackData BLOB, overviewWaveFormData BLOB,"
        " beatData BLOB, quickCues BLOB, loops BLOB, thirdPartySourceId INTEGER, activeOnLoadLoops INTEGER);"
        "INSERT INTO Track(id,path) VALUES(1,'" SC_SHADOW_REL "peer-f012/Contents/L''ete.wav'),"
        "(2,'../../../../music/local.wav'),(3,'" SC_SHADOW_REL "peer-f012/missing.wav'),"
        "(4,'" SC_SHADOW_REL "peer-f012/../outside.wav'),(5,'" SC_SHADOW_REL "peer-f012/corrupt.wav');"
        "INSERT INTO PerformanceData(trackId) VALUES(1),(2),(3),(4),(5);",
        NULL, NULL, NULL) == SQLITE_OK);
    return db;
}

static void check_flags(sqlite3 *db, int analysed_id) {
    sqlite3_stmt *stmt = NULL;
    assert(sqlite3_prepare_v2(db, "SELECT id,isAnalyzed,isBeatGridLocked FROM Track ORDER BY id", -1, &stmt, NULL) == SQLITE_OK);
    for (int id = 1; id <= 5; id++) {
        assert(sqlite3_step(stmt) == SQLITE_ROW && sqlite3_column_int(stmt, 0) == id);
        assert(sqlite3_column_int(stmt, 1) == (id == analysed_id));
        assert(sqlite3_column_int(stmt, 2) == 0);
    }
    assert(sqlite3_step(stmt) == SQLITE_DONE);
    sqlite3_finalize(stmt);
}

int main(void) {
    static const unsigned char payload[] = "RBXA1"
        "\5\0\0\0" "track" "\5\0\0\0" "waves" "\5\0\0\0" "beats"
        "\5\0\0\0" "cues!" "\0\0\0\0";
    char dir[] = "/tmp/shadow-analysis-XXXXXX", path[256], stick[256], contents[256], cache[256], sidecar[256], malformed[256];
    assert(mkdtemp(dir));
    sc_shadow_root = dir;
    snprintf(path, sizeof path, "%s/m.db", dir);
    snprintf(stick, sizeof stick, "%s/usb.db", dir);
    snprintf(cache, sizeof cache, "%s/peer-f012", dir);
    snprintf(contents, sizeof contents, "%s/peer-f012/Contents", dir);
    snprintf(sidecar, sizeof sidecar, "%s/peer-f012/Contents/L'ete.wav.analysis", dir);
    snprintf(malformed, sizeof malformed, "%s/peer-f012/corrupt.wav.analysis", dir);
    assert(!mkdir(cache, 0700) && !mkdir(contents, 0700));
    FILE *out = fopen(sidecar, "wb");
    assert(out && fwrite(payload, 1, sizeof payload - 1, out) == sizeof payload - 1 && !fclose(out));
    out = fopen(malformed, "wb");
    assert(out && fwrite(payload, 1, sizeof payload - 2, out) == sizeof payload - 2 && !fclose(out));
    char audio[256];
    snprintf(audio, sizeof audio, "%s/peer-f012/Contents/L'ete.wav", dir);
    assert(sc_shadow_analysis_staged(audio));
    snprintf(audio, sizeof audio, "%s/peer-f012/missing.wav", dir);
    assert(!sc_shadow_analysis_staged(audio));
    snprintf(audio, sizeof audio, "%s/peer-f012/Contents/../corrupt.wav", dir);
    assert(!sc_shadow_analysis_staged(audio));

    sc_shadow_library = path;
    sqlite3 *db = library(path), *usb = library(stick);
    const struct sc_shadow_sqlite api = {exec_sql, filename};
    assert(sc_shadow_analysis_bound(SC_SHADOW_TRACK_READ "t.isAnalyzed FROM Track AS t WHERE t.id = ? ;"));
    assert(!sc_shadow_analysis_bound(SC_SHADOW_TRACK_READ "t.isAnalyzed FROM Track AS t WHERE t.id = ? OR 1"));
    assert(!sc_shadow_analysis_bound(SC_SHADOW_SELECT "?"));
    assert(!sc_shadow_analysis_fill(&api, usb, 1)); // Same ID and path, wrong library.
    for (int i = 2; i <= 5; i++) assert(!sc_shadow_analysis_fill(&api, db, i));
    check_flags(usb, 0);
    check_flags(db, 0);

    sqlite3_stmt *stmt = NULL;
    assert(sqlite3_prepare_v2(db, "SELECT id,isAnalyzed FROM Track WHERE id=?", -1, &stmt, NULL) == SQLITE_OK);
    assert(sqlite3_bind_int64(stmt, 1, 1) == SQLITE_OK);
    assert(sqlite3_exec(db, "BEGIN", NULL, NULL, NULL) == SQLITE_OK);
    assert(sc_shadow_analysis_fill(&api, sqlite3_db_handle(stmt), 1));
    assert(sqlite3_step(stmt) == SQLITE_ROW && sqlite3_column_int(stmt, 1) == 1);
    sqlite3_finalize(stmt);
    check_flags(db, 1);
    assert(!sc_shadow_analysis_fill(&api, db, 1));
    assert(sqlite3_prepare_v2(db, SC_SHADOW_SELECT "?", -1, &stmt, NULL) == SQLITE_OK);
    assert(sqlite3_bind_int64(stmt, 1, 1) == SQLITE_OK);
    assert(sqlite3_step(stmt) == SQLITE_ROW);
    for (int i = 1; i < 5; i++) {
        const char *want[] = {"track", "waves", "beats", "cues!"};
        assert(sqlite3_column_bytes(stmt, i) == 5 && !memcmp(sqlite3_column_blob(stmt, i), want[i - 1], 5));
    }
    assert(sqlite3_column_type(stmt, 5) == SQLITE_NULL);
    sqlite3_finalize(stmt);
    assert(sqlite3_exec(db, "ROLLBACK", NULL, NULL, NULL) == SQLITE_OK);
    check_flags(db, 0);
    assert(sc_shadow_analysis_fill(&api, db, 1)); // The first fill followed Engine's transaction.
    check_flags(db, 1);
    // RIGHT, 2026-11-19: a copy is used again for the same track, and its row still held
    // what an earlier load had left (a grid of another build, an overview never finished),
    // so the shadow lane drew another grid than the source and no overview. The row is the
    // staged analysis again, and is written only when it differs.
    assert(!sc_shadow_analysis_fill(&api, db, 1));
    // RIGHT, 2026-11-26: 0.5-1 s into one copy load Engine saves its own decoder-trimmed
    // trackData and synced quickCues via SC_SHADOW_INSERT and re-reads Track. The hook must
    // fill only once per sidecar digest and must not overwrite Engine's post-load updates.
    assert(sqlite3_exec(db, "UPDATE PerformanceData SET trackData = X'7472696d6d6564', quickCues = X'73796e636564' WHERE trackId = 1", NULL, NULL, NULL) == SQLITE_OK);
    assert(!sc_shadow_analysis_fill(&api, db, 1));
    assert(sqlite3_prepare_v2(db, "SELECT CAST(trackData AS TEXT) || '/' || CAST(quickCues AS TEXT) FROM PerformanceData WHERE trackId = 1", -1, &stmt, NULL) == SQLITE_OK);
    assert(sqlite3_step(stmt) == SQLITE_ROW && !strcmp((const char *)sqlite3_column_text(stmt, 0), "trimmed/synced"));
    sqlite3_finalize(stmt);
    assert(sqlite3_exec(db, "UPDATE PerformanceData SET beatData = X'00', overviewWaveFormData = NULL, thirdPartySourceId = NULL WHERE trackId = 1;"
                            "UPDATE Track SET isAnalyzed = 0 WHERE id = 1", NULL, NULL, NULL) == SQLITE_OK);
    assert(sc_shadow_analysis_fill(&api, db, 1));
    check_flags(db, 1);
    // The source's grid was edited in rekordbox: the newer analysis replaces the copy's.
    static const unsigned char edited[] = "RBXA1"
        "\5\0\0\0" "track" "\5\0\0\0" "waves" "\5\0\0\0" "BEATS"
        "\5\0\0\0" "cues!" "\0\0\0\0";
    out = fopen(sidecar, "wb");
    assert(out && fwrite(edited, 1, sizeof edited - 1, out) == sizeof edited - 1 && !fclose(out));
    assert(sc_shadow_analysis_fill(&api, db, 1) && !sc_shadow_analysis_fill(&api, db, 1));
    assert(sqlite3_prepare_v2(db, "SELECT hex(overviewWaveFormData) || '/' || hex(beatData) FROM PerformanceData WHERE trackId = 1", -1, &stmt, NULL) == SQLITE_OK);
    assert(sqlite3_step(stmt) == SQLITE_ROW && !strcmp((const char *)sqlite3_column_text(stmt, 0), "7761766573/4245415453"));
    sqlite3_finalize(stmt);
    // Neighbours: a track of the library, a copy without a staged analysis, a path out of
    // the cache and a damaged record are left alone, also now that their rows are read.
    assert(sqlite3_exec(db, "UPDATE PerformanceData SET beatData = X'00' WHERE trackId > 1", NULL, NULL, NULL) == SQLITE_OK);
    for (int i = 2; i <= 5; i++) assert(!sc_shadow_analysis_fill(&api, db, i));
    assert(sqlite3_prepare_v2(db, "SELECT count(*) FROM PerformanceData WHERE trackId > 1 AND hex(beatData) = '00'", -1, &stmt, NULL) == SQLITE_OK);
    assert(sqlite3_step(stmt) == SQLITE_ROW && sqlite3_column_int(stmt, 0) == 4);
    sqlite3_finalize(stmt);
    assert(sqlite3_close(usb) == SQLITE_OK && sqlite3_close(db) == SQLITE_OK);
    assert(!unlink(path) && !unlink(stick) && !unlink(sidecar) && !unlink(malformed));
    assert(!rmdir(contents) && !rmdir(cache) && !rmdir(dir));
    return 0;
}
