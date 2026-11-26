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
    // The hook's own lock is free while Engine's connection runs a statement: Engine has a pool of
    // connections per drive, and a thread that holds the write lock on one of them may be on its
    // way into the hook.
    assert(pthread_mutex_trylock(&sc_rbs_mu) == 0);
    pthread_mutex_unlock(&sc_rbs_mu);
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

static const char *load_path(sqlite3 *db, const char *p, int n, long long *out_id) {
    const char *main = sc_rbs_drive(&engine, db);
    assert(main);
    return sc_rbs_fill_path(&engine, db, main, p, n, out_id);
}

struct lock_holder {
    sqlite3 *db;
    long long id;
    int locked;
};

static int exec_raw(void *db, const char *sql, int (*row)(void *, int, char **, char **), void *arg, char **err) {
    return sqlite3_exec(db, sql, row, arg, err);
}
static const struct sc_rbx_sqlite raw_engine = {exec_raw, db_main};

static void *hold_and_fill(void *arg) {
    struct lock_holder *h = arg;
    assert(sqlite3_exec(h->db, "BEGIN IMMEDIATE", NULL, NULL, NULL) == SQLITE_OK);
    __atomic_store_n(&h->locked, 1, __ATOMIC_RELEASE);
    struct timespec ts = {0, 25000000L}; // 25 ms
    nanosleep(&ts, NULL);
    const char *main = sc_rbs_drive(&raw_engine, h->db);
    assert(main && sc_rbs_fill_track(&raw_engine, h->db, main, h->id) != NULL);
    for (int i = 0; i < 40; i++) {
        int rc = sqlite3_exec(h->db, "COMMIT", NULL, NULL, NULL);
        if (rc == SQLITE_OK) return NULL;
        assert(rc == SQLITE_BUSY);
        struct timespec retry = {0, 5000000L};
        nanosleep(&retry, NULL);
    }
    assert(!"COMMIT timed out");
    return NULL;
}

static void *hold_shared_lock(void *arg) {
    struct lock_holder *h = arg;
    assert(sqlite3_exec(h->db, "BEGIN", NULL, NULL, NULL) == SQLITE_OK);
    sqlite3_stmt *st = NULL;
    assert(sqlite3_prepare_v2(h->db, "SELECT id FROM Track WHERE id = ?", -1, &st, NULL) == SQLITE_OK);
    assert(sqlite3_bind_int64(st, 1, h->id) == SQLITE_OK);
    assert(sqlite3_step(st) == SQLITE_ROW);
    __atomic_store_n(&h->locked, 1, __ATOMIC_RELEASE);
    struct timespec ts = {0, 25000000L}; // 25 ms
    nanosleep(&ts, NULL);
    sqlite3_finalize(st);
    assert(sqlite3_exec(h->db, "COMMIT", NULL, NULL, NULL) == SQLITE_OK);
    return NULL;
}

static int starts(const char *s, const char *prefix) { return s && !strncmp(s, prefix, strlen(prefix)); }

static int fake_sched_rr(void) { return SCHED_RR; }

struct od_mock {
    int listen_fd;
    int stop;
    int requests;
    int status;
    int delay_ms;
    char key[256];
    char wr[64];
    char wrnm[64];
    char row[64];
    const unsigned char *body;
    size_t body_n;
    char last_req[2048];
    pthread_mutex_t mu;
};

static void *od_mock_serve(void *arg) {
    struct od_mock *m = arg;
    for (;;) {
        struct pollfd pfd = {m->listen_fd, POLLIN, 0};
        int rc = poll(&pfd, 1, 50);
        if (__atomic_load_n(&m->stop, __ATOMIC_ACQUIRE)) break;
        if (rc <= 0) continue;
        int cfd = accept(m->listen_fd, NULL, NULL);
        if (cfd < 0) continue;
        char reqbuf[2048];
        memset(reqbuf, 0, sizeof reqbuf);
        ssize_t n = recv(cfd, reqbuf, sizeof reqbuf - 1, 0);
        if (n < 0) n = 0;
        reqbuf[n] = 0;

        pthread_mutex_lock(&m->mu);
        m->requests++;
        snprintf(m->last_req, sizeof m->last_req, "%s", reqbuf);
        int status = m->status;
        int delay_ms = m->delay_ms;
        char key[256], wr[64], wrnm[64], row[64];
        snprintf(key, sizeof key, "%s", m->key);
        snprintf(wr, sizeof wr, "%s", m->wr);
        snprintf(wrnm, sizeof wrnm, "%s", m->wrnm);
        snprintf(row, sizeof row, "%s", m->row);
        const unsigned char *body = m->body;
        size_t body_n = m->body_n;
        pthread_mutex_unlock(&m->mu);

        if (delay_ms > 0) {
            struct timespec ts = {delay_ms / 1000, (long)(delay_ms % 1000) * 1000000L};
            nanosleep(&ts, NULL);
        }
        char hdr[1024];
        int hlen = 0;
        if (status == 200) {
            hlen = snprintf(hdr, sizeof hdr,
                            "HTTP/1.1 200 OK\r\n"
                            "Content-Type: application/octet-stream\r\n"
                            "Content-Length: %zu\r\n"
                            "X-RB-Key: %s\r\n"
                            "X-RB-Written: %s\r\n"
                            "X-RB-Written-NoMain: %s\r\n"
                            "X-RB-Row: %s\r\n"
                            "X-RB-Active-Loops: 3\r\n"
                            "X-RB-Rules: 5\r\n"
                            "Connection: close\r\n\r\n",
                            body_n, key, wr, wrnm, row);
            (void)!send(cfd, hdr, (size_t)hlen, MSG_NOSIGNAL);
            if (body && body_n) (void)!send(cfd, body, body_n, MSG_NOSIGNAL);
        } else {
            hlen = snprintf(hdr, sizeof hdr,
                            "HTTP/1.1 %d Error\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}",
                            status);
            (void)!send(cfd, hdr, (size_t)hlen, MSG_NOSIGNAL);
        }
        close(cfd);
    }
    return NULL;
}

struct raw_http_arg {
    int listen_fd;
    const char *reply;
};

static void *raw_http_once(void *arg) {
    struct raw_http_arg *a = arg;
    int cfd = accept(a->listen_fd, NULL, NULL);
    if (cfd >= 0) {
        char reqb[1024];
        (void)!recv(cfd, reqb, sizeof reqb, 0);
        (void)!send(cfd, a->reply, strlen(a->reply), MSG_NOSIGNAL);
        close(cfd);
    }
    return NULL;
}

struct od_race_arg {
    sqlite3 *db;
    const char *path;
    long long out_id;
    char res[96];
    int set_gui_name;
    long long perf_id;
};

static void *od_race_worker(void *arg) {
    struct od_race_arg *a = arg;
#ifdef __linux__
    if (a->set_gui_name == 1) prctl(PR_SET_NAME, (unsigned long)"SceneGraph", 0, 0, 0);
    if (a->set_gui_name == 2) prctl(PR_SET_NAME, (unsigned long)"Thread (pooled)", 0, 0, 0);
#endif
    const char *r = a->perf_id > 0
                        ? sc_rbs_fill_perf(&raw_engine, a->db, sqlite3_db_filename(a->db, "main"), a->perf_id)
                        : sc_rbs_fill_path(&raw_engine, a->db, sqlite3_db_filename(a->db, "main"), a->path, -1, &a->out_id);
    snprintf(a->res, sizeof a->res, "%s", r ? r : "");
    return NULL;
}

