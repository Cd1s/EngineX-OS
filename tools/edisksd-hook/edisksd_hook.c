// LD_PRELOAD shim for /usr/libexec/edisksd. edisksd answers Engine's D-Bus Mount
// call only after mnt_context_mount() returns, so anything done here is complete
// before Engine can open the volume. util-linux 2.39 mounts through
// fsopen/move_mount, so mount(2) itself is not a usable interposition point.
//
// After a successful mount below /media:
//   1. read-only HFS+ with a rekordbox export gets its writable Engine Library
//      overlay (hfsplus.ko cannot take writes, and refuses to be a lower layer);
//   2. rb-bridge converts the export;
//   3. engine-guardian repairs table data only.
// Engine is not attached yet, so these are the only moments an external process
// may write its databases. Failures are logged and never fail the mount.
#define _GNU_SOURCE
#include <dirent.h>
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <sys/statvfs.h>
#include <sys/vfs.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#define HFSPLUS_MAGIC 0x482b
#define HOOK_BUDGET_MS 20000
#define LIB_ROOT "/data/sc5000-custom/hfs-lib"
#define EMPTY_LOWER "/run/sc5000-empty"

struct libmnt_context;
extern const char *mnt_context_get_target(struct libmnt_context *);
extern int mnt_context_get_status(struct libmnt_context *);

static void note(const char *what, const char *path, int err) {
	fprintf(stderr, "edisksd-hook: %s %s%s%s\n", what, path, err ? ": " : "", err ? strerror(err) : "");
}

// volume returns the name in /media/<name>, or NULL for anything else.
static const char *volume(const char *target) {
	if (!target || strncmp(target, "/media/", 7) != 0) return NULL;
	const char *name = target + 7;
	if (!*name || strchr(name, '/') || !strcmp(name, ".") || !strcmp(name, "..")) return NULL;
	return name;
}

static long now_ms(void) {
	struct timespec t;
	clock_gettime(CLOCK_MONOTONIC, &t);
	return t.tv_sec * 1000L + t.tv_nsec / 1000000L;
}

static int make_dirs(const char *path) {
	char p[PATH_MAX];
	if (snprintf(p, sizeof p, "%s", path) >= (int)sizeof p) return ENAMETOOLONG;
	for (char *s = p + 1; *s; s++) {
		if (*s != '/') continue;
		*s = 0;
		if (mkdir(p, 0755) && errno != EEXIST) return errno;
		*s = '/';
	}
	if (mkdir(p, 0755) && errno != EEXIST) return errno;
	return 0;
}

// has_export accepts PIONEER and macOS .PIONEER in any case, matching rb-bridge.
static int has_export(const char *target) {
	DIR *d = opendir(target);
	if (!d) return 0;
	int found = 0;
	for (struct dirent *e; !found && (e = readdir(d));)
		found = !strcasecmp(e->d_name, "PIONEER") || !strcasecmp(e->d_name, ".PIONEER");
	closedir(d);
	return found;
}

static void mount_overlay(const char *target, const char *name) {
	char lib[PATH_MAX], upper[PATH_MAX], work[PATH_MAX], opts[3 * PATH_MAX];
	snprintf(lib, sizeof lib, "%s/Engine Library", target);
	snprintf(upper, sizeof upper, LIB_ROOT "/%s/upper", name);
	snprintf(work, sizeof work, LIB_ROOT "/%s/work", name);
	struct statvfs v;
	if (statvfs(lib, &v)) {
		note("no Engine Library directory on read-only volume", target, errno);
		return;
	}
	if (!(v.f_flag & ST_RDONLY)) return;
	int e = make_dirs(EMPTY_LOWER);
	if (!e) e = make_dirs(upper);
	if (!e) e = make_dirs(work);
	if (e) {
		note("cannot prepare overlay for", target, e);
		return;
	}
	snprintf(opts, sizeof opts, "lowerdir=" EMPTY_LOWER ",upperdir=%s,workdir=%s", upper, work);
	if (mount("overlay", lib, "overlay", 0, opts)) note("overlay failed on", lib, errno);
	else note("overlay ready on", lib, 0);
}

// run executes argv under a shared deadline. An unfinished import is killed:
// its transaction rolls back, whereas leaving it running would write beneath
// an Engine that is about to attach.
static void run(char *const argv[], long deadline) {
	pid_t pid = fork();
	if (pid < 0) {
		note("fork failed for", argv[0], errno);
		return;
	}
	if (pid == 0) {
		setpgid(0, 0);
		unsetenv("LD_PRELOAD");
		setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", 1);
		execv(argv[0], argv);
		_exit(127);
	}
	setpgid(pid, pid);
	for (;;) {
		int status;
		pid_t r = waitpid(pid, &status, WNOHANG);
		if (r == pid) {
			if (!WIFEXITED(status) || WEXITSTATUS(status)) note("step failed:", argv[0], 0);
			return;
		}
		if (r < 0 && errno != EINTR) return;
		if (now_ms() >= deadline) {
			note("deadline exceeded, killing", argv[0], 0);
			kill(-pid, SIGKILL);
			waitpid(pid, &status, 0);
			return;
		}
		usleep(20000);
	}
}

static void after_mount(const char *target) {
	const char *name = volume(target);
	if (!name) return;
	long deadline = now_ms() + HOOK_BUDGET_MS;
	int pioneer = has_export(target);
	struct statfs f;
	struct statvfs v;
	if (pioneer && !statfs(target, &f) && f.f_type == HFSPLUS_MAGIC && !statvfs(target, &v) && (v.f_flag & ST_RDONLY))
		mount_overlay(target, name);
	if (pioneer) {
		char *const bridge[] = {"/usr/local/bin/rb-bridge", "--mount-path", (char *)target, NULL};
		run(bridge, deadline);
	}
	// A rekordbox volume's library comes from rb-bridge, and scanning its thousands
	// of tracks here would hold up every mount for 15 seconds or more.
	char db[PATH_MAX];
	snprintf(db, sizeof db, "%s/Engine Library/Database2/m.db", target);
	if (!pioneer && !access(db, F_OK)) {
		char *const guardian[] = {"/usr/local/bin/engine-guardian", "--heal-mount", (char *)target, "--data-only", NULL};
		run(guardian, deadline);
	}
}

int mnt_context_mount(struct libmnt_context *cxt) {
	static int (*real)(struct libmnt_context *);
	if (!real) real = dlsym(RTLD_NEXT, "mnt_context_mount");
	int rc = real(cxt);
	if (rc == 0 && mnt_context_get_status(cxt) == 1) {
		int saved = errno;
		after_mount(mnt_context_get_target(cxt));
		errno = saved;
	}
	return rc;
}

// The overlay is a submount, and would otherwise make every eject fail EBUSY.
int mnt_context_umount(struct libmnt_context *cxt) {
	static int (*real)(struct libmnt_context *);
	if (!real) real = dlsym(RTLD_NEXT, "mnt_context_umount");
	const char *target = mnt_context_get_target(cxt);
	if (volume(target)) {
		char lib[PATH_MAX];
		snprintf(lib, sizeof lib, "%s/Engine Library", target);
		struct statfs f;
		if (!statfs(lib, &f) && f.f_type == 0x794c7630 /* OVERLAYFS_SUPER_MAGIC */ && umount2(lib, MNT_DETACH))
			note("overlay detach failed on", lib, errno);
	}
	return real(cxt);
}
