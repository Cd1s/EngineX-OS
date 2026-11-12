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
#include <sys/mount.h>
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
#ifndef RUN_ROOT
#define RUN_ROOT "/run/sc5000-hfs-views"
#endif
#ifndef MEDIA_ROOT
#define MEDIA_ROOT "/media"
#endif
#ifndef UTAB_DIR
#define UTAB_DIR "/run/mount"
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
        if (lstat(copy,&st)) { if (!saved && missing_final && errno==ENOENT) return 0; return -1; }
        if (S_ISLNK(st.st_mode)) {errno=ELOOP;return -1;}
        if (saved && !S_ISDIR(st.st_mode)) {errno=ENOTDIR;return -1;}
        *p=saved; if (!saved) return 0;
    }
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
static int copy_tree(int src,int dst,int64_t deadline,unsigned depth) {
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
            rc=copy_tree(in,out,deadline,depth+1);if(out>=0)close(out);
        } else if(dst>=0) {
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
static int seed_library(const char *target,const char *identity,char library[PATH_MAX],int64_t deadline) {
    char native[PATH_MAX],root[PATH_MAX],stage[PATH_MAX],cache[PATH_MAX];struct stat st;
    if(path_join(native,sizeof native,target,"Engine Library")||safe_path(native,1)||
       lib_root(cache)||make_dirs(cache))return -1;
    struct stat cache_stat;
    if(lstat(cache,&cache_stat)||cache_stat.st_uid!=geteuid()||(cache_stat.st_mode&077)){errno=EACCES;return -1;}
    if(path_join(root,sizeof root,cache,identity)||make_dirs(root))return -1;
    uint64_t signature=0;int src=-1,reuse=0;
    if(seeded_ok(seeded_hint)){char prior[PATH_MAX];reuse=!path_join(prior,sizeof prior,root,seeded_hint)&&!lstat(prior,&st)&&S_ISDIR(st.st_mode);}
    if(reuse){}
    else if(!lstat(native,&st)) {
        if(!S_ISDIR(st.st_mode)){errno=ENOTDIR;return -1;}
        src=open(native,O_RDONLY|O_DIRECTORY|O_CLOEXEC|O_NOFOLLOW);if(src<0)return -1;
        if(copy_tree(src,-1,deadline,0)){close(src);return -1;} // validate reused native trees too
        // A changed PC-authored database gets a separate seed rather than
        // overwriting an older sidecar containing deck-authored cues.
        const char *files[]={"Database2/m.db","Database2/m.db-wal"};
        for(unsigned i=0;i<2;i++) {
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
        int rc=copy_tree(fd,-1,deadline,0);close(fd);
        if(!rc)snprintf(seeded_as,sizeof seeded_as,"%s",name);
        return rc;
    }
    if(errno!=ENOENT){if(src>=0)close(src);return -1;}
    if(snprintf(stage,sizeof stage,"%s/.seed-XXXXXX",root)>=(int)sizeof stage){if(src>=0)close(src);errno=ENAMETOOLONG;return -1;}
    if(!mkdtemp(stage)){if(src>=0)close(src);return -1;}
    int dst=open(stage,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC),rc=dst<0?-1:0;
    if(!rc&&src>=0)rc=copy_tree(src,dst,deadline,0);
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
    if(bind_checked(source,dest,MS_BIND|MS_REC))return -1;
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
static struct owned_view *find_view(const char *,const char *);
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
    closedir(d);if(scan_error||add_child(v,"Engine Library",library,1))goto done;
    // Make the synthetic root read-only, while the separate library bind stays rw.
    if(mount(NULL,view,"tmpfs",MS_REMOUNT|MS_RDONLY|MS_NODEV|MS_NOSUID|MS_NOEXEC,"mode=0755,size=16m")||
       budget(deadline)||bind_checked(view,target,MS_BIND|MS_REC))goto done;
    if(update_public_ids(v)) {
        int saved=errno;umount2(target,MNT_DETACH|UMOUNT_NOFOLLOW);errno=saved;goto done;
    }
    note("writable HFS library ready on",target,0);rc=0;
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

static int has_export(const char *target) {
    if(safe_path(target,0))return 0;
    DIR *d=opendir(target);if(!d)return 0;int found=0;struct dirent *e;
    while(!found&&(e=readdir(d)))if(!strcasecmp(e->d_name,"PIONEER")||!strcasecmp(e->d_name,".PIONEER")) {
        struct stat st;found=!fstatat(dirfd(d),e->d_name,&st,AT_SYMLINK_NOFOLLOW)&&S_ISDIR(st.st_mode);
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
// What after_mount does on a volume. Engine learns of a mount from edisksd's
// mountinfo monitor (update_mountinfo -> filesystem_mounted -> Mountpoints
// PropertiesChanged) while edisksd's mount thread is still inside this call, so
// anything done after the real mount races Engine's first look at the stick
// (#182: playlists only after a re-insert). The conversion therefore runs first,
// in a private mount namespace, and the visible mount then finds it done.
enum { MOUNT_PRE=1, MOUNT_POST=2, MOUNT_FULL=3 };
#define CONVERTED 1
#define TIMED_OUT 4
static int after_mount(struct libmnt_context *ctx,int mode,int64_t deadline) {
    const char *target=mnt_context_get_target(ctx);
    if(!volume(target)||safe_path(target,0))return 0;
    int pioneer=has_export(target);char lib[PATH_MAX];
    if(path_join(lib,sizeof lib,target,"Engine Library")||safe_path(lib,1)) {note("unsafe library path rejected on",target,errno);return 0;}
    struct stat existing;
    int hfs_ro=HFS_READONLY(target);
    if(!pioneer&&mode==MOUNT_PRE)return 0; // nothing to convert: the visible mount does the rest
    // The walk guards the tool run on the library below; after the private
    // conversion (MOUNT_POST on a rekordbox stick) none follows, so it is skipped.
    // An HFS+ view is validated by prepare_view.
    if(!hfs_ro&&!(pioneer&&mode==MOUNT_POST)&&!lstat(lib,&existing)) {
        int fd=open(lib,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
        if(fd<0)return 0;
        int unsafe=copy_tree(fd,-1,deadline,0);close(fd);
        if(unsafe){note("unsafe library contents rejected on",target,errno);return 0;}
    }
    if(hfs_ro&&(pioneer||mode!=MOUNT_PRE)) {
        char identity[80];
        if(VOLUME_IDENTITY(mnt_context_get_source(ctx),identity)||prepare_view(target,identity,deadline)){note("HFS view unavailable on",target,errno);return 0;}
    }
    int done=0;
    if(pioneer&&mode!=MOUNT_POST){const char *rb=RB_BRIDGE_PATH();char *const a[]={(char *)rb,"--mount-path",(char *)target,NULL};if(!run(a,deadline))done=CONVERTED;else{if(errno==ETIMEDOUT)done=TIMED_OUT;note("bridge failed on",target,errno);}}
    if(hfs_ro&&mode==MOUNT_PRE) { // the persistent library stays; the private view goes
        struct owned_view *v=load_view(target);if(v){umount2(target,MNT_DETACH|UMOUNT_NOFOLLOW);cleanup_view(v);free(v);}
    }
    char db[PATH_MAX];
    if(mode!=MOUNT_PRE&&!pioneer&&!path_join(db,sizeof db,lib,"Database2/m.db")&&!safe_path(db,0)) {
        char *const a[]={"/usr/local/bin/engine-guardian","--heal-mount",(char *)target,"--data-only",NULL};if(run(a,deadline))note("guardian failed on",target,errno);
    }
    return pioneer?done|2:0;
}
// A library too large to convert inside the budget would hold every later
// insert of the same export for the whole budget and still end unconverted.
// Remember it until reboot, keyed by the filesystem and the export files.
static int slow_marker(const char *target,char out[PATH_MAX]) {
    struct statvfs v;if(statvfs(target,&v))return -1;
    uint64_t h=UINT64_C(14695981039346656037);
    h=hash_bytes(h,&v.f_fsid,sizeof v.f_fsid);h=hash_bytes(h,&v.f_blocks,sizeof v.f_blocks);
    const char *files[]={"PIONEER/rekordbox/export.pdb",".PIONEER/rekordbox/export.pdb","PIONEER/rekordbox/exportLibrary.db",".PIONEER/rekordbox/exportLibrary.db"};
    for(unsigned i=0;i<4;i++) {
        char p[PATH_MAX];struct stat st;
        if(path_join(p,sizeof p,target,files[i])||lstat(p,&st))continue;
        int64_t size=(int64_t)st.st_size,sec=(int64_t)st.st_mtim.tv_sec,ns=st.st_mtim.tv_nsec;
        h=hash_bytes(h,&i,sizeof i);h=hash_bytes(h,&size,sizeof size);h=hash_bytes(h,&sec,sizeof sec);h=hash_bytes(h,&ns,sizeof ns);
    }
    if(snprintf(out,PATH_MAX,"%s/slow-%016llx",RUN_ROOT,(unsigned long long)h)>=PATH_MAX){errno=ENAMETOOLONG;return -1;}
    return 0;
}
// Time kept for the visible mount, the HFS view and the reply to Engine.
#ifndef POST_RESERVE_MS
#define POST_RESERVE_MS INT64_C(3000)
#endif
// A mount that waited longer than this for view_lock (another stick was being
// converted) did not get its whole conversion window.
#define LOCK_WAIT_SLACK_MS INT64_C(1000)
// Mount the volume in a private namespace and convert it there. Returns 1 when
// the stick held a rekordbox export and rb-bridge finished, 2 when it held one
// but the conversion did not finish (or is known not to) or no time was left to
// look, 0 when it held none or this failed. whole_window: the budget was not cut
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
        seeded_as[0]=0;
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
        if(!has_export(target)){umount2(target,UMOUNT_NOFOLLOW);_exit(0);}
        char marker[PATH_MAX];int known=0,r=0;
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
    int pre=premount(ctx,real,deadline-POST_RESERVE_MS,now_ms()-start<LOCK_WAIT_SLACK_MS);
    int rc=real(ctx),saved=errno;
    if(!rc&&mnt_context_get_status(ctx)==1){after_mount(ctx,pre?MOUNT_POST:MOUNT_FULL,deadline);errno=saved;}
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

// A rescue tree is cloned BEFORE changing the public view. Normal child
// unmounts detect open files; rollback moves the already allocated mounts back.
static int unmount_view(struct libmnt_context *ctx,struct owned_view *v,int (*real)(struct libmnt_context *)) {
    char view[PATH_MAX],rescue[PATH_MAX],pub[PATH_MAX],src[PATH_MAX];
    if(path_join(view,sizeof view,v->anchor,"view")||path_join(rescue,sizeof rescue,v->anchor,"rescue")||
       safe_path(v->target,0)||mount_id(v->target)!=v->public_id)return -EINVAL;
    for(unsigned i=0;i<v->count;i++) {
        if(!memchr(v->child[i].name,0,sizeof v->child[i].name)||strchr(v->child[i].name,'/')||
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
    if(safe_path(target,0)||path_join(lib,sizeof lib,target,"Engine Library")||safe_path(lib,1))return -errno;
    pthread_mutex_lock(&view_lock);struct owned_view *v=load_view(target);
    int rc=v?unmount_view(ctx,v,real):real(ctx);int saved=errno;free(v);pthread_mutex_unlock(&view_lock);errno=saved;return rc;
}

int mnt_context_umount(struct libmnt_context *ctx) {
    int (*real)(struct libmnt_context *)=dlsym(RTLD_NEXT,"mnt_context_umount");
    if(!real){errno=ENOSYS;return -ENOSYS;}
    return unmount_dispatch(ctx,real);
}