static const char *load_perf(sqlite3 *db, long long id) {
    const char *main = sc_rbs_drive(&engine, db);
    assert(main);
    return sc_rbs_fill_perf(&engine, db, main, id);
}

struct gui_guard_arg {
    const char *thr_name;
    sqlite3 *db;
    const char *main_db;
    const char *path;
    long long perf_id;
    int do_track_fill;
    const char *want_res;
    int ok;
};

static void *counted_gui_guard_worker(void *arg) {
    struct gui_guard_arg *a = arg;
#ifdef __linux__
    prctl(PR_SET_NAME, (unsigned long)a->thr_name, 0, 0, 0);
#endif
    long long out_id = 0;
    const char *r = NULL;
    if (a->do_track_fill) {
        r = sc_rbs_fill_track(&engine, a->db, a->main_db, a->perf_id);
    } else if (a->perf_id > 0) {
        r = sc_rbs_fill_perf(&engine, a->db, a->main_db, a->perf_id);
    } else {
        r = sc_rbs_fill_path(&raw_engine, a->db, a->main_db, a->path, -1, &out_id);
    }
    a->ok = (r && !strcmp(r, a->want_res));
    return NULL;
}

struct raw_bytes_arg {
    int listen_fd;
    const char *reply;
    size_t reply_len;
    size_t chunk_len;
    int chunk_delay_ms;
    int stop;
};

