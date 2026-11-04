// Unit tests run unprivileged; --mounts is used ONLY under unshare -m on IN.
#define _GNU_SOURCE
#include <limits.h>
static char test_media[PATH_MAX],test_library[PATH_MAX],test_runtime[PATH_MAX];
#define MEDIA_ROOT test_media
#define LIB_ROOT test_library
#define RUN_ROOT test_runtime
#include "edisksd_hook.c"
#include <assert.h>
struct libmnt_context { const char *target,*source; int status,fail; };
const char *mnt_context_get_target(struct libmnt_context *c){return c->target;}
const char *mnt_context_get_source(struct libmnt_context *c){return c->source;}
int mnt_context_get_status(struct libmnt_context *c){return c->status;}
int mnt_context_set_syscall_status(struct libmnt_context *c,int status){c->status=status;return 0;}
static unsigned real_calls;
static int real_unmount(struct libmnt_context *c) {
    real_calls++;
    if(c->fail){errno=EBUSY;return -EBUSY;}
    return umount2(c->target,UMOUNT_NOFOLLOW);
}
static void join(char *out,const char *a,const char *b){assert(!path_join(out,PATH_MAX,a,b));}
static void write_file(const char *path,const char *text) {
    int fd=open(path,O_WRONLY|O_CREAT|O_TRUNC|O_NOFOLLOW|O_CLOEXEC,0600);assert(fd>=0);
    assert(write(fd,text,strlen(text))==(ssize_t)strlen(text));assert(!close(fd));
}
static void expect_file(const char *path,const char *text) {
    char buf[256];int fd=open(path,O_RDONLY|O_NOFOLLOW|O_CLOEXEC);assert(fd>=0);
    ssize_t n=read(fd,buf,sizeof buf);assert(n==(ssize_t)strlen(text)&&!memcmp(buf,text,(size_t)n));close(fd);
}
static void test_paths_and_copy(const char *root) {
    char volume_path[PATH_MAX],lib[PATH_MAX],outside[PATH_MAX],src[PATH_MAX],dst[PATH_MAX],p[PATH_MAX];
    join(volume_path,MEDIA_ROOT,"UNIT");assert(!make_dirs(volume_path));join(lib,volume_path,"Engine Library");
    join(outside,root,"outside");assert(!make_dirs(outside));join(p,outside,"sentinel");write_file(p,"untouched");
    assert(!symlink(outside,lib));errno=0;assert(safe_path(lib,1)==-1&&errno==ELOOP);
    struct libmnt_context c={.target=volume_path};unsigned calls=real_calls;
    assert(unmount_dispatch(&c,real_unmount)==-ELOOP&&real_calls==calls);
    assert(prepare_view(volume_path,"unit",deadline_from(now_ms()))==-1&&errno==ELOOP);
    assert(!unlink(lib));assert(!make_dirs(lib));join(p,lib,"Database2");assert(!symlink(outside,p));
    assert(safe_path(p,0)==-1&&errno==ELOOP);
    char seeded[PATH_MAX];assert(seed_library(volume_path,"unit",seeded,deadline_from(now_ms()))==-1&&errno==ELOOP);
    assert(!unlink(p));assert(!make_dirs(p));join(p,lib,"Database2/m.db");write_file(p,"native metadata");
    assert(!seed_library(volume_path,"unit",seeded,deadline_from(now_ms())));
    char copy[PATH_MAX];join(copy,seeded,"Database2/m.db");expect_file(copy,"native metadata");
    write_file(copy,"persisted cue");
    assert(!seed_library(volume_path,"unit",seeded,deadline_from(now_ms())));expect_file(copy,"persisted cue");
    // A PC-side replacement selects a new namespace, retaining the old sidecar.
    write_file(p,"new PC database -- longer");
    assert(!seed_library(volume_path,"unit",dst,deadline_from(now_ms()))&&strcmp(seeded,dst));
    join(src,dst,"Database2/m.db");expect_file(src,"new PC database -- longer");expect_file(copy,"persisted cue");
    char previous_revision[PATH_MAX];snprintf(previous_revision,sizeof previous_revision,"%s",dst);
    assert(!seed_library(volume_path,"other-identity",dst,deadline_from(now_ms()))&&strncmp(seeded,dst,strlen(seeded)));
    // Same size and restored timestamps still select a new source revision.
    struct stat original;
    char native_db[PATH_MAX];join(native_db,lib,"Database2/m.db");assert(!stat(native_db,&original));
    struct timespec times[2]={original.st_atim,original.st_mtim};
    write_file(native_db,"new PC database -- LONGER");assert(!utimensat(AT_FDCWD,native_db,times,AT_SYMLINK_NOFOLLOW));
    char revision[PATH_MAX];assert(!seed_library(volume_path,"unit",revision,deadline_from(now_ms()))&&strcmp(revision,previous_revision));
    join(p,outside,"sentinel");expect_file(p,"untouched");
    assert(!volume("/etc"));assert(volume(volume_path));
    char unsafe[PATH_MAX];join(unsafe,root,"link-parent");assert(!symlink(outside,unsafe));join(p,unsafe,"child");
    assert(make_dirs(p)==-1&&errno==ELOOP);
    puts("#20/#21/#22/#37: missing library, lossless seed/reuse, changed native DB, identity separation, all symlink components: PASS");
}
static void test_time(void) {
    struct timespec t={.tv_sec=2147483,.tv_nsec=0};int64_t before=timespec_ms(&t),deadline=deadline_from(before);
    t.tv_nsec=100000000;assert(timespec_ms(&t)<deadline&&deadline==INT64_C(2147503000));
    t.tv_sec=2147503;t.tv_nsec=0;assert(timespec_ms(&t)==deadline);
    assert(deadline_from(INT64_MAX)==INT64_MAX);
    int64_t start=now_ms();char *const sleepy[]={"/bin/sleep","2",NULL};
    assert(run(sleepy,start+30)==-1&&errno==ETIMEDOUT);assert(now_ms()-start<1000);
    char *const ok[]={"/bin/true",NULL};assert(!run(ok,deadline_from(now_ms())));
    puts("#24: ARM32 millisecond boundary, saturation and real deadline kill/reap: PASS");
}
static void make_disk(const char *path,int native) {
    assert(!make_dirs(path));char p[PATH_MAX];join(p,path,"PIONEER");assert(!make_dirs(p));
    join(p,path,"Music");assert(!make_dirs(p));join(p,path,"Music/song.raw");write_file(p,"audio");
    if(native){join(p,path,"Engine Library/Database2");assert(!make_dirs(p));join(p,path,"Engine Library/Database2/m.db");write_file(p,"native playlist/cues");}
}
static void mount_ro(const char *disk,const char *target) {
    assert(!make_dirs(target));assert(!mount(disk,target,NULL,MS_BIND,NULL));
    assert(!mount(NULL,target,NULL,MS_BIND|MS_REMOUNT|MS_RDONLY,NULL));
}
static void test_mounts(const char *root) {
    // No mount operation may propagate out of this disposable namespace.
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char disk_a[PATH_MAX],disk_b[PATH_MAX],target[PATH_MAX],lib[PATH_MAX],song[PATH_MAX],p[PATH_MAX];
    join(disk_a,root,"disk-a");join(disk_b,root,"disk-b");make_disk(disk_a,1);make_disk(disk_b,0);
    join(p,disk_b,"PIONEER");assert(!rmdir(p)); // fresh loose-audio HFS also needs its first native library
    join(target,MEDIA_ROOT,"SAME-LABEL");mount_ro(disk_a,target);uint64_t raw_id=mount_id(target);
    assert(!prepare_view(target,"uuid-A",deadline_from(now_ms())));
    join(lib,target,"Engine Library/Database2/m.db");expect_file(lib,"native playlist/cues");
    char duplicate[PATH_MAX];join(duplicate,MEDIA_ROOT,"DIFFERENT-LABEL");mount_ro(disk_b,duplicate);
    uint64_t duplicate_id=mount_id(duplicate);
    assert(prepare_view(duplicate,"uuid-A",deadline_from(now_ms()))==-1&&errno==EBUSY);
    assert(mount_id(duplicate)==duplicate_id);assert(!umount2(duplicate,UMOUNT_NOFOLLOW));
    write_file(lib,"deck cue retained");join(song,target,"Music/song.raw");expect_file(song,"audio");
    int denied=open(song,O_WRONLY);assert(denied<0&&errno==EROFS);
    struct libmnt_context c={.target=target};
    // A busy child cannot be lazily removed. Previously removed siblings return.
    int held=open(lib,O_RDONLY);assert(held>=0);real_calls=0;
    assert(unmount_dispatch(&c,real_unmount)!=0&&errno==EBUSY&&real_calls==0);
    expect_file(lib,"deck cue retained");expect_file(song,"audio");close(held);
    // A busy presentation root also restores all detached children.
    held=open(target,O_RDONLY|O_DIRECTORY);assert(held>=0);real_calls=0;
    assert(unmount_dispatch(&c,real_unmount)!=0&&errno==EBUSY&&real_calls==0);
    expect_file(lib,"deck cue retained");expect_file(song,"audio");close(held);
    // Real parent failure after removing our view must restore it atomically
    // from the already cloned rescue tree, not reconstruct an empty library.
    c.fail=1;real_calls=0;
    assert(unmount_dispatch(&c,real_unmount)!=0&&errno==EBUSY&&real_calls==1);
    assert(mount_id(target)!=raw_id);expect_file(lib,"deck cue retained");expect_file(song,"audio");
    struct owned_view *reloaded=load_view(target);assert(reloaded);free(reloaded); // daemon restart ownership recovery
    c.fail=0;assert(!unmount_dispatch(&c,real_unmount));
    join(p,disk_a,"Engine Library/Database2/m.db");expect_file(p,"native playlist/cues");
    // Fresh disk, identical public label, independent persistent namespace.
    mount_ro(disk_b,target);assert(!prepare_view(target,"uuid-B",deadline_from(now_ms())));
    join(p,target,"Engine Library/Database2");assert(!make_dirs(p));
    join(p,target,"Engine Library/Database2/m.db");write_file(p,"B only");
    assert(!unmount_dispatch(&c,real_unmount));
    join(p,disk_b,"Engine Library");assert(access(p,F_OK)==-1&&errno==ENOENT);
    mount_ro(disk_a,target);assert(!prepare_view(target,"uuid-A",deadline_from(now_ms())));expect_file(lib,"deck cue retained");
    assert(!unmount_dispatch(&c,real_unmount));
    // An unrelated tmpfs mount is an outside sentinel. Neither mount nor eject
    // is allowed to follow an Engine Library symlink into it.
    char evil[PATH_MAX],outside[PATH_MAX];join(evil,root,"evil");make_disk(evil,0);
    join(outside,root,"outside-mount");assert(!make_dirs(outside));
    assert(!mount("outside-sentinel",outside,"tmpfs",0,"mode=0700"));uint64_t outside_id=mount_id(outside);
    join(p,evil,"Engine Library");assert(!symlink(outside,p));mount_ro(evil,target);raw_id=mount_id(target);
    assert(prepare_view(target,"evil",deadline_from(now_ms()))==-1&&errno==ELOOP);
    real_calls=0;assert(unmount_dispatch(&c,real_unmount)==-ELOOP&&real_calls==0);
    assert(mount_id(target)==raw_id&&mount_id(outside)==outside_id);
    assert(!umount2(target,UMOUNT_NOFOLLOW));assert(!umount2(outside,UMOUNT_NOFOLLOW));
    puts("REAL PRIVATE NAMESPACE #20/#21/#22/#23/#37: fresh/existing RO volumes, persistent isolation, busy child/root/parent rollback, restart record, outside mount preserved: PASS");
}
int main(int argc,char **argv) {
    char root[]="/tmp/sc5000-edisks-tests-XXXXXX";assert(mkdtemp(root));
    join(test_media,root,"media");join(test_library,root,"libraries");join(test_runtime,root,"runtime");
    assert(!make_dirs(test_media));test_paths_and_copy(root);test_time();
    if(argc>1&&!strcmp(argv[1],"--mounts"))test_mounts(root);
    // Mount tests have removed all namespace mounts before deleting fixtures.
    int tmp=open("/tmp",O_RDONLY|O_DIRECTORY);assert(tmp>=0);remove_tree(tmp,strrchr(root,'/')+1);close(tmp);
    puts("edisksd-hook: all requested tests passed");return 0;
}
