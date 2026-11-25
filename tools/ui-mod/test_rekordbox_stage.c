// The stage fill (rekordbox_stage.h) against real SQLite and the bridge's own library schema.
// tools/az01-packer/test_rekordbox_stage.py builds and runs it:
//   cc -O2 -Wall -Wextra -Werror -Wno-unused-function -fsanitize=address,undefined -pthread
//      tools/ui-mod/test_rekordbox_stage.c -o test_rekordbox_stage -lsqlite3
//   ./test_rekordbox_stage pkg/enginedb/schema.sql
// (-Wno-unused-function: rekordbox_analysis.h's Link functions are not called here.)
#include <assert.h>
#include <sqlite3.h>
#include "rekordbox_stage.h"

// Engine's connection, played by a real SQLite database: every statement the hook runs is
// counted, and a test can change the row just before the hook's next statement.
static int statements, meanwhile_at;
static const char *meanwhile; // runs just before the hook's statement number meanwhile_at
static int exec_counted(void *db, const char *sql, int (*row)(void *, int, char **, char **), void *arg, char **err) {
    if (++statements == meanwhile_at && meanwhile) {
        assert(sqlite3_exec(db, meanwhile, NULL, NULL, NULL) == SQLITE_OK);
        meanwhile = NULL;
    }
    return sqlite3_exec(db, sql, row, arg, err);
}
static const char *db_main(void *db, const char *name) { return sqlite3_db_filename(db, name); }
static const struct sc_rbx_sqlite engine = {exec_counted, db_main};

static char dir[64], schema[1 << 16];

static void put_file(const char *path, const void *data, size_t n, const char *mode) {
    FILE *f = fopen(path, mode);
    assert(f && fwrite(data, 1, n, f) == n && !fclose(f));
}

static void mkdirs(const char *path) {
    char p[PATH_MAX];
    snprintf(p, sizeof p, "%s", path);
    for (char *s = p + 1; *s; s++)
        if (*s == '/') {
            *s = 0;
            mkdir(p, 0700);
            *s = '/';
        }
    mkdir(p, 0700);
}

static char *text(sqlite3 *db, const char *fmt, long long id) {
    static char out[4096];
    char sql[1024];
    snprintf(sql, sizeof sql, fmt, id);
    sqlite3_stmt *st;
    assert(sqlite3_prepare_v2(db, sql, -1, &st, NULL) == SQLITE_OK);
    assert(sqlite3_step(st) == SQLITE_ROW);
    snprintf(out, sizeof out, "%s", sqlite3_column_text(st, 0) ? (const char *)sqlite3_column_text(st, 0) : "(null)");
    sqlite3_finalize(st);
    return out;
}

// A stick at dir/media/<label> whose library is the bridge's schema, with uuid in Information
// and, with snapshot, the bridge's snapshot naming snapshot as the library.
static sqlite3 *stick(const char *label, const char *uuid, const char *snapshot) {
    char lib[256], path[PATH_MAX];
    snprintf(lib, sizeof lib, "%s/media/%s/Engine Library/Database2", dir, label);
    mkdirs(lib);
    snprintf(path, sizeof path, "%s/m.db", lib);
    unlink(path);
    sqlite3 *db;
    assert(sqlite3_open(path, &db) == SQLITE_OK);
    assert(sqlite3_exec(db, schema, NULL, NULL, NULL) == SQLITE_OK);
    char sql[256];
    snprintf(sql, sizeof sql, "INSERT INTO Information(uuid, schemaVersionMajor) VALUES('%s', 2)", uuid);
    assert(sqlite3_exec(db, sql, NULL, NULL, NULL) == SQLITE_OK);
    if (snapshot) {
        char head[512];
        int n = snprintf(head, sizeof head, "RBS2 {\"format\":1,\"rules\":5,\"uuid\":\"%s\",\"stamp\":\"s\",\"layout\":\"folders\",\"sources\":[],\"tracks\":9,\"lists\":0,"
                                            "\"pending\":{\"Rows\":0,\"Lists\":false,\"Perf\":9,\"Art\":0,\"Verify\":false},\"applied\":1,\"commit\":2}\nT {}\nE 1 0\n", snapshot);
        snprintf(path, sizeof path, "%s/%s", lib, SC_RBS_SNAPSHOT);
        put_file(path, head, (size_t)n, "wb");
    }
    return db;
}

