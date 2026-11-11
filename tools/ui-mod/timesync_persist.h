// Timezone & monotonic clock persistence for Engine (stock QSettings TZEnvVar + timesyncd clock).
//
// Engine sets TZ in RAM via setenv("TZ", tz, 1) in TimeDateController::setSystemTimeZone, but only
// writes [TimeDateSettings] TZEnvVar to /data/.config/<product>.user.settings/AIR Music Technology/Engine.conf
// inside ~TimeDateController(), which never runs on AC power-off or SIGTERM. Persisting TZEnvVar on
// setenv("TZ", ...) lets stock TimeDateController restore the zone on offline boot and prevents
// fallback/empty/UTC responses from overwriting a valid Region/City zone.
#ifndef ENGINEX_TIMESYNC_PERSIST_H
#define ENGINEX_TIMESYNC_PERSIST_H

static pthread_mutex_t g_tz_mutex = PTHREAD_MUTEX_INITIALIZER;
static char g_qsettings_conf[PATH_MAX];
static void *raw_openat_cache, *setenv_cache, *unsetenv_cache;

static int ts_raw_open(const char *path, int flags, mode_t mode) {
    int (*real)(int, const char *, int, ...) = next_symbol(&raw_openat_cache, "openat");
    return real ? real(AT_FDCWD, path, flags, mode) : (int)syscall(SYS_openat, AT_FDCWD, path, flags, mode);
}

static int ts_raw_setenv(const char *name, const char *value, int overwrite) {
    int (*real)(const char *, const char *, int) = next_symbol(&setenv_cache, "setenv");
    if (!real) { errno = ENOSYS; return -1; }
    return real(name, value, overwrite);
}

static int ts_raw_unsetenv(const char *name) {
    int (*real)(const char *) = next_symbol(&unsetenv_cache, "unsetenv");
    if (!real) { errno = ENOSYS; return -1; }
    return real(name);
}

