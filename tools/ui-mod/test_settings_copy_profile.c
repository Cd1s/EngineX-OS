// gcc -std=c11 -Wall -Wextra -Werror test_settings_copy_profile.c -o test-settings-profile
#define _GNU_SOURCE
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>
#include "settings_copy_profile.h"

int main(void) {
    char marker[] = "/tmp/settings-copy-profile.XXXXXX";
    int fd = mkstemp(marker);
    assert(fd >= 0);
    const char *path = "/media/My USB/Engine Library/user.profile";
    const char *filtered = "/data/enginex/settings-copy/usb.profile";
    char out[PATH_MAX];
    assert(dprintf(fd, "%lld\n%s\n", (long long)time(NULL) + 20, path) > 0);
    close(fd);
    assert(sc_copy_profile_read(path, O_RDONLY | O_CLOEXEC, marker, filtered, out, sizeof out));
    assert(!strcmp(out, filtered));
    assert(!sc_copy_profile_read(path, O_WRONLY, marker, filtered, out, sizeof out));
    assert(!sc_copy_profile_read(path, O_RDWR, marker, filtered, out, sizeof out));
    assert(!sc_copy_profile_read("/media/Other/Engine Library/user.profile", O_RDONLY, marker, filtered, out, sizeof out));
    assert(!sc_copy_profile_read("/data/StreamingAccounts.json", O_RDONLY, marker, filtered, out, sizeof out));
    assert(!sc_copy_profile_read(path, O_RDONLY, marker, filtered, out, 2));
    fd = open(marker, O_WRONLY | O_TRUNC);
    assert(fd >= 0);
    assert(dprintf(fd, "%lld\n%s\n", (long long)time(NULL) - 1, path) > 0);
    close(fd);
    assert(sc_copy_profile_read(path, O_RDONLY, marker, filtered, out, sizeof out) == -1);
    fd = open(marker, O_WRONLY | O_TRUNC);
    assert(fd >= 0);
    assert(dprintf(fd, "%lld\n%s\nextra", (long long)time(NULL) + 20, path) > 0);
    close(fd);
    assert(sc_copy_profile_read(path, O_RDONLY, marker, filtered, out, sizeof out) <= 0);
    assert(!unlink(marker));
    assert(sc_copy_profile_read(path, O_RDONLY, marker, filtered, out, sizeof out) <= 0);
    puts("settings profile: exact consent path, read-only, expiry and untouched other files PASS");
    return 0;
}