// A row as the insert leaves it: the export's cue rows with the import mark, or nothing.
static long long track(sqlite3 *db, const char *path, int import_key, int export_cues) {
    char sql[1024];
    char *q = sqlite3_mprintf("INSERT INTO Track(path, filename, pdbImportKey, isAnalyzed) VALUES(%Q, 'f', %s, 0)", path, import_key ? "77" : "NULL");
    assert(sqlite3_exec(db, q, NULL, NULL, NULL) == SQLITE_OK);
    sqlite3_free(q);
    long long id = sqlite3_last_insert_rowid(db);
    if (export_cues) {
        snprintf(sql, sizeof sql, "UPDATE PerformanceData SET quickCues = X'c0', loops = X'10', activeOnLoadLoops = 0, thirdPartySourceId = 1 WHERE trackId = %lld", id);
        assert(sqlite3_exec(db, sql, NULL, NULL, NULL) == SQLITE_OK);
    }
    return id;
}

// An RBXA1 file: trackData, overviewWaveFormData, beatData, quickCues, loops, then Complete.
static size_t rbxa1(unsigned char *out, const char *td, const char *ov, const char *bt, const char *qc, const char *lp, int complete) {
    const char *blobs[5] = {td, ov, bt, qc, lp};
    size_t n = 5;
    memcpy(out, "RBXA1", 5);
    for (int i = 0; i < 5; i++) {
        uint32_t len = (uint32_t)strlen(blobs[i]);
        out[n++] = (unsigned char)len, out[n++] = (unsigned char)(len >> 8), out[n++] = (unsigned char)(len >> 16), out[n++] = (unsigned char)(len >> 24);
        memcpy(out + n, blobs[i], len);
        n += len;
    }
    if (complete >= 0) out[n++] = (unsigned char)complete;
    return n;
}

// One line of the index as Go's encoding/json writes it (Staged, then the index line's own fields).
static void stage_line(const char *uuid, const char *json) {
    char path[PATH_MAX];
    snprintf(path, sizeof path, "%s/%s", sc_rbs_root, uuid);
    mkdirs(path);
    snprintf(path, sizeof path, "%s/%s/%s", sc_rbs_root, uuid, SC_RBS_INDEX);
    put_file(path, json, strlen(json), "ab");
}

static void stage(const char *uuid, const char *key_json, const char *key, const char *rel, int rules, const char *extra,
                  const unsigned char *bin, size_t bin_n) {
    char line[4096], path[PATH_MAX], hex[41];
    snprintf(line, sizeof line,
             "{\"k\":\"%s\",\"sig\":{\"v\":%d,\"w\":\"row-%s\",\"a\":{\"r\":\"%s\",\"s\":1234,\"m\":5678},\"z\":[{\"r\":\"PIONEER/USBANLZ/P001/ANLZ0000.DAT\",\"s\":9,\"m\":1}],\"c\":{\"r\":\"\",\"s\":0,\"m\":0}},"
             "\"rel\":\"%s\",\"facts\":{\"dur\":301.5,\"rate\":44100,\"samples\":13296150,\"bpm\":128},\"wr\":\"AAECAwQFBgcICQoLDA0ODw==\"%s}\n",
             key_json, rules, uuid, rel, rel, extra);
    stage_line(uuid, line);
    if (bin) {
        sc_rbs_sha1_hex(key, strlen(key), hex);
        snprintf(path, sizeof path, "%s/%s/%s.bin", sc_rbs_root, uuid, hex);
        put_file(path, bin, bin_n, "wb");
    }
}

static const char *load(sqlite3 *db, long long id) {
    const char *main = sc_rbs_drive(&engine, db);
    assert(main);
    return sc_rbs_fill_track(&engine, db, main, id);
}

static int starts(const char *s, const char *prefix) { return s && !strncmp(s, prefix, strlen(prefix)); }

#define ROW "SELECT hex(trackData)||'/'||hex(overviewWaveFormData)||'/'||hex(beatData)||'/'||hex(quickCues)||'/'||hex(loops)||'/'||" \
            "ifnull(activeOnLoadLoops,'n')||'/'||ifnull(thirdPartySourceId,'n')||'/'||isAnalyzed FROM PerformanceData JOIN Track ON id = trackId WHERE trackId = %lld"