static int tz_char_ok(char c) {
    return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
           (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '+';
}

static int valid_iana_tz(const char *tz) {
    if (!tz || !*tz || tz[0] == '/') return 0;
    size_t len = strlen(tz);
    if (len >= 64 || tz[len - 1] == '/') return 0;
    int slashes = 0;
    const char *seg = tz;
    for (size_t i = 0; i <= len; i++) {
        if (i == len || tz[i] == '/') {
            size_t slen = (size_t)(tz + i - seg);
            if (slen == 0 || slen > 32) return 0;
            if ((slen == 1 && seg[0] == '.') || (slen == 2 && seg[0] == '.' && seg[1] == '.')) return 0;
            // Match Engine TimeDateController::isValidTimeZone() and :/timezones (340 Region/City zones, no UTC/GMT).
            if ((slen == 3 && (!strncasecmp(seg, "UTC", 3) || !strncasecmp(seg, "GMT", 3) || !strncasecmp(seg, "UCT", 3))) ||
                (slen == 4 && (!strncasecmp(seg, "GMT0", 4) || !strncasecmp(seg, "Zulu", 4))) ||
                (slen == 9 && !strncasecmp(seg, "Universal", 9)))
                return 0;
            if (i < len) slashes++;
            seg = tz + i + 1;
        } else if (!tz_char_ok(tz[i])) {
            return 0;
        }
    }
    if (slashes < 1) return 0;
    const char *zroot = getenv("ENGINEX_TEST_ZONEINFO_DIR");
    if (!zroot || !*zroot) zroot = "/usr/share/zoneinfo";
    char zpath[PATH_MAX];
    if (snprintf(zpath, sizeof zpath, "%s/%s", zroot, tz) >= (int)sizeof zpath) return 0;
    struct stat st;
    return fstatat(AT_FDCWD, zpath, &st, 0) == 0 && S_ISREG(st.st_mode);
}

static void note_qsettings_path(const char *path) {
    if (!path || path[0] != '/' || strlen(path) >= PATH_MAX) return;
    if (!strstr(path, "/.config/") || !strstr(path, ".user.settings/AIR Music Technology/")) return;
    const char *base = strrchr(path, '/');
    if (!base) return;
    base++;
    size_t blen = strlen(base);
    if (blen <= 5 || strcmp(base + blen - 5, ".conf") != 0) return;
    if (!strcmp(base, "AIR Music Technology.conf")) return;
    pthread_mutex_lock(&g_tz_mutex);
    strcpy(g_qsettings_conf, path);
    pthread_mutex_unlock(&g_tz_mutex);
}

static const char *tz_data_root(void) {
    const char *r = getenv("ENGINEX_TEST_DATA_ROOT");
    return (r && *r) ? r : "/data";
}

static int resolve_qsettings_conf(char out[PATH_MAX]) {
    const char *override = getenv("ENGINEX_TEST_QSETTINGS_CONF");
    if (override && *override && strlen(override) < PATH_MAX) {
        strcpy(out, override);
        return 1;
    }
    if (g_qsettings_conf[0]) {
        strcpy(out, g_qsettings_conf);
        return 1;
    }
    char cfg_dir[PATH_MAX];
    if (snprintf(cfg_dir, sizeof cfg_dir, "%s/.config", tz_data_root()) >= (int)sizeof cfg_dir) return 0;
    int dfd = ts_raw_open(cfg_dir, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC, 0);
    if (dfd < 0) return 0;
    DIR *d = fdopendir(dfd);
    if (!d) { close(dfd); return 0; }
    int found = 0;
    struct dirent *e;
    while ((e = readdir(d)) != NULL) {
        size_t nlen = strlen(e->d_name);
        if (nlen <= 14 || strcmp(e->d_name + nlen - 14, ".user.settings") != 0) continue;
        struct stat st;
        if (fstatat(dirfd(d), e->d_name, &st, AT_SYMLINK_NOFOLLOW) != 0 || !S_ISDIR(st.st_mode)) continue;
        if (snprintf(out, PATH_MAX, "%s/%s/AIR Music Technology/Engine.conf", cfg_dir, e->d_name) < PATH_MAX) {
            strcpy(g_qsettings_conf, out);
            found = 1;
            break;
        }
    }
    closedir(d);
    return found;
}

static int state_tz_path(char out[PATH_MAX]) {
    const char *root = tz_data_root();
    char edir[PATH_MAX];
    if (snprintf(edir, sizeof edir, "%s/enginex", root) >= (int)sizeof edir) return 0;
    struct stat st;
    if (fstatat(AT_FDCWD, edir, &st, AT_SYMLINK_NOFOLLOW) != 0 || !S_ISDIR(st.st_mode)) return 0;
    return snprintf(out, PATH_MAX, "%s/state/timezone", edir) < PATH_MAX;
}

static int ts_mkdirs_parent(const char *filepath, mode_t mode) {
    char tmp[PATH_MAX];
    if (strlen(filepath) >= sizeof tmp) return -1;
    strcpy(tmp, filepath);
    for (char *p = tmp + 1; *p; p++) {
        if (*p != '/') continue;
        *p = 0;
        struct stat st;
        if (fstatat(AT_FDCWD, tmp, &st, AT_SYMLINK_NOFOLLOW) == 0) {
            if (!S_ISDIR(st.st_mode)) return -1;
        } else if (mkdir(tmp, mode) != 0 && errno != EEXIST) {
            return -1;
        }
        *p = '/';
    }
    return 0;
}

static int write_atomic_regular(const char *path, const char *data, size_t len, mode_t mode) {
    struct stat st;
    if (fstatat(AT_FDCWD, path, &st, AT_SYMLINK_NOFOLLOW) == 0 && !S_ISREG(st.st_mode)) return -1;
    if (ts_mkdirs_parent(path, 0755) != 0) return -1;
    char tmp[PATH_MAX];
    if (snprintf(tmp, sizeof tmp, "%s.enginex.tmp", path) >= (int)sizeof tmp) return -1;
    unlink(tmp);
    int fd = ts_raw_open(tmp, O_WRONLY | O_CREAT | O_TRUNC | O_NOFOLLOW | O_CLOEXEC, mode);
    if (fd < 0) return -1;
    size_t off = 0;
    while (off < len) {
        ssize_t w = write(fd, data + off, len - off);
        if (w < 0) {
            if (errno == EINTR) continue;
            close(fd);
            unlink(tmp);
            return -1;
        }
        off += (size_t)w;
    }
    if (fsync(fd) != 0 || close(fd) != 0) { unlink(tmp); return -1; }
    if (rename(tmp, path) != 0) { unlink(tmp); return -1; }
    char parent[PATH_MAX];
    strcpy(parent, path);
    char *slash = strrchr(parent, '/');
    if (slash && slash != parent) {
        *slash = 0;
        int pfd = ts_raw_open(parent, O_RDONLY | O_DIRECTORY | O_CLOEXEC, 0);
        if (pfd >= 0) { fsync(pfd); close(pfd); }
    }
    return 0;
}

static int read_ini_tz(const char *conf_path, char out[64]) {
    int fd = ts_raw_open(conf_path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC, 0);
    if (fd < 0) return 0;
    char buf[65536];
    ssize_t total = 0;
    while ((size_t)total < sizeof buf - 1) {
        ssize_t r = read(fd, buf + total, sizeof buf - 1 - (size_t)total);
        if (r < 0) { if (errno == EINTR) continue; break; }
        if (r == 0) break;
        total += r;
    }
    close(fd);
    buf[total] = 0;
    int in_td = 0;
    char *p = buf;
    while (*p) {
        char *eol = strchr(p, '\n');
        size_t llen = eol ? (size_t)(eol - p) : strlen(p);
        while (llen > 0 && (p[llen - 1] == '\r' || p[llen - 1] == ' ' || p[llen - 1] == '\t')) llen--;
        const char *s = p;
        while (llen > 0 && (*s == ' ' || *s == '\t')) { s++; llen--; }
        if (llen >= 2 && s[0] == '[' && s[llen - 1] == ']') {
            in_td = (llen == 18 && !memcmp(s, "[TimeDateSettings]", 18));
        } else if (in_td && llen > 9 && !memcmp(s, "TZEnvVar=", 9)) {
            const char *v = s + 9;
            size_t vlen = llen - 9;
            if (vlen >= 2 && v[0] == '"' && v[vlen - 1] == '"') { v++; vlen -= 2; }
            if (vlen > 0 && vlen < 64) {
                char cand[64];
                memcpy(cand, v, vlen);
                cand[vlen] = 0;
                if (valid_iana_tz(cand)) {
                    strcpy(out, cand);
                    return 1;
                }
            }
        }
        if (!eol) break;
        p = eol + 1;
    }
    return 0;
}

static void update_qsettings_tz(const char *conf_path, const char *tz) {
    char cur[64];
    if (read_ini_tz(conf_path, cur) && !strcmp(cur, tz)) return;
    char in_buf[65536];
    ssize_t in_len = 0;
    int fd = ts_raw_open(conf_path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC, 0);
    if (fd >= 0) {
        struct stat st;
        if (fstat(fd, &st) != 0 || st.st_size >= (off_t)(sizeof in_buf - 256)) { close(fd); return; }
        while ((size_t)in_len < sizeof in_buf - 256) {
            ssize_t r = read(fd, in_buf + in_len, sizeof in_buf - 256 - (size_t)in_len);
            if (r < 0) { if (errno == EINTR) continue; close(fd); return; }
            if (r == 0) break;
            in_len += r;
        }
        close(fd);
    }
    in_buf[in_len] = 0;
    char out_buf[65536 + 256];
    size_t out_len = 0;
    int in_td = 0, saw_td = 0, wrote_key = 0;
    char *p = in_buf;
    while (*p) {
        char *eol = strchr(p, '\n');
        size_t raw_len = eol ? (size_t)(eol - p + 1) : strlen(p);
        size_t tlen = eol ? (size_t)(eol - p) : raw_len;
        while (tlen > 0 && (p[tlen - 1] == '\r' || p[tlen - 1] == ' ' || p[tlen - 1] == '\t')) tlen--;
        const char *s = p;
        while (tlen > 0 && (*s == ' ' || *s == '\t')) { s++; tlen--; }
        if (tlen >= 2 && s[0] == '[' && s[tlen - 1] == ']') {
            if (in_td && !wrote_key) {
                if (out_len > 0 && out_buf[out_len - 1] != '\n') out_buf[out_len++] = '\n';
                out_len += (size_t)snprintf(out_buf + out_len, sizeof out_buf - out_len, "TZEnvVar=%s\n", tz);
                wrote_key = 1;
            }
            in_td = (tlen == 18 && !memcmp(s, "[TimeDateSettings]", 18));
            if (in_td) saw_td = 1;
            memcpy(out_buf + out_len, p, raw_len);
            out_len += raw_len;
        } else if (in_td && tlen >= 9 && !memcmp(s, "TZEnvVar=", 9)) {
            if (!wrote_key) {
                out_len += (size_t)snprintf(out_buf + out_len, sizeof out_buf - out_len, "TZEnvVar=%s\n", tz);
                wrote_key = 1;
            }
        } else {
            memcpy(out_buf + out_len, p, raw_len);
            out_len += raw_len;
        }
        if (!eol) break;
        p = eol + 1;
    }
    if (saw_td && !wrote_key) {
        if (out_len > 0 && out_buf[out_len - 1] != '\n') out_buf[out_len++] = '\n';
        out_len += (size_t)snprintf(out_buf + out_len, sizeof out_buf - out_len, "TZEnvVar=%s\n", tz);
    } else if (!saw_td) {
        if (out_len > 0 && out_buf[out_len - 1] != '\n') out_buf[out_len++] = '\n';
        out_len += (size_t)snprintf(out_buf + out_len, sizeof out_buf - out_len, "%s[TimeDateSettings]\nTZEnvVar=%s\n",
                                    out_len > 0 ? "\n" : "", tz);
    }
    write_atomic_regular(conf_path, out_buf, out_len, 0644);
}

static int read_state_tz(const char *spath, char out[64]) {
    int fd = ts_raw_open(spath, O_RDONLY | O_NOFOLLOW | O_CLOEXEC, 0);
    if (fd < 0) return 0;
    char buf[80];
    ssize_t n = read(fd, buf, sizeof buf - 1);
    close(fd);
    if (n <= 0) return 0;
    buf[n] = 0;
    buf[strcspn(buf, "\r\n \t")] = 0;
    if (!valid_iana_tz(buf)) return 0;
    strcpy(out, buf);
    return 1;
}

static void persist_engine_tz(const char *tz) {
    pthread_mutex_lock(&g_tz_mutex);
    char conf[PATH_MAX];
    if (resolve_qsettings_conf(conf)) {
        update_qsettings_tz(conf, tz);
    }
    char spath[PATH_MAX];
    if (state_tz_path(spath)) {
        char cur[64];
        if (!read_state_tz(spath, cur) || strcmp(cur, tz) != 0) {
            char line[80];
            int n = snprintf(line, sizeof line, "%s\n", tz);
            if (n > 0 && (size_t)n < sizeof line) write_atomic_regular(spath, line, (size_t)n, 0644);
        }
    }
    pthread_mutex_unlock(&g_tz_mutex);
}

static int load_saved_engine_tz(char out[64]) {
    pthread_mutex_lock(&g_tz_mutex);
    char conf[PATH_MAX], spath[PATH_MAX], qtz[64] = {0}, stz[64] = {0};
    int has_conf = resolve_qsettings_conf(conf);
    int has_q = has_conf && read_ini_tz(conf, qtz);
    int has_s = state_tz_path(spath) && read_state_tz(spath, stz);
    if (has_q && has_s) {
        struct stat sq, ss;
        if (strcmp(qtz, stz) != 0 && fstatat(AT_FDCWD, conf, &sq, 0) == 0 && fstatat(AT_FDCWD, spath, &ss, 0) == 0 &&
            (ss.st_mtim.tv_sec > sq.st_mtim.tv_sec ||
             (ss.st_mtim.tv_sec == sq.st_mtim.tv_sec && ss.st_mtim.tv_nsec > sq.st_mtim.tv_nsec))) {
            strcpy(out, stz);
            update_qsettings_tz(conf, stz);
        } else {
            strcpy(out, qtz);
        }
        pthread_mutex_unlock(&g_tz_mutex);
        return 1;
    }
    if (has_q) { strcpy(out, qtz); pthread_mutex_unlock(&g_tz_mutex); return 1; }
    if (has_s) {
        strcpy(out, stz);
        if (has_conf) update_qsettings_tz(conf, stz);
        pthread_mutex_unlock(&g_tz_mutex);
        return 1;
    }
    pthread_mutex_unlock(&g_tz_mutex);
    return 0;
}

static void restore_persisted_timezone(void) {
    char saved[64];
    if (!load_saved_engine_tz(saved)) return;
    const char *cur = getenv("TZ");
    if (!cur || strcmp(cur, saved) != 0) {
        if (ts_raw_setenv("TZ", saved, 1) == 0) tzset();
    }
}

__attribute__((visibility("default")))
int setenv(const char *name, const char *value, int overwrite) {
    if (!is_engine() || !name || strcmp(name, "TZ") != 0)
        return ts_raw_setenv(name, value, overwrite);
    if (valid_iana_tz(value)) {
        int rc = ts_raw_setenv(name, value, overwrite);
        if (rc == 0) {
            tzset();
            const char *cur = getenv("TZ");
            if (cur && !strcmp(cur, value)) persist_engine_tz(value);
        }
        return rc;
    }
    // Reject empty/UTC/invalid overwrites when a valid Region/City timezone is already active or saved.
    const char *cur = getenv("TZ");
    if (valid_iana_tz(cur)) return 0;
    char saved[64];
    if (load_saved_engine_tz(saved)) {
        if (ts_raw_setenv("TZ", saved, 1) == 0) tzset();
        return 0;
    }
    return ts_raw_setenv(name, value, overwrite);
}

__attribute__((visibility("default")))
int unsetenv(const char *name) {
    const char * volatile nm = name;
    if (is_engine() && nm && !strcmp(nm, "TZ")) {
        const char *cur = getenv("TZ");
        if (valid_iana_tz(cur)) return 0;
        char saved[64];
        if (load_saved_engine_tz(saved)) {
            if (ts_raw_setenv("TZ", saved, 1) == 0) tzset();
            return 0;
        }
    }
    return ts_raw_unsetenv(name);
}

static time_t parse_etc_timestamp(void) {
    const char *path = getenv("ENGINEX_TEST_ETC_TIMESTAMP");
    if (!path || !*path) path = "/etc/timestamp";
    int fd = ts_raw_open(path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC, 0);
    if (fd < 0) return 0;
    char buf[32];
    ssize_t n = read(fd, buf, sizeof buf - 1);
    close(fd);
    if (n < 14) return 0;
    for (int i = 0; i < 14; i++) if (buf[i] < '0' || buf[i] > '9') return 0;
    int y = (buf[0]-'0')*1000 + (buf[1]-'0')*100 + (buf[2]-'0')*10 + (buf[3]-'0');
    int m = (buf[4]-'0')*10 + (buf[5]-'0');
    int d = (buf[6]-'0')*10 + (buf[7]-'0');
    int hh = (buf[8]-'0')*10 + (buf[9]-'0');
    int mm = (buf[10]-'0')*10 + (buf[11]-'0');
    int ss = (buf[12]-'0')*10 + (buf[13]-'0');
    if (y < 2020 || m < 1 || m > 12 || d < 1 || d > 31 || hh > 23 || mm > 59 || ss > 60) return 0;
    int yr = y - (m <= 2);
    int era = (yr >= 0 ? yr : yr - 399) / 400;
    unsigned yoe = (unsigned)(yr - era * 400);
    unsigned doy = (153u * (unsigned)(m + (m > 2 ? -3 : 9)) + 2u) / 5u + (unsigned)d - 1u;
    unsigned doe = yoe * 365u + yoe / 4u - yoe / 100u + doy;
    long long days = (long long)era * 146097LL + (long long)doe - 719468LL;
    return (time_t)(days * 86400LL + hh * 3600LL + mm * 60LL + ss);
}

static void timesync_owner(uid_t *uid_out, gid_t *gid_out) {
    *uid_out = (uid_t)-1;
    *gid_out = (gid_t)-1;
    int fd = ts_raw_open("/etc/passwd", O_RDONLY | O_CLOEXEC, 0);
    if (fd < 0) return;
    char buf[4096];
    ssize_t n = read(fd, buf, sizeof buf - 1);
    close(fd);
    if (n <= 0) return;
    buf[n] = 0;
    const char *p = strstr(buf, "systemd-timesync:x:");
    if (!p || (p != buf && p[-1] != '\n')) return;
    unsigned u = 0, g = 0;
    if (sscanf(p + 19, "%u:%u:", &u, &g) == 2) {
        *uid_out = (uid_t)u;
        *gid_out = (gid_t)g;
    }
}

static void persist_monotonic_clock(void) {
    const char *cpath = getenv("ENGINEX_TEST_CLOCK_FILE");
    if (!cpath || !*cpath) cpath = "/var/lib/systemd/timesync/clock";
    struct timespec now = {0, 0};
    clock_gettime(CLOCK_REALTIME, &now);
    time_t floor_ts = parse_etc_timestamp();
    struct timespec target = now;
    if (floor_ts > target.tv_sec) {
        target.tv_sec = floor_ts;
        target.tv_nsec = 0;
    }
    if (target.tv_sec < 1700000000) return;
    struct stat st;
    if (fstatat(AT_FDCWD, cpath, &st, AT_SYMLINK_NOFOLLOW) != 0) {
        if (errno != ENOENT) return;
        if (ts_mkdirs_parent(cpath, 0755) != 0) return;
        int cfd = ts_raw_open(cpath, O_WRONLY | O_CREAT | O_NOFOLLOW | O_CLOEXEC, 0644);
        if (cfd < 0) return;
        uid_t uid; gid_t gid;
        timesync_owner(&uid, &gid);
        if (uid != (uid_t)-1) {
            if (fchown(cfd, uid, gid) != 0) { /* best effort */ }
            char dir[PATH_MAX];
            if (strlen(cpath) < sizeof dir) {
                strcpy(dir, cpath);
                char *sl = strrchr(dir, '/');
                if (sl && sl != dir) { *sl = 0; if (chown(dir, uid, gid) != 0) { /* best effort */ } }
            }
        }
        close(cfd);
        st.st_mtim.tv_sec = 0;
        st.st_mtim.tv_nsec = 0;
    } else if (!S_ISREG(st.st_mode)) {
        return;
    }
    if (target.tv_sec < st.st_mtim.tv_sec ||
        (target.tv_sec == st.st_mtim.tv_sec && target.tv_nsec <= st.st_mtim.tv_nsec))
        return;
    struct timespec ts[2] = {target, target};
    if (utimensat(AT_FDCWD, cpath, ts, AT_SYMLINK_NOFOLLOW) == 0) {
        int fd = ts_raw_open(cpath, O_RDONLY | O_NOFOLLOW | O_CLOEXEC, 0);
        if (fd >= 0) { fsync(fd); close(fd); }
    }
}

#endif
