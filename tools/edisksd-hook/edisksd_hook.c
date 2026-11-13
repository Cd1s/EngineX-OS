// Writable HFS library presentation without using HFS as an overlayfs lower.
// All mount changes occur before Engine attaches. Only recorded mount IDs may
// be removed; a failed eject restores the preallocated private view tree.
#define _GNU_SOURCE
#include <dirent.h>
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
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
#include <sys/stat.h>
#include <sys/statvfs.h>
#include <sys/sysmacros.h>
#include <sys/vfs.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#ifndef LIB_ROOT
#define LIB_ROOT "/data/enginex/hfs-libraries"
#endif
#ifndef LEGACY_LIB_ROOT
#define LEGACY_LIB_ROOT "/data/sc5000-custom/hfs-libraries"
#endif
#ifndef PREPARE_STATE_PATH
#define PREPARE_STATE_PATH "/run/enginex-usb-preparing"
#endif
#ifndef RUN_ROOT
#define RUN_ROOT "/run/sc5000-hfs-views"
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
static void set_preparing(const char *target) {
    if(!volume(target))return;
    int saved=errno;
    char parent[PATH_MAX],tmp[PATH_MAX];
    if(snprintf(parent,sizeof parent,"%s",PREPARE_STATE_PATH)>=(int)sizeof parent)goto out;
    char *slash=strrchr(parent,'/');if(!slash||slash==parent)goto out;
    *slash=0;
    struct stat st;
    if(safe_path(parent,0)||lstat(parent,&st)||!S_ISDIR(st.st_mode))goto out;
    if(path_join(tmp,sizeof tmp,parent,".enginex-usb-prep.XXXXXX"))goto out;
    static const char alphabet[]="abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789";
    for(int attempt=0;attempt<8;attempt++) {
        unsigned char random[6];
        if(getrandom(random,sizeof random,GRND_NONBLOCK)!=(ssize_t)sizeof random)break;
        char *suffix=tmp+strlen(tmp)-6;
        for(unsigned i=0;i<6;i++)suffix[i]=alphabet[random[i]%(sizeof alphabet-1)];
        int fd=open(tmp,O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC,0600);
        if(fd<0){if(errno==EEXIST)continue;break;}
        int ok=write(fd,"1\n",2)==2&&!fchmod(fd,0644);
        if(close(fd))ok=0;
        if(ok)rename(tmp,PREPARE_STATE_PATH);
        unlink(tmp);
        break;
    }
out:
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
// The HFS library cache lives in /data/enginex. Since #103 /data/sc5000-custom is
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
static int add_child(struct owned_view *v,const char *name,const char *source,int isdir) {
    if(v->count==MAX_VIEW_ENTRIES){errno=E2BIG;return -1;}
    char root[PATH_MAX],dest[PATH_MAX];
    if(path_join(root,sizeof root,v->anchor,"view")||path_join(dest,sizeof dest,root,name))return -1;
    if(isdir){if(mkdir(dest,0700))return -1;}
    else {int fd=open(dest,O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC,0600);if(fd<0)return -1;close(fd);}
    // A bind of a shared mount (systemd makes /data MS_SHARED) is that mount's peer:
    // bind_native's binds inside it would propagate into /data. Private first.
    if(bind_checked(source,dest,MS_BIND|MS_REC)||mount(NULL,dest,NULL,MS_PRIVATE|MS_REC,NULL))return -1;
    snprintf(v->child[v->count++].name,NAME_MAX+1,"%s",name);return 0;
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
    if(bind_checked(target,backing,MS_BIND|MS_REC)||mount(NULL,backing,NULL,MS_PRIVATE|MS_REC,NULL)||
       mount("sc5000-hfs-view",view,"tmpfs",MS_NODEV|MS_NOSUID|MS_NOEXEC,"mode=0755,size=16m"))goto done;
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
        if(r==pid){if(WIFEXITED(status)&&!WEXITSTATUS(status))return 0;errno=EIO;return -1;}
        if(r<0&&errno!=EINTR)return -1;
        if(budget(deadline)){kill(-pid,SIGKILL);while(waitpid(pid,&status,0)<0&&errno==EINTR){}errno=ETIMEDOUT;return -1;}usleep(20000);
    }
}
// The mount tests replace these; a bind mount is never HFS+ nor a block device.
#ifndef HFS_READONLY
static int hfs_readonly(const char *target) {
    struct statfs f;struct statvfs v;
    return !statfs(target,&f)&&f.f_type==HFSPLUS_MAGIC&&!statvfs(target,&v)&&(v.f_flag&ST_RDONLY);
}
#define HFS_READONLY hfs_readonly
#endif
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
// A memfd with filename's mountinfo lines minus RUN_ROOT helper mounts, the view
// overlays (tmpfs "sc5000-hfs-view") and everything mounted inside one; the
// device mount underneath stays. -1: nothing to hide, or parse the file as is.
static int hide_views(const char *filename) {
    FILE *f=fopen(filename,"re");if(!f)return -1;
    size_t cap=65536,len=0;char *buf=malloc(cap+1);
    for(;buf;){size_t n=fread(buf+len,1,cap-len,f);len+=n;if(len<cap)break;char *b=realloc(buf,cap*2+1);if(!b){free(buf);buf=NULL;break;}buf=b;cap*=2;}
    int bad=ferror(f);fclose(f);if(!buf||bad){free(buf);return -1;}
    buf[len]=0;
    char run[PATH_MAX];int runlen=snprintf(run,sizeof run,"%s",RUN_ROOT);
    const char *views[64];size_t vlen[64];unsigned nv=0;int hidden=0;
    for(int pass=0;pass<2;pass++) {
        int fd=-1;
        if(pass){if(!hidden){free(buf);return -1;}fd=memfd_create("mountinfo",MFD_CLOEXEC);if(fd<0){free(buf);return -1;}}
        for(char *line=buf;*line;) {
            char *end=strchr(line,'\n');size_t ll=end?(size_t)(end-line+1):strlen(line);
            // fields: id parent maj:min root mountpoint ... " - " fstype source
            const char *mp=line;for(int i=0;i<4&&mp;i++){mp=memchr(mp,' ',ll-(size_t)(mp-line));if(mp)mp++;}
            const char *mpe=mp?memchr(mp,' ',ll-(size_t)(mp-line)):NULL;
            char *sep=NULL;if(mpe){char c=line[ll-1];line[ll-1]=0;sep=strstr(mpe," - ");line[ll-1]=c;}
            int hide=0;
            if(mpe&&sep) {
                size_t ml=(size_t)(mpe-mp);
                int overlay=!strncmp(sep+3,"tmpfs sc5000-hfs-view ",22);
                int helper=ml>(size_t)runlen&&starts_dir(mp,run,(size_t)runlen);
                if(!pass&&overlay&&!helper&&nv<64){views[nv]=mp;vlen[nv++]=ml;}
                hide=helper||overlay;
                for(unsigned i=0;i<nv&&!hide;i++)hide=ml>vlen[i]&&starts_dir(mp,views[i],vlen[i]);
            }
            if(!pass)hidden|=hide;
            else if(!hide) {
                for(size_t off=0;off<ll;){ssize_t w=write(fd,line+off,ll-off);if(w<0&&errno==EINTR)continue;if(w<=0){close(fd);free(buf);return -1;}off+=(size_t)w;}
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
static int after_mount(struct libmnt_context *ctx,int mode,int64_t deadline) {
    const char *target=mnt_context_get_target(ctx);
    if(!volume(target)||safe_path(target,0))return 0;
    int pioneer=has_export(target),heal=!pioneer&&mode==MOUNT_PRE&&has_engine_db(target);char lib[PATH_MAX];
    if(path_join(lib,sizeof lib,target,"Engine Library")||safe_path(lib,1)) {note("unsafe library path rejected on",target,errno);return 0;}
    struct stat existing;
    int hfs_ro=HFS_READONLY(target);
    if(!pioneer&&!heal&&mode==MOUNT_PRE)return 0; // nothing to convert or heal: the visible mount does the rest
    if(hfs_ro||(pioneer&&mode!=MOUNT_POST)||heal)set_preparing(target);
    // The walk guards the tool run on the library below; after the private
    // conversion or heal (MOUNT_POST) none follows, so it is skipped.
    // An HFS+ view is validated by prepare_view.
    if(!hfs_ro&&mode!=MOUNT_POST&&!lstat(lib,&existing)) {
        int fd=open(lib,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
        if(fd<0)return 0;
        int unsafe=copy_tree(fd,-1,deadline,0,NULL);close(fd);
        if(unsafe){note("unsafe library contents rejected on",target,errno);return 0;}
    }
    if(hfs_ro&&(pioneer||heal||mode!=MOUNT_PRE)) {
        char identity[80];
        if(VOLUME_IDENTITY(mnt_context_get_source(ctx),identity)||prepare_view(target,identity,deadline)){note("HFS view unavailable on",target,errno);return 0;}
    }
    release_post(); // the visible tree is final; a bridge or guardian run below must not hold edisksd's monitor
    int done=0;
    if(pioneer&&mode!=MOUNT_POST){const char *rb=RB_BRIDGE_PATH();char *const a[]={(char *)rb,"--mount-path",(char *)target,NULL};if(!run(a,deadline))done=CONVERTED;else{if(errno==ETIMEDOUT)done=TIMED_OUT;note("bridge failed on",target,errno);}}
    if(heal) {
        char *const a[]={(char *)GUARDIAN_PATH,"--heal-mount",(char *)target,"--data-only",NULL};
        if(run(a,deadline)){if(errno==ETIMEDOUT)done=TIMED_OUT;note("guardian failed on",target,errno);}
        done|=HEALED;
    }
    if(hfs_ro&&mode==MOUNT_PRE) { // the persistent library stays; the private view goes
        struct owned_view *v=load_view(target);if(v){umount2(target,MNT_DETACH|UMOUNT_NOFOLLOW);cleanup_view(v);free(v);}
    }
    return pioneer?done|2:done;
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
// Mount the volume in a private namespace and convert (or heal) it there.
// Returns 1 when the stick held a rekordbox export and rb-bridge finished, 2
// when it held one but the conversion did not finish (or is known not to) or no
// time was left to look, 3 when it held an Engine database and the heal ran (or
// is known to be too slow), 0 when it held neither or this failed. whole_window: the budget was not cut
// short by the wait for view_lock, so running out of it says the library is too
// large; only then is the stick remembered as such.
static int premount(struct libmnt_context *ctx,int (*real)(struct libmnt_context *),int64_t deadline,int whole_window) {
    const char *target=mnt_context_get_target(ctx);
    if(!volume(target)||safe_path(target,0))return 0;
    // Two sticks inserted together: the second one's wait used its window up. It is
    // mounted as it is and converted on its next insert, never after Engine sees it.
    if(budget(deadline)){note("no time left to convert before the mount on",target,0);return 2;}
    int up[2]={-1,-1};if(pipe2(up,O_CLOEXEC|O_NONBLOCK))up[0]=up[1]=-1;
    pid_t pid=fork();if(pid<0){close(up[0]);close(up[1]);return 0;}
    if(!pid) {
        seeded_as[0]=0;private_ns=1;
        // Nothing mounted here reaches edisksd's (and Engine's) namespace.
        if(unshare(CLONE_NEWNS)||mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL))_exit(0);
        // libmount writes the mount (edisksd passes uhelper=edisksd) into
        // /run/mount/utab, a file the mount namespace does not separate, and this
        // mount ends without libmount. Its record goes to a utab of its own.
        mkdir(UTAB_DIR,0755);
        if(mount("edisksd-hook",UTAB_DIR,"tmpfs",MS_NODEV|MS_NOSUID|MS_NOEXEC,"mode=0755,size=64k"))note("no private utab for the pre-mount of",target,errno);
        if(real(ctx)||mnt_context_get_status(ctx)!=1)_exit(0);
        // A stick without a rekordbox export costs one look at its root directory:
        // no statvfs (a FAT32 free-space scan) and no library walk, both of which
        // the visible mount would repeat on a cold cache inside the same budget.
        int pioneer=has_export(target);
        if(!pioneer&&!has_engine_db(target)){umount2(target,UMOUNT_NOFOLLOW);_exit(0);}
        char marker[PATH_MAX];int known=0,r=0;
        if(!pioneer) {
            if(!secure_runtime()&&!heal_marker(target,marker))known=!access(marker,F_OK);
            if(known)note("Engine library too large to heal at mount, skipped on",target,0);
            else r=after_mount(ctx,MOUNT_PRE,deadline);
            if(r&TIMED_OUT&&whole_window&&!heal_marker(target,marker)){int fd=open(marker,O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC,0600);if(fd>=0)close(fd);}
            if(*seeded_as&&write(up[1],seeded_as,strlen(seeded_as))<0){}
            umount2(target,UMOUNT_NOFOLLOW);
            _exit(3); // whatever happened, the heal is not repeated after the visible mount
        }
        if(!secure_runtime()&&!slow_marker(target,marker))known=!access(marker,F_OK);
        if(known){note("rekordbox library too large to convert at mount, skipped on",target,0);r=2;}
        else r=after_mount(ctx,MOUNT_PRE,deadline);
        if(r&TIMED_OUT&&whole_window&&!slow_marker(target,marker)){int fd=open(marker,O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC,0600);if(fd>=0)close(fd);}
        if(*seeded_as&&write(up[1],seeded_as,strlen(seeded_as))<0){} // best effort: without it the visible mount seeds again
        umount2(target,UMOUNT_NOFOLLOW); // the namespace ending would also drop it
        _exit(r&CONVERTED?1:r?2:0);
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
        if(now_ms()>=deadline+1000){kill(pid,SIGKILL);while(waitpid(pid,&status,0)<0&&errno==EINTR){}drop_orphans();result=2;break;}
        usleep(20000);
    }
    close(up[0]);return result;
}
static int hooked_mount(struct libmnt_context *ctx,int (*real)(struct libmnt_context *)) {
    int64_t start=now_ms(),deadline=deadline_from(start);
    pthread_mutex_lock(&view_lock);
    seeded_hint[0]=0;
    in_hook=1;
    int pre=premount(ctx,real,deadline-POST_RESERVE_MS,now_ms()-start<LOCK_WAIT_SLACK_MS);
    pthread_mutex_lock(&post_lock);holding_post=1; // after_mount releases it once the visible tree is final
    int rc=real(ctx),saved=errno;
    if(!rc&&mnt_context_get_status(ctx)==1){after_mount(ctx,pre?MOUNT_POST:MOUNT_FULL,deadline);errno=saved;}
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
// A rescue tree is cloned BEFORE changing the public view. Normal child
// unmounts detect open files; rollback moves the already allocated mounts back.
static int unmount_view(struct libmnt_context *ctx,struct owned_view *v,int (*real)(struct libmnt_context *)) {
    char view[PATH_MAX],rescue[PATH_MAX],pub[PATH_MAX],src[PATH_MAX];
    if(path_join(view,sizeof view,v->anchor,"view")||path_join(rescue,sizeof rescue,v->anchor,"rescue")||
       safe_path(v->target,0)||mount_id(v->target)!=v->public_id)return -EINVAL;
    for(unsigned i=0;i<v->count;i++) {
        if(!memchr(v->child[i].name,0,sizeof v->child[i].name)||!child_name_ok(v->child[i].name)||
           path_join(pub,sizeof pub,v->target,v->child[i].name)||mount_id(pub)!=v->child[i].mount_id)return -EINVAL;
    }
    if(bind_checked(view,rescue,MS_BIND|MS_REC))return -errno;
    if(mount(NULL,rescue,NULL,MS_PRIVATE|MS_REC,NULL)){int saved=errno;umount2(rescue,MNT_DETACH|UMOUNT_NOFOLLOW);errno=saved;return -saved;}
    unsigned removed=0;int rc=-1,saved=0;
    for(;removed<v->count;removed++) {
        if(path_join(pub,sizeof pub,v->target,v->child[removed].name)||umount2(pub,UMOUNT_NOFOLLOW)) {saved=errno;goto restore_children;}
    }
    if(umount2(v->target,UMOUNT_NOFOLLOW)){saved=errno;goto restore_children;}
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
 failed:
    mnt_context_set_syscall_status(ctx,-(saved?saved:EBUSY));errno=saved?saved:EBUSY;return rc?rc:-EBUSY;
}
static int unmount_dispatch(struct libmnt_context *ctx,int (*real)(struct libmnt_context *)) {
    const char *target=mnt_context_get_target(ctx);
    if(!volume(target))return real(ctx);
    char lib[PATH_MAX];
    if(safe_path(target,0)||path_join(lib,sizeof lib,target,"Engine Library"))return -errno;
    // A symlinked library is refused. Any other error is the stick's, e.g. EIO once it was
    // pulled without Eject: that must not keep the dead mount until a reboot (BH-O55-5).
    if(safe_path(lib,1)&&errno==ELOOP)return -ELOOP;
    // Only this target's own mount makes its view record, under view_lock. Without one it
    // is the stock umount and does not wait for view_lock, which another stick's
    // conversion holds for up to its whole budget (BH-O55-3).
    struct owned_view *v=load_view(target);
    if(!v){in_hook=1;int rc=real(ctx),saved=errno;in_hook=0;errno=saved;return rc;}
    free(v);
    pthread_mutex_lock(&view_lock);in_hook=1;pthread_mutex_lock(&post_lock);v=load_view(target);
    int rc=v?unmount_view(ctx,v,real):real(ctx);int saved=errno;free(v);
    pthread_mutex_unlock(&post_lock);in_hook=0;pthread_mutex_unlock(&view_lock);errno=saved;return rc;
}

int mnt_context_umount(struct libmnt_context *ctx) {
    int (*real)(struct libmnt_context *)=dlsym(RTLD_NEXT,"mnt_context_umount");
    if(!real){errno=ENOSYS;return -ENOSYS;}
    return unmount_dispatch(ctx,real);
}