static void *raw_bytes_once(void *arg) {
    struct raw_bytes_arg *a = arg;
    int cfd = accept(a->listen_fd, NULL, NULL);
    if (cfd >= 0) {
        char reqb[1024];
        (void)!recv(cfd, reqb, sizeof reqb, 0);
        size_t sent = 0;
        while (sent < a->reply_len && !__atomic_load_n(&a->stop, __ATOMIC_ACQUIRE)) {
            size_t step = a->reply_len - sent;
            if (a->chunk_len > 0 && step > a->chunk_len) step = a->chunk_len;
            ssize_t w = send(cfd, a->reply + sent, step, MSG_NOSIGNAL);
            if (w <= 0) break;
            sent += (size_t)w;
            if (a->chunk_delay_ms > 0 && sent < a->reply_len) {
                struct timespec ts = {0, (long)a->chunk_delay_ms * 1000000L};
                nanosleep(&ts, NULL);
            }
        }
        close(cfd);
    }
    return NULL;
}

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
    sc_rbs_ondemand_sock = "";
    sc_rbs_ondemand_url = NULL;
    sc_rbs_ondemand_env_init = 1;
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
    snprintf(want, sizeof want, "{\"id\":%lld,\"k\":\"contents/artist/track one.mp3\",\"wr\":\"AAECAwQFBgcICQoLDA0ODw==\",\"al\":3,\"v\":5,\"w\":\"row-lib-a\",\"at\":", t1);
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
    // A stick without a bridge snapshot (not a bridge stick): no statement, then or later; the
    // stick is not read again for sc_rbs_recheck seconds, and after that a snapshot is seen.
    sqlite3 *plain = stick("PLAIN", "lib-c", NULL);
    long long p1 = track(plain, "../Contents/Artist/Track One.MP3", 1, 1);
    statements = 0;
    assert(!strcmp(load(plain, p1), "skipped (no bridge snapshot)") && !statements);
    sqlite3_close(plain);
    plain = stick("PLAIN", "lib-a", "lib-a"); // the same path, now a bridge stick
    p1 = track(plain, "../Contents/Artist/Track One.MP3", 1, 1);
    stage("lib-a", "contents/artist/track one.mp3", "contents/artist/track one.mp3", "Contents/Artist/Track One.MP3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    for (unsigned i = 0; i < SC_RBS_CONNS; i++) // Engine's new connection may have the old one's address
        if (sc_rbs_conns[i].known < 0) sc_rbs_conns[i].db = plain;
    assert(!strcmp(load(plain, p1), "skipped (no bridge snapshot)") && !statements);
    sc_rbs_recheck = 0;
    assert(starts(load(plain, p1), "filled (") && statements == 2 && !strcmp(text(plain, ROW, p1), FILLED));
    sc_rbs_recheck = 30;
    sqlite3_close(plain);
    // The header line is longer than the hook reads (many export files): the uuid comes first.
    char lib[256], head[8192];
    snprintf(lib, sizeof lib, "%s/media/LONG/Engine Library/Database2", dir);
    sqlite3 *longhead = stick("LONG", "lib-a", NULL);
    int at = snprintf(head, sizeof head, "RBS2 {\"format\":1,\"rules\":5,\"uuid\":\"lib-a\",\"stamp\":\"s\",\"layout\":\"folders\",\"sources\":[");
    for (int i = 0; i < 100; i++) at += snprintf(head + at, sizeof head - (size_t)at, "%s{\"r\":\"PIONEER/rekordbox/export%03d.pdb\",\"s\":1,\"m\":2}", i ? "," : "", i);
    at += snprintf(head + at, sizeof head - (size_t)at, "]}\nE 0 0\n");
    assert(at > 4096 && (size_t)at < sizeof head);
    snprintf(path, sizeof path, "%s/%s", lib, SC_RBS_SNAPSHOT);
    put_file(path, head, (size_t)at, "wb");
    long long l1 = track(longhead, "../Contents/Artist/Track One.MP3", 1, 1);
    statements = 0;
    assert(starts(load(longhead, l1), "filled (") && statements == 2);
    sqlite3_close(longhead);
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
    pthread_mutex_lock(&sc_rbs_mu);
    struct sc_rbs_stage *s = sc_rbs_stage_of("lib-a");
    assert(s && s->live == 20000);
    const off_t parsed = s->parsed;
    pthread_mutex_unlock(&sc_rbs_mu);
    stage("lib-a", "contents/late.mp3", "contents/late.mp3", "Contents/Late.mp3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    assert(starts(load(db, t14), "filled (") && !strcmp(text(db, ROW, t14), FILLED));
    pthread_mutex_lock(&sc_rbs_mu);
    assert(sc_rbs_stage_of("lib-a")->live == 20001 && sc_rbs_stage_of("lib-a")->parsed > parsed);
    pthread_mutex_unlock(&sc_rbs_mu);
    printf("index of 20,000 lines read in %.1f ms\n", (double)(b.tv_sec - a.tv_sec) * 1e3 + (double)(b.tv_nsec - a.tv_nsec) / 1e6);

    // Path-based read matcher (WHERE t.path = ? [OR t.path = ?]).
    assert(sc_rbs_path_bound(SC_RBS_TRACK_READ "t.isAnalyzed FROM Track AS t WHERE t.path = ? OR t.path = ?") == 2);
    assert(sc_rbs_path_bound(SC_RBS_TRACK_READ "t.isAnalyzed FROM Track AS t WHERE t.path = ? ; ") == 1);
    assert(sc_rbs_path_bound(SC_RBS_TRACK_READ "t.isAnalyzed FROM Track AS t WHERE t.id = ?") == 0);
    assert(sc_rbs_path_bound("SELECT * FROM Track AS t WHERE t.path = ? OR t.path = ?") == 0);
    assert(sc_rbs_path_bound(SC_RBS_TRACK_READ "t.isAnalyzed FROM Track AS t WHERE t.id = ? AND t.path = ?") == 0);
    assert(sc_rbs_path_bound(SC_RBS_TRACK_READ "t.isAnalyzed FROM Track AS t WHERE t.path = ? OR t.path = ? LIMIT 1") == 0);

    // Path-based fill: unstaged path costs 0 SQL statements; staged ASCII (n=-1 and non-NUL n>0),
    // quoted path, non-ASCII path, and missing DB row all behave cleanly.
    long long t15 = track(db, "../Contents/PathUnstaged.mp3", 1, 1);
    long long out_id = -1;
    statements = 0;
    assert(!strcmp(load_path(db, "../Contents/PathUnstaged.mp3", -1, &out_id), "skipped (not staged)") && !statements);
    assert(!strcmp(text(db, ROW, t15), INSERTED));

    long long t16 = track(db, "../Contents/PathOne.mp3", 1, 1);
    stage("lib-a", "contents/pathone.mp3", "contents/pathone.mp3", "Contents/PathOne.mp3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    statements = 0;
    out_id = 0;
    assert(starts(load_path(db, "../Contents/PathOne.mp3", -1, &out_id), "filled (") && statements == 2 && out_id == t16);
    assert(!strcmp(text(db, ROW, t16), FILLED));

    long long t17 = track(db, "../Contents/O'Brien.mp3", 1, 1);
    stage("lib-a", "contents/o'brien.mp3", "contents/o'brien.mp3", "Contents/O'Brien.mp3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    const char non_nul[] = "../Contents/O'Brien.mp3EXTRA";
    statements = 0;
    out_id = 0;
    assert(starts(load_path(db, non_nul, 23, &out_id), "filled (") && statements == 2 && out_id == t17);
    assert(!strcmp(text(db, ROW, t17), FILLED));

    long long t18 = track(db, "../Contents/Caf\xc3\xa9/Track.mp3", 1, 1);
    stage("lib-a", "contents/caf\\u00e9/track.mp3", "contents/caf\xc3\xa9/track.mp3", "Contents/Caf\xc3\xa9/Track.mp3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    statements = 0;
    out_id = 0;
    assert(starts(load_path(db, "../Contents/Caf\xc3\xa9/Track.mp3", -1, &out_id), "filled (") && statements == 2 && out_id == t18);
    assert(!strcmp(text(db, ROW, t18), FILLED));

    stage("lib-a", "contents/missing-row.mp3", "contents/missing-row.mp3", "Contents/Missing-Row.mp3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    assert(!strcmp(load_path(db, "../Contents/Missing-Row.mp3", -1, NULL), "skipped (no row)"));

    // Atomic savepoint: if UPDATE Track fails, UPDATE PerformanceData must also roll back
    // (never exposing filled PerformanceData with isAnalyzed = 0) and leave autocommit clean.
    long long t19 = track(db, "../Contents/Atomic.mp3", 1, 1);
    stage("lib-a", "contents/atomic.mp3", "contents/atomic.mp3", "Contents/Atomic.mp3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    char trig[256];
    snprintf(trig, sizeof trig, "CREATE TEMP TRIGGER fail_track BEFORE UPDATE ON Track WHEN NEW.id = %lld BEGIN SELECT RAISE(ABORT, 'boom'); END;", t19);
    assert(sqlite3_exec(db, trig, NULL, NULL, NULL) == SQLITE_OK);
    assert(!strcmp(load(db, t19), "update failed"));
    assert(!strcmp(text(db, ROW, t19), INSERTED));
    assert(sqlite3_get_autocommit(db) == 1);
    assert(sqlite3_exec(db, "DROP TRIGGER fail_track;", NULL, NULL, NULL) == SQLITE_OK);
    assert(starts(load(db, t19), "filled (") && !strcmp(text(db, ROW, t19), FILLED));

    // Concurrent second connection with busy_timeout=0 during an in-flight fill inside an outer
    // transaction: retries past SQLITE_BUSY, sees the committed atomic fill, leaves autocommit clean.
    long long t20 = track(db, "../Contents/RacePath.mp3", 1, 1);
    stage("lib-a", "contents/racepath.mp3", "contents/racepath.mp3", "Contents/RacePath.mp3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    sqlite3 *db2 = NULL;
    assert(sqlite3_open(sqlite3_db_filename(db, "main"), &db2) == SQLITE_OK);
    assert(sqlite3_busy_timeout(db2, 0) == SQLITE_OK);
    struct lock_holder holder = {db, t20, 0};
    pthread_t th;
    assert(!pthread_create(&th, NULL, hold_and_fill, &holder));
    while (!__atomic_load_n(&holder.locked, __ATOMIC_ACQUIRE)) {
        struct timespec ts = {0, 1000000L};
        nanosleep(&ts, NULL);
    }
    const char *r2 = load_path(db2, "../Contents/RacePath.mp3", -1, &out_id);
    assert(r2 && (!strcmp(r2, "skipped (row changed)") || !strcmp(r2, "skipped (row has analysis)")));
    assert(sqlite3_get_autocommit(db2) == 1);
    assert(!pthread_join(th, NULL));
    assert(!strcmp(text(db2, ROW, t20), FILLED));
    sqlite3_close(db2);

    // Path-based fill writes Track.id (from the row) to filled.jsonl, and index lines with
    // optional "id" between "k" and "sig" (R1) still populate "rel" for non-ASCII paths.
    char jpath[PATH_MAX], jline[1024], jwant[256];
    snprintf(jpath, sizeof jpath, "%s/lib-a/%s", root, SC_RBS_JOURNAL);
    snprintf(jwant, sizeof jwant, "{\"id\":%lld,\"k\":\"contents/pathone.mp3\",", t16);
    FILE *jf = fopen(jpath, "r");
    int found_j = 0;
    assert(jf);
    while (fgets(jline, sizeof jline, jf))
        if (starts(jline, jwant)) found_j = 1;
    assert(!fclose(jf) && found_j);

    long long t21 = track(db, "../Contents/R1-Caf\xc3\xa9.mp3", 1, 1);
    {
        char r1line[1024], r1hex[41], r1bin[PATH_MAX];
        const char *r1key = "contents/r1-caf\xc3\xa9.mp3";
        snprintf(r1line, sizeof r1line,
                 "{\"k\":\"contents/r1-caf\\u00e9.mp3\",\"id\":%lld,\"sig\":{\"v\":5,\"w\":\"row-lib-a\",\"a\":{\"r\":\"Contents/R1-Caf\xc3\xa9.mp3\",\"s\":1,\"m\":1},\"z\":[],\"c\":{\"r\":\"\",\"s\":0,\"m\":0}},"
                 "\"rel\":\"Contents/R1-Caf\xc3\xa9.mp3\",\"wr\":\"AAECAwQFBgcICQoLDA0ODw==\",\"perf\":true,\"active\":3}\n",
                 t21);
        stage_line("lib-a", r1line);
        sc_rbs_sha1_hex(r1key, strlen(r1key), r1hex);
        snprintf(r1bin, sizeof r1bin, "%s/lib-a/%s.bin", sc_rbs_root, r1hex);
        put_file(r1bin, bin, bin_n, "wb");
    }
    statements = 0;
    out_id = 0;
    assert(starts(load_path(db, "../Contents/R1-Caf\xc3\xa9.mp3", -1, &out_id), "filled (") && statements == 2 && out_id == t21);
    assert(!strcmp(text(db, ROW, t21), FILLED));

    // Non-WAL (DELETE journal) RELEASE SQLITE_BUSY recovery with extended result codes: a concurrent
    // SHARED reader blocks commit of SAVEPOINT sc_rbs; rollback must not leak an open savepoint.
    assert(sqlite3_exec(db, "PRAGMA journal_mode=DELETE;", NULL, NULL, NULL) == SQLITE_OK);
    assert(sqlite3_busy_timeout(db, 0) == SQLITE_OK);
    assert(sqlite3_extended_result_codes(db, 1) == SQLITE_OK);
    long long t22 = track(db, "../Contents/BusyRelease.mp3", 1, 1);
    stage("lib-a", "contents/busyrelease.mp3", "contents/busyrelease.mp3", "Contents/BusyRelease.mp3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    sqlite3 *db_r = NULL;
    assert(sqlite3_open(sqlite3_db_filename(db, "main"), &db_r) == SQLITE_OK);
    assert(sqlite3_busy_timeout(db_r, 0) == SQLITE_OK);
    struct lock_holder sh = {db_r, t22, 0};
    assert(!pthread_create(&th, NULL, hold_shared_lock, &sh));
    while (!__atomic_load_n(&sh.locked, __ATOMIC_ACQUIRE)) {
        struct timespec ts = {0, 1000000L};
        nanosleep(&ts, NULL);
    }
    out_id = 0;
    assert(starts(load_path(db, "../Contents/BusyRelease.mp3", -1, &out_id), "filled (") && out_id == t22);
    assert(sqlite3_get_autocommit(db) == 1);
    assert(!pthread_join(th, NULL));
    assert(!strcmp(text(db_r, ROW, t22), FILLED));
    sqlite3_close(db_r);

    // Non-zero busy_timeout (as tune_database sets on decks) must NOT be multiplied by 40 retries:
    // wall-clock deadline bounds both sc_rbs_exec_write and sc_rbs_exec_read and leaves autocommit clean.
    long long t25 = track(db, "../Contents/BusyCap.mp3", 1, 1);
    stage("lib-a", "contents/busycap.mp3", "contents/busycap.mp3", "Contents/BusyCap.mp3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    sqlite3 *db_b = NULL;
    assert(sqlite3_open(sqlite3_db_filename(db, "main"), &db_b) == SQLITE_OK);
    assert(sqlite3_busy_timeout(db_b, 120) == SQLITE_OK);
    assert(sqlite3_extended_result_codes(db_b, 1) == SQLITE_OK);
    assert(sqlite3_exec(db, "BEGIN IMMEDIATE", NULL, NULL, NULL) == SQLITE_OK);
    clock_gettime(CLOCK_MONOTONIC, &a);
    assert(!strcmp(load_path(db_b, "../Contents/BusyCap.mp3", -1, &out_id), "update failed"));
    clock_gettime(CLOCK_MONOTONIC, &b);
    double w_ms = (double)(b.tv_sec - a.tv_sec) * 1e3 + (double)(b.tv_nsec - a.tv_nsec) / 1e6;
    assert(w_ms < 600.0 && sqlite3_get_autocommit(db_b) == 1);
    assert(sqlite3_exec(db, "ROLLBACK", NULL, NULL, NULL) == SQLITE_OK);
    assert(sqlite3_exec(db, "BEGIN EXCLUSIVE", NULL, NULL, NULL) == SQLITE_OK);
    clock_gettime(CLOCK_MONOTONIC, &a);
    assert(!strcmp(load_path(db_b, "../Contents/BusyCap.mp3", -1, &out_id), "skipped (no row)"));
    clock_gettime(CLOCK_MONOTONIC, &b);
    double r_ms = (double)(b.tv_sec - a.tv_sec) * 1e3 + (double)(b.tv_nsec - a.tv_nsec) / 1e6;
    assert(r_ms < 600.0 && sqlite3_get_autocommit(db_b) == 1);
    assert(sqlite3_exec(db, "ROLLBACK", NULL, NULL, NULL) == SQLITE_OK);
    out_id = 0;
    assert(starts(load_path(db_b, "../Contents/BusyCap.mp3", -1, &out_id), "filled (") && out_id == t25);
    assert(!strcmp(text(db, ROW, t25), FILLED));
    sqlite3_close(db_b);

    // Nested savepoint inside an explicit outer BEGIN: a failing fill rolls back only sc_rbs
    // without aborting Engine's outer transaction.
    long long t23 = track(db, "../Contents/NestedTx.mp3", 1, 1);
    stage("lib-a", "contents/nestedtx.mp3", "contents/nestedtx.mp3", "Contents/NestedTx.mp3", 5, ",\"perf\":true,\"active\":3", bin, bin_n);
    snprintf(trig, sizeof trig, "CREATE TEMP TRIGGER fail_track2 BEFORE UPDATE ON Track WHEN NEW.id = %lld BEGIN SELECT RAISE(ABORT, 'boom'); END;", t23);
    assert(sqlite3_exec(db, trig, NULL, NULL, NULL) == SQLITE_OK);
    assert(sqlite3_exec(db, "BEGIN", NULL, NULL, NULL) == SQLITE_OK);
    long long t24 = track(db, "../Contents/OuterRow.mp3", 0, 0);
    assert(!strcmp(load(db, t23), "update failed"));
    assert(sqlite3_get_autocommit(db) == 0);
    assert(sqlite3_exec(db, "COMMIT", NULL, NULL, NULL) == SQLITE_OK);
    assert(sqlite3_get_autocommit(db) == 1);
    assert(!strcmp(text(db, ROW, t23), INSERTED));
    assert(!strcmp(text(db, ROW, t24), NOTHING));
    assert(sqlite3_exec(db, "DROP TRIGGER fail_track2;", NULL, NULL, NULL) == SQLITE_OK);
    assert(starts(load(db, t23), "filled (") && !strcmp(text(db, ROW, t23), FILLED));

    // --- On-demand direct-read tests (no background pass, no stage dir required) ---------------
    {
        // Point sc_rbs_root at a directory that DOES NOT EXIST YET: proves on-demand works with
        // zero pre-existing /data/enginex/rb-stage directory and creates filled.jsonl cleanly.
        char od_stage[160], od_sock[160];
        snprintf(od_stage, sizeof od_stage, "%s/no-stage-yet/rb-stage", dir);
        snprintf(od_sock, sizeof od_sock, "%s/od.sock", dir);
        sc_rbs_root = od_stage;
        sc_rbs_ondemand_reset();

        int ufd = socket(AF_UNIX, SOCK_STREAM, 0);
        assert(ufd >= 0);
        struct sockaddr_un su;
        memset(&su, 0, sizeof su);
        su.sun_family = AF_UNIX;
        assert(strlen(od_sock) < sizeof su.sun_path);
        memcpy(su.sun_path, od_sock, strlen(od_sock) + 1);
        assert(!bind(ufd, (struct sockaddr *)&su, sizeof su));
        assert(!listen(ufd, 16));

        struct od_mock mock;
        memset(&mock, 0, sizeof mock);
        mock.listen_fd = ufd;
        mock.status = 200;
        snprintf(mock.key, sizeof mock.key, "contents/ondemand1.mp3");
        snprintf(mock.wr, sizeof mock.wr, "AAECAwQFBgcICQoLDA0ODw==");
        snprintf(mock.wrnm, sizeof mock.wrnm, "EBESExQVFhcYGRobHB0eHw==");
        snprintf(mock.row, sizeof mock.row, "row-od-1");
        mock.body = bin;
        mock.body_n = bin_n;
        pthread_mutex_init(&mock.mu, NULL);
        pthread_t sth;
        assert(!pthread_create(&sth, NULL, od_mock_serve, &mock));

        sc_rbs_ondemand_sock = od_sock;
        sc_rbs_ondemand_url = NULL;
        sc_rbs_ondemand_timeout_ms = 1500;

        // 1. Unstaged track with NO stage directory at all fills on demand over AF_UNIX and creates filled.jsonl with wr + wrnm.
        long long od1 = track(db, "../Contents/OnDemand1.mp3", 1, 1);
        out_id = 0;
        const char *od_res = load_path(db, "../Contents/OnDemand1.mp3", -1, &out_id);
        assert(starts(od_res, "filled on-demand (") && out_id == od1);
        assert(!strcmp(text(db, ROW, od1), FILLED));
        assert(mock.requests == 1);
        char od_jpath[PATH_MAX], od_jline[1024];
        snprintf(od_jpath, sizeof od_jpath, "%s/lib-a/%s", od_stage, SC_RBS_JOURNAL);
        FILE *od_jf = fopen(od_jpath, "r");
        assert(od_jf && fgets(od_jline, sizeof od_jline, od_jf));
        assert(!fclose(od_jf));
        assert(strstr(od_jline, "\"k\":\"contents/ondemand1.mp3\""));
        assert(strstr(od_jline, "\"wr\":\"AAECAwQFBgcICQoLDA0ODw==\""));
        assert(strstr(od_jline, "\"wrnm\":\"EBESExQVFhcYGRobHB0eHw==\""));
        assert(strstr(od_jline, "\"al\":3"));

        // 2. Second load of already-filled track does ZERO IPC.
        int reqs_before = mock.requests;
        assert(!strcmp(load_path(db, "../Contents/OnDemand1.mp3", -1, &out_id), "skipped (row has analysis)"));
        assert(mock.requests == reqs_before);

        // 3. Browse scan (sc_rbs_fill_track: Track WHERE t.id = ?) NEVER calls on-demand IPC.
        //    (od_stage/lib-a now exists because test #1 wrote filled.jsonl, with no index.jsonl.)
        long long od_browse = track(db, "../Contents/OnDemandBrowseOnly.mp3", 1, 1);
        reqs_before = mock.requests;
        statements = 0;
        assert(!strcmp(load(db, od_browse), "skipped (nothing staged for this library)"));
        assert(mock.requests == reqs_before && statements == 0);
        assert(!strcmp(text(db, ROW, od_browse), INSERTED));

        // 4. PerformanceData WHERE trackId = ? (sc_rbs_fill_perf) DOES fill on demand as fallback.
        pthread_mutex_lock(&mock.mu);
        snprintf(mock.key, sizeof mock.key, "contents/ondemandbrowseonly.mp3");
        pthread_mutex_unlock(&mock.mu);
        assert(starts(load_perf(db, od_browse), "filled on-demand ("));
        assert(!strcmp(text(db, ROW, od_browse), FILLED));
        assert(mock.requests == reqs_before + 1);

        // 5. Non-ASCII path works end-to-end via Go FoldPath key returned in X-RB-Key.
        long long od_utf = track(db, "../Contents/OnDemand-Caf\xc3\xa9.mp3", 1, 1);
        pthread_mutex_lock(&mock.mu);
        snprintf(mock.key, sizeof mock.key, "contents/ondemand-caf\xc3\xa9.mp3");
        pthread_mutex_unlock(&mock.mu);
        out_id = 0;
        assert(starts(load_path(db, "../Contents/OnDemand-Caf\xc3\xa9.mp3", -1, &out_id), "filled on-demand (") && out_id == od_utf);
        assert(!strcmp(text(db, ROW, od_utf), FILLED));

        // 6. Degraded (422) populates negative cache: second call skips with 0 SQL and 0 IPC.
        long long od_deg = track(db, "../Contents/OnDemandDegraded.mp3", 1, 1);
        pthread_mutex_lock(&mock.mu);
        mock.status = 422;
        pthread_mutex_unlock(&mock.mu);
        reqs_before = mock.requests;
        assert(!strcmp(load_path(db, "../Contents/OnDemandDegraded.mp3", -1, &out_id), "skipped (ondemand degraded/missing)"));
        assert(mock.requests == reqs_before + 1);
        statements = 0;
        assert(!strcmp(load_path(db, "../Contents/OnDemandDegraded.mp3", -1, &out_id), "skipped (ondemand cached miss)"));
        assert(mock.requests == reqs_before + 1 && statements == 0);
        assert(!strcmp(text(db, ROW, od_deg), INSERTED));

        // 7. Timeout cap: 500 ms server delay with 120 ms timeout aborts promptly and leaves DB untouched.
        long long od_to = track(db, "../Contents/OnDemandTimeout.mp3", 1, 1);
        pthread_mutex_lock(&mock.mu);
        mock.status = 200;
        mock.delay_ms = 400;
        pthread_mutex_unlock(&mock.mu);
        sc_rbs_ondemand_timeout_ms = 120;
        clock_gettime(CLOCK_MONOTONIC, &a);
        assert(!strcmp(load_path(db, "../Contents/OnDemandTimeout.mp3", -1, &out_id), "ondemand fetch failed"));
        clock_gettime(CLOCK_MONOTONIC, &b);
        double to_ms = (double)(b.tv_sec - a.tv_sec) * 1e3 + (double)(b.tv_nsec - a.tv_nsec) / 1e6;
        assert(to_ms < 350.0 && !strcmp(text(db, ROW, od_to), INSERTED) && sqlite3_get_autocommit(db) == 1);
        sc_rbs_ondemand_timeout_ms = 1500;
        pthread_mutex_lock(&mock.mu);
        mock.delay_ms = 0;
        pthread_mutex_unlock(&mock.mu);

        // 8. RT thread safety guard (SCHED_RR): immediate bailout, 0 SQL, 0 IPC.
        long long od_rt = track(db, "../Contents/OnDemandRT.mp3", 1, 1);
        reqs_before = mock.requests;
        statements = 0;
        sc_rbs_sched_override = fake_sched_rr;
        assert(!strcmp(load_path(db, "../Contents/OnDemandRT.mp3", -1, &out_id), "skipped (rt thread)"));
        sc_rbs_sched_override = NULL;
        assert(mock.requests == reqs_before && statements == 0 && !strcmp(text(db, ROW, od_rt), INSERTED));

#ifdef __linux__
        // 9. GUI thread (SceneGraph) and browse worker (Thread (pooled) querying PerformanceData)
        //    safety guards: immediate bailout, 0 SQL, 0 IPC.
        struct od_race_arg gui_arg = {db, "../Contents/OnDemandRT.mp3", 0, "", 1, 0};
        reqs_before = mock.requests;
        statements = 0;
        pthread_t gth;
        assert(!pthread_create(&gth, NULL, od_race_worker, &gui_arg));
        assert(!pthread_join(gth, NULL));
        assert(!strcmp(gui_arg.res, "skipped (gui thread)") && mock.requests == reqs_before && statements == 0);

        struct od_race_arg brw_arg = {db, NULL, 0, "", 2, od_rt};
        reqs_before = mock.requests;
        statements = 0;
        pthread_t bth;
        assert(!pthread_create(&bth, NULL, od_race_worker, &brw_arg));
        assert(!pthread_join(bth, NULL));
        assert(!strcmp(brw_arg.res, "skipped (browse thread)") && mock.requests == reqs_before && statements == 0 &&
               !strcmp(text(db, ROW, od_rt), INSERTED));
#endif

        // 10. Concurrent 2-thread load race (TrackLoad1 + J:Searchingtrac on 2 separate SQLite connections):
        //     must coalesce to EXACTLY 1 HTTP request and both threads must see FILLED on return.
        long long od_race = track(db, "../Contents/OnDemandRace.mp3", 1, 1);
        sqlite3 *db_r1 = NULL, *db_r2 = NULL;
        assert(sqlite3_open(sqlite3_db_filename(db, "main"), &db_r1) == SQLITE_OK);
        assert(sqlite3_open(sqlite3_db_filename(db, "main"), &db_r2) == SQLITE_OK);
        assert(sqlite3_busy_timeout(db_r1, 500) == SQLITE_OK);
        assert(sqlite3_busy_timeout(db_r2, 500) == SQLITE_OK);
        pthread_mutex_lock(&mock.mu);
        mock.status = 200;
        mock.delay_ms = 80;
        snprintf(mock.key, sizeof mock.key, "contents/ondemandrace.mp3");
        pthread_mutex_unlock(&mock.mu);
        reqs_before = mock.requests;
        struct od_race_arg ra1 = {db_r1, "../Contents/OnDemandRace.mp3", 0, "", 0, 0};
        struct od_race_arg ra2 = {db_r2, "../Contents/OnDemandRace.mp3", 0, "", 0, 0};
        pthread_t rt1, rt2;
        assert(!pthread_create(&rt1, NULL, od_race_worker, &ra1));
        assert(!pthread_create(&rt2, NULL, od_race_worker, &ra2));
        assert(!pthread_join(rt1, NULL));
        assert(!pthread_join(rt2, NULL));
        assert(mock.requests == reqs_before + 1);
        int one_filled = (starts(ra1.res, "filled on-demand (") && !strcmp(ra2.res, "skipped (row has analysis)")) ||
                         (starts(ra2.res, "filled on-demand (") && !strcmp(ra1.res, "skipped (row has analysis)"));
        assert(one_filled && ra1.out_id == od_race && ra2.out_id == od_race);
        assert(!strcmp(text(db, ROW, od_race), FILLED));
        sqlite3_close(db_r1);
        sqlite3_close(db_r2);

        __atomic_store_n(&mock.stop, 1, __ATOMIC_RELEASE);
        assert(!pthread_join(sth, NULL));
        close(ufd);
        unlink(od_sock);
        pthread_mutex_destroy(&mock.mu);

        // 11. Loopback TCP (AF_INET) override works identically.
        int tfd = socket(AF_INET, SOCK_STREAM, 0);
        assert(tfd >= 0);
        int one = 1;
        setsockopt(tfd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
        struct sockaddr_in sin;
        memset(&sin, 0, sizeof sin);
        sin.sin_family = AF_INET;
        sin.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
        sin.sin_port = 0;
        assert(!bind(tfd, (struct sockaddr *)&sin, sizeof sin));
        socklen_t slen = sizeof sin;
        assert(!getsockname(tfd, (struct sockaddr *)&sin, &slen));
        assert(!listen(tfd, 8));
        char tcp_url[128];
        snprintf(tcp_url, sizeof tcp_url, "http://127.0.0.1:%u%s", (unsigned)ntohs(sin.sin_port), SC_RBS_ONDEMAND_ROUTE);

        struct od_mock tmock;
        memset(&tmock, 0, sizeof tmock);
        tmock.listen_fd = tfd;
        tmock.status = 200;
        snprintf(tmock.key, sizeof tmock.key, "contents/ondemandtcp.mp3");
        snprintf(tmock.wr, sizeof tmock.wr, "AAECAwQFBgcICQoLDA0ODw==");
        snprintf(tmock.wrnm, sizeof tmock.wrnm, "EBESExQVFhcYGRobHB0eHw==");
        snprintf(tmock.row, sizeof tmock.row, "row-od-tcp");
        tmock.body = bin;
        tmock.body_n = bin_n;
        pthread_mutex_init(&tmock.mu, NULL);
        assert(!pthread_create(&sth, NULL, od_mock_serve, &tmock));

        sc_rbs_ondemand_sock = "";
        sc_rbs_ondemand_url = tcp_url;
        long long od_tcp = track(db, "../Contents/OnDemandTCP.mp3", 1, 1);
        out_id = 0;
        assert(starts(load_path(db, "../Contents/OnDemandTCP.mp3", -1, &out_id), "filled on-demand (") && out_id == od_tcp);
        assert(!strcmp(text(db, ROW, od_tcp), FILLED) && tmock.requests == 1);

        __atomic_store_n(&tmock.stop, 1, __ATOMIC_RELEASE);
        assert(!pthread_join(sth, NULL));
        close(tfd);
        pthread_mutex_destroy(&tmock.mu);
        sc_rbs_ondemand_url = NULL;
    }

    // 12. Locale-independent BPM parse, 16-slot inflight full (-2), and strict Content-Length rejection
    //     on a pthread server (safe under ASan).
    {
        assert(sc_rbs_parse_pos_double("128.5000") > 128.49 && sc_rbs_parse_pos_double("128.5000") < 128.51);
        assert(sc_rbs_parse_pos_double("128,5000") == 0.0);
        long long peer = 0;
        int claimed[SC_RBS_INFLIGHT_SLOTS];
        for (int i = 0; i < SC_RBS_INFLIGHT_SLOTS; i++) {
            claimed[i] = sc_rbs_inflight_enter((uint64_t)(9000 + i), sc_rbs_mono_ns() + 1000000000LL, &peer);
            assert(claimed[i] >= 0);
        }
        assert(sc_rbs_inflight_enter(99999ULL, sc_rbs_mono_ns() + 1000000000LL, &peer) == -2);
        for (int i = 0; i < SC_RBS_INFLIGHT_SLOTS; i++) sc_rbs_inflight_leave(claimed[i], 0, 0);

        struct sc_rbs_line bad_l;
        int bad_status = 0;
        unsigned char *bad_body = NULL;
        size_t bad_bn = 0;
        struct sc_rbs_row bad_r;
        memset(&bad_r, 0, sizeof bad_r);
        snprintf(bad_r.path, sizeof bad_r.path, "../Contents/BadCL.mp3");
        bad_r.id = 1;

        int bfd = socket(AF_INET, SOCK_STREAM, 0);
        assert(bfd >= 0);
        int one2 = 1;
        setsockopt(bfd, SOL_SOCKET, SO_REUSEADDR, &one2, sizeof one2);
        struct sockaddr_in bsin;
        memset(&bsin, 0, sizeof bsin);
        bsin.sin_family = AF_INET;
        bsin.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
        assert(!bind(bfd, (struct sockaddr *)&bsin, sizeof bsin));
        socklen_t bslen = sizeof bsin;
        assert(!getsockname(bfd, (struct sockaddr *)&bsin, &bslen));
        assert(!listen(bfd, 4));
        char burl[128];
        snprintf(burl, sizeof burl, "http://127.0.0.1:%u%s", (unsigned)ntohs(bsin.sin_port), SC_RBS_ONDEMAND_ROUTE);
        sc_rbs_ondemand_url = burl;

        const char *bad_replies[2] = {
            "HTTP/1.1 200 OK\r\nContent-Length: 4294967301\r\nX-RB-Key: k\r\nX-RB-Written: AAEC\r\nX-RB-Rules: 5\r\n\r\nRBXA1",
            "HTTP/1.1 200 OK\r\nContent-Length: abc\r\n\r\n",
        };
        for (int k = 0; k < 2; k++) {
            struct raw_http_arg ha = {bfd, bad_replies[k]};
            pthread_t bt;
            assert(!pthread_create(&bt, NULL, raw_http_once, &ha));
            bad_body = NULL;
            bad_bn = 0;
            bad_status = 0;
            assert(sc_rbs_ondemand_fetch(sqlite3_db_filename(db, "main"), "lib-a", &bad_r, sc_rbs_mono_ns() + 500000000LL, &bad_l, &bad_body, &bad_bn, &bad_status) < 0);
            assert(bad_body == NULL && bad_bn == 0);
            assert(!pthread_join(bt, NULL));
        }
        sc_rbs_ondemand_url = NULL;
        close(bfd);
        (void)bad_l;
        (void)bad_status;
    }

    // 13. Review proof tests: fail on f65d4fdf for unfixed invariants (Findings 1 & 3).
    {
        int review_failures = 0;
        const char *main_db = sqlite3_db_filename(db, "main");
        long long od_gui = track(db, "../Contents/ReviewStageGUI.mp3", 1, 1);
        sc_rbs_root = root;
        sc_rbs_ondemand_sock = "";
        sc_rbs_ondemand_url = "http://127.0.0.1:1" SC_RBS_ONDEMAND_ROUTE;

        // Proof A (Finding 1): GUI threads (SceneGraph, EMain, EPlanckApp, QSGRenderThread) and browse
        // threads (Thread (pooled)) must bail BEFORE any stat/access/malloc/sc_rbs_mu/snapshot read/SQLite
        // in sc_rbs_fill_perf, sc_rbs_fill_path, AND sc_rbs_fill_track. Holding sc_rbs_mu proves zero mutex lock.
        const struct {
            const char *thr;
            const char *want;
        } thr_cases[] = {
            {"SceneGraph", "skipped (gui thread)"},
            {"EMain", "skipped (gui thread)"},
            {"EPlanckApp", "skipped (gui thread)"},
            {"QSGRenderThread", "skipped (gui thread)"},
            {"Thread (pooled)", "skipped (browse thread)"},
        };
        pthread_mutex_lock(&sc_rbs_mu);
        for (size_t i = 0; i < sizeof thr_cases / sizeof *thr_cases; i++) {
            for (int call_kind = 0; call_kind < 3; call_kind++) {
                statements = 0;
                struct gui_guard_arg ga = {
                    thr_cases[i].thr,
                    db,
                    main_db,
                    call_kind == 2 ? "../Contents/ReviewStageGUI.mp3" : NULL,
                    call_kind == 2 ? 0 : od_gui,
                    call_kind == 1,
                    thr_cases[i].want,
                    0,
                };
                pthread_t th;
                assert(!pthread_create(&th, NULL, counted_gui_guard_worker, &ga));
                assert(!pthread_join(th, NULL));
                if (!ga.ok || statements != 0) {
                    fprintf(stderr, "REVIEW PROOF FAIL (Finding 1): %s kind=%d ok=%d statements=%d\n",
                            thr_cases[i].thr, call_kind, ga.ok, statements);
                    review_failures++;
                }
            }
        }
        pthread_mutex_unlock(&sc_rbs_mu);

        // Proof B (Finding 3): >16 KiB header bypass and per-step deadline enforcement in sc_rbs_ondemand_fetch.
        struct sc_rbs_line rev_l;
        int rev_status = 0;
        unsigned char *rev_body = NULL;
        size_t rev_bn = 0;
        struct sc_rbs_row rev_r;
        memset(&rev_r, 0, sizeof rev_r);
        snprintf(rev_r.path, sizeof rev_r.path, "../Contents/ReviewHTTP.mp3");
        rev_r.id = 1;

        int hfd = socket(AF_INET, SOCK_STREAM, 0);
        assert(hfd >= 0);
        int one3 = 1;
        setsockopt(hfd, SOL_SOCKET, SO_REUSEADDR, &one3, sizeof one3);
        struct sockaddr_in hsin;
        memset(&hsin, 0, sizeof hsin);
        hsin.sin_family = AF_INET;
        hsin.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
        assert(!bind(hfd, (struct sockaddr *)&hsin, sizeof hsin));
        socklen_t hslen = sizeof hsin;
        assert(!getsockname(hfd, (struct sockaddr *)&hsin, &hslen));
        assert(!listen(hfd, 4));
        char hurl[128];
        snprintf(hurl, sizeof hurl, "http://127.0.0.1:%u%s", (unsigned)ntohs(hsin.sin_port), SC_RBS_ONDEMAND_ROUTE);
        sc_rbs_ondemand_url = hurl;

        // Proof B1: >16 KiB HTTP headers delivered in one recv() (< 32 KiB) must be rejected.
        size_t pad_len = 16500;
        char *big_hdr = malloc(pad_len + 512 + bin_n);
        assert(big_hdr);
        int hlen = snprintf(big_hdr, pad_len + 512,
                            "HTTP/1.1 200 OK\r\nContent-Length: %zu\r\nX-RB-Key: k\r\nX-RB-Written: AAEC\r\nX-RB-Rules: 5\r\nX-Pad: ",
                            bin_n);
        memset(big_hdr + hlen, 'A', pad_len);
        memcpy(big_hdr + hlen + pad_len, "\r\n\r\n", 4);
        memcpy(big_hdr + hlen + pad_len + 4, bin, bin_n);
        size_t big_tot = (size_t)hlen + pad_len + 4 + bin_n;
        struct raw_bytes_arg bha = {hfd, big_hdr, big_tot, 0, 0, 0};
        pthread_t bht;
        assert(!pthread_create(&bht, NULL, raw_bytes_once, &bha));
        int rc_big = sc_rbs_ondemand_fetch(main_db, "lib-a", &rev_r,
                                           sc_rbs_mono_ns() + 500000000LL, &rev_l, &rev_body, &rev_bn, &rev_status);
        assert(!pthread_join(bht, NULL));
        free(big_hdr);
        if (rc_big == 0) {
            fprintf(stderr, "REVIEW PROOF FAIL (Finding 3): >16 KiB header (%zu bytes) accepted (status=%d body=%zu)\n",
                    (size_t)hlen + pad_len + 4, rev_status, rev_bn);
            free(rev_body);
            rev_body = NULL;
            review_failures++;
        }

        // Proof B2a: Already-expired deadline against an immediate 200 OK server must fail immediately.
        char ok_reply[512];
        int ok_hlen = snprintf(ok_reply, sizeof ok_reply,
                               "HTTP/1.1 200 OK\r\nContent-Length: %zu\r\nX-RB-Key: k\r\nX-RB-Written: AAEC\r\nX-RB-Rules: 5\r\n\r\n",
                               bin_n);
        assert(ok_hlen > 0 && (size_t)ok_hlen + bin_n <= sizeof ok_reply);
        memcpy(ok_reply + ok_hlen, bin, bin_n);
        struct raw_bytes_arg expa = {hfd, ok_reply, (size_t)ok_hlen + bin_n, 0, 0, 0};
        pthread_t expt;
        assert(!pthread_create(&expt, NULL, raw_bytes_once, &expa));
        int rc_exp = sc_rbs_ondemand_fetch(main_db, "lib-a", &rev_r,
                                           sc_rbs_mono_ns() - 1000LL, &rev_l, &rev_body, &rev_bn, &rev_status);
        assert(!pthread_join(expt, NULL));
        if (rc_exp == 0) {
            fprintf(stderr, "REVIEW PROOF FAIL (Finding 3): expired deadline accepted immediate HTTP 200\n");
            free(rev_body);
            rev_body = NULL;
            review_failures++;
        }

        // Proof B2b: Slow 1-byte header trickle (5 ms/byte) must fail when deadline passes mid-stream.
        struct raw_bytes_arg trick = {hfd, ok_reply, (size_t)ok_hlen + bin_n, 1, 5, 0};
        pthread_t trt;
        assert(!pthread_create(&trt, NULL, raw_bytes_once, &trick));
        struct timespec t_start, t_end;
        clock_gettime(CLOCK_MONOTONIC, &t_start);
        int rc_trick = sc_rbs_ondemand_fetch(main_db, "lib-a", &rev_r,
                                             sc_rbs_mono_ns() + 45000000LL, &rev_l, &rev_body, &rev_bn, &rev_status);
        clock_gettime(CLOCK_MONOTONIC, &t_end);
        __atomic_store_n(&trick.stop, 1, __ATOMIC_RELEASE);
        assert(!pthread_join(trt, NULL));
        double trick_ms = (double)(t_end.tv_sec - t_start.tv_sec) * 1e3 + (double)(t_end.tv_nsec - t_start.tv_nsec) / 1e6;
        if (rc_trick == 0 || trick_ms > 200.0) {
            fprintf(stderr, "REVIEW PROOF FAIL (Finding 3): trickle deadline failed (rc=%d took=%.1f ms)\n",
                    rc_trick, trick_ms);
            free(rev_body);
            rev_body = NULL;
            review_failures++;
        }

        sc_rbs_ondemand_url = NULL;
        close(hfd);
        assert(review_failures == 0);
    }

    // 14. RD-3: Analyzer/import/freeze Engine threads must bail out immediately on allow_ondemand=1
    //     with 0 SQL statements and 0 IPC, and concurrent sc_rbs_fill_path + sc_rbs_fill_perf for
    //     the same track on separate connections must coalesce onto 1 HTTP request.
    {
        const char *main_db = sqlite3_db_filename(db, "main");
        long long od_blk = track(db, "../Contents/OnDemandBlockedThr.mp3", 1, 1);
#ifdef __linux__
        const char *extra_threads[] = {
            "Analysis Queue", "OfflineAnalyzer", "J:I:Collecting",
            "J:I:Takingfreez", "J:Importingtrac", "J:N:Searching o",
        };
        sc_rbs_ondemand_url = "http://127.0.0.1:1" SC_RBS_ONDEMAND_ROUTE;
        for (size_t i = 0; i < sizeof extra_threads / sizeof extra_threads[0]; i++) {
            for (int call_kind = 0; call_kind < 2; call_kind++) {
                statements = 0;
                struct gui_guard_arg ga = {
                    extra_threads[i],
                    db,
                    main_db,
                    call_kind == 1 ? "../Contents/OnDemandBlockedThr.mp3" : NULL,
                    call_kind == 1 ? 0 : od_blk,
                    0,
                    "skipped (browse thread)",
                    0,
                };
                pthread_t th;
                assert(!pthread_create(&th, NULL, counted_gui_guard_worker, &ga));
                assert(!pthread_join(th, NULL));
                assert(ga.ok && statements == 0);
            }
        }
        sc_rbs_ondemand_url = NULL;
#else
        (void)od_blk;
#endif
        int mfd = socket(AF_INET, SOCK_STREAM, 0);
        assert(mfd >= 0);
        int one = 1;
        setsockopt(mfd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
        struct sockaddr_in msin;
        memset(&msin, 0, sizeof msin);
        msin.sin_family = AF_INET;
        msin.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
        msin.sin_port = 0;
        assert(!bind(mfd, (struct sockaddr *)&msin, sizeof msin));
        socklen_t mlen = sizeof msin;
        assert(!getsockname(mfd, (struct sockaddr *)&msin, &mlen));
        assert(!listen(mfd, 8));
        char murl[128];
        snprintf(murl, sizeof murl, "http://127.0.0.1:%u%s", (unsigned)ntohs(msin.sin_port), SC_RBS_ONDEMAND_ROUTE);

        struct od_mock mmock;
        memset(&mmock, 0, sizeof mmock);
        mmock.listen_fd = mfd;
        mmock.status = 200;
        mmock.delay_ms = 120;
        snprintf(mmock.key, sizeof mmock.key, "contents/ondemandmixedrace.mp3");
        snprintf(mmock.wr, sizeof mmock.wr, "AAECAwQFBgcICQoLDA0ODw==");
        snprintf(mmock.wrnm, sizeof mmock.wrnm, "EBESExQVFhcYGRobHB0eHw==");
        snprintf(mmock.row, sizeof mmock.row, "row-od-mixed");
        mmock.body = bin;
        mmock.body_n = bin_n;
        pthread_mutex_init(&mmock.mu, NULL);
        pthread_t mth;
        assert(!pthread_create(&mth, NULL, od_mock_serve, &mmock));

        sc_rbs_ondemand_url = murl;
        long long od_mixed = track(db, "../Contents/OnDemandMixedRace.mp3", 1, 1);
        sqlite3 *db_m1 = NULL, *db_m2 = NULL;
        assert(sqlite3_open(sqlite3_db_filename(db, "main"), &db_m1) == SQLITE_OK);
        assert(sqlite3_open(sqlite3_db_filename(db, "main"), &db_m2) == SQLITE_OK);
        assert(sqlite3_busy_timeout(db_m1, 500) == SQLITE_OK);
        assert(sqlite3_busy_timeout(db_m2, 500) == SQLITE_OK);
        struct od_race_arg ma1 = {db_m1, "../Contents/OnDemandMixedRace.mp3", 0, "", 0, 0};
        struct od_race_arg ma2 = {db_m2, NULL, 0, "", 0, od_mixed};
        pthread_t mt1, mt2;
        assert(!pthread_create(&mt1, NULL, od_race_worker, &ma1));
        struct timespec s15 = {0, 15000000L};
        nanosleep(&s15, NULL);
        assert(!pthread_create(&mt2, NULL, od_race_worker, &ma2));
        assert(!pthread_join(mt1, NULL));
        assert(!pthread_join(mt2, NULL));
        assert(mmock.requests == 1);
        assert(starts(ma1.res, "filled on-demand (") && !strcmp(ma2.res, "skipped (row has analysis)"));
        assert(ma1.out_id == od_mixed && !strcmp(text(db, ROW, od_mixed), FILLED));
        sqlite3_close(db_m1);
        sqlite3_close(db_m2);

        __atomic_store_n(&mmock.stop, 1, __ATOMIC_RELEASE);
        assert(!pthread_join(mth, NULL));
        close(mfd);
        pthread_mutex_destroy(&mmock.mu);
        sc_rbs_ondemand_url = NULL;
    }

    sqlite3_close(db);
    char rm[160];
    snprintf(rm, sizeof rm, "rm -rf '%s'", dir);
    assert(!system(rm));
    puts("rekordbox stage fill: PASS");
    return 0;
}
