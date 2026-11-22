#include "../legacy_names.h"
// Writable HFS library presentation without using HFS as an overlayfs lower.
// All mount changes occur before Engine attaches. Only recorded mount IDs may
// be removed; a failed eject restores the preallocated private view tree.
#define _GNU_SOURCE
#include <dirent.h>
#include <dlfcn.h>
#include <elf.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <stdint.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <sched.h>
#include <sys/mman.h>
#include <sys/mount.h>
#include <sys/random.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <sys/statvfs.h>
#include <sys/syscall.h>
#include <sys/sysmacros.h>
#include <sys/utsname.h>
#include <sys/vfs.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#ifndef LIB_ROOT
#define LIB_ROOT "/data/enginex/hfs-libraries"
#endif
#ifndef LEGACY_LIB_ROOT
#define LEGACY_LIB_ROOT LEGACY_HFS_LIB_ROOT
#endif
#ifndef PREPARE_STATE_PATH
#define PREPARE_STATE_PATH "/run/enginex-usb-preparing"
#endif
#ifndef RUN_ROOT
#define RUN_ROOT "/run/enginex-hfs-views"
#endif
// Where a hook before 5.3.0.5 (#234) kept its helper mounts. Hidden as well: an in-place update
// restarts edisksd under this hook while views the old one mounted are still there.
#ifndef LEGACY_RUN_ROOT
#define LEGACY_RUN_ROOT LEGACY_HFS_RUN_ROOT
#endif
#ifndef MEDIA_ROOT
#define MEDIA_ROOT "/media"
#endif
#ifndef UTAB_DIR
#define UTAB_DIR "/run/mount"
#endif
#ifndef LSTAT
#define LSTAT lstat // the tests give it the I/O error of a stick pulled without Eject
#endif
#define HFSPLUS_MAGIC 0x482b
#define HOOK_BUDGET_MS INT64_C(20000)
#define GUARDIAN_SOFT_MARGIN_MS INT64_C(3000) // measured 1.12 s worst-case from soft expiry to child reap on armhf, 20 ms polling
#define MAX_VIEW_ENTRIES 4096
#define VIEW_MAGIC UINT64_C(0x5343484653563031)
struct libmnt_context;
extern const char *mnt_context_get_target(struct libmnt_context *);
extern const char *mnt_context_get_source(struct libmnt_context *);
extern int mnt_context_get_status(struct libmnt_context *);
extern int mnt_context_set_syscall_status(struct libmnt_context *, int);
static pthread_mutex_t view_lock = PTHREAD_MUTEX_INITIALIZER;
struct view_child { char name[NAME_MAX + 1]; uint64_t mount_id; };
struct owned_view {
    uint64_t magic, base_id, public_id;
    unsigned count;
    char target[PATH_MAX], anchor[PATH_MAX], identity[80];
    struct view_child child[MAX_VIEW_ENTRIES];
};

static void note(const char *what, const char *path, int err) {
    fprintf(stderr,"edisksd-hook: %s %s%s%s\n",what,path,err?": ":"",err?strerror(err):"");
}
static int path_join(char *out, size_t size, const char *a, const char *b) {
    if (snprintf(out,size,"%s/%s",a,b) >= (int)size) { errno=ENAMETOOLONG; return -1; }
    return 0;
}
static const char *volume(const char *target) {
    size_t n=strlen(MEDIA_ROOT);
    if (!target || strncmp(target,MEDIA_ROOT,n) || target[n]!='/') return NULL;
    const char *name=target+n+1;
    if (!*name || strchr(name,'/') || !strcmp(name,".") || !strcmp(name,"..")) return NULL;
    return name;
}
static int64_t timespec_ms(const struct timespec *t) {
    return (int64_t)t->tv_sec * INT64_C(1000) + t->tv_nsec/1000000;
}
static int64_t deadline_from(int64_t now) {
    return now>INT64_MAX-HOOK_BUDGET_MS?INT64_MAX:now+HOOK_BUDGET_MS;
}
static int64_t now_ms(void) {
    struct timespec t;
    if (clock_gettime(CLOCK_MONOTONIC,&t)) return INT64_MAX;
    return timespec_ms(&t);
}
static int budget(int64_t deadline) {
    if (now_ms() >= deadline) { errno=ETIMEDOUT; return -1; }
    return 0;
}
static int64_t guardian_soft_budget_ms(int64_t left) {
    int64_t soft = left > INT64_C(4000) ? left - GUARDIAN_SOFT_MARGIN_MS : (left * 3) / 4;
    return soft < INT64_C(500) ? INT64_C(500) : soft;
}
static int guardian_window_available(int64_t left) {
    return left >= INT64_C(1000);
}

// Check EVERY component. A missing final component is allowed only when the
// caller is about to create it in an already checked, trusted parent.
static int safe_path(const char *path, int missing_final) {
    if (!path || path[0]!='/') { errno=EINVAL; return -1; }
    char copy[PATH_MAX];
    if (snprintf(copy,sizeof copy,"%s",path)>=(int)sizeof copy) {errno=ENAMETOOLONG;return -1;}
    for (char *p=copy+1;;p++) if (!*p || *p=='/') {
        char saved=*p; *p=0;
        const char *part=strrchr(copy,'/'); part=part?part+1:copy;
        if (!*part || !strcmp(part,".") || !strcmp(part,"..")) {errno=EINVAL;return -1;}
        struct stat st;
        if (LSTAT(copy,&st)) { if (!saved && missing_final && errno==ENOENT) return 0; return -1; }
        if (S_ISLNK(st.st_mode)) {errno=ELOOP;return -1;}
        if (saved && !S_ISDIR(st.st_mode)) {errno=ENOTDIR;return -1;}
        *p=saved; if (!saved) return 0;
    }
}
// Status only: atomic publication on /run, with no durability work on the mount path.
static int prepare_err_path(char out[PATH_MAX]) {
    return snprintf(out,PATH_MAX,"%s.err",PREPARE_STATE_PATH)>=PATH_MAX?-1:0;
}
static void write_run_atomic(const char *dst,const char *body) {
    char parent[PATH_MAX],tmp[PATH_MAX];
    if(snprintf(parent,sizeof parent,"%s",dst)>=(int)sizeof parent)return;
    char *slash=strrchr(parent,'/');if(!slash||slash==parent)return;
    *slash=0;
    struct stat st;
    if(safe_path(parent,0)||lstat(parent,&st)||!S_ISDIR(st.st_mode))return;
    if(path_join(tmp,sizeof tmp,parent,".enginex-usb-prep.XXXXXX"))return;
    static const char alphabet[]="abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789";
    size_t len=strlen(body);
    for(int attempt=0;attempt<8;attempt++) {
        unsigned char random[6];
        if(getrandom(random,sizeof random,GRND_NONBLOCK)!=(ssize_t)sizeof random)break;
        char *suffix=tmp+strlen(tmp)-6;
        for(unsigned i=0;i<6;i++)suffix[i]=alphabet[random[i]%(sizeof alphabet-1)];
        int fd=open(tmp,O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC,0600);
        if(fd<0){if(errno==EEXIST)continue;break;}
        int ok=write(fd,body,len)==(ssize_t)len&&!fchmod(fd,0644);
        if(close(fd))ok=0;
        if(ok)rename(tmp,dst);
        unlink(tmp);
        break;
    }
}
static void set_preparing(const char *target,int clear_err) {
    if(!volume(target))return;
    int saved=errno;
    if(clear_err) {
        char err_path[PATH_MAX];
        if(!prepare_err_path(err_path))unlink(err_path);
    }
    write_run_atomic(PREPARE_STATE_PATH,"1\n");
    errno=saved;
}
static void set_preparing_error(const char *target,const char *source,const char *err_msg) {
    if(!err_msg||!*err_msg)return;
    int saved=errno;
    char err_path[PATH_MAX],body[512];
    if(!prepare_err_path(err_path)&&access(err_path,F_OK)!=0) {
        const char *kind=((target&&strstr(target,"mmcblk"))||(source&&strstr(source,"mmcblk")))?"sd":"usb";
        if(snprintf(body,sizeof body,"{\"state\":\"error\",\"kind\":\"%s\",\"error\":\"%s\"}\n",kind,err_msg)<(int)sizeof body)
            write_run_atomic(err_path,body);
    }
    errno=saved;
}
static void clear_preparing(void) {
    int saved=errno;unlink(PREPARE_STATE_PATH);errno=saved;
}
static int make_dirs(const char *path) {
    char copy[PATH_MAX];
    if (snprintf(copy,sizeof copy,"%s",path)>=(int)sizeof copy) {errno=ENAMETOOLONG;return -1;}
    for (char *p=copy+1;;p++) if (!*p || *p=='/') {
        char saved=*p; *p=0;
        if (safe_path(copy,1)) return -1;
        if (mkdir(copy,0700) && errno!=EEXIST) return -1;
        struct stat st;
        if (lstat(copy,&st) || !S_ISDIR(st.st_mode)) {errno=ENOTDIR;return -1;}
        *p=saved; if (!saved) return 0;
    }
}
// The HFS library cache lives in /data/enginex. Since #103 the legacy data directory is
// a relative symlink to that sibling, and safe_path refuses every symlink, so a
// cache path through it failed with ELOOP for every read-only HFS+ stick before
// rb-bridge ran. Use the first configured root whose parent is a real directory,
// following only that one compat link: a plain sibling name, to a directory we
// own that nobody else can write. Everything below it is still walked safely.
static int lib_root(char out[PATH_MAX]) {
    const char *roots[]={LIB_ROOT,LEGACY_LIB_ROOT};int err=ENOENT,absent=0,tried=0;
    for(unsigned i=0;i<2;i++) {
        if(!roots[i]||!*roots[i])continue;
        tried++;
        char parent[PATH_MAX];
        if(snprintf(parent,sizeof parent,"%s",roots[i])>=(int)sizeof parent){err=ENAMETOOLONG;continue;}
        char *slash=strrchr(parent,'/');if(!slash||slash==parent||!slash[1]){err=EINVAL;continue;}
        *slash=0;const char *base=slash+1;
        struct stat st;
        if(lstat(parent,&st)){err=errno;if(errno==ENOENT)absent++;continue;}
        if(S_ISLNK(st.st_mode)) {
            char link[NAME_MAX+1];ssize_t n=readlink(parent,link,sizeof link-1);
            if(n<=0||n>=(ssize_t)sizeof link-1){err=ELOOP;continue;}
            link[n]=0;
            if(strchr(link,'/')||!strcmp(link,".")||!strcmp(link,"..")){err=ELOOP;continue;}
            char *dir=strrchr(parent,'/');*dir=0;
            char sibling[PATH_MAX];
            if(snprintf(sibling,sizeof sibling,"%s/%s",*parent?parent:"",link)>=(int)sizeof sibling){err=ENAMETOOLONG;continue;}
            if(safe_path(sibling,0)||lstat(sibling,&st)||!S_ISDIR(st.st_mode)||st.st_uid!=geteuid()||(st.st_mode&022)){err=ELOOP;continue;}
            if(snprintf(out,PATH_MAX,"%s/%s",sibling,base)>=PATH_MAX){err=ENAMETOOLONG;continue;}
            return 0;
        }
        if(!S_ISDIR(st.st_mode)){err=ENOTDIR;continue;}
        if(safe_path(parent,0)||st.st_uid!=geteuid()||(st.st_mode&022)){err=EACCES;continue;}
        if(snprintf(out,PATH_MAX,"%s",roots[i])>=PATH_MAX){err=ENAMETOOLONG;continue;}
        return 0;
    }
    // A blank /data (first boot, or edisksd started before the prepare unit):
    // neither directory exists yet, so make_dirs creates the primary one 0700,
    // as the hook did before. A dangling or foreign link is not "absent".
    if(tried&&absent==tried&&roots[0]&&*roots[0]) {
        if(snprintf(out,PATH_MAX,"%s",roots[0])>=PATH_MAX){errno=ENAMETOOLONG;return -1;}
        return 0;
    }
    errno=err;return -1;
}
static uint64_t mount_id(const char *path) {
    struct statx st;
    if (safe_path(path,0) || statx(AT_FDCWD,path,AT_SYMLINK_NOFOLLOW|AT_NO_AUTOMOUNT,STATX_MNT_ID,&st) ||
        !(st.stx_mask&STATX_MNT_ID)) return 0;
    return st.stx_mnt_id;
}
static int secure_runtime(void) {
    struct stat st;
    if (make_dirs(RUN_ROOT) || lstat(RUN_ROOT,&st) || st.st_uid!=geteuid() || (st.st_mode&077)) {
        errno=EACCES;return -1;
    }
    return 0;
}
static uint64_t hash_bytes(uint64_t h,const void *data,size_t n) {
    const unsigned char *p=data;
    while(n--) {h^=*p++;h*=UINT64_C(1099511628211);} return h;
}
#ifndef VOLUME_IDENTITY
static int volume_identity(const char *source,char out[80]) {
    char device[PATH_MAX]; struct stat st;
    if (!source || !realpath(source,device) || strncmp(device,"/dev/",5) ||
        stat(device,&st) || !S_ISBLK(st.st_mode)) {errno=ENODEV;return -1;}
    // libmount already depends on libblkid on the supported image. Resolve the
    // optional API dynamically so the shim has no new loader dependency.
    char *(*get_tag)(void *,const char *,const char *)=dlsym(RTLD_DEFAULT,"blkid_get_tag_value");
    char *uuid=get_tag?get_tag(NULL,"UUID",device):NULL;
    uint64_t h=UINT64_C(14695981039346656037);
    if (uuid && *uuid) {
        h=hash_bytes(h,uuid,strlen(uuid)); free(uuid);
        snprintf(out,80,"uuid-%016llx",(unsigned long long)h);return 0;
    }
    free(uuid);
    int fd=open(device,O_RDONLY|O_CLOEXEC|O_NOFOLLOW);
    if(fd<0)return -1;
    unsigned char header[8192]; ssize_t n=pread(fd,header,sizeof header,0);int saved=errno;close(fd);errno=saved;
    if(n<1024){if(n>=0)errno=EIO;return -1;}
    h=hash_bytes(h,header,(size_t)n);
    snprintf(out,80,"signature-%016llx",(unsigned long long)h);return 0;
}
#define VOLUME_IDENTITY volume_identity
#endif

// Keep the I/O buffer out of recursive directory-walk frames.
static __attribute__((noinline)) int copy_file_data(int in,int out,int64_t deadline) {
    char buf[8192];ssize_t n;
    while((n=read(in,buf,sizeof buf))!=0) {
        if(n<0){if(errno==EINTR)continue;return -1;}
        if(budget(deadline))return -1;
        for(ssize_t pos=0;pos<n;) {
            ssize_t wrote=write(out,buf+pos,(size_t)(n-pos));
            if(wrote<0&&errno==EINTR)continue;
            if(wrote<=0){if(!wrote)errno=EIO;return -1;}pos+=wrote;
        }
    }
    return fsync(out);
}