#define FILLED "5444/4F56/4254/5143/4C50/3/n/1"     // TD, OV, BT, QC, LP, active 3, no mark, analysed
#define INSERTED "///C0/10/0/1/0"                    // the export's cue rows, marked
#define NOTHING "/////n/n/0"                          // a row the export gave no cues

int main(int argc, char **argv) {
    assert(argc == 2);
    FILE *f = fopen(argv[1], "r");
    assert(f);
    size_t n = fread(schema, 1, sizeof schema - 1, f);
    assert(n > 0 && n < sizeof schema - 1 && !fclose(f));

    // SHA-1 (FIPS 180-4 vectors; the stage's names are the hex SHA-1 of the key).
    char hex[41];
    sc_rbs_sha1_hex("abc", 3, hex);
    assert(!strcmp(hex, "a9993e364706816aba3e25717850c26c9cd0d89d"));
    sc_rbs_sha1_hex("", 0, hex);
    assert(!strcmp(hex, "da39a3ee5e6b4b0d3255bfef95601890afd80709"));
    const char *two = "abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq";
    sc_rbs_sha1_hex(two, strlen(two), hex);
    assert(!strcmp(hex, "84983e441c3bd26ebaae4aa1f95129e5e54670f1"));
    static char million[1000000];
    memset(million, 'a', sizeof million);
    sc_rbs_sha1_hex(million, sizeof million, hex);
    assert(!strcmp(hex, "34aa973cd4c4daa4f61eeb2bdbad27316534016f"));

    // rbstate.FoldPath on what the hook can fold.
    char key[256];
    assert(sc_rbs_fold("../Contents/Artist/Track One.MP3", key, sizeof key, 1) >= 0 && !strcmp(key, "contents/artist/track one.mp3"));
    assert(sc_rbs_fold("..\\Contents\\A.mp3", key, sizeof key, 1) >= 0 && !strcmp(key, "contents/a.mp3"));
    assert(sc_rbs_fold("//Music/X.wav", key, sizeof key, 1) >= 0 && !strcmp(key, "music/x.wav"));
    assert(sc_rbs_fold("/../x", key, sizeof key, 1) >= 0 && !strcmp(key, "../x"));            // Go trims "../" before "/"
    assert(sc_rbs_fold("../../X", key, sizeof key, 1) >= 0 && !strcmp(key, "../x"));          // one "../" only
    assert(sc_rbs_fold("../Contents/Café.mp3", key, sizeof key, 1) < 0);                       // Unicode is Go's
    assert(sc_rbs_fold("../Contents/Café.mp3", key, sizeof key, 0) >= 0 && !strcmp(key, "Contents/Café.mp3"));
    assert(sc_rbs_fold("../Contents/a.mp3", key, 8, 1) < 0);                                   // does not fit

    // JSON as Go writes it: HTML characters as \u00XX, a surrogate pair, a quote, a backslash.
    const char *js = "\"Rock \\u0026 Roll \\\"Q\\\" \\\\ \\ud83c\\udfb5 \\u00e9\"";
    size_t len = 0;
    assert(sc_rbs_string(js, js + strlen(js), key, sizeof key, &len) == js + strlen(js));
    assert(!strcmp(key, "Rock & Roll \"Q\" \\ \xf0\x9f\x8e\xb5 \xc3\xa9") && len == strlen(key));
    const char *bad[] = {"\"cut", "\"\\x\"", "\"\\ud83c\"", "\"\\udfb5\"", "\"a\nb\""};
    for (unsigned i = 0; i < sizeof bad / sizeof *bad; i++) assert(!sc_rbs_string(bad[i], bad[i] + strlen(bad[i]), key, sizeof key, NULL));
    struct sc_rbs_line *l = malloc(sizeof *l);
    const char *line = "{\"k\":\"a\",\"sig\":{\"v\":5,\"w\":\"h\",\"z\":[{\"r\":\"x}]\",\"s\":1}]},\"rel\":\"A\",\"facts\":{\"dur\":1.5},\"wr\":\"d2E=\",\"deg\":\"\",\"perf\":true,\"active\":7}";
    assert(sc_rbs_parse_line(line, strlen(line), l) && l->put && !l->drop && !strcmp(l->key, "a") && !strcmp(l->rel, "A") && l->rules == 5 &&
           !strcmp(l->row, "h") && !strcmp(l->wr, "d2E=") && l->perf && !l->degraded && l->active == 7);
    line = "{\"drop\":\"a\"}";
    assert(sc_rbs_parse_line(line, strlen(line), l) && l->drop && !l->put && !strcmp(l->key, "a"));
    line = "{\"k\":\"a\",\"perf\":tru}";
    assert(!sc_rbs_parse_line(line, strlen(line), l));
    line = "{\"k\":\"a\",\"deg\":\"bad header\"}";
    assert(sc_rbs_parse_line(line, strlen(line), l) && l->degraded);
    free(l);

    const char *tmp = getenv("TMPDIR");
    snprintf(dir, sizeof dir, "%s/rbx-stage-XXXXXX", tmp && *tmp ? tmp : "/tmp");
    assert(mkdtemp(dir));
    char drives[96], root[96];
    snprintf(drives, sizeof drives, "%s/media/", dir);
    sc_rbx_drives = drives;
    snprintf(root, sizeof root, "%s/stage", dir);
    sc_rbs_root = root;
    sc_rbx_verbose = 1;

    sqlite3 *db = stick("REKORDBOX", "lib-a", "lib-a");
    long long t1 = track(db, "../Contents/Artist/Track One.MP3", 1, 1);
    assert(!strcmp(text(db, ROW, t1), INSERTED));

    // Not a drive: the caller leaves Engine's own library and any other database alone.
    sqlite3 *internal;
    char other[128];
    snprintf(other, sizeof other, "%s/m.db", dir);
    assert(sqlite3_open(other, &internal) == SQLITE_OK && !sc_rbs_drive(&engine, internal));
    sqlite3_close(internal);
    unlink(other);

    // Nothing staged on the deck: not one statement, not a word without the trace.
    statements = 0;
    assert(!strcmp(load(db, t1), "skipped (nothing staged on the deck)") && !statements);
    sc_rbx_verbose = 0;
    assert(!load(db, t1) && !statements);
    sc_rbx_verbose = 1;

    // A stage for the library, but no index yet: still no statement.
    char path[PATH_MAX];
    snprintf(path, sizeof path, "%s/lib-a", root);
    mkdirs(path);
    assert(!strcmp(load(db, t1), "skipped (nothing staged for this library)") && !statements);

    // The track staged, and its row as the insert left it: filled on Engine's connection, one
    // read and one write, the row the staged data exactly, the import mark gone, analysed.
    unsigned char bin[256];
    size_t bin_n = rbxa1(bin, "TD", "OV", "BT", "QC", "LP", 1);
    stage("lib-a", "contents/artist/track one.mp3", "contents/artist/track one.mp3", "Contents/Artist/Track One.MP3", 5, ",\"perf\":true,\"rgb\":true,\"active\":3", bin, bin_n);
    const char *r = load(db, t1);
    assert(starts(r, "filled (") && statements == 2);
    assert(!strcmp(text(db, ROW, t1), FILLED));
    // The record beside the index.
    snprintf(path, sizeof path, "%s/lib-a/%s", root, SC_RBS_JOURNAL);
    char journal[1024] = "";
    f = fopen(path, "r");
    assert(f && fgets(journal, sizeof journal, f) && !fclose(f));
    char want[512];
    snprintf(want, sizeof want, "{\"id\":%lld,\"k\":\"contents/artist/track one.mp3\",\"wr\":\"AAECAwQFBgcICQoLDA0ODw==\",\"v\":5,\"w\":\"row-lib-a\",\"at\":", t1);
    assert(starts(journal, want) && journal[strlen(journal) - 1] == '\n');
    // Without the trace a fill still says so.
    sc_rbx_verbose = 0;
    long long t5 = track(db, "../Contents/No Cues.wav", 1, 0);
    assert(!strcmp(text(db, ROW, t5), NOTHING));
    stage("lib-a", "contents/no cues.wav", "contents/no cues.wav", "Contents/No Cues.wav", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    statements = 0;
    assert(starts(load(db, t5), "filled (") && statements == 2 && !strcmp(text(db, ROW, t5), FILLED));
    sc_rbx_verbose = 1;

    // A row that has its analysis is never written again: one read.
    statements = 0;
    assert(!strcmp(load(db, t1), "skipped (row has analysis)") && statements == 1);

    // Rows the deck worked on, and Engine's own rows, stay as they are, staged or not.
    long long t2 = track(db, "../Contents/Edited.mp3", 1, 1);
    assert(sqlite3_exec(db, "UPDATE PerformanceData SET quickCues = X'ee', thirdPartySourceId = NULL WHERE trackId = (SELECT id FROM Track WHERE path = '../Contents/Edited.mp3')", NULL, NULL, NULL) == SQLITE_OK);
    long long t2b = track(db, "../Contents/Edited loop.mp3", 1, 0);
    assert(sqlite3_exec(db, "UPDATE PerformanceData SET loops = X'ee' WHERE trackId = (SELECT id FROM Track WHERE path = '../Contents/Edited loop.mp3')", NULL, NULL, NULL) == SQLITE_OK);
    long long t3 = track(db, "../Contents/Analysed.mp3", 1, 1);
    assert(sqlite3_exec(db, "UPDATE PerformanceData SET trackData = X'01', thirdPartySourceId = NULL WHERE trackId = (SELECT id FROM Track WHERE path = '../Contents/Analysed.mp3')", NULL, NULL, NULL) == SQLITE_OK);
    long long t3b = track(db, "../Contents/Grid only.mp3", 1, 1);
    assert(sqlite3_exec(db, "UPDATE PerformanceData SET beatData = X'01' WHERE trackId = (SELECT id FROM Track WHERE path = '../Contents/Grid only.mp3')", NULL, NULL, NULL) == SQLITE_OK);
    long long t4 = track(db, "../Contents/Engine made.mp3", 0, 0);
    const struct { long long id; const char *key, *rel, *result; } kept[] = {
        {t2, "contents/edited.mp3", "Contents/Edited.mp3", "skipped (row has the deck's cues)"},
        {t2b, "contents/edited loop.mp3", "Contents/Edited loop.mp3", "skipped (row has the deck's cues)"},
        {t3, "contents/analysed.mp3", "Contents/Analysed.mp3", "skipped (row has analysis)"},
        {t3b, "contents/grid only.mp3", "Contents/Grid only.mp3", "skipped (row has analysis)"},
        {t4, "contents/engine made.mp3", "Contents/Engine made.mp3", "skipped (Engine's own row)"},
    };
    for (unsigned i = 0; i < sizeof kept / sizeof *kept; i++) {
        stage("lib-a", kept[i].key, kept[i].key, kept[i].rel, 5, ",\"perf\":true", bin, bin_n);
        char before[4096];
        snprintf(before, sizeof before, "%s", text(db, ROW, kept[i].id));
        statements = 0;
        assert(!strcmp(load(db, kept[i].id), kept[i].result) && statements == 1);
        assert(!strcmp(text(db, ROW, kept[i].id), before));
    }

    // Not staged, and staged entries that are not whole: the row is left to Engine's analysis.
    long long t6 = track(db, "../Contents/Not staged.mp3", 1, 1);
    statements = 0;
    assert(!strcmp(load(db, t6), "skipped (not staged)") && statements == 1);
    unsigned char cut[256], open_[256], nogrid[256], magic[256];
    size_t cut_n = rbxa1(cut, "TD", "OV", "BT", "QC", "LP", 1) - 3;
    size_t open_n = rbxa1(open_, "TD", "OV", "BT", "QC", "LP", 0);
    size_t nogrid_n = rbxa1(nogrid, "TD", "OV", "", "QC", "LP", 1);
    size_t magic_n = rbxa1(magic, "TD", "OV", "BT", "QC", "LP", 1);
    magic[4] = '0';
    const struct { const char *name; int rules; const char *extra; const unsigned char *bin; size_t n; const char *result; } partial[] = {
        {"no file", 5, ",\"perf\":true", NULL, 0, "skipped (no staged file)"},
        {"cut", 5, ",\"perf\":true", cut, cut_n, "skipped (staged file cut short)"},
        {"open", 5, ",\"perf\":true", open_, open_n, "skipped (staged file not complete)"},
        {"no grid", 5, ",\"perf\":true", nogrid, nogrid_n, "skipped (staged file no grid or track data)"},
        {"magic", 5, ",\"perf\":true", magic, magic_n, "skipped (staged file not an RBXA1 file)"},
        {"no perf", 5, "", bin, bin_n, "skipped (staged without analysis)"},
        {"degraded", 5, ",\"deg\":\"bad audio header\",\"perf\":true", bin, bin_n, "skipped (staged without analysis)"},
        {"missing", 5, ",\"missing\":true,\"perf\":true", bin, bin_n, "skipped (staged without analysis)"},
        {"old rules", 4, ",\"perf\":true", bin, bin_n, "skipped (staged by other rules)"},
    };
    for (unsigned i = 0; i < sizeof partial / sizeof *partial; i++) {
        char p[128], k[128], rel[128];
        snprintf(p, sizeof p, "../Contents/Partial %s.mp3", partial[i].name);
        snprintf(k, sizeof k, "contents/partial %s.mp3", partial[i].name);
        snprintf(rel, sizeof rel, "Contents/Partial %s.mp3", partial[i].name);
        long long id = track(db, p, 1, 1);
        stage("lib-a", k, k, rel, partial[i].rules, partial[i].extra, partial[i].bin, partial[i].n);
        statements = 0;
        r = load(db, id);
        assert(r && !strcmp(r, partial[i].result) && statements == 1);
        assert(!strcmp(text(db, ROW, id), INSERTED));
    }

    // A window applied an entry (a drop line): it is not staged any more.
    long long t7 = track(db, "../Contents/Applied.mp3", 1, 1);
    stage("lib-a", "contents/applied.mp3", "contents/applied.mp3", "Contents/Applied.mp3", 5, ",\"perf\":true", bin, bin_n);
    stage_line("lib-a", "{\"drop\":\"contents/applied.mp3\"}\n");
    assert(!strcmp(load(db, t7), "skipped (not staged)"));
    // A line the job is still writing is not read; once it is whole it is.
    long long t8 = track(db, "../Contents/Torn.mp3", 1, 1);
    char hex8[41];
    sc_rbs_sha1_hex("contents/torn.mp3", 17, hex8);
    snprintf(path, sizeof path, "%s/lib-a/%s.bin", root, hex8);
    put_file(path, bin, bin_n, "wb");
    stage_line("lib-a", "{\"k\":\"contents/torn.mp3\",\"sig\":{\"v\":5,\"w\":\"row\"},\"rel\":\"Contents/Torn.mp3\",\"wr\":\"AAEC\",\"perf\":true,\"act");
    assert(!strcmp(load(db, t8), "skipped (not staged)"));
    stage_line("lib-a", "ive\":3}\n");
    assert(starts(load(db, t8), "filled (") && !strcmp(text(db, ROW, t8), FILLED));
    // The last line of a key wins: staged again with other data.
    long long t9 = track(db, "../Contents/Again.mp3", 1, 1);
    unsigned char first[256];
    size_t first_n = rbxa1(first, "T0", "O0", "B0", "Q0", "L0", 1);
    stage("lib-a", "contents/again.mp3", "contents/again.mp3", "Contents/Again.mp3", 5, ",\"perf\":true,\"active\":1", first, first_n);
    stage("lib-a", "contents/again.mp3", "contents/again.mp3", "Contents/Again.mp3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    assert(starts(load(db, t9), "filled (") && !strcmp(text(db, ROW, t9), FILLED));

    // Keys Go escapes in JSON, and the case of an ASCII path.
    long long t10 = track(db, "../CONTENTS/Rock & Roll/\"Q\" <Mix>.mp3", 1, 1);
    stage("lib-a", "contents/rock \\u0026 roll/\\\"q\\\" \\u003cmix\\u003e.mp3", "contents/rock & roll/\"q\" <mix>.mp3",
          "CONTENTS/Rock \\u0026 Roll/\\\"Q\\\" \\u003cMix\\u003e.mp3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    assert(starts(load(db, t10), "filled (") && !strcmp(text(db, ROW, t10), FILLED));
    // A non-ASCII path is found by the volume's spelling ("rel"); the key is Go's fold.
    long long t11 = track(db, "../Contents/Caf\xc3\xa9/\xc3\x89t\xc3\xa9.mp3", 1, 1);
    stage("lib-a", "contents/caf\\u00e9/\\u00e9t\\u00e9.mp3", "contents/caf\xc3\xa9/\xc3\xa9t\xc3\xa9.mp3", "Contents/Caf\xc3\xa9/\xc3\x89t\xc3\xa9.mp3", 5,
          ",\"perf\":true,\"active\":3", bin, bin_n);
    assert(starts(load(db, t11), "filled (") && !strcmp(text(db, ROW, t11), FILLED));
    // A row whose spelling is not the volume's (NFD against NFC here) is not found.
    long long t12 = track(db, "../Contents/Cafe\xcc\x81.mp3", 1, 1);
    stage("lib-a", "contents/caf\\u00e9.mp3", "contents/caf\xc3\xa9.mp3", "Contents/Caf\xc3\xa9.mp3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    assert(!strcmp(load(db, t12), "skipped (not staged)") && !strcmp(text(db, ROW, t12), INSERTED));

    // The row changes between the hook's read and its write: the write's own conditions keep
    // what Engine wrote.
    long long t13 = track(db, "../Contents/Race.mp3", 1, 1);
    stage("lib-a", "contents/race.mp3", "contents/race.mp3", "Contents/Race.mp3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    char race[256];
    snprintf(race, sizeof race, "UPDATE PerformanceData SET quickCues = X'dd', thirdPartySourceId = NULL WHERE trackId = %lld", t13);
    statements = 0;
    meanwhile_at = 2; // after the hook's read, before its write
    meanwhile = race;
    assert(!strcmp(load(db, t13), "skipped (row changed)") && statements == 2 && !meanwhile);
    assert(!strcmp(text(db, ROW, t13), "///DD/10/0/n/0"));

    // A snapshot copied from another library: no write, and the snapshot is read again next time.
    sqlite3 *copy = stick("COPY", "lib-b", "lib-a");
    long long c1 = track(copy, "../Contents/Artist/Track One.MP3", 1, 1);
    statements = 0;
    assert(!strcmp(load(copy, c1), "skipped (the snapshot names another library)") && statements == 1);
    assert(!strcmp(text(copy, ROW, c1), INSERTED));
    // A stick without a bridge snapshot (not a bridge stick): no statement, then or later.
    sqlite3 *plain = stick("PLAIN", "lib-c", NULL);
    long long p1 = track(plain, "../Contents/Artist/Track One.MP3", 1, 1);
    statements = 0;
    assert(!strcmp(load(plain, p1), "skipped (no bridge snapshot)") && !statements);
    assert(!strcmp(load(plain, p1), "skipped (no bridge snapshot)") && !statements);
    sqlite3_close(plain);
    sqlite3_close(copy);

    // A window applied everything: the index starts again with only its mark; nothing to do,
    // and no statement.
    snprintf(path, sizeof path, "%s/lib-a/%s", root, SC_RBS_INDEX);
    unlink(path);
    stage_line("lib-a", "{\"verified\":\"stamp\"}\n");
    long long t14 = track(db, "../Contents/Late.mp3", 1, 1);
    statements = 0;
    assert(!strcmp(load(db, t14), "skipped (nothing staged for this library)") && !statements);

    // A big stage: 20,000 tracks, then one more line, read on its own.
    snprintf(path, sizeof path, "%s/lib-a/%s", root, SC_RBS_INDEX);
    f = fopen(path, "a");
    assert(f);
    for (int i = 0; i < 20000; i++)
        fprintf(f, "{\"k\":\"contents/bulk/%05d.mp3\",\"sig\":{\"v\":5,\"w\":\"r\"},\"rel\":\"Contents/Bulk/%05d.mp3\",\"wr\":\"AAEC\",\"perf\":true}\n", i, i);
    assert(!fclose(f));
    struct timespec a, b;
    clock_gettime(CLOCK_MONOTONIC, &a);
    assert(!strcmp(load(db, t14), "skipped (not staged)"));
    clock_gettime(CLOCK_MONOTONIC, &b);
    struct sc_rbs_stage *s = sc_rbs_stage_of("lib-a");
    assert(s && s->live == 20000);
    const off_t parsed = s->parsed;
    stage("lib-a", "contents/late.mp3", "contents/late.mp3", "Contents/Late.mp3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    assert(starts(load(db, t14), "filled (") && !strcmp(text(db, ROW, t14), FILLED));
    assert(sc_rbs_stage_of("lib-a")->live == 20001 && sc_rbs_stage_of("lib-a")->parsed > parsed);
    printf("index of 20,000 lines read in %.1f ms\n", (double)(b.tv_sec - a.tv_sec) * 1e3 + (double)(b.tv_nsec - a.tv_nsec) / 1e6);

    sqlite3_close(db);
    char rm[160];
    snprintf(rm, sizeof rm, "rm -rf '%s'", dir);
    assert(!system(rm));
    puts("rekordbox stage fill: PASS");
    return 0;
}
