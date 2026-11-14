#ifndef SC_SETTINGS_COPY_PROFILE_H
#define SC_SETTINGS_COPY_PROFILE_H

/* The stock USB loader keeps its normal path. During an explicitly approved
 * import, only that exact read sees the validated private profile. The user's
 * drive is never rewritten. libc's native ABI is shared by armhf and AZ04. */
static int sc_copy_profile_read(const char *path, int flags, const char *marker,
                                const char *filtered, char *out, size_t capacity) {
    static const char tail[] = "/Engine Library/user.profile";
    if (!path || (flags & O_ACCMODE) != O_RDONLY || (flags & (O_CREAT | O_TRUNC | O_APPEND))) return 0;
    size_t n = strlen(path), suffix = sizeof(tail) - 1;
    if (n < suffix || strcmp(path + n - suffix, tail)) return 0;
    int fd = (int)syscall(SYS_openat, AT_FDCWD, marker, O_RDONLY | O_CLOEXEC, 0);
    if (fd < 0) return 0;
    char buf[PATH_MAX + 64];
    ssize_t count = read(fd, buf, sizeof(buf) - 1);
    close(fd);
    if (count <= 0 || count == (ssize_t)sizeof(buf) - 1) return 0;
    buf[count] = 0;
    char *end;
    errno = 0;
    long long expires = strtoll(buf, &end, 10);
    if (errno || end == buf || *end != '\n') return 0;
    time_t now = time(NULL);
    char *original = end + 1;
    char *newline = strchr(original, '\n');
    if (!newline || newline[1] != 0) return 0;
    *newline = 0;
    if (strcmp(path, original) || strlen(filtered) >= capacity) return 0;
    // A delayed approved read must never fall through to the unfiltered USB.
    if (expires < now || expires - now > 30) return -1;
    strcpy(out, filtered);
    return 1;
}
#endif