// No links, special files or traversal are accepted in seeded Engine metadata.
// O_NOFOLLOW/openat pins each source directory and each copied file.
static int copy_tree(int src,int dst,int64_t deadline,unsigned depth,uint64_t *bytes) {
    if(depth>64){errno=ELOOP;return -1;}
    int copyfd=openat(src,".",O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(copyfd<0)return -1;
    DIR *dir=fdopendir(copyfd);if(!dir){close(copyfd);return -1;}
    int rc=0;struct dirent *e;
    for(;;) {
        errno=0;e=readdir(dir);if(!e){if(errno)rc=-1;break;}
        if(!strcmp(e->d_name,".") || !strcmp(e->d_name,".."))continue;
        if(budget(deadline)){rc=-1;break;}
        struct stat st;
        if(fstatat(src,e->d_name,&st,AT_SYMLINK_NOFOLLOW)){rc=-1;break;}
        if(S_ISLNK(st.st_mode) || (!S_ISDIR(st.st_mode)&&!S_ISREG(st.st_mode))){errno=ELOOP;rc=-1;break;}
        int in=openat(src,e->d_name,O_RDONLY|O_CLOEXEC|O_NOFOLLOW|(S_ISDIR(st.st_mode)?O_DIRECTORY:0));
        if(in<0){rc=-1;break;}
        if(S_ISDIR(st.st_mode)) {
            int out=-1;
            if(dst>=0) {
                if(mkdirat(dst,e->d_name,0700)){close(in);rc=-1;break;}
                out=openat(dst,e->d_name,O_RDONLY|O_DIRECTORY|O_CLOEXEC|O_NOFOLLOW);
                if(out<0){close(in);rc=-1;break;}
            }
            rc=copy_tree(in,out,deadline,depth+1,bytes);if(out>=0)close(out);
        } else if(bytes)*bytes+=(uint64_t)st.st_size;
        if(!S_ISDIR(st.st_mode)&&dst>=0) {
            int out=openat(dst,e->d_name,O_WRONLY|O_CREAT|O_EXCL|O_CLOEXEC|O_NOFOLLOW,0600);
            if(out<0){close(in);rc=-1;break;}
            rc=copy_file_data(in,out,deadline);
            close(out);
        }
        close(in);if(rc)break;
    }
    if(!rc&&dst>=0&&fsync(dst))rc=-1;
    int saved=errno;closedir(dir);errno=saved;return rc;
}
static void remove_tree(int parent,const char *name) {
    int fd=openat(parent,name,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
    if(fd<0){unlinkat(parent,name,0);return;}
    DIR *d=fdopendir(fd);if(!d){close(fd);return;}
    struct dirent *e;
    while((e=readdir(d)))if(strcmp(e->d_name,".")&&strcmp(e->d_name,"..")) {
        struct stat st;
        if(!fstatat(fd,e->d_name,&st,AT_SYMLINK_NOFOLLOW)&&S_ISDIR(st.st_mode))remove_tree(fd,e->d_name);
        else unlinkat(fd,e->d_name,0);
    }
    closedir(d);unlinkat(parent,name,AT_REMOVEDIR);
}
// The private conversion's library goes to the visible mount that follows it
// under the same view_lock: seeded_as is what seed_library last resolved (sent
// up by the pre-mount child), seeded_hint what the visible mount may reuse. The
// stick's Engine database is then not read from the stick a second time inside
// POST_RESERVE_MS; it was hashed and its tree checked a moment ago on this very
// mount, and the view hides it. The cached library is still checked.
static char seeded_as[32],seeded_hint[32];
static int seeded_ok(const char *name) {
    return !strncmp(name,"library-",8)&&strlen(name)==24&&strspn(name+8,"0123456789abcdef")==16;
}
// BH-O55-1: only Engine's metadata, Database2, is copied to /data. The rest of
// the stick's Engine Library (Engine DJ puts the audio in Engine Library/Music)
// is bound read-only from the stick into the view (bind_native). Copying it all
// wrote every track to /data on each insert, ran out of the budget on a real
// collection (no view at all) and kept a full copy per PC export.
#ifndef HFS_CACHE_RESERVE
#define HFS_CACHE_RESERVE (UINT64_C(512)<<20)
#endif
// Set in the pre-mount child: edisksd's views carry other mount ids there, so
// nothing in the cache may be judged unused (and removed) from inside it.
static int private_ns;
// The top level of a stick's Engine Library: no links or special files. *meta
// is its Database2 directory, -1 when it has none.
static int library_top(int lib,int *meta) {
    *meta=-1;
    int fd=openat(lib,".",O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(fd<0)return -1;
    DIR *d=fdopendir(fd);if(!d){close(fd);return -1;}
    int rc=0;struct dirent *e;
    for(;;) {
        errno=0;e=readdir(d);if(!e){if(errno)rc=-1;break;}
        if(!strcmp(e->d_name,".")||!strcmp(e->d_name,".."))continue;
        struct stat st;
        if(fstatat(lib,e->d_name,&st,AT_SYMLINK_NOFOLLOW)){rc=-1;break;}
        if(S_ISLNK(st.st_mode)||(!S_ISDIR(st.st_mode)&&!S_ISREG(st.st_mode))){errno=ELOOP;rc=-1;break;}
    }
    int saved=errno;closedir(d);errno=saved;if(rc)return -1;
    *meta=openat(lib,"Database2",O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
    if(*meta<0&&errno!=ENOENT&&errno!=ENOTDIR)return -1; // a plain file named so is bound like the rest
    return 0;
}
static void touch_library(const char *library){utimensat(AT_FDCWD,library,NULL,AT_SYMLINK_NOFOLLOW);} // last use, for the LRU below
static int newer(const struct stat *a,const struct stat *b) {
    return a->st_mtim.tv_sec!=b->st_mtim.tv_sec?a->st_mtim.tv_sec>b->st_mtim.tv_sec:a->st_mtim.tv_nsec>b->st_mtim.tv_nsec;
}
static struct owned_view *find_view(const char *,const char *);
// The least recently used generation in the cache that may go: never one of the
// stick being seeded (`identity`: its own previous generation holds the cues set
// on the deck; prune_generations keeps two once its view is up), never one of a
// stick whose view is up. *total (when asked): the bytes of all that may go.
static int oldest_generation(const char *cache,const char *identity,uint64_t *total,char id_out[NAME_MAX+1],char name_out[NAME_MAX+1]) {
    DIR *d=opendir(cache);if(!d)return -1;
    struct stat best_st={0};struct dirent *e;*id_out=*name_out=0;
    while((e=readdir(d))) {
        if(e->d_name[0]=='.'||!strcmp(e->d_name,identity))continue;
        struct owned_view *live=find_view(NULL,e->d_name);if(live){free(live);continue;}
        int id=openat(dirfd(d),e->d_name,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(id<0)continue;
        DIR *g=fdopendir(id);if(!g){close(id);continue;}struct dirent *l;
        while((l=readdir(g))) {
            struct stat st;char path[PATH_MAX];
            if(!seeded_ok(l->d_name)||fstatat(id,l->d_name,&st,AT_SYMLINK_NOFOLLOW)||!S_ISDIR(st.st_mode))continue;
            if(snprintf(path,sizeof path,"%s/%s/%s",cache,e->d_name,l->d_name)>=(int)sizeof path)continue;
            if(total){int fd=openat(id,l->d_name,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);uint64_t n=0;if(fd>=0){if(!copy_tree(fd,-1,INT64_MAX,0,&n))*total+=n;close(fd);}}
            if(!*name_out||newer(&best_st,&st)){best_st=st;snprintf(id_out,NAME_MAX+1,"%s",e->d_name);snprintf(name_out,NAME_MAX+1,"%s",l->d_name);}
        }
        closedir(g);
    }
    closedir(d);return *name_out?0:-1;
}
// /data keeps HFS_CACHE_RESERVE free after a seed of `bytes`: Engine's own
// settings, EngineX state and OTA downloads live there too (BH-G1-6). Unused
// libraries go, least recently used first, but only when that makes the room.
static int have_room(const char *cache,const char *identity,uint64_t bytes) {
    char id[NAME_MAX+1],name[NAME_MAX+1];int checked=0;
    for(unsigned i=0;i<256;i++) {
        struct statvfs v;if(statvfs(cache,&v))return 0; // unknown: as before
        uint64_t avail=(uint64_t)v.f_bavail*(uint64_t)v.f_frsize,need=HFS_CACHE_RESERVE+bytes;
        if(need<bytes)break;
        if(avail>=need)return 0;
        if(private_ns)break;
        if(!checked){uint64_t total=0;oldest_generation(cache,identity,&total,id,name);if(avail+total<need)break;checked=1;}
        if(oldest_generation(cache,identity,NULL,id,name))break;
        int fd=open(cache,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(fd<0)break;
        int g=openat(fd,id,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
        if(g>=0){remove_tree(g,name);close(g);unlinkat(fd,id,AT_REMOVEDIR);note("reclaimed an unused HFS library of",id,0);}
        close(fd);if(g<0)break;
    }
    errno=ENOSPC;return -1;
}
// Per stick the library in use and the one used before it (cues set on the deck
// before the last PC export) stay; older generations and stages of a pre-mount
// killed at its deadline go (BH-O55-2). Only in edisksd's own namespace, under
// view_lock, after prepare_view found no view of this stick up.
static void prune_generations(const char *library) {
    if(private_ns)return;
    char root[PATH_MAX];snprintf(root,sizeof root,"%s",library);
    char *slash=strrchr(root,'/');if(!slash)return;*slash=0;const char *keep=slash+1;
    int fd=open(root,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(fd<0)return;
    DIR *d=fdopendir(fd);if(!d){close(fd);return;}
    char previous[NAME_MAX+1]="";struct stat prev_st={0};struct dirent *e;
    while((e=readdir(d))) {
        struct stat st;
        if(!seeded_ok(e->d_name)||!strcmp(e->d_name,keep)||fstatat(fd,e->d_name,&st,AT_SYMLINK_NOFOLLOW)||!S_ISDIR(st.st_mode))continue;
        if(!*previous||newer(&st,&prev_st)){prev_st=st;snprintf(previous,sizeof previous,"%s",e->d_name);}
    }
    rewinddir(d);
    while((e=readdir(d))) {
        int stage=!strncmp(e->d_name,".seed-",6);
        if(!stage&&(!seeded_ok(e->d_name)||!strcmp(e->d_name,keep)||!strcmp(e->d_name,previous)))continue;
        struct stat st;if(fstatat(fd,e->d_name,&st,AT_SYMLINK_NOFOLLOW)||!S_ISDIR(st.st_mode))continue;
        remove_tree(fd,e->d_name);
    }
    closedir(d);
}
static int seed_library(const char *target,const char *identity,char library[PATH_MAX],int64_t deadline) {
    char native[PATH_MAX],root[PATH_MAX],stage[PATH_MAX],cache[PATH_MAX];struct stat st;
    if(path_join(native,sizeof native,target,"Engine Library")||safe_path(native,1)||
       lib_root(cache)||make_dirs(cache))return -1;
    struct stat cache_stat;int cache_fd;
    // Older builds left this cache 0755; tighten our own real directory instead of refusing every HFS+ stick.
    if(!lstat(cache,&cache_stat)&&S_ISDIR(cache_stat.st_mode)&&cache_stat.st_uid==geteuid()&&(cache_stat.st_mode&077)&&
       (cache_fd=open(cache,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC))>=0){if(fchmod(cache_fd,0700))note("cannot tighten",cache,errno);close(cache_fd);}
    if(lstat(cache,&cache_stat)||cache_stat.st_uid!=geteuid()||(cache_stat.st_mode&077)){errno=EACCES;return -1;}
    if(path_join(root,sizeof root,cache,identity)||make_dirs(root))return -1;
    uint64_t signature=0,bytes=0;int src=-1,reuse=0;
    if(seeded_ok(seeded_hint)){char prior[PATH_MAX];reuse=!path_join(prior,sizeof prior,root,seeded_hint)&&!lstat(prior,&st)&&S_ISDIR(st.st_mode);}
    if(reuse){}
    else if(!lstat(native,&st)) {
        if(!S_ISDIR(st.st_mode)){errno=ENOTDIR;return -1;}
        int lib=open(native,O_RDONLY|O_DIRECTORY|O_CLOEXEC|O_NOFOLLOW);if(lib<0)return -1;
        int top=library_top(lib,&src);close(lib);if(top)return -1;
        if(src>=0&&copy_tree(src,-1,deadline,0,&bytes)){close(src);return -1;} // validate reused native trees too
        // A changed PC-authored database gets a separate seed rather than
        // overwriting an older sidecar containing deck-authored cues.
        const char *files[]={"Database2/m.db","Database2/m.db-wal"};
        for(unsigned i=0;src>=0&&i<2;i++) {
            char db[PATH_MAX];
            if(path_join(db,sizeof db,native,files[i])||safe_path(db,1)){if(errno==ENOENT)continue;close(src);return -1;}
            if(lstat(db,&st)){if(errno==ENOENT)continue;close(src);return -1;}
            // Content, not struct padding or coarse HFS timestamps, identifies
            // PC-side changes. A copied file with preserved mtime must refresh.
            int64_t size=(int64_t)st.st_size;
            signature=hash_bytes(signature,&i,sizeof i);
            signature=hash_bytes(signature,&size,sizeof size);
            int fd=open(db,O_RDONLY|O_NOFOLLOW|O_CLOEXEC);if(fd<0){close(src);return -1;}
            char buf[8192];ssize_t n;
            for(;;) {
                if(budget(deadline)){close(fd);close(src);return -1;}
                n=read(fd,buf,sizeof buf);
                if(n<0&&errno==EINTR)continue;
                if(n<=0)break;
                signature=hash_bytes(signature,buf,(size_t)n);
            }
            int saved=errno;close(fd);errno=saved;
            if(n<0){close(src);return -1;}
        }
    } else if(errno!=ENOENT)return -1;
    char name[sizeof seeded_as];snprintf(name,sizeof name,"library-%016llx",(unsigned long long)signature);
    if(reuse)snprintf(name,sizeof name,"%s",seeded_hint);
    if(path_join(library,PATH_MAX,root,name)){if(src>=0)close(src);return -1;}
    if(!lstat(library,&st)) {
        if(src>=0)close(src);
        if(safe_path(library,0)||!S_ISDIR(st.st_mode)){errno=ELOOP;return -1;}
        int fd=open(library,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(fd<0)return -1;
        int rc=copy_tree(fd,-1,deadline,0,NULL);close(fd);
        if(!rc){snprintf(seeded_as,sizeof seeded_as,"%s",name);touch_library(library);}
        return rc;
    }
    if(errno!=ENOENT||have_room(cache,identity,bytes)){if(src>=0)close(src);return -1;}
    if(snprintf(stage,sizeof stage,"%s/.seed-XXXXXX",root)>=(int)sizeof stage){if(src>=0)close(src);errno=ENAMETOOLONG;return -1;}
    if(!mkdtemp(stage)){if(src>=0)close(src);return -1;}
    int dst=open(stage,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC),rc=dst<0?-1:0;
    if(!rc&&src>=0) {
        int meta=-1;
        if(mkdirat(dst,"Database2",0700)||(meta=openat(dst,"Database2",O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC))<0)rc=-1;
        else rc=copy_tree(src,meta,deadline,0,NULL);
        if(meta>=0)close(meta);
    }
    if(!rc&&fsync(dst))rc=-1;
    if(dst>=0)close(dst);
    if(src>=0)close(src);
    if(!rc)rc=rename(stage,library);
    if(!rc){int parent=open(root,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(parent<0)rc=-1;else{rc=fsync(parent);close(parent);}}
    if(rc){int saved=errno;int parent=open(root,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(parent>=0){remove_tree(parent,strrchr(stage,'/')+1);close(parent);}errno=saved;}
    else snprintf(seeded_as,sizeof seeded_as,"%s",name);
    return rc;
}
static int bind_checked(const char *source,const char *target,unsigned long flags) {
    if(safe_path(source,0)||safe_path(target,0))return -1;
    return mount(source,target,NULL,flags,NULL);
}
static int save_record(struct owned_view *v) {
    char path[PATH_MAX],temp[PATH_MAX];
    if(path_join(path,sizeof path,v->anchor,"record")||path_join(temp,sizeof temp,v->anchor,"record.new"))return -1;
    if(unlink(temp)&&errno!=ENOENT)return -1;
    int fd=open(temp,O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC,0600);if(fd<0)return -1;
    size_t bytes=offsetof(struct owned_view,child)+(size_t)v->count*sizeof v->child[0],done=0;
    int rc=0;
    while(done<bytes){ssize_t n=write(fd,(char *)v+done,bytes-done);if(n<0&&errno==EINTR)continue;if(n<=0){rc=-1;break;}done+=(size_t)n;}
    if(!rc)rc=fsync(fd);
    int saved=errno;close(fd);errno=saved;
    if(!rc)rc=rename(temp,path);
    if(!rc){int parent=open(v->anchor,O_RDONLY|O_DIRECTORY|O_CLOEXEC|O_NOFOLLOW);if(parent<0)rc=-1;else{rc=fsync(parent);close(parent);}}
    if(rc){saved=errno;unlink(temp);errno=saved;}
    return rc;
}
static void cleanup_anchor(const char *anchor) {
    // The anchor is our own private mount tree, never a media-controlled path.
    if(safe_path(anchor,0))return;
    if(umount2(anchor,MNT_DETACH|UMOUNT_NOFOLLOW)&&errno!=EINVAL&&errno!=ENOENT)return;
    char path[PATH_MAX];if(!path_join(path,sizeof path,anchor,"record"))unlink(path);
    if(!path_join(path,sizeof path,anchor,"record.new"))unlink(path);
    const char *names[]={"backing","view","rescue"};
    for(unsigned i=0;i<3;i++)if(!path_join(path,sizeof path,anchor,names[i]))rmdir(path);
    rmdir(anchor);
}
static void cleanup_view(struct owned_view *v) { cleanup_anchor(v->anchor); }
static int bind_view_child(struct owned_view *v,const char *name,const char *source,int isdir,int create) {
    if(v->count==MAX_VIEW_ENTRIES){errno=E2BIG;return -1;}
    char root[PATH_MAX],dest[PATH_MAX];
    if(path_join(root,sizeof root,v->anchor,"view")||path_join(dest,sizeof dest,root,name))return -1;
    if(create) {
        if(isdir){if(mkdir(dest,0700))return -1;}
        else {int fd=open(dest,O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC,0600);if(fd<0)return -1;close(fd);}
    }
    // A bind of a shared mount (systemd makes /data MS_SHARED) is that mount's peer:
    // bind_native's binds inside it would propagate into /data. Private first.
    if(bind_checked(source,dest,MS_BIND|MS_REC)||mount(NULL,dest,NULL,MS_PRIVATE|MS_REC,NULL))return -1;
    snprintf(v->child[v->count++].name,NAME_MAX+1,"%s",name);return 0;
}
static int add_child(struct owned_view *v,const char *name,const char *source,int isdir) {
    return bind_view_child(v,name,source,isdir,1);
}
static int update_public_ids(struct owned_view *v) {
    v->public_id=mount_id(v->target);if(!v->public_id)return -1;
    for(unsigned i=0;i<v->count;i++) {
        char p[PATH_MAX];if(path_join(p,sizeof p,v->target,v->child[i].name))return -1;
        v->child[i].mount_id=mount_id(p);if(!v->child[i].mount_id)return -1;
    }
    return save_record(v);
}
static int empty_dir(int parent,const char *name) {
    int fd=openat(parent,name,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(fd<0)return 0;
    DIR *d=fdopendir(fd);if(!d){close(fd);return 0;}
    struct dirent *e;int empty=1;
    while(empty&&(e=readdir(d)))empty=!strcmp(e->d_name,".")||!strcmp(e->d_name,"..");
    closedir(d);return empty;
}
// An empty directory or file in the cached library for a stick entry to be bound
// on. What an older build copied there (the stick's own audio) is hidden by that
// bind for good, so it goes.
static int stand_in(int lib,const char *name,int isdir) {
    struct stat st;
    if(!fstatat(lib,name,&st,AT_SYMLINK_NOFOLLOW)) {
        if(isdir&&S_ISDIR(st.st_mode)&&empty_dir(lib,name))return 0;
        if(!isdir&&S_ISREG(st.st_mode)&&!st.st_size)return 0;
        if(S_ISDIR(st.st_mode))remove_tree(lib,name);else unlinkat(lib,name,0);
    } else if(errno!=ENOENT)return -1;
    if(isdir)return mkdirat(lib,name,0700);
    int fd=openat(lib,name,O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC,0600);if(fd<0)return -1;
    return close(fd);
}
#define NESTED "Engine Library/"
// BH2-A1-4: Engine 5.3.0 and rb-bridge write covers as Engine Library/Artwork/<hash>.jpg
// (pkg/enginedb/artwork.go). A read-only bind of the stick's Artwork refused them. The
// stick's covers are mirrored into the cached library's own Artwork instead, which the
// library bind shows writable: linked from another generation of this stick where it has
// them, copied otherwise. Copies are made only in the pre-mount, inside its budget and
// above the /data reserve; one interrupted is finished on a later insert. Returns 0 when
// the mirror is complete, 1 when it is not (the stick's folder is then bound read-only
// over it, as before). A cover is the same when its size is: names are content hashes.
static int same_size(int dir,const char *name,off_t size) {
    struct stat st;return !fstatat(dir,name,&st,AT_SYMLINK_NOFOLLOW)&&S_ISREG(st.st_mode)&&st.st_size==size;
}
static int mirror_copy(int in,int out,int64_t deadline) {
    char buf[65536];ssize_t n;
    while((n=read(in,buf,sizeof buf))!=0) {
        if(n<0){if(errno==EINTR)continue;return -1;}
        if(budget(deadline))return -1;
        for(ssize_t pos=0;pos<n;){ssize_t w=write(out,buf+pos,(size_t)(n-pos));if(w<0&&errno==EINTR)continue;if(w<=0){if(!w)errno=EIO;return -1;}pos+=w;}
    }
    return 0;
}
static int mirror_artwork(int lib,const char *library,const char *source,int64_t deadline) {
    struct stat st;
    if(fstatat(lib,"Artwork",&st,AT_SYMLINK_NOFOLLOW)){if(errno!=ENOENT||mkdirat(lib,"Artwork",0700))return 1;}
    else if(!S_ISDIR(st.st_mode))return 1;
    int dst=openat(lib,"Artwork",O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(dst<0)return 1;
    int src=open(source,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(src<0){close(dst);return 1;}
    // Other generations of this stick: cache/<identity>/library-*/Artwork.
    int sib[4],nsib=0;char root[PATH_MAX];snprintf(root,sizeof root,"%s",library);
    char *slash=strrchr(root,'/');
    if(slash){*slash=0;DIR *g=opendir(root);struct dirent *l;
        while(g&&nsib<4&&(l=readdir(g)))if(seeded_ok(l->d_name)&&strcmp(l->d_name,slash+1)) {
            char a[PATH_MAX];if(snprintf(a,sizeof a,"%s/%s/Artwork",root,l->d_name)>=(int)sizeof a)continue;
            int fd=open(a,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(fd>=0)sib[nsib++]=fd;
        }
        if(g)closedir(g);}
    int dup=fcntl(src,F_DUPFD_CLOEXEC,0);DIR *d=dup<0?NULL:fdopendir(dup);if(!d&&dup>=0)close(dup);
    int complete=d!=NULL,copied=0;struct dirent *e;
    while(complete&&(errno=0,e=readdir(d))) {
        if(!strcmp(e->d_name,".")||!strcmp(e->d_name,".."))continue;
        if(fstatat(src,e->d_name,&st,AT_SYMLINK_NOFOLLOW)||!S_ISREG(st.st_mode)){complete=0;break;} // only flat covers are mirrored
        if(same_size(dst,e->d_name,st.st_size))continue;
        if(budget(deadline)){complete=0;break;}
        unlinkat(dst,e->d_name,0);
        int linked=0;
        for(int i=0;!linked&&i<nsib;i++)linked=same_size(sib[i],e->d_name,st.st_size)&&!linkat(sib[i],e->d_name,dst,e->d_name,0);
        if(linked)continue;
        struct statvfs v;
        if(!private_ns||fstatvfs(dst,&v)||(uint64_t)v.f_bavail*(uint64_t)v.f_frsize<HFS_CACHE_RESERVE+(uint64_t)st.st_size){complete=0;break;}
        int in=openat(src,e->d_name,O_RDONLY|O_NOFOLLOW|O_CLOEXEC);
        unlinkat(dst,".mirror.tmp",0);
        int out=in<0?-1:openat(dst,".mirror.tmp",O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC,0600);
        int rc=out<0?-1:mirror_copy(in,out,deadline);
        if(out>=0&&close(out))rc=-1;
        if(in>=0)close(in);
        if(rc||renameat(dst,".mirror.tmp",dst,e->d_name)){unlinkat(dst,".mirror.tmp",0);complete=0;break;}
        copied=1;
    }
    if(d){if(errno&&complete)complete=0;closedir(d);}
    if(copied)syncfs(dst);
    for(int i=0;i<nsib;i++)close(sib[i]);
    close(src);close(dst);return complete?0:1;
}
// BH-O55-1: every entry of the stick's own Engine Library except Database2 (the
// copied metadata), Engine DJ's Music folder above all, shown read-only from the
// stick inside the cached library's bind. The record lists these binds before
// "Engine Library", so an eject removes them first.
static int bind_native(struct owned_view *v,const char *backing,const char *library,int64_t deadline) {
    char native[PATH_MAX];struct stat st;unsigned top=v->count-1;
    if(path_join(native,sizeof native,backing,"Engine Library"))return -1;
    if(lstat(native,&st))return errno==ENOENT?0:-1;
    if(!S_ISDIR(st.st_mode)){errno=ENOTDIR;return -1;}
    int lib=open(library,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(lib<0)return -1;
    DIR *d=opendir(native);if(!d){int saved=errno;close(lib);errno=saved;return -1;}
    int rc=0;struct dirent *e;
    for(;;) {
        errno=0;e=readdir(d);if(!e){if(errno)rc=-1;break;}
        if(!strcmp(e->d_name,".")||!strcmp(e->d_name,".."))continue;
        char source[PATH_MAX],dest[PATH_MAX],name[NAME_MAX+1];
        if(budget(deadline)||path_join(source,sizeof source,native,e->d_name)||lstat(source,&st)){rc=-1;break;}
        if(S_ISLNK(st.st_mode)||(!S_ISDIR(st.st_mode)&&!S_ISREG(st.st_mode))){errno=ELOOP;rc=-1;break;}
        if(S_ISDIR(st.st_mode)&&!strcmp(e->d_name,"Database2"))continue;
        int mirror=0; // a partial Artwork mirror stays under the bind, for a later insert
        if(S_ISDIR(st.st_mode)&&!strcmp(e->d_name,"Artwork")) {
            if(!mirror_artwork(lib,library,source,deadline))continue; // the cached library's own, writable
            struct stat a;mirror=!fstatat(lib,"Artwork",&a,AT_SYMLINK_NOFOLLOW)&&S_ISDIR(a.st_mode);
        }
        // The view's record keeps "Engine Library/<name>" in NAME_MAX bytes: an entry
        // with a longer name is left out of the view, not the whole stick (BH2-A1-6).
        if(snprintf(name,sizeof name,NESTED "%s",e->d_name)>=(int)sizeof name){note("name too long for the HFS view, left out:",e->d_name,ENAMETOOLONG);continue;}
        if(v->count==MAX_VIEW_ENTRIES){errno=E2BIG;rc=-1;break;}
        if(snprintf(dest,sizeof dest,"%s/view/%s",v->anchor,name)>=(int)sizeof dest){errno=ENAMETOOLONG;rc=-1;break;}
        if((!mirror&&stand_in(lib,e->d_name,S_ISDIR(st.st_mode)))||bind_checked(source,dest,MS_BIND|MS_REC)||mount(NULL,dest,NULL,MS_PRIVATE|MS_REC,NULL)){rc=-1;break;}
        snprintf(v->child[v->count++].name,NAME_MAX+1,"%s",name);
        if(mount(NULL,dest,NULL,MS_BIND|MS_REMOUNT|MS_RDONLY|MS_NOSUID|MS_NODEV|MS_NOEXEC,NULL)){rc=-1;break;}
    }
    int saved=errno;closedir(d);close(lib);errno=saved;
    if(!rc&&v->count-1>top){struct view_child c=v->child[top];memmove(&v->child[top],&v->child[top+1],(v->count-1-top)*sizeof c);v->child[v->count-1]=c;}
    return rc;
}
static int prepare_view(const char *target,const char *identity,int64_t deadline) {
    if(secure_runtime()||safe_path(target,0))return -1;
    struct owned_view *active=find_view(NULL,identity);
    if(active){free(active);errno=EBUSY;return -1;}
    char library[PATH_MAX];if(seed_library(target,identity,library,deadline))return -1;
    struct owned_view *v=calloc(1,sizeof *v);if(!v)return -1;
    v->magic=VIEW_MAGIC;snprintf(v->target,sizeof v->target,"%s",target);snprintf(v->identity,sizeof v->identity,"%s",identity);
    v->base_id=mount_id(target);
    if(!v->base_id||snprintf(v->anchor,sizeof v->anchor,"%s/view-XXXXXX",RUN_ROOT)>=(int)sizeof v->anchor||!mkdtemp(v->anchor)){free(v);return -1;}
    int rc=-1;char backing[PATH_MAX],view[PATH_MAX],rescue[PATH_MAX];
    if(bind_checked(v->anchor,v->anchor,MS_BIND)||mount(NULL,v->anchor,NULL,MS_PRIVATE|MS_REC,NULL))goto done;
    if(path_join(backing,sizeof backing,v->anchor,"backing")||path_join(view,sizeof view,v->anchor,"view")||path_join(rescue,sizeof rescue,v->anchor,"rescue")||
       mkdir(backing,0700)||mkdir(view,0700)||mkdir(rescue,0700))goto done;
    if(bind_checked(target,backing,MS_BIND|MS_REC)||mount(NULL,backing,NULL,MS_PRIVATE|MS_REC,NULL))goto done;
    char native[PATH_MAX];struct stat el_st;int has_native_el=0;
    if(path_join(native,sizeof native,backing,"Engine Library"))goto done;
    if(!lstat(native,&el_st)) {
        if(S_ISLNK(el_st.st_mode)){errno=ELOOP;goto done;}
        if(!S_ISDIR(el_st.st_mode)){errno=ENOTDIR;goto done;}
        has_native_el=1;
    } else if(errno!=ENOENT)goto done;
    if(has_native_el) {
        // Bind the volume root once instead of per-root-entry binds, then overlay Engine Library.
        if(bind_checked(backing,view,MS_BIND)||mount(NULL,view,NULL,MS_PRIVATE|MS_REC,NULL)||
           bind_view_child(v,"Engine Library",library,1,0)||bind_native(v,backing,library,deadline)||
           budget(deadline)||bind_checked(view,target,MS_BIND|MS_REC))goto done;
    } else {
        if(mount("enginex-hfs-view",view,"tmpfs",MS_NODEV|MS_NOSUID|MS_NOEXEC,"mode=0755,size=16m"))goto done;
        DIR *d=opendir(backing);if(!d)goto done;
        struct dirent *e;int scan_error=0;
        for(;;) {
            errno=0;e=readdir(d);if(!e){if(errno)scan_error=1;break;}
            if(!strcmp(e->d_name,".")||!strcmp(e->d_name,"..")||!strcmp(e->d_name,"Engine Library"))continue;
            char source[PATH_MAX];struct stat st;
            if(budget(deadline)||path_join(source,sizeof source,backing,e->d_name)||lstat(source,&st)||S_ISLNK(st.st_mode)||
               (!S_ISDIR(st.st_mode)&&!S_ISREG(st.st_mode))||add_child(v,e->d_name,source,S_ISDIR(st.st_mode))){if(!errno)errno=ELOOP;scan_error=1;break;}
        }
        closedir(d);if(scan_error||add_child(v,"Engine Library",library,1)||bind_native(v,backing,library,deadline))goto done;
        // Make the synthetic root read-only, while the separate library bind stays rw.
        if(mount(NULL,view,"tmpfs",MS_REMOUNT|MS_RDONLY|MS_NODEV|MS_NOSUID|MS_NOEXEC,"mode=0755,size=16m")||
           budget(deadline)||bind_checked(view,target,MS_BIND|MS_REC))goto done;
    }
    if(update_public_ids(v)) {
        int saved=errno;umount2(target,MNT_DETACH|UMOUNT_NOFOLLOW);errno=saved;goto done;
    }
    note("writable HFS library ready on",target,0);rc=0;prune_generations(library);
 done: {int saved=errno;if(rc)cleanup_view(v);free(v);errno=saved;}return rc;
}
// The record in one RUN_ROOT/view-* directory, when it is ours, intact and
// describes a view that is mounted in this namespace now.
static struct owned_view *live_view(const char *name) {
    char anchor[PATH_MAX],path[PATH_MAX];
    if(path_join(anchor,sizeof anchor,RUN_ROOT,name)||safe_path(anchor,0)||path_join(path,sizeof path,anchor,"record")||safe_path(path,0))return NULL;
    int fd=open(path,O_RDONLY|O_NOFOLLOW|O_CLOEXEC);if(fd<0)return NULL;struct stat st;
    if(fstat(fd,&st)||!S_ISREG(st.st_mode)||st.st_uid!=geteuid()||(st.st_mode&077)){close(fd);return NULL;}
    struct owned_view *v=calloc(1,sizeof *v);if(!v){close(fd);return NULL;}
    ssize_t bytes=0;
    if(st.st_size>=(off_t)offsetof(struct owned_view,child)&&st.st_size<=(off_t)sizeof *v) {
        while(bytes<st.st_size){ssize_t n=read(fd,(char *)v+bytes,(size_t)(st.st_size-bytes));if(n<0&&errno==EINTR)continue;if(n<=0){bytes=-1;break;}bytes+=n;}
    }
    close(fd);
    if(bytes>=(ssize_t)offsetof(struct owned_view,child)&&v->magic==VIEW_MAGIC&&v->count<=MAX_VIEW_ENTRIES&&
       bytes==(ssize_t)(offsetof(struct owned_view,child)+(size_t)v->count*sizeof v->child[0])&&
       memchr(v->target,0,sizeof v->target)&&memchr(v->anchor,0,sizeof v->anchor)&&memchr(v->identity,0,sizeof v->identity)&&
       !strcmp(v->anchor,anchor)&&mount_id(v->target)==v->public_id)return v;
    free(v);return NULL;
}
static struct owned_view *find_view(const char *target,const char *identity) {
    if(secure_runtime()||(target&&safe_path(target,0)))return NULL;
    DIR *d=opendir(RUN_ROOT);if(!d)return NULL;struct dirent *e;struct owned_view *found=NULL;
    while(!found&&(e=readdir(d))) {
        if(strncmp(e->d_name,"view-",5))continue;
        struct owned_view *v=live_view(e->d_name);
        if(v&&(!target||!strcmp(v->target,target))&&(!identity||!strcmp(v->identity,identity)))found=v;
        else free(v);
    }
    closedir(d);return found;
}
// A pre-mount child killed at its deadline cannot remove its view: the mounts
// end with its namespace, the anchor directory and record in RUN_ROOT stay, and
// a record with a recycled mount id could later pass for a live view. Under
// view_lock every view of this namespace has a live record, so whatever has none
// is such a leftover. Only for edisksd's own namespace, never the child's, where
// the views of the other sticks carry other mount ids.
static void drop_orphans(void) {
    if(secure_runtime())return;
    DIR *d=opendir(RUN_ROOT);if(!d)return;struct dirent *e;
    while((e=readdir(d))) {
        if(strncmp(e->d_name,"view-",5))continue;
        struct owned_view *v=live_view(e->d_name);char anchor[PATH_MAX];
        if(v)free(v);
        else if(!path_join(anchor,sizeof anchor,RUN_ROOT,e->d_name))cleanup_anchor(anchor);
    }
    closedir(d);
}

static struct owned_view *load_view(const char *target) { return find_view(target,NULL); }

// A rekordbox stick is one rb-bridge has something to convert from (pkg/bridge
// exportCandidates): exportLibrary.db, exportOneLibrary.db or export.pdb under
// PIONEER/rekordbox, plain or hidden, names matched as the stick's filesystem and
// rb-bridge's resolver do, without regard to case. BH2-A1-7: any PIONEER folder
// counted, so an Engine stick that had only a player's settings in it, or the
// export.pdb EngineX itself writes for CDJ link (pkg/prolink GeneratedExport,
// which rb-bridge leaves alone), was taken for a rekordbox stick and never healed.
static int generated_export(int rekordbox,const struct stat *pdb) {
    char want[64],got[64];
    snprintf(want,sizeof want,"%lld:%lld",(long long)pdb->st_size,(long long)pdb->st_mtim.tv_sec*1000000000LL+pdb->st_mtim.tv_nsec);
    int fd=openat(rekordbox,".enginex-generated",O_RDONLY|O_NOFOLLOW|O_CLOEXEC);if(fd<0)return 0;
    ssize_t n=read(fd,got,sizeof got-1);close(fd);
    return n>0&&(size_t)n==strlen(want)&&!memcmp(got,want,(size_t)n);
}
static DIR *open_dir_at(int dir,const char *name) {
    int fd=openat(dir,name,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(fd<0)return NULL;
    DIR *d=fdopendir(fd);if(!d)close(fd);
    return d;
}
static int export_in(DIR *pioneer,int hidden) {
    int found=0;struct dirent *e,*f;
    while(!found&&(e=readdir(pioneer))) {
        if(strcasecmp(e->d_name,"rekordbox"))continue;
        DIR *rb=open_dir_at(dirfd(pioneer),e->d_name);if(!rb)continue;
        while(!found&&(f=readdir(rb))) {
            int pdb=!strcasecmp(f->d_name,"export.pdb");struct stat st;
            if(!pdb&&strcasecmp(f->d_name,"exportLibrary.db")&&strcasecmp(f->d_name,"exportOneLibrary.db"))continue;
            if(fstatat(dirfd(rb),f->d_name,&st,AT_SYMLINK_NOFOLLOW)||!S_ISREG(st.st_mode))continue;
            found=!pdb||hidden||!generated_export(dirfd(rb),&st); // EngineX writes its own only into PIONEER
        }
        closedir(rb);
    }
    return found;
}
static int has_export(const char *target) {
    if(safe_path(target,0))return 0;
    DIR *d=opendir(target);if(!d)return 0;int found=0;struct dirent *e;
    while(!found&&(e=readdir(d)))if(!strcasecmp(e->d_name,"PIONEER")||!strcasecmp(e->d_name,".PIONEER")) {
        DIR *pioneer=open_dir_at(dirfd(d),e->d_name);if(!pioneer)continue;
        found=export_in(pioneer,e->d_name[0]=='.');closedir(pioneer);
    }
    closedir(d);return found;
}
static int run(char *const argv[],int64_t deadline) {
    if(budget(deadline))return -1;
    pid_t pid=fork();if(pid<0)return -1;
    if(!pid){setpgid(0,0);unsetenv("LD_PRELOAD");setenv("PATH","/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",1);execv(argv[0],argv);_exit(127);}
    setpgid(pid,pid);
    for(;;){int status;pid_t r=waitpid(pid,&status,WNOHANG);
        if(r==pid){
            if(WIFEXITED(status)&&!WEXITSTATUS(status))return 0;
            if(WIFEXITED(status)&&WEXITSTATUS(status)==124){errno=ETIMEDOUT;return -1;}
            errno=EIO;return -1;
        }
        if(r<0&&errno!=EINTR)return -1;
        if(budget(deadline)){kill(-pid,SIGKILL);while(waitpid(pid,&status,0)<0&&errno==EINTR){}errno=ETIMEDOUT;return -1;}usleep(20000);
    }
}
#ifndef SYSTEMCTL_PATH
#define SYSTEMCTL_PATH "/bin/systemctl"
#endif
// The unit of a volume's background first import (rb-bridge --import-path, #301): FNV-1a 64 of
// the mount path, as internal/cli importUnit names it.
static void import_unit(const char *target,char out[64]) {
    snprintf(out,64,"enginex-rb-import-%016llx.service",(unsigned long long)hash_bytes(UINT64_C(14695981039346656037),target,strlen(target)));
}
// The job writes its stage on the stick: stop it before the unmount, so an ejected stick is never
// pulled half-written. Stopped, it goes on from its last pass at the next mount. Not running is fine.
static void stop_import(const char *target) {
    char unit[64];import_unit(target,unit);
    char *const a[]={(char *)SYSTEMCTL_PATH,"stop","--quiet",unit,NULL};
    if(run(a,now_ms()+8000)&&errno==ETIMEDOUT)note("background import did not stop on",target,errno);
}
#ifndef EJECT_MARK_DIR
#define EJECT_MARK_DIR "/run"
#endif
// An Eject (edisksd's Unmount, which is never lazy; its removal path is). rb-bridge --remount
// publishes a finished background import by a synthetic udev remove and add of the partition
// (#301): it must not mount again a stick the DJ ejected. Same name hash as import_unit.
// EJECTING marks every Eject attempt and stays until the next mount: the Pro DJ Link export
// (pkg/prolink EjectingMark) stops opening the stick, so a peer's reads cannot keep it busy.
#define EJECTED "rb-ejected"
#define EJECTING "ejecting"
static int eject_mark_path(const char *kind,const char *target,char p[PATH_MAX]) {
    return target&&volume(target)&&snprintf(p,PATH_MAX,"%s/enginex-%s-%016llx",EJECT_MARK_DIR,kind,
                                            (unsigned long long)hash_bytes(UINT64_C(14695981039346656037),target,strlen(target)))<(int)PATH_MAX?0:-1;
}
static void mark_eject(const char *kind,const char *target) {
    char p[PATH_MAX];if(eject_mark_path(kind,target,p))return;
    int fd=open(p,O_WRONLY|O_CREAT|O_TRUNC|O_NOFOLLOW|O_CLOEXEC,0600);if(fd>=0)close(fd);
}
static void clear_eject(const char *kind,const char *target) {
    char p[PATH_MAX];if(!eject_mark_path(kind,target,p))unlink(p);
}
#ifndef FSCK_HFSPLUS_SYSTEM
#define FSCK_HFSPLUS_SYSTEM "/usr/sbin/fsck.hfsplus"
#endif
#ifndef FSCK_HFSPLUS_BUNDLE
#define FSCK_HFSPLUS_BUNDLE "/data/enginex/bundle/bin/fsck.hfsplus"
#endif
#ifndef FSCK_HFSPLUS_PATH
static const char *fsck_hfsplus_path(void) {
    if(!access(FSCK_HFSPLUS_SYSTEM,X_OK))return FSCK_HFSPLUS_SYSTEM;
    if(!access(FSCK_HFSPLUS_BUNDLE,X_OK))return FSCK_HFSPLUS_BUNDLE;
    return NULL;
}
#define FSCK_HFSPLUS_PATH fsck_hfsplus_path
#endif
#ifndef LKLFUSE_SYSTEM
#define LKLFUSE_SYSTEM "/usr/bin/lklfuse"
#endif
#ifndef LKLFUSE_BUNDLE
#define LKLFUSE_BUNDLE "/data/enginex/bundle/bin/lklfuse"
#endif
#ifndef LKLFUSE_PATH
static const char *lklfuse_path(void) {
    if(!access(LKLFUSE_SYSTEM,X_OK))return LKLFUSE_SYSTEM;
    if(!access(LKLFUSE_BUNDLE,X_OK))return LKLFUSE_BUNDLE;
    return NULL;
}
#define LKLFUSE_PATH lklfuse_path
#endif
#ifndef LKLFUSE_NICE
#define LKLFUSE_NICE 2
#endif
#ifndef LKLFUSE_MB
#define LKLFUSE_MB 32
#endif
#ifndef LKLFUSE_AS_LIMIT_BYTES
#define LKLFUSE_AS_LIMIT_BYTES (128ULL * 1024ULL * 1024ULL)
#endif
#ifndef LKLFUSE_STACK_LIMIT_BYTES
#define LKLFUSE_STACK_LIMIT_BYTES (1024ULL * 1024ULL)
#endif
#ifndef LKLFUSE_MOUNT_TIMEOUT_MS
#define LKLFUSE_MOUNT_TIMEOUT_MS INT64_C(10000)
#endif
#ifndef LKLFUSE_WAIT_EXIT_MS
#define LKLFUSE_WAIT_EXIT_MS INT64_C(15000)
#endif
#ifndef GEN1_ROOTFS_MARKER
#define GEN1_ROOTFS_MARKER "/etc/enginex/release-tag"
#endif
#ifndef AZ04_UFSD_DT_PATH
#define AZ04_UFSD_DT_PATH "/sys/firmware/devicetree/base/chosen/inmusic,hfsplus-rw"
#endif
#ifndef PROC_FILESYSTEMS_PATH
#define PROC_FILESYSTEMS_PATH "/proc/filesystems"
#endif
#ifndef PROC_MOUNTINFO_PATH
#define PROC_MOUNTINFO_PATH "/proc/self/mountinfo"
#endif
#ifndef SYS_DEV_BLOCK_DIR
#define SYS_DEV_BLOCK_DIR "/sys/dev/block"
#endif
#ifndef HOOK_UNAME
#define HOOK_UNAME uname
#endif
#ifndef FINIT_MODULE
#define FINIT_MODULE(fd) ((int)syscall(__NR_finit_module,(fd),"",0))
#endif
#ifndef HFS_PLATFORM_MODE
#if defined(__aarch64__)
#define HFS_PLATFORM_MODE() 2 // AZ04: stock hfsplus rw
#elif defined(__arm__)
#define HFS_PLATFORM_MODE() 1 // ARMv7: Gen-1 hfsplusx; Gen-2 armhf stays read-only
#else
#define HFS_PLATFORM_MODE() 0
#endif
#endif

#ifndef REAL_MNT_RESET_CONTEXT
#define REAL_MNT_RESET_CONTEXT() ((int (*)(struct libmnt_context *))dlsym(RTLD_NEXT,"mnt_reset_context"))
#endif
#ifndef REAL_MNT_CONTEXT_SET_SOURCE
#define REAL_MNT_CONTEXT_SET_SOURCE() ((int (*)(struct libmnt_context *,const char *))dlsym(RTLD_NEXT,"mnt_context_set_source"))
#endif
#ifndef REAL_MNT_CONTEXT_SET_TARGET
#define REAL_MNT_CONTEXT_SET_TARGET() ((int (*)(struct libmnt_context *,const char *))dlsym(RTLD_NEXT,"mnt_context_set_target"))
#endif
#ifndef REAL_MNT_CONTEXT_DISABLE_HELPERS
#define REAL_MNT_CONTEXT_DISABLE_HELPERS() ((int (*)(struct libmnt_context *,int))dlsym(RTLD_NEXT,"mnt_context_disable_helpers"))
#endif
#ifndef REAL_MNT_CONTEXT_SET_FSTYPE_PATTERN
#define REAL_MNT_CONTEXT_SET_FSTYPE_PATTERN() ((int (*)(struct libmnt_context *,const char *))dlsym(RTLD_NEXT,"mnt_context_set_fstype_pattern"))
#endif
#ifndef REAL_MNT_CONTEXT_SET_FSTYPE
#define REAL_MNT_CONTEXT_SET_FSTYPE() ((int (*)(struct libmnt_context *,const char *))dlsym(RTLD_NEXT,"mnt_context_set_fstype"))
#endif
#ifndef REAL_MNT_CONTEXT_SET_OPTIONS
#define REAL_MNT_CONTEXT_SET_OPTIONS() ((int (*)(struct libmnt_context *,const char *))dlsym(RTLD_NEXT,"mnt_context_set_options"))
#endif
#ifndef REAL_MNT_CONTEXT_ENABLE_LAZY
#define REAL_MNT_CONTEXT_ENABLE_LAZY() ((int (*)(struct libmnt_context *,int))dlsym(RTLD_NEXT,"mnt_context_enable_lazy"))
#endif
#ifndef REAL_MNT_CONTEXT_GET_MFLAGS
#define REAL_MNT_CONTEXT_GET_MFLAGS() ((int (*)(struct libmnt_context *,unsigned long *))dlsym(RTLD_NEXT,"mnt_context_get_mflags"))
#endif
#ifndef REAL_MNT_CONTEXT_SET_MFLAGS
#define REAL_MNT_CONTEXT_SET_MFLAGS() ((int (*)(struct libmnt_context *,unsigned long))dlsym(RTLD_NEXT,"mnt_context_set_mflags"))
#endif
#ifndef REAL_MNT_FREE_CONTEXT
#define REAL_MNT_FREE_CONTEXT() ((void (*)(struct libmnt_context *))dlsym(RTLD_NEXT,"mnt_free_context"))
#endif
#ifndef REAL_MOUNT
#define REAL_MOUNT() ((int (*)(const char *,const char *,const char *,unsigned long,const void *))dlsym(RTLD_NEXT,"mount"))
#endif
#ifndef REAL_FSOPEN
#define REAL_FSOPEN() ((int (*)(const char *,unsigned int))dlsym(RTLD_NEXT,"fsopen"))
#endif
#ifndef REAL_FSCONFIG
#define REAL_FSCONFIG() ((int (*)(int,unsigned int,const char *,const void *,int))dlsym(RTLD_NEXT,"fsconfig"))
#endif

enum hfs_rw_policy { HFS_RW_NONE = 0, HFS_RW_HFSPLUSX = 1, HFS_RW_STOCK = 2, HFS_RW_LKLFUSE = 3 };
static __thread int hfs_rw_approved = HFS_RW_NONE;
static int hfs_fs_is_rw_approved(const char *fstype) {
    if(!fstype)return 0;
    if(!strcmp(fstype,"hfsplusx"))return hfs_rw_approved==HFS_RW_HFSPLUSX;
    if(!strcmp(fstype,"hfsplus"))return hfs_rw_approved==HFS_RW_STOCK;
    return 1;
}
static pthread_mutex_t fs_fd_lock=PTHREAD_MUTEX_INITIALIZER;
static int forced_ro_fds[64];
static int fs_fd_mark(int fd,int force_ro) {
    if(fd<0)return fd;
    pthread_mutex_lock(&fs_fd_lock);
    int empty=-1;
    for(unsigned i=0;i<64;i++) {
        if(forced_ro_fds[i]==fd+1){if(!force_ro)forced_ro_fds[i]=0;pthread_mutex_unlock(&fs_fd_lock);return fd;}
        if(empty<0&&!forced_ro_fds[i])empty=(int)i;
    }
    if(force_ro) {
        if(empty>=0)forced_ro_fds[empty]=fd+1;
        else{pthread_mutex_unlock(&fs_fd_lock);close(fd);errno=EMFILE;return -1;}
    }
    pthread_mutex_unlock(&fs_fd_lock);
    return fd;
}
static int fs_fd_take(int fd,int remove) {
    pthread_mutex_lock(&fs_fd_lock);
    int found=0;
    for(unsigned i=0;i<64;i++)if(forced_ro_fds[i]==fd+1){found=1;if(remove)forced_ro_fds[i]=0;break;}
    pthread_mutex_unlock(&fs_fd_lock);
    return found;
}
int mount(const char *source,const char *target,const char *filesystemtype,unsigned long mountflags,const void *data) {
    int (*real)(const char *,const char *,const char *,unsigned long,const void *)=REAL_MOUNT();
    if(!real){errno=ENOSYS;return -1;}
    if(filesystemtype&&(!strcmp(filesystemtype,"hfsplus")||!strcmp(filesystemtype,"hfsplusx"))&&!hfs_fs_is_rw_approved(filesystemtype)) {
        if(!(mountflags&MS_RDONLY))note("forcing MS_RDONLY on unapproved HFS+ mount",target?target:filesystemtype,0);
        mountflags|=MS_RDONLY;
    }
    return real(source,target,filesystemtype,mountflags,data);
}
int fsopen(const char *fsname,unsigned int flags) {
    int (*real)(const char *,unsigned int)=REAL_FSOPEN();
    if(!real){errno=ENOSYS;return -1;}
    int fd=real(fsname,flags);
    if(fd>=0) {
        int is_hfs=fsname&&(!strcmp(fsname,"hfsplus")||!strcmp(fsname,"hfsplusx"));
        int force_ro=is_hfs&&!hfs_fs_is_rw_approved(fsname);
        if(force_ro)note("forcing ro via fsopen/fsconfig on unapproved HFS+ fs",fsname,0);
        fd=fs_fd_mark(fd,force_ro);
    }
    return fd;
}
int fsconfig(int fd,unsigned int cmd,const char *key,const void *value,int aux) {
    int (*real)(int,unsigned int,const char *,const void *,int)=REAL_FSCONFIG();
    if(!real){errno=ENOSYS;return -1;}
    int remove=(cmd==FSCONFIG_CMD_CREATE);
#ifdef FSCONFIG_CMD_CREATE_EXCL
    if(cmd==FSCONFIG_CMD_CREATE_EXCL)remove=1;
#endif
    int forced=fs_fd_take(fd,remove);
    if(forced) {
        if(cmd==FSCONFIG_SET_FLAG&&key&&!strcmp(key,"rw"))return real(fd,FSCONFIG_SET_FLAG,"ro",NULL,0);
        if(remove) {
            int rc=real(fd,FSCONFIG_SET_FLAG,"ro",NULL,0);
            if(rc)return rc;
        }
    }
    return real(fd,cmd,key,value,aux);
}

struct hfs_ctx_state {
    struct libmnt_context *ctx;
    char fstype[64];
    char opts[512];
    int has_fstype,set_by_fstype,has_opts,lazy,has_lazy,disable_helpers,has_disable_helpers;
};
static pthread_mutex_t ctx_lock=PTHREAD_MUTEX_INITIALIZER;
static struct hfs_ctx_state ctx_states[64];
static unsigned ctx_next_evict;

static struct hfs_ctx_state *ctx_slot(struct libmnt_context *ctx,int create) {
    if(!ctx)return NULL;
    for(unsigned i=0;i<64;i++)if(ctx_states[i].ctx==ctx)return &ctx_states[i];
    if(!create)return NULL;
    for(unsigned i=0;i<64;i++)if(!ctx_states[i].ctx){memset(&ctx_states[i],0,sizeof ctx_states[i]);ctx_states[i].ctx=ctx;return &ctx_states[i];}
    struct hfs_ctx_state *s=&ctx_states[ctx_next_evict++%64];
    memset(s,0,sizeof *s);s->ctx=ctx;return s;
}
// edisksd asks for a lazy unmount when the stick goes away (udev remove), never for an Eject.
static int ctx_wants_lazy(struct libmnt_context *ctx) {
    pthread_mutex_lock(&ctx_lock);
    struct hfs_ctx_state *s=ctx_slot(ctx,0);
    int lazy=s&&s->has_lazy&&s->lazy;
    pthread_mutex_unlock(&ctx_lock);
    return lazy;
}
int mnt_reset_context(struct libmnt_context *ctx) {
    int (*real)(struct libmnt_context *)=REAL_MNT_RESET_CONTEXT();
    if(!real)return -ENOSYS;
    pthread_mutex_lock(&ctx_lock);
    struct hfs_ctx_state *s=ctx_slot(ctx,0);
    if(s){memset(s,0,sizeof *s);s->ctx=ctx;}
    pthread_mutex_unlock(&ctx_lock);
    return real(ctx);
}
int mnt_context_disable_helpers(struct libmnt_context *ctx,int disable) {
    int (*real)(struct libmnt_context *,int)=REAL_MNT_CONTEXT_DISABLE_HELPERS();
    if(!real)return -ENOSYS;
    pthread_mutex_lock(&ctx_lock);
    struct hfs_ctx_state *s=ctx_slot(ctx,1);
    if(s){s->has_disable_helpers=1;s->disable_helpers=disable;}
    pthread_mutex_unlock(&ctx_lock);
    return real(ctx,disable);
}
int mnt_context_set_fstype_pattern(struct libmnt_context *ctx,const char *pattern) {
    int (*real)(struct libmnt_context *,const char *)=REAL_MNT_CONTEXT_SET_FSTYPE_PATTERN();
    if(!real)return -ENOSYS;
    pthread_mutex_lock(&ctx_lock);
    struct hfs_ctx_state *s=ctx_slot(ctx,1);
    if(s){s->has_fstype=1;s->set_by_fstype=0;snprintf(s->fstype,sizeof s->fstype,"%s",pattern?pattern:"");}
    pthread_mutex_unlock(&ctx_lock);
    return real(ctx,pattern);
}
int mnt_context_set_fstype(struct libmnt_context *ctx,const char *fstype) {
    int (*real)(struct libmnt_context *,const char *)=REAL_MNT_CONTEXT_SET_FSTYPE();
    if(!real)return -ENOSYS;
    pthread_mutex_lock(&ctx_lock);
    struct hfs_ctx_state *s=ctx_slot(ctx,1);
    if(s){s->has_fstype=1;s->set_by_fstype=1;snprintf(s->fstype,sizeof s->fstype,"%s",fstype?fstype:"");}
    pthread_mutex_unlock(&ctx_lock);
    return real(ctx,fstype);
}
int mnt_context_set_options(struct libmnt_context *ctx,const char *optstr) {
    int (*real)(struct libmnt_context *,const char *)=REAL_MNT_CONTEXT_SET_OPTIONS();
    if(!real)return -ENOSYS;
    pthread_mutex_lock(&ctx_lock);
    struct hfs_ctx_state *s=ctx_slot(ctx,1);
    if(s){s->has_opts=1;snprintf(s->opts,sizeof s->opts,"%s",optstr?optstr:"");}
    pthread_mutex_unlock(&ctx_lock);
    return real(ctx,optstr);
}
void mnt_free_context(struct libmnt_context *ctx) {
    void (*real)(struct libmnt_context *)=REAL_MNT_FREE_CONTEXT();
    pthread_mutex_lock(&ctx_lock);
    struct hfs_ctx_state *s=ctx_slot(ctx,0);
    if(s)memset(s,0,sizeof *s);
    pthread_mutex_unlock(&ctx_lock);
    if(real)real(ctx);
}

static void unescape_mountinfo_path(const char *src,char *dst,size_t dstsz) {
    size_t j=0;
    while(*src&&j+1<dstsz) {
        if(src[0]=='\\'&&src[1]>='0'&&src[1]<='7'&&src[2]>='0'&&src[2]<='7'&&src[3]>='0'&&src[3]<='7') {
            dst[j++]=(char)(((src[1]-'0')<<6)|((src[2]-'0')<<3)|(src[3]-'0'));
            src+=4;
        } else {
            dst[j++]=*src++;
        }
    }
    dst[j]=0;
}
static int mountinfo_lookup(const char *target,char *fstype,size_t ftsz,char *majmin,size_t mmsz,char *source,size_t srcsz) {
    if(!target)return -1;
    FILE *f=fopen(PROC_MOUNTINFO_PATH,"re");if(!f)return -1;
    char *line=NULL;size_t cap=0;int found=-1;
    while(getline(&line,&cap,f)>0) {
        char *sp[5]={0};char *p=line;int idx=0;
        for(;idx<5;idx++){sp[idx]=p;char *q=strchr(p,' ');if(!q)break;p=q+1;}
        if(idx<5)continue;
        char *mm_end=strchr(sp[2],' ');
        char *mp_end=strchr(sp[4],' ');if(!mm_end||!mp_end)continue;
        *mp_end=0;
        char mp[PATH_MAX];unescape_mountinfo_path(sp[4],mp,sizeof mp);
        if(strcmp(mp,target))continue;
        char *sep=strstr(mp_end+1," - ");if(!sep)continue;
        char *ft=sep+3;char *ft_end=strpbrk(ft," \t\r\n");
        char *src=NULL;
        if(ft_end) {
            char sep_ch=*ft_end;*ft_end=0;
            if(sep_ch==' '||sep_ch=='\t') {
                src=ft_end+1;src+=strspn(src," \t");
                char *src_end=strpbrk(src," \t\r\n");if(src_end)*src_end=0;
            }
        }
        if(*ft) {
            if(fstype&&ftsz)snprintf(fstype,ftsz,"%s",ft);
            if(majmin&&mmsz){size_t ml=(size_t)(mm_end-sp[2]);if(ml>=mmsz)ml=mmsz-1;memcpy(majmin,sp[2],ml);majmin[ml]=0;}
            if(source&&srcsz){if(src&&*src)unescape_mountinfo_path(src,source,srcsz);else source[0]=0;}
            found=0;
        }
    }
    free(line);fclose(f);return found;
}
static int mountinfo_fstype(const char *target,char *out,size_t outsz) {
    return mountinfo_lookup(target,out,outsz,NULL,0,NULL,0);
}
static int hfs_dev_present_probe(const char *target,const char *ctx_source) {
    char ft[64]="",majmin[32]="",msrc[PATH_MAX]="";
    if(!target||mountinfo_lookup(target,ft,sizeof ft,majmin,sizeof majmin,msrc,sizeof msrc))return 0;
    unsigned maj=0,min=0;
    if(sscanf(majmin,"%u:%u",&maj,&min)!=2)return 0;
    if(maj>0) {
        char sysblk[PATH_MAX];
        if(snprintf(sysblk,sizeof sysblk,"%s/%u:%u",SYS_DEV_BLOCK_DIR,maj,min)>=(int)sizeof sysblk)return 0;
        struct stat sst;
        if(lstat(sysblk,&sst))return 0;
    }
    const char *src=(msrc[0]=='/')?msrc:(ctx_source&&ctx_source[0]=='/')?ctx_source:NULL;
    if(!src)return maj>0?1:0;
    struct stat st;
    if(stat(src,&st))return 0;
    if(maj>0) {
        if(!S_ISBLK(st.st_mode)||major(st.st_rdev)!=maj||minor(st.st_rdev)!=min)return 0;
        int fd=open(src,O_RDONLY|O_CLOEXEC);
        if(fd<0)return 0;
        close(fd);
    }
    return 1;
}
#ifndef HFS_DEV_PRESENT
#define HFS_DEV_PRESENT hfs_dev_present_probe
#endif
// The mount tests replace these; a bind mount is never HFS+ nor a block device.
#ifndef HFS_READONLY
static int hfs_readonly(const char *target) {
    struct statfs f;struct statvfs v;char ft[64]="";
    if(statfs(target,&f)||f.f_type!=HFSPLUS_MAGIC||statvfs(target,&v)||!(v.f_flag&ST_RDONLY))return 0;
    if(!mountinfo_fstype(target,ft,sizeof ft)&&!strcmp(ft,"ufsd"))return 0;
    return 1;
}
#define HFS_READONLY hfs_readonly
#endif
#ifndef HFS_RW_MOUNTED
static int hfs_rw_mounted(const char *target) {
    struct statfs f;struct statvfs v;char ft[64]="";
    if(!target||statfs(target,&f)||f.f_type!=HFSPLUS_MAGIC||statvfs(target,&v)||(v.f_flag&ST_RDONLY))return 0;
    if(!mountinfo_fstype(target,ft,sizeof ft))return !strcmp(ft,"hfsplus")||!strcmp(ft,"hfsplusx");
    return 1;
}
#define HFS_RW_MOUNTED hfs_rw_mounted
#endif

int mnt_context_enable_lazy(struct libmnt_context *ctx,int enable) {
    int (*real)(struct libmnt_context *,int)=REAL_MNT_CONTEXT_ENABLE_LAZY();
    if(!real)return -ENOSYS;
    const char *target=mnt_context_get_target(ctx);
    if(enable&&target&&HFS_RW_MOUNTED(target)&&HFS_DEV_PRESENT(target,mnt_context_get_source(ctx))) {
        note("disabling lazy unmount (MNT_DETACH) for rw HFS+ volume",target,0);
        enable=0;
    }
    pthread_mutex_lock(&ctx_lock);
    struct hfs_ctx_state *s=ctx_slot(ctx,1);
    if(s){s->has_lazy=1;s->lazy=enable;}
    pthread_mutex_unlock(&ctx_lock);
    return real(ctx,enable);
}

static uint16_t hfs_be16(const uint8_t *p){return (uint16_t)(((uint16_t)p[0]<<8)|p[1]);}
static uint32_t hfs_be32(const uint8_t *p){return ((uint32_t)p[0]<<24)|((uint32_t)p[1]<<16)|((uint32_t)p[2]<<8)|p[3];}
static uint64_t hfs_be64(const uint8_t *p){return ((uint64_t)hfs_be32(p)<<32)|hfs_be32(p+4);}
static uint32_t hfs_le32(const uint8_t *p){return ((uint32_t)p[3]<<24)|((uint32_t)p[2]<<16)|((uint32_t)p[1]<<8)|p[0];}
static uint64_t hfs_le64(const uint8_t *p){return ((uint64_t)hfs_le32(p+4)<<32)|hfs_le32(p);}
static int pread_full(int fd,void *buf,size_t n,off_t off) {
    uint8_t *p=buf;size_t got=0;
    while(got<n) {
        ssize_t r=pread(fd,p+got,n-got,off+(off_t)got);
        if(r>0){got+=(size_t)r;continue;}
        if(r<0&&errno==EINTR)continue;
        if(r==0)errno=EIO;
        return -1;
    }
    return 0;
}
static int hfs_find_volume_header(int fd,uint8_t vh[512],uint64_t *emb_off_out,uint64_t *emb_limit_out) {
    uint8_t buf[512];
    if(pread_full(fd,buf,sizeof buf,1024))return -1;
    uint16_t sig=hfs_be16(buf);
    uint64_t emb_off=0,emb_limit=0;
    if(sig==0x4244) {
        uint32_t hfs_bs=hfs_be32(buf+20);
        uint16_t hfs_ab_st=hfs_be16(buf+28);
        uint16_t emb_sig=hfs_be16(buf+124),emb_st=hfs_be16(buf+126),emb_cnt=hfs_be16(buf+128);
        if(emb_sig!=0x482b||hfs_bs<512||(hfs_bs&(hfs_bs-1))||!emb_cnt){errno=EINVAL;return -1;}
        emb_off=(uint64_t)hfs_ab_st*512ULL+(uint64_t)emb_st*(uint64_t)hfs_bs;
        emb_limit=(uint64_t)emb_cnt*(uint64_t)hfs_bs;
        if(pread_full(fd,buf,sizeof buf,(off_t)(emb_off+1024)))return -1;
        sig=hfs_be16(buf);
    }
    uint16_t ver=hfs_be16(buf+2);
    if(!((sig==0x482b&&ver==4)||(sig==0x4858&&ver==5))){errno=EINVAL;return -1;}
    memcpy(vh,buf,512);
    if(emb_off_out)*emb_off_out=emb_off;
    if(emb_limit_out)*emb_limit_out=emb_limit;
    return 0;
}
static int hfs_probe_source_signature(const char *source) {
    if(!source||!*source)return 0;
    struct stat st;
    int fd=open(source,O_RDONLY|O_CLOEXEC);if(fd<0)return 0;
    int is_hfs=0;
    if(!fstat(fd,&st)&&(S_ISBLK(st.st_mode)||S_ISREG(st.st_mode))) {
        uint8_t vh[512];
        if(!hfs_find_volume_header(fd,vh,NULL,NULL))is_hfs=1;
    }
    close(fd);return is_hfs;
}
static int hfs_check_volume_and_journal(const char *source,int *is_journaled,int allow_unclean,int *needs_clean) {
    *is_journaled=0;
    if(needs_clean)*needs_clean=0;
    int fd=open(source,O_RDONLY|O_CLOEXEC);if(fd<0)return -1;
    struct stat st;
    if(fstat(fd,&st)||(!S_ISBLK(st.st_mode)&&!S_ISREG(st.st_mode))){close(fd);errno=EINVAL;return -1;}
    uint8_t vh[512];uint64_t emb_off=0,emb_limit=0;
    if(hfs_find_volume_header(fd,vh,&emb_off,&emb_limit)){close(fd);return -1;}
    uint32_t attr=hfs_be32(vh+4),jinfo_blk=hfs_be32(vh+12),bs=hfs_be32(vh+40),tb=hfs_be32(vh+44);
    if(attr&(1u<<15)){close(fd);errno=EIO;return -1;} // always reject SOFTLOCK
    // UNMNT (bit 8) cleared or Apple INCNSTNT (bit 14) set means unclean; Linux bit 11 is ignored by kernel mount and untouched by fsck.hfsplus.
    int unclean=!(attr&(1u<<8))||(attr&(1u<<14));
    if(needs_clean)*needs_clean=unclean;
    if(!allow_unclean&&unclean){close(fd);errno=EIO;return -1;}
    if(bs<512||bs>(1u<<20)||(bs&(bs-1))||!tb){close(fd);errno=EINVAL;return -1;}
    uint64_t vol_bytes=(uint64_t)tb*(uint64_t)bs;
    if(emb_limit&&vol_bytes>emb_limit){close(fd);errno=EINVAL;return -1;}
    if(!(attr&(1u<<13))){close(fd);return 0;} // non-journaled HFS+
    *is_journaled=1;
    if(!jinfo_blk||jinfo_blk>=tb){close(fd);errno=EIO;return -1;}
    uint8_t jib[64];
    if(pread_full(fd,jib,sizeof jib,(off_t)(emb_off+(uint64_t)jinfo_blk*bs))){close(fd);return -1;}
    uint32_t jflags=hfs_be32(jib);
    if(!(jflags&1u)||(jflags&2u)){close(fd);errno=EIO;return -1;} // require kJIJournalInFSMask, !kJIJournalOnOtherDeviceMask
    uint64_t joff=hfs_be64(jib+36),jsize=hfs_be64(jib+44);
    if(joff<bs||jsize<4096||joff>vol_bytes||jsize>vol_bytes-joff){close(fd);errno=EIO;return -1;}
    uint8_t jh[44];
    if(pread_full(fd,jh,sizeof jh,(off_t)(emb_off+joff))){close(fd);return -1;}
    close(fd);
    if(jflags&4u) { // kJIJournalNeedInitMask: allowed only if journal header is uninitialized zeros
        for(size_t i=0;i<sizeof jh;i++)if(jh[i]){errno=EIO;return -1;}
        return 0;
    }
    uint32_t magic,jhdr_size;uint64_t start,end,jh_size;
    if(hfs_le32(jh+4)==0x12345678u) {
        magic=hfs_le32(jh);start=hfs_le64(jh+8);end=hfs_le64(jh+16);jh_size=hfs_le64(jh+24);jhdr_size=hfs_le32(jh+40);
    } else if(hfs_be32(jh+4)==0x12345678u) {
        magic=hfs_be32(jh);start=hfs_be64(jh+8);end=hfs_be64(jh+16);jh_size=hfs_be64(jh+24);jhdr_size=hfs_be32(jh+40);
    } else {
        errno=EIO;return -1;
    }
    if(magic!=0x4a4e4c78u||jh_size!=jsize||jhdr_size<44||jhdr_size>jsize||
       start<jhdr_size||start>=jsize||end<jhdr_size||end>=jsize||start!=end) {
        errno=EIO;return -1;
    }
    return 0;
}
static int run_fsck_hfsplus(const char *fsck_bin,const char *source,int mark_clean,int64_t deadline) {
    if(!fsck_bin||!source||budget(deadline))return -1;
    int pfd[2];if(pipe2(pfd,O_CLOEXEC|O_NONBLOCK))return -1;
    pid_t pid=fork();if(pid<0){close(pfd[0]);close(pfd[1]);return -1;}
    if(!pid) {
        setpgid(0,0);unsetenv("LD_PRELOAD");setenv("PATH","/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",1);
        dup2(pfd[1],1);dup2(pfd[1],2);close(pfd[0]);if(pfd[1]>2)close(pfd[1]);
        char *const argv[]={(char *)fsck_bin,mark_clean?"-y":"-n","-f",(char *)source,NULL};
        execv(argv[0],argv);_exit(127);
    }
    setpgid(pid,pid);close(pfd[1]);
    char buf[8192];size_t len=0;int overflow=0,eof=0,status=0,reaped=0;
    while(!eof||!reaped) {
        if(!eof) {
            struct pollfd pf={.fd=pfd[0],.events=POLLIN};
            (void)poll(&pf,1,20);
            for(;;) {
                char tmp[512];ssize_t n=read(pfd[0],tmp,sizeof tmp);
                if(n>0) {
                    size_t avail=(sizeof buf-1)-len,cp=(size_t)n<=avail?(size_t)n:avail;
                    if(cp){memcpy(buf+len,tmp,cp);len+=cp;}
                    if((size_t)n>avail)overflow=1;
                } else if(n==0){eof=1;break;}
                else if(errno==EINTR)continue;
                else if(errno==EAGAIN||errno==EWOULDBLOCK)break;
                else{eof=1;break;}
            }
        }
        if(reaped)break;
        pid_t r=waitpid(pid,&status,WNOHANG);
        if(r==pid)reaped=1;
        else if(r<0&&errno!=EINTR){close(pfd[0]);return -1;}
        // -y writes the stick: killed mid-write it could leave it worse than it found it (hfs_prepare_mount
        // starts one only with the time for it).
        if(!reaped&&!mark_clean&&budget(deadline)){kill(-pid,SIGKILL);while(waitpid(pid,&status,0)<0&&errno==EINTR){}close(pfd[0]);errno=ETIMEDOUT;return -1;}
        if(eof&&!reaped)usleep(20000);
    }
    close(pfd[0]);buf[len]=0;
    if(!WIFEXITED(status)||WEXITSTATUS(status)!=0||overflow){errno=EIO;return -1;}
    if(!strstr(buf,"appears to be OK.")||strstr(buf,"No checking performed")||
       strstr(buf,"needs to be repaired")||strstr(buf,"could not be verified")||
       strstr(buf,"Journal need to be replayed")||strstr(buf,"Repairing volume")||
       strstr(buf,"was repaired")){errno=EIO;return -1;}
    return 0;
}
static int fs_supported(const char *fsname) {
    FILE *f=fopen(PROC_FILESYSTEMS_PATH,"re");if(!f)return 0;
    char line[256];int ok=0;
    while(fgets(line,sizeof line,f)) {
        char *tab=strchr(line,'\t');char *name=tab?tab+1:line;
        char *end=strpbrk(name," \t\r\n");if(end)*end=0;
        if(!strcmp(name,fsname)){ok=1;break;}
    }
    fclose(f);return ok;
}
static int check_ko_vermagic(int fd,const char *release) {
    struct stat st;
    if(fstat(fd,&st)||!S_ISREG(st.st_mode)||st.st_size<(off_t)sizeof(Elf32_Ehdr)||st.st_size>8*1024*1024)return -1;
    Elf32_Ehdr eh;if(pread_full(fd,&eh,sizeof eh,0))return -1;
    if(memcmp(eh.e_ident,ELFMAG,SELFMAG)||eh.e_ident[EI_CLASS]!=ELFCLASS32||eh.e_ident[EI_DATA]!=ELFDATA2LSB||
       eh.e_type!=ET_REL||eh.e_machine!=EM_ARM||eh.e_shentsize!=sizeof(Elf32_Shdr)||
       !eh.e_shnum||eh.e_shnum>4096||eh.e_shstrndx>=eh.e_shnum||
       (uint64_t)eh.e_shoff+(uint64_t)eh.e_shnum*sizeof(Elf32_Shdr)>(uint64_t)st.st_size){errno=ENOEXEC;return -1;}
    Elf32_Shdr *sh=calloc(eh.e_shnum,sizeof *sh);if(!sh)return -1;
    if(pread_full(fd,sh,eh.e_shnum*sizeof *sh,(off_t)eh.e_shoff)){free(sh);return -1;}
    uint64_t str_off=sh[eh.e_shstrndx].sh_offset,str_sz=sh[eh.e_shstrndx].sh_size;
    if(!str_sz||str_sz>256*1024||str_off+str_sz>(uint64_t)st.st_size){free(sh);errno=ENOEXEC;return -1;}
    char *strtab=calloc((size_t)str_sz+1,1);
    if(!strtab||pread_full(fd,strtab,(size_t)str_sz,(off_t)str_off)){free(strtab);free(sh);return -1;}
    int vermagic_ok=0,name_ok=1;size_t rlen=strlen(release);
    for(unsigned i=0;i<eh.e_shnum;i++) {
        if(sh[i].sh_name>=str_sz||strcmp(strtab+sh[i].sh_name,".modinfo"))continue;
        uint64_t m_off=sh[i].sh_offset,m_sz=sh[i].sh_size;
        if(!m_sz||m_sz>256*1024||m_off+m_sz>(uint64_t)st.st_size)break;
        char *mi=calloc((size_t)m_sz+1,1);
        if(!mi||pread_full(fd,mi,(size_t)m_sz,(off_t)m_off)){free(mi);break;}
        for(size_t off=0;off<(size_t)m_sz;) {
            char *entry=mi+off;size_t elen=strlen(entry);
            if(!strncmp(entry,"vermagic=",9)){const char *v=entry+9;if(!strncmp(v,release,rlen)&&v[rlen]==' ')vermagic_ok=1;}
            else if(!strncmp(entry,"name=",5)){if(strcmp(entry+5,"hfsplusx"))name_ok=0;}
            off+=elen+1;
        }
        free(mi);break;
    }
    free(strtab);free(sh);
    if(!vermagic_ok||!name_ok){errno=ENOEXEC;return -1;}
    return 0;
}
// LoadPin on Gen-1 enforces rootfs-only module loading: read only from /lib/modules/<rel>/extra/
// (never fall back to /data or /etc/enginex).
#ifndef HFSPLUSX_MODULES_ROOT
#define HFSPLUSX_MODULES_ROOT "/lib/modules"
#endif
static int hfs_ensure_hfsplusx(void) {
    if(fs_supported("hfsplusx"))return 0;
    struct utsname u;if(HOOK_UNAME(&u))return -1;
    char path[PATH_MAX];
    const char *mroot=HFSPLUSX_MODULES_ROOT;
    if(!mroot||!*mroot||!u.release[0]||strchr(u.release,'/')||!strcmp(u.release,".")||!strcmp(u.release,"..")){errno=ENOENT;return -1;}
    if(snprintf(path,sizeof path,"%s/%s/extra/hfsplusx.ko",mroot,u.release)>=(int)sizeof path){errno=ENAMETOOLONG;return -1;}
    int fd=open(path,O_RDONLY|O_NOFOLLOW|O_CLOEXEC);
    if(fd<0)return -1;
    if(check_ko_vermagic(fd,u.release)){int saved=errno;close(fd);note("hfsplusx vermagic mismatch on",path,saved);errno=saved;return -1;}
    if(FINIT_MODULE(fd)!=0) {
        int saved=errno;close(fd);
        if(saved==EEXIST&&fs_supported("hfsplusx"))return 0;
        note("insmod hfsplusx failed on",path,saved);errno=saved;return -1;
    }
    close(fd);return 0;
}
static int has_opt_token(const char *opts,const char *tok) {
    if(!opts)return 0;
    size_t tlen=strlen(tok);const char *p=opts;
    while(*p) {
        const char *comma=strchr(p,',');size_t len=comma?(size_t)(comma-p):strlen(p);
        if(len==tlen&&!strncmp(p,tok,tlen))return 1;
        if(!comma)break;
        p=comma+1;
    }
    return 0;
}
static int build_hfs_opts(char *out,size_t outsz,const char *orig,int rw,int force) {
    size_t used=0;out[0]=0;
    const char *p=orig?orig:"";
    while(*p) {
        const char *comma=strchr(p,',');size_t len=comma?(size_t)(comma-p):strlen(p);
        if(len&&!((len==2&&(!strncmp(p,"ro",2)||!strncmp(p,"rw",2)))||
                  (len==5&&!strncmp(p,"force",5))||(len==4&&!strncmp(p,"utf8",4)))) {
            if(used+len+(used?1:0)>=outsz){errno=ENAMETOOLONG;return -1;}
            if(used)out[used++]=',';
            memcpy(out+used,p,len);used+=len;out[used]=0;
        }
        if(!comma)break;
        p=comma+1;
    }
    const char *tail=rw?(force?"force,rw":"rw"):"ro";
    size_t tlen=strlen(tail);
    if(used+tlen+(used?1:0)>=outsz){errno=ENAMETOOLONG;return -1;}
    if(used)out[used++]=',';
    memcpy(out+used,tail,tlen+1);
    return 0;
}
struct hfs_ctx_backup {
    int tried_rw,set_by_fstype,has_disable_helpers,disable_helpers,has_lazy,lazy,is_journaled;
    unsigned long orig_mflags;
    char orig_source[PATH_MAX],orig_target[PATH_MAX],orig_fstype[64],orig_opts[512];
};
static void hfs_apply_ro(struct libmnt_context *ctx,const char *orig_fstype,int set_by_fstype,const char *orig_opts) {
    int (*set_pat)(struct libmnt_context *,const char *)=REAL_MNT_CONTEXT_SET_FSTYPE_PATTERN();
    int (*set_ft)(struct libmnt_context *,const char *)=REAL_MNT_CONTEXT_SET_FSTYPE();
    int (*set_opt)(struct libmnt_context *,const char *)=REAL_MNT_CONTEXT_SET_OPTIONS();
    int (*get_mf)(struct libmnt_context *,unsigned long *)=REAL_MNT_CONTEXT_GET_MFLAGS();
    int (*set_mf)(struct libmnt_context *,unsigned long)=REAL_MNT_CONTEXT_SET_MFLAGS();
    char ro_opts[512];
    if(!build_hfs_opts(ro_opts,sizeof ro_opts,orig_opts,0,0)&&set_opt)set_opt(ctx,ro_opts);
    unsigned long mf=0;
    if(get_mf&&set_mf&&!get_mf(ctx,&mf))set_mf(ctx,mf|MS_RDONLY);
    const char *ft=(orig_fstype&&*orig_fstype&&strcmp(orig_fstype,"hfsplusx"))?orig_fstype:"hfsplus";
    if(set_by_fstype&&set_ft)set_ft(ctx,ft);
    else if(set_pat)set_pat(ctx,ft);
}
static void hfs_restore_ro(struct libmnt_context *ctx,const struct hfs_ctx_backup *bk) {
    int (*reset_ctx)(struct libmnt_context *)=REAL_MNT_RESET_CONTEXT();
    int (*set_src)(struct libmnt_context *,const char *)=REAL_MNT_CONTEXT_SET_SOURCE();
    int (*set_tgt)(struct libmnt_context *,const char *)=REAL_MNT_CONTEXT_SET_TARGET();
    int (*dis_hlp)(struct libmnt_context *,int)=REAL_MNT_CONTEXT_DISABLE_HELPERS();
    int (*set_lazy)(struct libmnt_context *,int)=REAL_MNT_CONTEXT_ENABLE_LAZY();
    int (*set_mf)(struct libmnt_context *,unsigned long)=REAL_MNT_CONTEXT_SET_MFLAGS();
    if(reset_ctx&&set_src&&set_tgt&&!reset_ctx(ctx)) {
        if(*bk->orig_source)set_src(ctx,bk->orig_source);
        if(*bk->orig_target)set_tgt(ctx,bk->orig_target);
        if(bk->has_disable_helpers&&dis_hlp)dis_hlp(ctx,bk->disable_helpers);
        if(bk->has_lazy&&set_lazy)set_lazy(ctx,bk->lazy);
        if(set_mf&&bk->orig_mflags)set_mf(ctx,bk->orig_mflags|MS_RDONLY);
    }
    hfs_apply_ro(ctx,bk->orig_fstype,bk->set_by_fstype,bk->orig_opts);
}
// A stick read-only until now and with no Engine Library of its own has the library Engine built
// for it only in /data: the generation in use (seed_library touches it) is library-<zero signature>.
// Read-write, Engine would start an empty library on the stick and no longer show that one, so
// carry_library moves it onto the stick first. Its path goes to out. Once the stick has a library
// of its own, a newer generation is in use.
static char carry_from[PATH_MAX];
static int deck_library_only(const char *source,char out[PATH_MAX]) {
    char identity[80],cache[PATH_MAX],root[PATH_MAX],db[PATH_MAX],newest[NAME_MAX+1]="";struct stat st,best={0};
    if(VOLUME_IDENTITY(source,identity)||lib_root(cache)||path_join(root,sizeof root,cache,identity))return 0;
    DIR *d=opendir(root);if(!d)return 0;
    struct dirent *e;
    while((e=readdir(d)))
        if(seeded_ok(e->d_name)&&!fstatat(dirfd(d),e->d_name,&st,AT_SYMLINK_NOFOLLOW)&&S_ISDIR(st.st_mode)&&(!*newest||newer(&st,&best))) {
            best=st;snprintf(newest,sizeof newest,"%s",e->d_name);
        }
    closedir(d);
    return !strcmp(newest,"library-0000000000000000")&&!path_join(out,PATH_MAX,root,newest)&&!path_join(db,sizeof db,out,"Database2/m.db")&&
           !lstat(db,&st)&&S_ISREG(st.st_mode)&&st.st_size>0;
}
// A carry-over that ran out of the mount budget is not tried again until reboot: each insert would
// wait the whole budget for it and end read-only anyway.
static int carry_marker(const char *library,char out[PATH_MAX]) {
    uint64_t h=hash_bytes(UINT64_C(14695981039346656037),library,strlen(library));
    if(snprintf(out,PATH_MAX,"%s/slowcarry-%016llx",RUN_ROOT,(unsigned long long)h)>=PATH_MAX){errno=ENAMETOOLONG;return -1;}
    return 0;
}
static int hfs_prepare_mount(struct libmnt_context *ctx,struct hfs_ctx_backup *bk,int64_t deadline) {
    memset(bk,0,sizeof *bk);
    const char *target=mnt_context_get_target(ctx),*source=mnt_context_get_source(ctx);
    if(!target||!source||volume(target)==NULL||safe_path(target,0))return HFS_RW_NONE;
    char orig_ft[64]="",orig_opts[512]="";int has_ft=0,set_by_fstype=0,has_dis=0,dis=0,has_lazy=0,lazy=0;
    pthread_mutex_lock(&ctx_lock);
    struct hfs_ctx_state *s=ctx_slot(ctx,0);
    if(s){has_ft=s->has_fstype;set_by_fstype=s->set_by_fstype;has_dis=s->has_disable_helpers;dis=s->disable_helpers;has_lazy=s->has_lazy;lazy=s->lazy;snprintf(orig_ft,sizeof orig_ft,"%s",s->fstype);snprintf(orig_opts,sizeof orig_opts,"%s",s->opts);}
    pthread_mutex_unlock(&ctx_lock);
    if(has_ft&&*orig_ft) {
        if(!strcmp(orig_ft,"ufsd")||strstr(orig_ft,"ufsd"))return HFS_RW_NONE; // coexist with stock ufsd when AZ04 DT flag is present
        if(strcmp(orig_ft,"hfsplus")&&strcmp(orig_ft,"hfsplusx"))return HFS_RW_NONE;
    } else if(!hfs_probe_source_signature(source)) {
        return HFS_RW_NONE;
    }
    unsigned long orig_mflags=0;
    int (*get_mf)(struct libmnt_context *,unsigned long *)=REAL_MNT_CONTEXT_GET_MFLAGS();
    if(get_mf&&get_mf(ctx,&orig_mflags))orig_mflags=0;
    if(has_opt_token(orig_opts,"ro")||(orig_mflags&MS_RDONLY)) {
        hfs_apply_ro(ctx,orig_ft,set_by_fstype,orig_opts);
        return HFS_RW_NONE;
    }
    int mode=HFS_PLATFORM_MODE();
    int gen2=(mode==1&&access(GEN1_ROOTFS_MARKER,F_OK)!=0);
    if(gen2&&!LKLFUSE_PATH()) {
        hfs_apply_ro(ctx,orig_ft,set_by_fstype,orig_opts);
        return HFS_RW_NONE;
    }
    if(mode!=1&&mode!=2){hfs_apply_ro(ctx,orig_ft,set_by_fstype,orig_opts);return HFS_RW_NONE;}
    // AZ04 (mode == 2) uses stock hfsplus; Gen-1 (mode == 1) uses hfsplusx.
    // Even if AZ04_UFSD_DT_PATH exists, hfsplus mounts stay on stock hfsplus rw while ufsd mounts above pass untouched.
    (void)AZ04_UFSD_DT_PATH;
    char marker[PATH_MAX];
    if(!deck_library_only(source,carry_from))carry_from[0]=0;
    else if(!carry_marker(carry_from,marker)&&!access(marker,F_OK)) {
        note("HFS+ stick's library on /data too large to carry over at mount, keeping read-only view on",target,0);
        carry_from[0]=0;hfs_apply_ro(ctx,orig_ft,set_by_fstype,orig_opts);return HFS_RW_NONE;
    }
    set_preparing(target,1);
    const char *fsck=FSCK_HFSPLUS_PATH();
    int is_journaled=0,needs_clean=0;
    int64_t checked=now_ms();
    if(!fsck||run_fsck_hfsplus(fsck,source,0,deadline)) {
        note("HFS+ fsck -n -f failed, keeping read-only view on",target,errno);
        hfs_apply_ro(ctx,orig_ft,set_by_fstype,orig_opts);return HFS_RW_NONE;
    }
    if(hfs_check_volume_and_journal(source,&is_journaled,1,&needs_clean)) {
        note("HFS+ volume/journal check failed, keeping read-only view on",target,errno);
        hfs_apply_ro(ctx,orig_ft,set_by_fstype,orig_opts);return HFS_RW_NONE;
    }
    int64_t now=now_ms();
    if(needs_clean&&now+2*(now-checked)>deadline) { // -y and the check after it each take about as long
        note("HFS+ volume unclean, no time left to mark it clean, keeping read-only view on",target,ETIMEDOUT);
        hfs_apply_ro(ctx,orig_ft,set_by_fstype,orig_opts);return HFS_RW_NONE;
    }
    if(needs_clean) {
        note("HFS+ volume unclean after clean fsck -n -f, marking clean on",source,0);
        if(run_fsck_hfsplus(fsck,source,1,deadline)||
           run_fsck_hfsplus(fsck,source,0,deadline)||
           hfs_check_volume_and_journal(source,&is_journaled,0,NULL)) {
            note("HFS+ mark-clean or re-check failed, keeping read-only view on",target,errno);
            hfs_apply_ro(ctx,orig_ft,set_by_fstype,orig_opts);return HFS_RW_NONE;
        }
    }
    if(snprintf(bk->orig_source,sizeof bk->orig_source,"%s",source)>=(int)sizeof bk->orig_source||
       snprintf(bk->orig_target,sizeof bk->orig_target,"%s",target)>=(int)sizeof bk->orig_target) {
        hfs_apply_ro(ctx,orig_ft,set_by_fstype,orig_opts);return HFS_RW_NONE;
    }
    snprintf(bk->orig_fstype,sizeof bk->orig_fstype,"%s",(*orig_ft&&strcmp(orig_ft,"hfsplusx"))?orig_ft:"hfsplus");
    snprintf(bk->orig_opts,sizeof bk->orig_opts,"%s",orig_opts);
    bk->set_by_fstype=set_by_fstype;
    bk->has_disable_helpers=has_dis;bk->disable_helpers=dis;
    bk->has_lazy=has_lazy;bk->lazy=lazy;
    bk->orig_mflags=orig_mflags;
    bk->is_journaled=is_journaled;
    if(gen2)return HFS_RW_LKLFUSE;
    const char *target_fs="hfsplus";
    if(mode==1) {
        if(hfs_ensure_hfsplusx()) {
            note("hfsplusx load unavailable, keeping read-only view on",target,errno);
            hfs_apply_ro(ctx,orig_ft,set_by_fstype,orig_opts);return HFS_RW_NONE;
        }
        target_fs="hfsplusx";
    }
    char rw_opts[512];
    if(build_hfs_opts(rw_opts,sizeof rw_opts,orig_opts,1,is_journaled)) {
        hfs_apply_ro(ctx,orig_ft,set_by_fstype,orig_opts);return HFS_RW_NONE;
    }
    int (*set_pat)(struct libmnt_context *,const char *)=REAL_MNT_CONTEXT_SET_FSTYPE_PATTERN();
    int (*set_ft)(struct libmnt_context *,const char *)=REAL_MNT_CONTEXT_SET_FSTYPE();
    int (*set_opt)(struct libmnt_context *,const char *)=REAL_MNT_CONTEXT_SET_OPTIONS();
    if((set_by_fstype?!set_ft:!set_pat)||!set_opt) {
        hfs_apply_ro(ctx,orig_ft,set_by_fstype,orig_opts);return HFS_RW_NONE;
    }
    if((set_by_fstype?set_ft(ctx,target_fs):set_pat(ctx,target_fs))||set_opt(ctx,rw_opts)) {
        hfs_apply_ro(ctx,bk->orig_fstype,bk->set_by_fstype,bk->orig_opts);return HFS_RW_NONE;
    }
    bk->tried_rw=1;
    return mode==1?HFS_RW_HFSPLUSX:HFS_RW_STOCK;
}
#ifndef RB_BRIDGE_PATH
static const char *rb_bridge_path(void) {
    return access("/usr/local/bin/rb-bridge",X_OK)?"/data/enginex/bundle/bin/rb-bridge":"/usr/local/bin/rb-bridge";
}
#define RB_BRIDGE_PATH rb_bridge_path
#endif
// engine-guardian the same way: a second-generation deck keeps the vendor's
// rootfs and has the tools only in the data bundle (BH2-A1-1: the hook asked
// for /usr/local/bin alone, so no stick was ever healed at mount there).
#ifndef GUARDIAN_SYSTEM
#define GUARDIAN_SYSTEM "/usr/local/bin/engine-guardian"
#endif
#ifndef GUARDIAN_BUNDLE
#define GUARDIAN_BUNDLE "/data/enginex/bundle/bin/engine-guardian"
#endif
#ifndef GUARDIAN_PATH
static const char *guardian_path(void) { return access(GUARDIAN_SYSTEM,X_OK)?GUARDIAN_BUNDLE:GUARDIAN_SYSTEM; }
#define GUARDIAN_PATH guardian_path()
#endif
// What after_mount does on a volume. Engine learns of a mount from edisksd's
// mountinfo monitor (update_mountinfo -> filesystem_mounted -> Mountpoints
// PropertiesChanged) while edisksd's mount thread is still inside this call, so
// anything done after the real mount races Engine's first look at the stick
// (#182: playlists only after a re-insert). The conversion therefore runs first,
// in a private mount namespace, and the visible mount then finds it done.
enum { MOUNT_PRE=1, MOUNT_POST=2, MOUNT_FULL=3 };
#define CONVERTED 1
#define TIMED_OUT 4
#define HEALED 8
// An Engine stick (no rekordbox export): its database is repaired by
// engine-guardian. BH-F52-1: that ran after the visible mount, a second writer
// on m.db/hm.db while Engine opened them. It now runs in the private pre-mount,
// like the conversion; the visible mount never runs it.
static int has_engine_db(const char *target) {
    char db[PATH_MAX];
    return !path_join(db,sizeof db,target,"Engine Library/Database2/m.db")&&!safe_path(db,0);
}
// edisksd's monitor thread reads /proc/self/mountinfo while the mount runs on
// another thread. It announced the bare read-only HFS+ mount ~120 ms before the
// view covered it; Engine then saw no Engine library, chose OneLibrary, and its
// migration failed EROFS, leaving the stick "Updating" with no playlists (r2).
// So the monitor waits until the visible mount (and its view) is in place, and
// each view is shown to it as the plain device mount it stands for.
static pthread_mutex_t post_lock=PTHREAD_MUTEX_INITIALIZER;
static __thread int in_hook,holding_post;
static void release_post(void){if(holding_post){holding_post=0;pthread_mutex_unlock(&post_lock);}}
static int starts_dir(const char *s,const char *dir,size_t n){return !strncmp(s,dir,n)&&s[n]=='/';}
static int is_vol_mp(const char *mp,size_t ml,const char *root,size_t rlen) {
    if(!rlen||ml<=rlen+1||!starts_dir(mp,root,rlen))return 0;
    return memchr(mp+rlen+1,'/',ml-(rlen+1))==NULL;
}
// A memfd with filename's mountinfo lines minus RUN_ROOT helper mounts, the view
// overlays (current or legacy tmpfs views, or stacked volume-root binds) and
// everything mounted inside one; the device mount underneath stays. -1: nothing
// to hide, or parse the file as is.
#define HFS_GEN2_MAGIC UINT64_C(0x454e47584c4b4c31)
#define MAX_GEN2_MOUNTS 16
struct hfs_gen2_mount {
    uint64_t magic;
    uint64_t pid_starttime;
    uint32_t dev_maj,dev_min;
    uint32_t fuse_maj,fuse_min;
    int32_t pid;
    int32_t is_journaled;
    char target[PATH_MAX];
    char source[PATH_MAX];
};
static pthread_mutex_t gen2_lock=PTHREAD_MUTEX_INITIALIZER;
static struct hfs_gen2_mount gen2_slots[MAX_GEN2_MOUNTS];
static unsigned hfs_gen2_load_records(struct hfs_gen2_mount *out,unsigned max);
static int write_all(int fd,const void *p,size_t n) {
    for(size_t off=0;off<n;){ssize_t w=write(fd,(const char *)p+off,n-off);if(w<0&&errno==EINTR)continue;if(w<=0)return -1;off+=(size_t)w;}
    return 0;
}
static int hide_views(const char *filename) {
    FILE *f=fopen(filename,"re");if(!f)return -1;
    size_t cap=65536,len=0;char *buf=malloc(cap+1);
    for(;buf;){size_t n=fread(buf+len,1,cap-len,f);len+=n;if(len<cap)break;char *b=realloc(buf,cap*2+1);if(!b){free(buf);buf=NULL;break;}buf=b;cap*=2;}
    int bad=ferror(f);fclose(f);if(!buf||bad){free(buf);return -1;}
    buf[len]=0;
    char run[PATH_MAX];int runlen=snprintf(run,sizeof run,"%s",RUN_ROOT);
    char legacy[PATH_MAX];int legacylen=snprintf(legacy,sizeof legacy,"%s",LEGACY_RUN_ROOT);
    char media[PATH_MAX];int medialen=snprintf(media,sizeof media,"%s",MEDIA_ROOT);
    struct hfs_gen2_mount recs[MAX_GEN2_MOUNTS];unsigned nrecs=hfs_gen2_load_records(recs,MAX_GEN2_MOUNTS);
    struct {unsigned long long id;const char *mp;size_t ml;} seen[64];unsigned nseen=0;
    const char *views[64];size_t vlen[64];unsigned long long stacked_ids[64];unsigned nv=0;int hidden=0;
    for(int pass=0;pass<2;pass++) {
        int fd=-1;
        if(pass){if(!hidden){free(buf);return -1;}fd=memfd_create("mountinfo",MFD_CLOEXEC);if(fd<0){free(buf);return -1;}}
        for(char *line=buf;*line;) {
            char *end=strchr(line,'\n');size_t ll=end?(size_t)(end-line+1):strlen(line);
            char *ep1=NULL,*ep2=NULL;
            unsigned long long mid=strtoull(line,&ep1,10),mpid=(ep1&&*ep1==' ')?strtoull(ep1+1,&ep2,10):0;
            int have_ids=(ep1&&*ep1==' '&&ep2&&*ep2==' '&&mid>0);
            const char *f2=(ep2&&*ep2==' ')?ep2+1:NULL;
            const char *f2e=f2?memchr(f2,' ',ll-(size_t)(f2-line)):NULL;
            // fields: id parent maj:min root mountpoint ... " - " fstype source
            const char *mp=line;for(int i=0;i<4&&mp;i++){mp=memchr(mp,' ',ll-(size_t)(mp-line));if(mp)mp++;}
            const char *mpe=mp?memchr(mp,' ',ll-(size_t)(mp-line)):NULL;
            char *sep=NULL;if(mpe){char c=line[ll-1];line[ll-1]=0;sep=strstr(mpe," - ");line[ll-1]=c;}
            int hide=0;const struct hfs_gen2_mount *rewrite=NULL;
            if(mpe&&sep) {
                size_t ml=(size_t)(mpe-mp);
                int overlay=!strncmp(sep+3,"tmpfs enginex-hfs-view ",23)||!strncmp(sep+3,LEGACY_HFS_MOUNT,22);
                int helper=(runlen>0&&ml>(size_t)runlen&&starts_dir(mp,run,(size_t)runlen))||
                           (legacylen>0&&ml>(size_t)legacylen&&starts_dir(mp,legacy,(size_t)legacylen));
                int stacked=0;
                if(!pass&&!helper&&!overlay&&have_ids&&
                   (is_vol_mp(mp,ml,media,medialen>0?(size_t)medialen:0)||is_vol_mp(mp,ml,"/media",6))) {
                    unsigned j=0;
                    for(;j<nseen;j++)if(seen[j].ml==ml&&!memcmp(seen[j].mp,mp,ml))break;
                    if(j<nseen&&seen[j].id==mpid){stacked=1;seen[j].id=mid;}
                    else if(j==nseen&&nseen<64){seen[nseen].id=mid;seen[nseen].mp=mp;seen[nseen].ml=ml;nseen++;}
                }
                if(!pass&&(overlay||stacked)&&!helper&&nv<64){views[nv]=mp;vlen[nv]=ml;stacked_ids[nv++]=stacked?mid:0;}
                hide=helper||overlay||stacked;
                for(unsigned i=0;i<nv&&!hide;i++)
                    hide=(stacked_ids[i]&&have_ids&&mid==stacked_ids[i])||(ml>vlen[i]&&starts_dir(mp,views[i],vlen[i]));
                if(!hide&&nrecs>0&&f2&&f2e&&!strncmp(sep+3,"fuse",4)&&f2e-f2>2&&f2[0]=='0'&&f2[1]==':') {
                    char mp_raw[PATH_MAX],mp_unesc[PATH_MAX];
                    if(ml<sizeof mp_raw) {
                        memcpy(mp_raw,mp,ml);mp_raw[ml]=0;
                        unescape_mountinfo_path(mp_raw,mp_unesc,sizeof mp_unesc);
                        for(unsigned j=0;j<nrecs;j++) {
                            if(!(recs[j].dev_maj||recs[j].dev_min)||strcmp(mp_unesc,recs[j].target))continue;
                            if(recs[j].fuse_maj||recs[j].fuse_min) {
                                char fbuf[32];int fl=snprintf(fbuf,sizeof fbuf,"%u:%u",recs[j].fuse_maj,recs[j].fuse_min);
                                if(fl!=(int)(f2e-f2)||memcmp(f2,fbuf,(size_t)fl))continue;
                            }
                            rewrite=&recs[j];break;
                        }
                    }
                }
            }
            if(!pass)hidden|=hide||(rewrite!=NULL);
            else if(!hide) {
                if(rewrite) {
                    char devbuf[32];int dl=snprintf(devbuf,sizeof devbuf,"%u:%u",rewrite->dev_maj,rewrite->dev_min);
                    size_t pre_len=(size_t)(f2-line),post_len=ll-(size_t)(f2e-line);
                    if(write_all(fd,line,pre_len)||write_all(fd,devbuf,(size_t)dl)||write_all(fd,f2e,post_len)){close(fd);free(buf);return -1;}
                } else if(write_all(fd,line,ll)){close(fd);free(buf);return -1;}
            }
            line+=ll;
        }
        if(pass){free(buf);return fd;}
    }
    free(buf);return -1;
}
struct libmnt_table;
int mnt_table_parse_file(struct libmnt_table *tb,const char *filename) {
    int (*real)(struct libmnt_table *,const char *)=(int (*)(struct libmnt_table *,const char *))dlsym(RTLD_NEXT,"mnt_table_parse_file");
    if(!real)return -ENOSYS;
    if(in_hook||!filename||strcmp(filename,"/proc/self/mountinfo"))return real(tb,filename); // libmount inside our own mount/umount
    pthread_mutex_lock(&post_lock);
    int fd=hide_views(filename),rc;
    if(fd<0)rc=real(tb,filename);
    else{char p[64];snprintf(p,sizeof p,"/proc/self/fd/%d",fd);rc=real(tb,p);close(fd);}
    pthread_mutex_unlock(&post_lock);return rc;
}
static int after_mount_at(const char *mount_path,const char *label_target,const char *src,int mode,int64_t deadline) {
    if(!volume(label_target)||safe_path(mount_path,0))return 0;
    int pioneer=has_export(mount_path),heal=!pioneer&&mode==MOUNT_PRE&&has_engine_db(mount_path);char lib[PATH_MAX];
    if(path_join(lib,sizeof lib,mount_path,"Engine Library")||safe_path(lib,1)) {note("unsafe library path rejected on",label_target,errno);return 0;}
    struct stat existing;
    int hfs_ro=HFS_READONLY(mount_path);
    if(!pioneer&&!heal&&mode==MOUNT_PRE)return 0; // nothing to convert or heal: the visible mount does the rest
    if(hfs_ro||(pioneer&&mode!=MOUNT_POST)||heal)set_preparing(label_target,mode!=MOUNT_POST);
    // The walk guards the tool run on the library below; after the private
    // conversion or heal (MOUNT_POST) none follows, so it is skipped.
    // An HFS+ view is validated by prepare_view.
    if(!hfs_ro&&mode!=MOUNT_POST&&!lstat(lib,&existing)) {
        int fd=open(lib,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
        if(fd<0)return 0;
        int unsafe=copy_tree(fd,-1,deadline,0,NULL);close(fd);
        if(unsafe){note("unsafe library contents rejected on",label_target,errno);return 0;}
    }
    if(hfs_ro&&(pioneer||heal||mode!=MOUNT_PRE)) {
        char identity[80];
        if(VOLUME_IDENTITY(src,identity)||prepare_view(label_target,identity,deadline)){note("HFS view unavailable on",label_target,errno);return 0;}
    }
    release_post(); // the visible tree is final; a bridge or guardian run below must not hold edisksd's monitor
    int done=0;
    if(pioneer&&mode!=MOUNT_POST){const char *rb=RB_BRIDGE_PATH();char *const a[]={(char *)rb,"--mount-path",(char *)mount_path,NULL};if(!run(a,deadline))done=CONVERTED;else{int is_sd=(label_target&&strstr(label_target,"mmcblk"))||(src&&strstr(src,"mmcblk"));if(errno==ETIMEDOUT){done=TIMED_OUT;set_preparing_error(label_target,src,is_sd?"SD card conversion timed out":"USB conversion timed out");}else{set_preparing_error(label_target,src,is_sd?"SD card conversion failed":"USB conversion failed");}note("bridge failed on",label_target,errno);}}
    if(heal) {
		char timeout[32];
		int64_t left=deadline-now_ms();
		if(guardian_window_available(left)) {
			snprintf(timeout,sizeof timeout,"%lldms",(long long)guardian_soft_budget_ms(left));
			char *const a[]={(char *)GUARDIAN_PATH,"--heal-mount",(char *)mount_path,"--data-only","--timeout",timeout,NULL};
			if(run(a,deadline)){if(errno==ETIMEDOUT)done=TIMED_OUT;note("guardian failed on",label_target,errno);}
		}
        done|=HEALED;
    }
    if(hfs_ro&&mode==MOUNT_PRE) { // the persistent library stays; the private view goes
        struct owned_view *v=load_view(label_target);if(v){umount2(label_target,MNT_DETACH|UMOUNT_NOFOLLOW);cleanup_view(v);free(v);}
    }
    return pioneer?done|2:done;
}
static int after_mount(struct libmnt_context *ctx,int mode,int64_t deadline) {
    const char *target=mnt_context_get_target(ctx);
    return after_mount_at(target,target,mnt_context_get_source(ctx),mode,deadline);
}
// A library too large to convert inside the budget would hold every later
// insert of the same export for the whole budget and still end unconverted.
// Remember it until reboot, keyed by the size of the volume and the export files.
// Not by f_fsid: on FAT, exFAT and HFS+ that is the device number, which follows
// the port and the insert order, so the same stick in the other port waited the
// whole budget again (BH2-A1-2).
static int slow_marker(const char *target,char out[PATH_MAX]) {
    struct statvfs v;if(statvfs(target,&v))return -1;
    uint64_t h=UINT64_C(14695981039346656037);
    h=hash_bytes(h,&v.f_blocks,sizeof v.f_blocks);
    // Every export file rb-bridge may convert from (pkg/bridge).
    const char *files[]={"PIONEER/rekordbox/export.pdb",".PIONEER/rekordbox/export.pdb","PIONEER/rekordbox/exportLibrary.db",".PIONEER/rekordbox/exportLibrary.db",
                         "PIONEER/rekordbox/exportOneLibrary.db",".PIONEER/rekordbox/exportOneLibrary.db"};
    for(unsigned i=0;i<sizeof files/sizeof *files;i++) {
        char p[PATH_MAX];struct stat st;
        if(path_join(p,sizeof p,target,files[i])||lstat(p,&st))continue;
        int64_t size=(int64_t)st.st_size,sec=(int64_t)st.st_mtim.tv_sec,ns=st.st_mtim.tv_nsec;
        h=hash_bytes(h,&i,sizeof i);h=hash_bytes(h,&size,sizeof size);h=hash_bytes(h,&sec,sizeof sec);h=hash_bytes(h,&ns,sizeof ns);
    }
    if(snprintf(out,PATH_MAX,"%s/slow-%016llx",RUN_ROOT,(unsigned long long)h)>=PATH_MAX){errno=ENAMETOOLONG;return -1;}
    return 0;
}
// The same for a database too large to heal inside the budget, keyed by the
// size of the volume and of m.db. Not by f_fsid, the device number on FAT,
// exFAT and HFS+: the same stick in the other port waited again (BH2-A1-2).
static int heal_marker(const char *target,char out[PATH_MAX]) {
    struct statvfs v;if(statvfs(target,&v))return -1;
    uint64_t h=UINT64_C(14695981039346656037);
    h=hash_bytes(h,&v.f_blocks,sizeof v.f_blocks);
    // Sizes only: Engine and a killed heal both touch m.db's time on every use.
    char p[PATH_MAX];struct stat st;
    if(!path_join(p,sizeof p,target,"Engine Library/Database2/m.db")&&!lstat(p,&st)){int64_t size=(int64_t)st.st_size;h=hash_bytes(h,&size,sizeof size);}
    if(snprintf(out,PATH_MAX,"%s/slowheal-%016llx",RUN_ROOT,(unsigned long long)h)>=PATH_MAX){errno=ENAMETOOLONG;return -1;}
    return 0;
}
// Time kept for the visible mount, the HFS view and the reply to Engine.
#ifndef POST_RESERVE_MS
#define POST_RESERVE_MS INT64_C(3000)
#endif
// A mount that waited longer than this for view_lock (another stick was being
// converted) did not get its whole conversion window.
#define LOCK_WAIT_SLACK_MS INT64_C(1000)
// In a forked child: the volume mounted in a mount namespace of its own. Nothing mounted
// here reaches edisksd's (and Engine's) namespace.
static int private_mount(struct libmnt_context *ctx,int (*real)(struct libmnt_context *),const char *target) {
    private_ns=1;
    if(unshare(CLONE_NEWNS)||mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL))return -1;
    // libmount writes the mount (edisksd passes uhelper=edisksd) into
    // /run/mount/utab, a file the mount namespace does not separate, and this
    // mount ends without libmount. Its record goes to a utab of its own.
    mkdir(UTAB_DIR,0755);
    if(mount("edisksd-hook",UTAB_DIR,"tmpfs",MS_NODEV|MS_NOSUID|MS_NOEXEC,"mode=0755,size=64k"))note("no private utab for the pre-mount of",target,errno);
    return real(ctx)||mnt_context_get_status(ctx)!=1?-1:0;
}
// Mount the volume in a private namespace and convert (or heal) it there.
// Returns 1 when the stick held a rekordbox export and rb-bridge finished, 2
// when it held one but the conversion did not finish (or is known not to) or no
// time was left to look, 3 when it held an Engine database and the heal ran (or
// is known to be too slow), 0 when it held neither or this failed. whole_window: the budget was not cut
// short by the wait for view_lock, so running out of it says the library is too
// large; only then is the stick remembered as such.
static int run_pre_vis_at(const char *mount_path,const char *label_target,const char *src,int64_t deadline,int whole_window) {
    int pioneer=has_export(mount_path);
    if(!pioneer&&!has_engine_db(mount_path))return 0;
    char marker[PATH_MAX];int known=0,r=0;
    if(!pioneer) {
        if(!secure_runtime()&&!heal_marker(mount_path,marker))known=!access(marker,F_OK);
        if(known)note("Engine library too large to heal at mount, skipped on",label_target,0);
        else r=after_mount_at(mount_path,label_target,src,MOUNT_PRE,deadline);
        if(r&TIMED_OUT&&whole_window&&!heal_marker(mount_path,marker)){int fd=open(marker,O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC,0600);if(fd>=0)close(fd);}
        return 3; // whatever happened, the heal is not repeated after the visible mount
    }
    if(!secure_runtime()&&!slow_marker(mount_path,marker))known=!access(marker,F_OK);
    if(known){note("rekordbox library too large to convert at mount, skipped on",label_target,0);r=2;}
    else r=after_mount_at(mount_path,label_target,src,MOUNT_PRE,deadline);
    if(r&TIMED_OUT&&whole_window&&!slow_marker(mount_path,marker)){int fd=open(marker,O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC,0600);if(fd>=0)close(fd);}
    return r&CONVERTED?1:r?2:0;
}
static int premount(struct libmnt_context *ctx,int (*real)(struct libmnt_context *),int64_t deadline,int whole_window) {
    const char *target=mnt_context_get_target(ctx);
    if(!volume(target)||safe_path(target,0))return 0;
    // Two sticks inserted together: the second one's wait used its window up. It is
    // mounted as it is and converted on its next insert, never after Engine sees it.
    if(budget(deadline)){note("no time left to convert before the mount on",target,0);return 2;}
    int up[2]={-1,-1};if(pipe2(up,O_CLOEXEC|O_NONBLOCK))up[0]=up[1]=-1;
    pid_t pid=fork();if(pid<0){close(up[0]);close(up[1]);return 0;}
    if(!pid) {
        seeded_as[0]=0;
        if(private_mount(ctx,real,target))_exit(0);
        // A stick without a rekordbox export costs one look at its root directory:
        // no statvfs (a FAT32 free-space scan) and no library walk, both of which
        // the visible mount would repeat on a cold cache inside the same budget.
        int code=run_pre_vis_at(target,target,mnt_context_get_source(ctx),deadline,whole_window);
        if(*seeded_as&&write(up[1],seeded_as,strlen(seeded_as))<0){} // best effort: without it the visible mount seeds again
        umount2(target,UMOUNT_NOFOLLOW); // the namespace ending would also drop it
        _exit(code);
    }
    close(up[1]);int result;
    for(;;) {
        int status;pid_t r=waitpid(pid,&status,WNOHANG);
        if(r==pid) {
            result=WIFEXITED(status)?WEXITSTATUS(status):0;
            ssize_t n=read(up[0],seeded_hint,sizeof seeded_hint-1);seeded_hint[n>0?n:0]=0;
            break;
        }
        if(r<0&&errno!=EINTR){result=0;break;}
        if(now_ms()>=deadline+1000){kill(pid,SIGKILL);while(waitpid(pid,&status,0)<0&&errno==EINTR){}drop_orphans();if(access(PREPARE_STATE_PATH,F_OK)==0){const char *tgt=mnt_context_get_target(ctx),*src=mnt_context_get_source(ctx);int is_sd=(tgt&&strstr(tgt,"mmcblk"))||(src&&strstr(src,"mmcblk"));set_preparing_error(tgt,src,is_sd?"SD card conversion timed out":"USB conversion timed out");}result=2;break;}
        usleep(20000);
    }
    close(up[0]);return result;
}
// Free space left on the stick after a carry-over.
#ifndef CARRY_SLACK
#define CARRY_SLACK (UINT64_C(16)<<20)
#endif
#define CARRY_STAGE ".enginex-carry-"
// The library Engine built in /data for a stick that was read-only goes onto the stick, whole or not
// at all: copied into a stage beside where it goes, synced, checked by engine-guardian (m.db's
// integrity, and its tracks' paths, which Engine keeps relative to the library folder, resolve on
// this stick), then renamed to "Engine Library". An Engine Library already on the stick is never
// overwritten: it wins, as it does in seed_library. On any failure the stage goes, the stick stays
// read-only and the library stays in /data. The /data copy also stays after a carry-over, for a
// rollback, until the cache's own rules reclaim it.
static int carry_library(const char *mount_path,const char *label_target,const char *from,int64_t deadline) {
    char stage[PATH_MAX]="";const char *why="cannot read the library on /data";struct stat st;uint64_t bytes=0;
    int root=open(mount_path,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC),src=-1,dst=-1,rc=-1;
    if(root<0){note("library carry-over: cannot open",label_target,errno);return -1;}
    if(!fstatat(root,"Engine Library",&st,AT_SYMLINK_NOFOLLOW)){close(root);note("stick has an Engine Library of its own, library on /data left there for",label_target,0);return 0;}
    if(errno!=ENOENT){why="cannot look for Engine Library";goto done;}
    // A stage left by an attempt killed at its deadline (or by a power cut) goes first.
    int dup=fcntl(root,F_DUPFD_CLOEXEC,0);DIR *d=dup<0?NULL:fdopendir(dup);struct dirent *e;
    if(!d&&dup>=0)close(dup);
    while(d&&(e=readdir(d)))if(!strncmp(e->d_name,CARRY_STAGE,strlen(CARRY_STAGE)))remove_tree(root,e->d_name);
    if(d)closedir(d);
    if((src=open(from,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC))<0||copy_tree(src,-1,deadline,0,&bytes))goto done;
    struct statvfs v;
    if(fstatvfs(root,&v)){why="cannot read the stick's free space";goto done;}
    if((uint64_t)v.f_bavail*(uint64_t)v.f_frsize<bytes+CARRY_SLACK){errno=ENOSPC;why="no room on the stick";goto done;}
    why="cannot copy the library onto the stick";
    if(path_join(stage,sizeof stage,mount_path,CARRY_STAGE "XXXXXX")||!mkdtemp(stage)){stage[0]=0;goto done;}
    if((dst=open(stage,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC))<0||copy_tree(src,dst,deadline,0,NULL)||fsync(dst))goto done;
    char *const check[]={(char *)GUARDIAN_PATH,"--check-copy",stage,NULL};
    why="copy refused by engine-guardian --check-copy";
    if(run(check,deadline))goto done;
    why="cannot rename the copy to Engine Library";
    if(renameat2(root,strrchr(stage,'/')+1,root,"Engine Library",RENAME_NOREPLACE)||fsync(root))goto done;
    stage[0]=0;rc=0;note("library Engine built on /data carried over to",label_target,0);
 done: {
        int saved=errno;
        if(rc)note(why,label_target,saved);
        if(*stage){remove_tree(root,strrchr(stage,'/')+1);fsync(root);}
        if(dst>=0)close(dst);
        if(src>=0)close(src);
        close(root);errno=saved;return rc;
    }
}
// carry_library in a private rw mount before any other: 0 when the stick may be mounted rw, -1
// when it stays read-only with its library in /data.
static int carry_premount(struct libmnt_context *ctx,int (*real)(struct libmnt_context *),int64_t deadline) {
    const char *target=mnt_context_get_target(ctx);
    pid_t pid=fork();if(pid<0)return -1;
    if(!pid) {
        struct statvfs v;
        if(private_mount(ctx,real,target)||statvfs(target,&v)||(v.f_flag&ST_RDONLY))_exit(1);
        int rc=carry_library(target,target,carry_from,deadline),err=errno;
        umount2(target,UMOUNT_NOFOLLOW);
        _exit(!rc?0:err==ETIMEDOUT?2:1);
    }
    int result=1;
    for(;;) {
        int status;pid_t r=waitpid(pid,&status,WNOHANG);
        if(r==pid){result=WIFEXITED(status)?WEXITSTATUS(status):1;break;}
        if(r<0&&errno!=EINTR)break;
        if(now_ms()>=deadline+1000){kill(pid,SIGKILL);while(waitpid(pid,&status,0)<0&&errno==EINTR){}result=2;break;}
        usleep(20000);
    }
    char marker[PATH_MAX];
    if(result==2&&!secure_runtime()&&!carry_marker(carry_from,marker)){int fd=open(marker,O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC,0600);if(fd>=0)close(fd);}
    return result?-1:0;
}

static uint64_t hfs_gen2_pid_starttime(pid_t pid) {
    if(pid<=0)return 0;
    char path[64],buf[1024];
    snprintf(path,sizeof path,"/proc/%d/stat",(int)pid);
    int fd=open(path,O_RDONLY|O_CLOEXEC);if(fd<0)return 0;
    ssize_t n=read(fd,buf,sizeof buf-1);close(fd);
    if(n<=0)return 0;
    buf[n]=0;
    char *rp=strrchr(buf,')');
    if(!rp||(rp[1]==' '&&rp[2]=='Z'))return 0;
    char *p=rp+1;int field=2;
    while(*p&&field<22) {
        while(*p==' ')p++;
        if(!*p)break;
        if(++field==22)return strtoull(p,NULL,10);
        while(*p&&*p!=' ')p++;
    }
    return 0;
}
static int hfs_gen2_rec_path(const char *target,char *out,size_t outsz) {
    uint64_t h=hash_bytes(UINT64_C(14695981039346656037),target,strlen(target));
    if(snprintf(out,outsz,"%s/lkl-%016llx.rec",RUN_ROOT,(unsigned long long)h)>=(int)outsz){errno=ENAMETOOLONG;return -1;}
    return 0;
}
static void hfs_gen2_slot_put(const struct hfs_gen2_mount *rec) {
    pthread_mutex_lock(&gen2_lock);
    int empty=-1;
    for(unsigned i=0;i<MAX_GEN2_MOUNTS;i++) {
        if(gen2_slots[i].magic==HFS_GEN2_MAGIC&&!strcmp(gen2_slots[i].target,rec->target)){gen2_slots[i]=*rec;pthread_mutex_unlock(&gen2_lock);return;}
        if(empty<0&&gen2_slots[i].magic!=HFS_GEN2_MAGIC)empty=(int)i;
    }
    if(empty>=0)gen2_slots[empty]=*rec;
    pthread_mutex_unlock(&gen2_lock);
}
static void hfs_gen2_slot_del(const char *target) {
    pthread_mutex_lock(&gen2_lock);
    for(unsigned i=0;i<MAX_GEN2_MOUNTS;i++) {
        if(gen2_slots[i].magic==HFS_GEN2_MAGIC&&!strcmp(gen2_slots[i].target,target))memset(&gen2_slots[i],0,sizeof gen2_slots[i]);
    }
    pthread_mutex_unlock(&gen2_lock);
}
static int hfs_gen2_read_rec_file(const char *path,struct hfs_gen2_mount *rec) {
    struct stat st;if(lstat(path,&st)||!S_ISREG(st.st_mode)||st.st_size!=(off_t)sizeof *rec)return -1;
    int fd=open(path,O_RDONLY|O_NOFOLLOW|O_CLOEXEC);if(fd<0)return -1;
    size_t got=0;
    while(got<sizeof *rec){ssize_t n=read(fd,(char *)rec+got,sizeof *rec-got);if(n<0&&errno==EINTR)continue;if(n<=0){close(fd);return -1;}got+=(size_t)n;}
    close(fd);
    if(rec->magic!=HFS_GEN2_MAGIC||!memchr(rec->target,0,sizeof rec->target)||!memchr(rec->source,0,sizeof rec->source))return -1;
    return 0;
}
static unsigned hfs_gen2_load_records(struct hfs_gen2_mount *out,unsigned max) {
    unsigned n=0;
    if(!secure_runtime()) {
        DIR *d=opendir(RUN_ROOT);
        if(d) {
            struct dirent *e;
            while(n<max&&(e=readdir(d))) {
                if(strncmp(e->d_name,"lkl-",4))continue;
                size_t nl=strlen(e->d_name);
                if(nl<8||strcmp(e->d_name+nl-4,".rec"))continue;
                char p[PATH_MAX];struct hfs_gen2_mount r;
                if(!path_join(p,sizeof p,RUN_ROOT,e->d_name)&&!hfs_gen2_read_rec_file(p,&r))out[n++]=r;
            }
            closedir(d);
        }
    }
    pthread_mutex_lock(&gen2_lock);
    for(unsigned i=0;i<MAX_GEN2_MOUNTS&&n<max;i++) {
        if(gen2_slots[i].magic!=HFS_GEN2_MAGIC)continue;
        int dup=0;
        for(unsigned j=0;j<n;j++)if(!strcmp(out[j].target,gen2_slots[i].target)){dup=1;break;}
        if(!dup)out[n++]=gen2_slots[i];
    }
    pthread_mutex_unlock(&gen2_lock);
    return n;
}
static int hfs_gen2_save_record(const struct hfs_gen2_mount *rec) {
    hfs_gen2_slot_put(rec);
    if(secure_runtime())return 0;
    char path[PATH_MAX],tmp[PATH_MAX];
    if(hfs_gen2_rec_path(rec->target,path,sizeof path))return -1;
    if(snprintf(tmp,sizeof tmp,"%s.tmp.%ld",path,(long)getpid())>=(int)sizeof tmp){errno=ENAMETOOLONG;return -1;}
    int fd=open(tmp,O_WRONLY|O_CREAT|O_TRUNC|O_NOFOLLOW|O_CLOEXEC,0600);if(fd<0)return -1;
    int bad=write_all(fd,rec,sizeof *rec)||close(fd);
    if(bad||rename(tmp,path)){unlink(tmp);return -1;}
    return 0;
}
static int hfs_gen2_find_record(const char *target,struct hfs_gen2_mount *rec) {
    if(!target)return -1;
    pthread_mutex_lock(&gen2_lock);
    for(unsigned i=0;i<MAX_GEN2_MOUNTS;i++) {
        if(gen2_slots[i].magic==HFS_GEN2_MAGIC&&!strcmp(gen2_slots[i].target,target)){*rec=gen2_slots[i];pthread_mutex_unlock(&gen2_lock);return 0;}
    }
    pthread_mutex_unlock(&gen2_lock);
    if(secure_runtime())return -1;
    char path[PATH_MAX];
    if(hfs_gen2_rec_path(target,path,sizeof path)||hfs_gen2_read_rec_file(path,rec))return -1;
    if(strcmp(rec->target,target))return -1;
    hfs_gen2_slot_put(rec);
    return 0;
}
static void hfs_gen2_remove_record(const char *target) {
    hfs_gen2_slot_del(target);
    char path[PATH_MAX];
    if(!hfs_gen2_rec_path(target,path,sizeof path))unlink(path);
}
static int hfs_gen2_append_opt(char *out,size_t outsz,size_t *used,const char *tok) {
    size_t tl=strlen(tok);
    if(*used+tl+(*used?1:0)>=outsz){errno=ENAMETOOLONG;return -1;}
    if(*used)out[(*used)++]=(char)',';
    memcpy(out+*used,tok,tl+1);*used+=tl;
    return 0;
}
static int hfs_gen2_build_opts(char *out,size_t outsz,const char *source,const char *orig_opts,unsigned long orig_mflags,int is_journaled) {
    size_t used=0;out[0]=0;
    char head[64];
    snprintf(head,sizeof head,"type=hfsplus,mb=%d",LKLFUSE_MB);
    if(hfs_gen2_append_opt(out,outsz,&used,head))return -1;
    if(is_journaled&&hfs_gen2_append_opt(out,outsz,&used,"opts=force"))return -1;
    char fsname[PATH_MAX*2+16];size_t fp=0;
    int hdr=snprintf(fsname,sizeof fsname,"fsname=");
    if(hdr<0||(size_t)hdr>=sizeof fsname)return -1;
    fp=(size_t)hdr;
    for(const char *s=source;*s;s++) {
        if(*s==','||*s=='\\'){if(fp+2>=sizeof fsname){errno=ENAMETOOLONG;return -1;}fsname[fp++]='\\';}
        else if(fp+1>=sizeof fsname){errno=ENAMETOOLONG;return -1;}
        fsname[fp++]=*s;
    }
    fsname[fp]=0;
    if(hfs_gen2_append_opt(out,outsz,&used,fsname))return -1;
    if(hfs_gen2_append_opt(out,outsz,&used,"subtype=hfsplus,allow_other,default_permissions"))return -1;
    static const struct { const char *name; unsigned long flag; } pass_opts[]={
        {"nosuid",MS_NOSUID},{"nodev",MS_NODEV},{"noexec",MS_NOEXEC},
        {"relatime",MS_RELATIME},{"noatime",MS_NOATIME},{"nodiratime",MS_NODIRATIME},
        {"sync",MS_SYNCHRONOUS},{"dirsync",MS_DIRSYNC},
    };
    for(unsigned i=0;i<sizeof pass_opts/sizeof *pass_opts;i++) {
        if((has_opt_token(orig_opts,pass_opts[i].name)||(orig_mflags&pass_opts[i].flag))&&
           hfs_gen2_append_opt(out,outsz,&used,pass_opts[i].name))return -1;
    }
    return 0;
}
static void hfs_gen2_cap_rlimit(int res,uint64_t bytes) {
    struct rlimit rl;
    if(!getrlimit(res,&rl)) {
        rlim_t target=(rlim_t)bytes;
        if(rl.rlim_max==RLIM_INFINITY||target<rl.rlim_max)rl.rlim_max=target;
        if(target<rl.rlim_cur||rl.rlim_cur==RLIM_INFINITY)rl.rlim_cur=rl.rlim_max;
        (void)setrlimit(res,&rl);
    }
}
static void hfs_gen2_apply_limits(void) {
    setenv("MALLOC_ARENA_MAX","1",1);
    hfs_gen2_cap_rlimit(RLIMIT_STACK,LKLFUSE_STACK_LIMIT_BYTES);
#if LKLFUSE_AS_LIMIT_BYTES > 0
    hfs_gen2_cap_rlimit(RLIMIT_AS,LKLFUSE_AS_LIMIT_BYTES);
#endif
    struct sched_param sp={.sched_priority=0};
    (void)sched_setscheduler(0,SCHED_OTHER,&sp);
    (void)setpriority(PRIO_PROCESS,0,LKLFUSE_NICE);
}
static void hfs_gen2_cleanup_anchor(const char *anchor,const char *stage) {
    if(stage&&!safe_path(stage,0)){(void)umount2(stage,MNT_DETACH|UMOUNT_NOFOLLOW);rmdir(stage);}
    if(anchor&&!safe_path(anchor,0)){(void)umount2(anchor,MNT_DETACH|UMOUNT_NOFOLLOW);rmdir(anchor);}
}
static int hfs_gen2_pid_alive(pid_t pid,uint64_t starttime) {
    if(pid<=0)return 0;
    int st=0;pid_t r=waitpid(pid,&st,WNOHANG);
    if(r==pid)return 0;
    if(r==0)return 1;
    if(errno!=ECHILD)return 1;
    if(kill(pid,0))return 0;
    if(starttime) {
        uint64_t cur=hfs_gen2_pid_starttime(pid);
        if(!cur||cur!=starttime)return 0;
    }
    return 1;
}
static int hfs_gen2_wait_exit(pid_t pid,uint64_t starttime,int64_t wait_ms) {
    int64_t t0=now_ms(),dl=(t0>INT64_MAX-wait_ms)?INT64_MAX:(t0+wait_ms);
    while(hfs_gen2_pid_alive(pid,starttime)) {
        if(now_ms()>=dl)return -1;
        usleep(20000);
    }
    return 0;
}
static void hfs_gen2_kill_and_reap(pid_t pid,uint64_t starttime) {
    if(pid<=0||!hfs_gen2_pid_alive(pid,starttime))return;
    (void)kill(pid,SIGKILL);
    (void)hfs_gen2_wait_exit(pid,starttime,1000);
}
static void hfs_gen2_teardown_stage(const char *anchor,const char *stage,pid_t pid) {
    if(stage&&!safe_path(stage,0)) {
        if(!umount2(stage,UMOUNT_NOFOLLOW)) {
            if(pid>0&&hfs_gen2_wait_exit(pid,0,2000)) {
                (void)kill(pid,SIGTERM);
                (void)hfs_gen2_wait_exit(pid,0,500);
            }
        } else if(errno!=EINVAL&&errno!=ENOENT) {
            (void)umount2(stage,MNT_DETACH|UMOUNT_NOFOLLOW);
        }
    }
    if(pid>0)hfs_gen2_kill_and_reap(pid,0);
    hfs_gen2_cleanup_anchor(anchor,stage);
}
static int hfs_gen2_source_pulled(const struct hfs_gen2_mount *rec) {
    struct stat st;
    if(stat(rec->source,&st))return 1;
    if(rec->dev_maj||rec->dev_min) {
        if(!S_ISBLK(st.st_mode)||st.st_rdev!=makedev(rec->dev_maj,rec->dev_min))return 1;
    }
    int fd=open(rec->source,O_RDONLY|O_NONBLOCK|O_CLOEXEC);
    if(fd<0) {
        if(errno==ENOENT||errno==ENXIO||errno==ENODEV||errno==ENOMEDIUM)return 1;
        return 0;
    }
    close(fd);
    return 0;
}
static int hfs_gen2_mount_flow(struct libmnt_context *ctx,const struct hfs_ctx_backup *bk,int64_t deadline,int whole_window) {
    const char *lkl=LKLFUSE_PATH();
    if(!lkl)return -1;
    const char *source=bk->orig_source,*target=bk->orig_target;
    struct stat st;
    if(stat(source,&st)||(!S_ISBLK(st.st_mode)&&!S_ISREG(st.st_mode)))return -1;
    uint32_t dev_maj=S_ISBLK(st.st_mode)?(uint32_t)major(st.st_rdev):0;
    uint32_t dev_min=S_ISBLK(st.st_mode)?(uint32_t)minor(st.st_rdev):0;
    char lkl_opts[1024];
    if(hfs_gen2_build_opts(lkl_opts,sizeof lkl_opts,source,bk->orig_opts,bk->orig_mflags,bk->is_journaled))return -1;
    if(secure_runtime())return -1;
    char anchor[PATH_MAX],stage[PATH_MAX];
    if(snprintf(anchor,sizeof anchor,"%s/lkl-XXXXXX",RUN_ROOT)>=(int)sizeof anchor||!mkdtemp(anchor))return -1;
    if(bind_checked(anchor,anchor,MS_BIND)||mount(NULL,anchor,NULL,MS_PRIVATE|MS_REC,NULL)||
       path_join(stage,sizeof stage,anchor,"mnt")||mkdir(stage,0700)) {
        hfs_gen2_cleanup_anchor(anchor,NULL);return -1;
    }
    pid_t pid=fork();
    if(pid<0){hfs_gen2_cleanup_anchor(anchor,stage);return -1;}
    if(!pid) {
        setpgid(0,0);unsetenv("LD_PRELOAD");
        setenv("PATH","/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",1);
        hfs_gen2_apply_limits();
        int dn=open("/dev/null",O_RDWR|O_CLOEXEC);
        if(dn>=0){dup2(dn,0);dup2(dn,1);dup2(dn,2);if(dn>2)close(dn);}
        char *const argv[]={(char *)lkl,(char *)source,(char *)stage,"-f","-s","-o",lkl_opts,NULL};
        execv(argv[0],argv);_exit(127);
    }
    setpgid(pid,pid);
    int64_t m_now=now_ms(),m_dl=(m_now>INT64_MAX-LKLFUSE_MOUNT_TIMEOUT_MS)?INT64_MAX:(m_now+LKLFUSE_MOUNT_TIMEOUT_MS);
    if(deadline-POST_RESERVE_MS<m_dl&&deadline-POST_RESERVE_MS>m_now)m_dl=deadline-POST_RESERVE_MS;
    uint32_t fuse_maj=0,fuse_min=0;int mounted=0;char majmin[32]="";
    for(;;) {
        if(!mountinfo_lookup(stage,NULL,0,majmin,sizeof majmin,NULL,0)) {
            unsigned a=0,b=0;
            if(sscanf(majmin,"%u:%u",&a,&b)==2){fuse_maj=a;fuse_min=b;}
            mounted=1;break;
        }
        int mst=0;pid_t r=waitpid(pid,&mst,WNOHANG);
        if(r==pid){pid=-1;break;}
        if(now_ms()>=m_dl)break;
        usleep(20000);
    }
    if(!mounted) {
        if(pid>0)hfs_gen2_kill_and_reap(pid,0);
        hfs_gen2_cleanup_anchor(anchor,stage);
        return -1;
    }
    if(*carry_from&&carry_library(stage,target,carry_from,deadline-POST_RESERVE_MS)) {
        int err=errno;char marker[PATH_MAX];
        if(err==ETIMEDOUT&&whole_window&&!secure_runtime()&&!carry_marker(carry_from,marker)) {
            int fd=open(marker,O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC,0600);
            if(fd>=0)close(fd);
        }
        note("HFS+ library carry-over failed, keeping read-only view on",target,0);
        hfs_gen2_teardown_stage(anchor,stage,pid);
        errno=err;return -1;
    }
    if(!budget(deadline-POST_RESERVE_MS))(void)run_pre_vis_at(stage,target,source,deadline-POST_RESERVE_MS,whole_window);
    else note("no time left to convert before the mount on",target,0);
    seeded_as[0]=0;
    pthread_mutex_lock(&post_lock);holding_post=1;
    if(mount(stage,target,NULL,MS_MOVE,NULL)) {
        int saved=errno;
        release_post();
        hfs_gen2_teardown_stage(anchor,stage,pid);
        errno=saved;return -1;
    }
    struct hfs_gen2_mount rec={0};
    rec.magic=HFS_GEN2_MAGIC;
    rec.pid=(int32_t)pid;
    rec.pid_starttime=hfs_gen2_pid_starttime(pid);
    rec.dev_maj=dev_maj;rec.dev_min=dev_min;
    rec.fuse_maj=fuse_maj;rec.fuse_min=fuse_min;
    rec.is_journaled=bk->is_journaled;
    snprintf(rec.target,sizeof rec.target,"%s",target);
    snprintf(rec.source,sizeof rec.source,"%s",source);
    (void)hfs_gen2_save_record(&rec);
    rmdir(stage);
    (void)umount2(anchor,UMOUNT_NOFOLLOW);
    rmdir(anchor);
    mnt_context_set_syscall_status(ctx,0);
    return 0;
}
static int hfs_gen2_unmount(struct libmnt_context *ctx,const struct hfs_gen2_mount *rec,int (*real)(struct libmnt_context *)) {
    int lazy=0;
    pthread_mutex_lock(&ctx_lock);
    struct hfs_ctx_state *s=ctx_slot(ctx,0);
    if(s&&s->has_lazy)lazy=s->lazy;
    pthread_mutex_unlock(&ctx_lock);
    int pulled=lazy||hfs_gen2_source_pulled(rec)||!hfs_gen2_pid_alive((pid_t)rec->pid,rec->pid_starttime);
    if(pulled) {
        int (*set_lazy)(struct libmnt_context *,int)=REAL_MNT_CONTEXT_ENABLE_LAZY();
        if(set_lazy)set_lazy(ctx,1);
        (void)real(ctx);
        if(umount2(rec->target,MNT_DETACH|UMOUNT_NOFOLLOW)&&errno!=EINVAL&&errno!=ENOENT) {
            note("Gen-2 lazy detach failed on",rec->target,errno);
        }
        hfs_gen2_kill_and_reap((pid_t)rec->pid,rec->pid_starttime);
        hfs_gen2_remove_record(rec->target);
        mnt_context_set_syscall_status(ctx,0);
        return 0;
    }
    int rc=real(ctx),saved=errno;
    if(rc||mnt_context_get_status(ctx)!=1) {
        int e=saved?saved:EBUSY;
        if(!rc)mnt_context_set_syscall_status(ctx,-e);
        errno=e;return rc?rc:-e;
    }
    if(hfs_gen2_wait_exit((pid_t)rec->pid,rec->pid_starttime,LKLFUSE_WAIT_EXIT_MS)) {
        note("lklfuse did not exit within wait budget, terminating on",rec->target,ETIMEDOUT);
        if(hfs_gen2_pid_alive((pid_t)rec->pid,rec->pid_starttime)) {
            (void)kill((pid_t)rec->pid,SIGTERM);
            if(hfs_gen2_wait_exit((pid_t)rec->pid,rec->pid_starttime,1000)) {
                hfs_gen2_kill_and_reap((pid_t)rec->pid,rec->pid_starttime);
            }
        }
    }
    hfs_gen2_remove_record(rec->target);
    return 0;
}
static int hooked_mount(struct libmnt_context *ctx,int (*real)(struct libmnt_context *)) {
    int64_t start=now_ms(),deadline=deadline_from(start);
    pthread_mutex_lock(&view_lock);
    seeded_hint[0]=0;carry_from[0]=0;
    in_hook=1;
    struct hfs_ctx_backup hfs_bk={0};
    int whole_window=now_ms()-start<LOCK_WAIT_SLACK_MS;
    hfs_rw_approved=hfs_prepare_mount(ctx,&hfs_bk,deadline-POST_RESERVE_MS);
    if(hfs_rw_approved==HFS_RW_LKLFUSE) {
        if(!hfs_gen2_mount_flow(ctx,&hfs_bk,deadline,whole_window)) {
            clear_eject(EJECTED,mnt_context_get_target(ctx));clear_eject(EJECTING,mnt_context_get_target(ctx));
            hfs_rw_approved=HFS_RW_NONE;
            release_post();clear_preparing();in_hook=0;
            seeded_hint[0]=0;carry_from[0]=0;
            pthread_mutex_unlock(&view_lock);return 0;
        }
        note("Gen-2 lklfuse mount failed, falling back to ro on",mnt_context_get_target(ctx),errno);
        hfs_rw_approved=HFS_RW_NONE;
        hfs_restore_ro(ctx,&hfs_bk);
        carry_from[0]=0;
    }
    if(hfs_bk.tried_rw&&*carry_from&&carry_premount(ctx,real,deadline-POST_RESERVE_MS)) {
        note("HFS+ library carry-over failed, keeping read-only view on",mnt_context_get_target(ctx),0);
        hfs_rw_approved=HFS_RW_NONE;hfs_restore_ro(ctx,&hfs_bk);hfs_bk.tried_rw=0;
    }
    int pre=premount(ctx,real,deadline-POST_RESERVE_MS,now_ms()-start<LOCK_WAIT_SLACK_MS);
    pthread_mutex_lock(&post_lock);holding_post=1; // after_mount releases it once the visible tree is final
    int rc=real(ctx),saved=errno;
    if((rc||mnt_context_get_status(ctx)!=1)&&hfs_bk.tried_rw) {
        note("HFS+ rw mount failed, falling back to ro on",mnt_context_get_target(ctx),saved);
        hfs_rw_approved=HFS_RW_NONE;
        hfs_restore_ro(ctx,&hfs_bk);
        rc=real(ctx);saved=errno;
    }
    hfs_rw_approved=HFS_RW_NONE;
    if(!rc&&mnt_context_get_status(ctx)==1){clear_eject(EJECTED,mnt_context_get_target(ctx));clear_eject(EJECTING,mnt_context_get_target(ctx));after_mount(ctx,pre?MOUNT_POST:MOUNT_FULL,deadline);errno=saved;}
    release_post();clear_preparing();in_hook=0;
    seeded_hint[0]=0;
    pthread_mutex_unlock(&view_lock);return rc;
}
#ifndef REAL_MNT_CONTEXT_MOUNT
#define REAL_MNT_CONTEXT_MOUNT() ((int (*)(struct libmnt_context *))dlsym(RTLD_NEXT,"mnt_context_mount"))
#endif
int mnt_context_mount(struct libmnt_context *ctx) {
    int (*real)(struct libmnt_context *)=REAL_MNT_CONTEXT_MOUNT();if(!real){errno=ENOSYS;return -ENOSYS;}
    return hooked_mount(ctx,real);
}

// A view entry: a name in the stick's root, or one inside its Engine Library
// (bind_native) whose parent "Engine Library" is listed after it.
static int child_name_ok(const char *name) {
    const char *leaf=strncmp(name,NESTED,strlen(NESTED))?name:name+strlen(NESTED);
    return *leaf&&!strchr(leaf,'/')&&strcmp(leaf,".")&&strcmp(leaf,"..");
}
// A nested entry comes back with its parent when that was removed too.
static int restored_with_parent(struct owned_view *v,unsigned i,unsigned removed) {
    if(strncmp(v->child[i].name,NESTED,strlen(NESTED)))return 0;
    for(unsigned j=i+1;j<removed;j++)if(!strcmp(v->child[j].name,"Engine Library"))return 1;
    return 0;
}
// The stick under a view's base mount is gone: mountinfo names its device, /sys no longer does.
static int base_device_gone(uint64_t base_id) {
    FILE *f=fopen(PROC_MOUNTINFO_PATH,"re");if(!f)return 0;
    char *line=NULL;size_t cap=0;int gone=0;
    while(getline(&line,&cap,f)>0) {
        unsigned long long id;unsigned maj,min;char sysblk[PATH_MAX];struct stat st;
        if(sscanf(line,"%llu %*u %u:%u",&id,&maj,&min)!=3||id!=base_id)continue;
        gone=maj>0&&snprintf(sysblk,sizeof sysblk,"%s/%u:%u",SYS_DEV_BLOCK_DIR,maj,min)<(int)sizeof sysblk&&lstat(sysblk,&st)&&errno==ENOENT;
        break;
    }
    free(line);fclose(f);return gone;
}
// A rescue tree is cloned BEFORE changing the public view. Normal child
// unmounts detect open files; rollback moves the already allocated mounts back.
// detach: the stick goes away (pulled, or a udev remove), so what Engine holds open on it
// cannot keep the view: everything is detached lazily, as edisksd does with the stick itself.
static int unmount_view(struct libmnt_context *ctx,struct owned_view *v,int (*real)(struct libmnt_context *),int detach) {
    char view[PATH_MAX],rescue[PATH_MAX],pub[PATH_MAX],src[PATH_MAX];
    if(path_join(view,sizeof view,v->anchor,"view")||path_join(rescue,sizeof rescue,v->anchor,"rescue")||
       safe_path(v->target,0)||mount_id(v->target)!=v->public_id)return -EINVAL;
    for(unsigned i=0;i<v->count;i++) {
        if(!memchr(v->child[i].name,0,sizeof v->child[i].name)||!child_name_ok(v->child[i].name)||
           path_join(pub,sizeof pub,v->target,v->child[i].name)||mount_id(pub)!=v->child[i].mount_id)return -EINVAL;
    }
    if(bind_checked(view,rescue,MS_BIND|MS_REC))return -errno;
    if(mount(NULL,rescue,NULL,MS_PRIVATE|MS_REC,NULL)){int saved=errno;umount2(rescue,MNT_DETACH|UMOUNT_NOFOLLOW);errno=saved;return -saved;}
    unsigned removed=0;int rc=0,saved=0;
    for(;removed<v->count;removed++) {
        // "Engine Library", last, is the cached library on /data. Engine keeps its database open
        // there across Eject; that holds nothing of the stick, so it is detached (a later failure
        // moves the rescue copy back, #23). On an Eject every entry from the stick is still
        // refused in use. On a removal Engine still holds a loaded track's source, and refusing
        // left the dead mount and its view until a reboot.
        int lazy=detach||!strcmp(v->child[removed].name,"Engine Library")?MNT_DETACH:0;
        if(path_join(pub,sizeof pub,v->target,v->child[removed].name)||umount2(pub,UMOUNT_NOFOLLOW|lazy)) {saved=errno;goto restore_children;}
    }
    if(umount2(v->target,UMOUNT_NOFOLLOW|(detach?MNT_DETACH:0))){saved=errno;goto restore_children;}
    if(mount_id(v->target)!=v->base_id) {saved=EBUSY;goto restore_root;}
    rc=real(ctx);saved=errno;
    if(!rc) {cleanup_view(v);return 0;}
 restore_root:
    if(bind_checked(rescue,v->target,MS_MOVE))note("FATAL: restoring HFS view failed on",v->target,errno);
    else if(update_public_ids(v))note("record update failed on",v->target,errno);
    goto failed;
 restore_children:
    for(unsigned i=0;i<removed;i++) {
        if(restored_with_parent(v,i,removed))continue;
        if(path_join(src,sizeof src,rescue,v->child[i].name)||path_join(pub,sizeof pub,v->target,v->child[i].name)||
           bind_checked(src,pub,MS_MOVE))note("FATAL: restoring HFS child failed on",v->target,errno);
    }
    if(update_public_ids(v))note("record update failed on",v->target,errno);
    umount2(rescue,MNT_DETACH|UMOUNT_NOFOLLOW); // only our private spare tree
 failed: // edisksd reports only what this returns (mnt_context_get_excode): -1 read "Operation not permitted"
    mnt_context_set_syscall_status(ctx,-(saved?saved:EBUSY));errno=saved?saved:EBUSY;return rc?rc:-errno;
}
static int unmount_dispatch(struct libmnt_context *ctx,int (*real)(struct libmnt_context *)) {
    const char *target=mnt_context_get_target(ctx);
    if(!volume(target))return real(ctx);
    char lib[PATH_MAX];
    int target_err=safe_path(target,0)?errno:0;
    if(target_err&&target_err!=ENOTCONN){errno=target_err;return -target_err;}
    if(!target_err) {
        if(path_join(lib,sizeof lib,target,"Engine Library"))return -errno;
        // A symlinked library is refused. Any other error is the stick's, e.g. EIO once it was
        // pulled without Eject: that must not keep the dead mount until a reboot (BH-O55-5).
        if(safe_path(lib,1)&&errno==ELOOP)return -ELOOP;
    }
    stop_import(target);
    int eager=!ctx_wants_lazy(ctx);
    if(eager){mark_eject(EJECTING,target);mark_eject(EJECTED,target);}
    // Only this target's own mount makes its view record, under view_lock. Without one it
    // is the stock umount and does not wait for view_lock, which another stick's
    // conversion holds for up to its whole budget (BH-O55-3).
    struct hfs_gen2_mount gen2_rec;
    if(!hfs_gen2_find_record(target,&gen2_rec)) {
        in_hook=1;
        int rc=hfs_gen2_unmount(ctx,&gen2_rec,real);
        int saved=errno;if(eager&&rc)clear_eject(EJECTED,target);in_hook=0;errno=saved;return rc;
    }
    if(target_err){if(eager)clear_eject(EJECTED,target);errno=target_err;return -target_err;}
    struct owned_view *v=load_view(target);
    if(!v) {
        if(HFS_RW_MOUNTED(target)) {
            if(HFS_DEV_PRESENT(target,mnt_context_get_source(ctx))) {
                int fd=open(target,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
                if(fd>=0){syncfs(fd);close(fd);}
                int (*set_lazy)(struct libmnt_context *,int)=REAL_MNT_CONTEXT_ENABLE_LAZY();
                if(set_lazy)set_lazy(ctx,0);
                pthread_mutex_lock(&ctx_lock);
                struct hfs_ctx_state *s=ctx_slot(ctx,0);
                if(s){s->has_lazy=1;s->lazy=0;}
                pthread_mutex_unlock(&ctx_lock);
            } else {
                note("device gone, keeping lazy unmount for rw HFS+ volume",target,0);
            }
        }
        in_hook=1;int rc=real(ctx),saved=errno;if(eager&&(rc||mnt_context_get_status(ctx)!=1))clear_eject(EJECTED,target);in_hook=0;errno=saved;return rc;
    }
    free(v);
    pthread_mutex_lock(&view_lock);in_hook=1;pthread_mutex_lock(&post_lock);v=load_view(target);
    int rc=v?unmount_view(ctx,v,real,ctx_wants_lazy(ctx)||base_device_gone(v->base_id)):real(ctx);int saved=errno;
    if(eager&&(rc||(!v&&mnt_context_get_status(ctx)!=1)))clear_eject(EJECTED,target);
    free(v);
    pthread_mutex_unlock(&post_lock);in_hook=0;pthread_mutex_unlock(&view_lock);errno=saved;return rc;
}

int mnt_context_umount(struct libmnt_context *ctx) {
    int (*real)(struct libmnt_context *)=dlsym(RTLD_NEXT,"mnt_context_umount");
    if(!real){errno=ENOSYS;return -ENOSYS;}
    return unmount_dispatch(ctx,real);
}
