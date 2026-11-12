// Unit tests run unprivileged; --mounts is used ONLY under unshare -m on Oracle-Batam-arm.
#define _GNU_SOURCE
#include <limits.h>
#include <stdio.h>
#include <string.h>
static char test_media[PATH_MAX],test_library[PATH_MAX],test_legacy_library[PATH_MAX],test_runtime[PATH_MAX],test_utab[PATH_MAX];
#define MEDIA_ROOT test_media
#define LIB_ROOT test_library
#define LEGACY_LIB_ROOT test_legacy_library
#define RUN_ROOT test_runtime
#define UTAB_DIR test_utab
// after_mount seams: a bind mount is neither HFS+ nor a block device.
static char test_hfs_target[PATH_MAX],test_rb_bridge[PATH_MAX];
static int test_hfs_readonly(const char *target){return *test_hfs_target&&!strcmp(target,test_hfs_target);}
static int test_volume_identity(const char *source,char out[80]){(void)source;snprintf(out,80,"uuid-hfs-stick");return 0;}
static int test_bridge_hangs;
static const char *test_rb_bridge_path(void);
#define HFS_READONLY test_hfs_readonly
#define VOLUME_IDENTITY test_volume_identity
#define RB_BRIDGE_PATH test_rb_bridge_path
// The "real" libmount mount: a bind of test_disk onto the target.
struct libmnt_context;
static int test_real_mount(struct libmnt_context *c);
#define REAL_MNT_CONTEXT_MOUNT() test_real_mount
// Leaves 1.5 s of the 20 s budget for the private conversion.
#include <stdint.h>
static int64_t test_post_reserve=18500;
#define POST_RESERVE_MS test_post_reserve
#include "edisksd_hook.c"
#include <assert.h>
struct libmnt_context { const char *target,*source; int status,fail; const char *disk; };
const char *mnt_context_get_target(struct libmnt_context *c){return c->target;}
const char *mnt_context_get_source(struct libmnt_context *c){return c->source;}
int mnt_context_get_status(struct libmnt_context *c){return c->status;}
int mnt_context_set_syscall_status(struct libmnt_context *c,int status){c->status=status;return 0;}
static const char *test_rb_bridge_path(void){while(test_bridge_hangs)pause();return test_rb_bridge;}
static char test_disk[PATH_MAX];static int test_disk_ro;
static int test_real_mount(struct libmnt_context *c) {
    if(mount(c->disk?c->disk:test_disk,c->target,NULL,MS_BIND,NULL))return -errno;
    // As libmount does for a mount with a userspace option: a line in <UTAB_DIR>/utab.
    char utab[PATH_MAX+8];snprintf(utab,sizeof utab,"%s/utab",test_utab);
    int fd=open(utab,O_WRONLY|O_CREAT|O_APPEND|O_CLOEXEC,0600);
    if(fd>=0){if(write(fd,c->target,strlen(c->target))<0||write(fd,"\n",1)<0){}close(fd);}
    if(test_disk_ro&&mount(NULL,c->target,NULL,MS_BIND|MS_REMOUNT|MS_RDONLY,NULL))return -errno;
    c->status=1;return 0;
}
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
// 5.3.0.3 made /data/sc5000-custom a symlink to enginex (#103). The cache path
// through it failed with ELOOP, so no read-only HFS+ stick got a view or rb-bridge.
static void test_compat_symlink_root(const char *root) {
    char data[PATH_MAX],enginex[PATH_MAX],legacy[PATH_MAX],p[PATH_MAX],seeded[PATH_MAX],volume_path[PATH_MAX];
    char saved_lib[PATH_MAX],saved_legacy[PATH_MAX];
    snprintf(saved_lib,sizeof saved_lib,"%s",test_library);snprintf(saved_legacy,sizeof saved_legacy,"%s",test_legacy_library);
    join(data,root,"data");join(enginex,data,"enginex");assert(!make_dirs(enginex));
    join(legacy,data,"sc5000-custom");assert(!symlink("enginex",legacy));
    join(volume_path,MEDIA_ROOT,"HFS-STICK");assert(!make_dirs(volume_path));
    // The hook's old cache path, through the compat link.
    join(test_library,legacy,"hfs-libraries");test_legacy_library[0]=0;
    assert(!seed_library(volume_path,"uuid-a",seeded,deadline_from(now_ms())));
    join(p,enginex,"hfs-libraries");assert(!strncmp(seeded,p,strlen(p)));
    struct stat st;assert(!lstat(p,&st)&&S_ISDIR(st.st_mode)&&!(st.st_mode&077));
    // The default: /data/enginex first, the legacy root only when enginex is missing.
    join(test_library,enginex,"hfs-libraries");join(test_legacy_library,legacy,"hfs-libraries");
    char again[PATH_MAX];assert(!seed_library(volume_path,"uuid-a",again,deadline_from(now_ms()))&&!strcmp(again,seeded));
    char old_data[PATH_MAX],old_real[PATH_MAX];join(old_data,root,"old-data");join(old_real,old_data,"sc5000-custom");assert(!make_dirs(old_real));
    join(test_library,old_data,"enginex/hfs-libraries");join(test_legacy_library,old_real,"hfs-libraries");
    assert(!seed_library(volume_path,"uuid-a",again,deadline_from(now_ms())));join(p,old_real,"hfs-libraries");assert(!strncmp(again,p,strlen(p)));
    // A blank /data: neither directory exists yet, so the primary one is created 0700.
    char blank[PATH_MAX];join(blank,root,"blank-data");assert(!make_dirs(blank));
    join(test_library,blank,"enginex/hfs-libraries");join(test_legacy_library,blank,"sc5000-custom/hfs-libraries");
    assert(!seed_library(volume_path,"uuid-a",again,deadline_from(now_ms())));
    join(p,blank,"enginex/hfs-libraries");assert(!strncmp(again,p,strlen(p)));
    join(p,blank,"enginex");assert(!lstat(p,&st)&&S_ISDIR(st.st_mode)&&(st.st_mode&0777)==0700);
    join(p,blank,"sc5000-custom");assert(access(p,F_OK)==-1&&errno==ENOENT);
    // A dangling compat link is not a blank /data.
    char dangling[PATH_MAX];join(dangling,root,"dangling-data");assert(!make_dirs(dangling));
    join(p,dangling,"sc5000-custom");assert(!symlink("enginex",p));
    join(test_library,dangling,"enginex/hfs-libraries");join(test_legacy_library,dangling,"sc5000-custom/hfs-libraries");errno=0;
    assert(seed_library(volume_path,"uuid-a",again,deadline_from(now_ms()))==-1&&errno==ELOOP);
    join(p,dangling,"enginex");assert(access(p,F_OK)==-1&&errno==ENOENT);
    // A real root directory that others can write is refused like a shared link target.
    char open_data[PATH_MAX];join(open_data,root,"open-data");join(p,open_data,"enginex");assert(!make_dirs(p));assert(!chmod(p,0777));
    join(test_library,open_data,"enginex/hfs-libraries");test_legacy_library[0]=0;errno=0;
    assert(seed_library(volume_path,"uuid-a",again,deadline_from(now_ms()))==-1&&errno==EACCES);
    // Only a plain sibling link to a private directory we own is followed.
    char outside[PATH_MAX],bad[PATH_MAX];join(outside,root,"outside-cache");assert(!make_dirs(outside));
    const char *links[]={outside,"../outside-cache",".","enginex-missing"};
    for(unsigned i=0;i<4;i++) {
        join(bad,data,"bad-link");unlink(bad);assert(!symlink(links[i],bad));
        join(test_library,bad,"hfs-libraries");test_legacy_library[0]=0;errno=0;
        assert(seed_library(volume_path,"uuid-a",again,deadline_from(now_ms()))==-1&&(errno==ELOOP||errno==ENOENT));
    }
    char shared[PATH_MAX];join(shared,data,"shared");assert(!make_dirs(shared));assert(!chmod(shared,0777));
    unlink(bad);assert(!symlink("shared",bad));join(test_library,bad,"hfs-libraries");errno=0;
    assert(seed_library(volume_path,"uuid-a",again,deadline_from(now_ms()))==-1&&errno==ELOOP);
    char chained[PATH_MAX];join(chained,data,"chained");assert(!symlink("sc5000-custom",chained));
    join(test_library,chained,"hfs-libraries");errno=0;
    assert(seed_library(volume_path,"uuid-a",again,deadline_from(now_ms()))==-1&&errno==ELOOP);
    join(p,outside,"hfs-libraries");assert(access(p,F_OK)==-1&&errno==ENOENT);
    snprintf(test_library,sizeof test_library,"%s",saved_lib);snprintf(test_legacy_library,sizeof test_legacy_library,"%s",saved_legacy);
    puts("#103 HFS: cache root through the /data/sc5000-custom -> enginex compat link, legacy-only deck, blank /data, dangling link and writable root, foreign links refused: PASS");
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
// The deck's layout end to end: a read-only HFS+ rekordbox stick with a hidden
// .PIONEER export on a deck whose /data/sc5000-custom is the compat symlink.
// The view must be prepared and rb-bridge launched on it (#103 regression,
// found on a rekordbox 7 macOS stick, #216).
static void test_foreign_owned_root(const char *root) {
    char saved_lib[PATH_MAX],saved_legacy[PATH_MAX],data[PATH_MAX],p[PATH_MAX],volume_path[PATH_MAX],seeded[PATH_MAX];
    snprintf(saved_lib,sizeof saved_lib,"%s",test_library);snprintf(saved_legacy,sizeof saved_legacy,"%s",test_legacy_library);
    join(volume_path,MEDIA_ROOT,"HFS-STICK");assert(!make_dirs(volume_path));
    join(data,root,"foreign-data");join(p,data,"enginex");assert(!make_dirs(p));assert(!chown(p,4242,4242));
    join(test_library,p,"hfs-libraries");test_legacy_library[0]=0;errno=0;
    assert(seed_library(volume_path,"uuid-a",seeded,deadline_from(now_ms()))==-1&&errno==EACCES);
    join(p,data,"enginex/hfs-libraries");assert(access(p,F_OK)==-1&&errno==ENOENT);
    snprintf(test_library,sizeof test_library,"%s",saved_lib);snprintf(test_legacy_library,sizeof test_legacy_library,"%s",saved_legacy);
    puts("REAL PRIVATE NAMESPACE #103: a cache root owned by another user is refused: PASS");
}
static void test_hfs_mount_runs_bridge(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char data[PATH_MAX],enginex[PATH_MAX],legacy[PATH_MAX],disk[PATH_MAX],target[PATH_MAX],p[PATH_MAX],log[PATH_MAX];
    join(data,root,"deck-data");join(enginex,data,"enginex");assert(!make_dirs(enginex));
    join(legacy,data,"sc5000-custom");assert(!symlink("enginex",legacy));
    join(test_library,legacy,"hfs-libraries");test_legacy_library[0]=0;
    join(disk,root,"hfs-disk");assert(!make_dirs(disk));join(p,disk,".PIONEER/rekordbox");assert(!make_dirs(p));
    join(p,disk,".PIONEER/rekordbox/exportLibrary.db");write_file(p,"rekordbox 7");
    join(log,root,"rb-bridge.log");join(test_rb_bridge,root,"rb-bridge");
    char script[PATH_MAX*2];snprintf(script,sizeof script,"#!/bin/sh\necho \"$*\" > '%s'\n: > \"$2/Engine Library/bridged\"\n",log);
    write_file(test_rb_bridge,script);assert(!chmod(test_rb_bridge,0700));
    join(target,MEDIA_ROOT,"vivi");mount_ro(disk,target);uint64_t raw_id=mount_id(target);
    snprintf(test_hfs_target,sizeof test_hfs_target,"%s",target);
    struct libmnt_context c={.target=target,.source="/dev/sdz1",.status=1};
    after_mount(&c,MOUNT_FULL,deadline_from(now_ms()));
    assert(mount_id(target)!=raw_id); // the writable view is in place
    char expect[PATH_MAX+32];snprintf(expect,sizeof expect,"--mount-path %s\n",target);expect_file(log,expect);
    join(p,target,"Engine Library/bridged");assert(!access(p,F_OK));
    join(p,disk,"Engine Library");assert(access(p,F_OK)==-1&&errno==ENOENT); // nothing written to the stick
    join(p,enginex,"hfs-libraries");assert(!access(p,F_OK));
    assert(!unmount_dispatch(&c,real_unmount));test_hfs_target[0]=0;
    puts("REAL PRIVATE NAMESPACE #103: read-only HFS+ .PIONEER stick with the sc5000-custom compat link gets its view and rb-bridge: PASS");
}
static unsigned count_mounts(const char *target) {
    FILE *f=fopen("/proc/self/mountinfo","r");assert(f);char line[4096];unsigned n=0;size_t len=strlen(target);
    while(fgets(line,sizeof line,f)){char *p=strchr(line,' ');for(int i=0;p&&i<3;i++)p=strchr(p+1,' ');if(p&&!strncmp(p+1,target,len)&&(p[1+len]==' '||p[1+len]=='/'))n++;}
    fclose(f);return n;
}
static unsigned count_runtime(const char *prefix) {
    DIR *d=opendir(test_runtime);if(!d)return 0;unsigned n=0;struct dirent *e;
    while((e=readdir(d)))if(!strncmp(e->d_name,prefix,strlen(prefix)))n++;
    closedir(d);return n;
}
static unsigned count_views(void){return count_runtime("view-");}
// #182: Engine hears of a mount from edisksd's mountinfo monitor while the hook
// is still running, so a stick converted after the visible mount showed up as a
// plain folder drive. The conversion must be complete before that mount exists.
static void test_first_insert_converted_before_visible(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char data[PATH_MAX],disk[PATH_MAX],target[PATH_MAX],p[PATH_MAX],log[PATH_MAX],pid[32],script[PATH_MAX*3];
    join(data,root,"first-data");join(p,data,"enginex");assert(!make_dirs(p));
    join(test_library,p,"hfs-libraries");test_legacy_library[0]=0;
    join(log,root,"first-bridge.log");join(test_rb_bridge,root,"first-bridge");
    snprintf(pid,sizeof pid,"%d",(int)getpid());assert(!setenv("EDISKSD_TEST_PID",pid,1));
    // Records whether the volume is visible in the caller's (edisksd's) namespace, then converts.
    snprintf(script,sizeof script,"#!/bin/sh\nif grep -q \" $2 \" /proc/$EDISKSD_TEST_PID/mountinfo; then v=visible; else v=private; fi\n"
             "echo \"$v $*\" >> '%s'\nmkdir -p \"$2/Engine Library/Database2\" && echo converted > \"$2/Engine Library/Database2/m.db\"\n",log);
    write_file(test_rb_bridge,script);assert(!chmod(test_rb_bridge,0700));
    char expect[PATH_MAX+64];
    // FAT-style writable stick: the database lands on the stick before Engine can see it.
    join(disk,root,"first-fat");join(p,disk,"PIONEER/rekordbox");assert(!make_dirs(p));
    join(target,MEDIA_ROOT,"FIRST");assert(!make_dirs(target));
    snprintf(test_disk,sizeof test_disk,"%s",disk);test_disk_ro=0;
    struct libmnt_context c={.target=target,.source="/dev/sdy1"};
    assert(!mnt_context_mount(&c)&&c.status==1);
    snprintf(expect,sizeof expect,"private --mount-path %s\n",target);expect_file(log,expect); // ran once, unseen
    join(p,target,"Engine Library/Database2/m.db");expect_file(p,"converted\n");
    assert(count_mounts(target)==1); // the private mount never leaked
    assert(!umount2(target,UMOUNT_NOFOLLOW));unlink(log);
    // A stick without an export is mounted once, untouched.
    char plain[PATH_MAX];join(plain,root,"first-plain");join(p,plain,"Music");assert(!make_dirs(p));
    snprintf(test_disk,sizeof test_disk,"%s",plain);c.status=0;
    assert(!mnt_context_mount(&c)&&c.status==1);assert(access(log,F_OK)==-1);
    join(p,plain,"Engine Library");assert(access(p,F_OK)==-1);assert(!umount2(target,UMOUNT_NOFOLLOW));
    // Read-only HFS+ with a hidden .PIONEER: the converted library is in the view at once.
    char hfs[PATH_MAX];join(hfs,root,"first-hfs");join(p,hfs,".PIONEER/rekordbox");assert(!make_dirs(p));
    snprintf(test_disk,sizeof test_disk,"%s",hfs);test_disk_ro=1;snprintf(test_hfs_target,sizeof test_hfs_target,"%s",target);c.status=0;
    unsigned views_before=count_views();
    assert(!mnt_context_mount(&c)&&c.status==1);
    expect_file(log,expect);
    join(p,target,"Engine Library/Database2/m.db");expect_file(p,"converted\n");
    join(p,hfs,"Engine Library");assert(access(p,F_OK)==-1); // nothing written to the stick
    assert(count_views()==views_before+1); // the private namespace's view record was cleaned up
    assert(!unmount_dispatch(&c,real_unmount));test_hfs_target[0]=0;unlink(log);
    // A conversion that fails is not run a second time after the visible mount.
    snprintf(script,sizeof script,"#!/bin/sh\necho \"$*\" >> '%s'\nexit 1\n",log);write_file(test_rb_bridge,script);
    snprintf(test_disk,sizeof test_disk,"%s",disk);test_disk_ro=0;join(p,disk,"Engine Library/Database2/m.db");assert(!unlink(p));c.status=0;
    assert(!mnt_context_mount(&c)&&c.status==1);
    snprintf(expect,sizeof expect,"--mount-path %s\n",target);expect_file(log,expect);
    assert(!umount2(target,UMOUNT_NOFOLLOW));unlink(log);
    // A library too large for the budget: killed once, then skipped until the export changes.
    snprintf(script,sizeof script,"#!/bin/sh\necho \"$*\" >> '%s'\nsleep 5\n",log);write_file(test_rb_bridge,script);
    join(p,disk,"PIONEER/rekordbox/export.pdb");write_file(p,"big library");
    for(int i=0;i<2;i++) {
        int64_t t0=now_ms();c.status=0;assert(!mnt_context_mount(&c)&&c.status==1);
        int64_t took=now_ms()-t0;assert(took<(i?1000:3500)); // never the whole budget again
        expect_file(log,expect);join(p,target,"Engine Library/Database2/m.db");assert(access(p,F_OK)==-1);
        assert(!umount2(target,UMOUNT_NOFOLLOW));
    }
    join(p,disk,"PIONEER/rekordbox/export.pdb");write_file(p,"re-exported library");
    struct timespec later[2]={{.tv_sec=2000000000},{.tv_sec=2000000000}};assert(!utimensat(AT_FDCWD,p,later,0));
    c.status=0;assert(!mnt_context_mount(&c)&&c.status==1);assert(!umount2(target,UMOUNT_NOFOLLOW));
    char twice[PATH_MAX*2+8];snprintf(twice,sizeof twice,"%s%s",expect,expect);expect_file(log,twice); // a new export is tried again
    unsetenv("EDISKSD_TEST_PID");
    puts("REAL PRIVATE NAMESPACE #182: rekordbox sticks (FAT and read-only HFS+) are converted before the visible mount, plain sticks mounted once, failed conversion not repeated, too-large library skipped until re-exported: PASS");
}
static unsigned count_notes(const char *log,const char *what) {
    static char buf[65536];int fd=open(log,O_RDONLY|O_CLOEXEC);assert(fd>=0);
    ssize_t n=read(fd,buf,sizeof buf-1);assert(n>=0);close(fd);buf[n]=0;
    unsigned found=0;for(char *p=buf;(p=strstr(p,what));p+=strlen(what))found++;
    return found;
}
// The hook's notes (stderr of the mount and of its private pre-mount) go to a file.
static int notes_to(const char *log) {
    int saved=dup(2),fd=open(log,O_WRONLY|O_CREAT|O_TRUNC|O_CLOEXEC,0600);assert(saved>=0&&fd>=0&&dup2(fd,2)==2);close(fd);return saved;
}
static void notes_done(int saved){assert(dup2(saved,2)==2);close(saved);}
// #182 review: the private pre-mount made every stick pay twice. A stick without
// a rekordbox export had its Engine Library walked there and again after the
// visible mount (on a cold cache, inside one budget), and a converted stick was
// walked once more for nothing. A link in the library makes each walk leave a note.
static void test_library_is_walked_once(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char disk[PATH_MAX],target[PATH_MAX],p[PATH_MAX],notes[PATH_MAX],saved_runtime[PATH_MAX],script[PATH_MAX*2];
    const char *walked="unsafe library contents rejected";
    join(disk,root,"once-native");join(p,disk,"Engine Library/Database2");assert(!make_dirs(p));
    join(p,disk,"Engine Library/Database2/link");assert(!symlink("/",p));
    join(target,MEDIA_ROOT,"ONCE");assert(!make_dirs(target));join(notes,root,"once-notes");
    snprintf(test_disk,sizeof test_disk,"%s",disk);test_disk_ro=0;
    // A runtime directory that does not exist yet: the pre-mount of a plain stick
    // looks at the stick's root directory and at nothing else.
    snprintf(saved_runtime,sizeof saved_runtime,"%s",test_runtime);join(test_runtime,root,"once-runtime");
    struct libmnt_context c={.target=target,.source="/dev/sdx1"};
    int saved=notes_to(notes);
    assert(!mnt_context_mount(&c)&&c.status==1);
    notes_done(saved);
    assert(count_notes(notes,walked)==1);
    assert(access(test_runtime,F_OK)==-1&&errno==ENOENT);
    assert(count_mounts(target)==1);assert(!umount2(target,UMOUNT_NOFOLLOW));
    snprintf(test_runtime,sizeof test_runtime,"%s",saved_runtime);
    // A rekordbox stick: converted privately, then mounted without another walk.
    join(disk,root,"once-rekordbox");join(p,disk,"PIONEER/rekordbox");assert(!make_dirs(p));
    join(test_rb_bridge,root,"once-bridge");
    snprintf(script,sizeof script,"#!/bin/sh\nmkdir -p \"$2/Engine Library/Database2\" && echo converted > \"$2/Engine Library/Database2/m.db\" && ln -s / \"$2/Engine Library/link\"\n");
    write_file(test_rb_bridge,script);assert(!chmod(test_rb_bridge,0700));
    snprintf(test_disk,sizeof test_disk,"%s",disk);c.status=0;
    saved=notes_to(notes);
    assert(!mnt_context_mount(&c)&&c.status==1);
    notes_done(saved);
    join(p,target,"Engine Library/Database2/m.db");expect_file(p,"converted\n");
    assert(count_notes(notes,walked)==0);
    assert(!umount2(target,UMOUNT_NOFOLLOW));
    puts("REAL PRIVATE NAMESPACE #182: a stick without a rekordbox export is only looked at before the visible mount and its library walked once, a converted stick is not walked again: PASS");
}
struct insert { struct libmnt_context c; int rc; };
static void *insert_thread(void *arg){struct insert *i=arg;i->rc=mnt_context_mount(&i->c);return NULL;}
// #182 review: the budget starts before view_lock is taken. Two rekordbox sticks
// inserted together (or present at boot) are converted one after the other, so
// the second one's wait cut its window short; it ran out of time and was then
// remembered as "too large" and skipped on every insert until a reboot.
static void test_two_sticks_at_once(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char a[PATH_MAX],b[PATH_MAX],ta[PATH_MAX],tb[PATH_MAX],p[PATH_MAX],log[PATH_MAX],script[PATH_MAX*2];
    join(a,root,"pair-a");join(p,a,"PIONEER/rekordbox");assert(!make_dirs(p));
    join(b,root,"pair-b");join(p,b,"PIONEER/rekordbox");assert(!make_dirs(p));
    join(p,b,"PIONEER/rekordbox/export.pdb");write_file(p,"the second stick");
    join(ta,MEDIA_ROOT,"PAIR-A");assert(!make_dirs(ta));join(tb,MEDIA_ROOT,"PAIR-B");assert(!make_dirs(tb));
    join(log,root,"pair-bridge.log");join(test_rb_bridge,root,"pair-bridge");
    // Each conversion takes 2.5 s of a 4 s window.
    snprintf(script,sizeof script,"#!/bin/sh\necho \"$2\" >> '%s'\nsleep 2.5\nmkdir -p \"$2/Engine Library/Database2\" && echo converted > \"$2/Engine Library/Database2/m.db\"\n",log);
    write_file(test_rb_bridge,script);assert(!chmod(test_rb_bridge,0700));
    int64_t saved_reserve=test_post_reserve;test_post_reserve=16000;test_disk_ro=0;
    unsigned slow_before=count_runtime("slow-");
    struct insert ia={.c={.target=ta,.source="/dev/sdp1",.disk=a}},ib={.c={.target=tb,.source="/dev/sdq1",.disk=b}};
    pthread_t first,second;
    assert(!pthread_create(&first,NULL,insert_thread,&ia));usleep(300000);
    assert(!pthread_create(&second,NULL,insert_thread,&ib));
    assert(!pthread_join(first,NULL)&&!pthread_join(second,NULL));
    assert(!ia.rc&&ia.c.status==1&&!ib.rc&&ib.c.status==1); // both are mounted
    join(p,ta,"Engine Library/Database2/m.db");expect_file(p,"converted\n");
    join(p,tb,"Engine Library/Database2/m.db");assert(access(p,F_OK)==-1); // its window was cut short
    assert(count_runtime("slow-")==slow_before); // which says nothing about its size
    assert(!umount2(ta,UMOUNT_NOFOLLOW)&&!umount2(tb,UMOUNT_NOFOLLOW));
    // Inserted again on its own, the second stick is converted.
    ib.c.status=0;assert(!mnt_context_mount(&ib.c)&&ib.c.status==1);
    join(p,tb,"Engine Library/Database2/m.db");expect_file(p,"converted\n");
    assert(!umount2(tb,UMOUNT_NOFOLLOW));join(p,b,"Engine Library/Database2/m.db");assert(!unlink(p));unlink(log);
    // A wait that used the whole window up: nothing is tried, nothing remembered,
    // and the conversion is not run after the visible mount either.
    test_post_reserve=HOOK_BUDGET_MS+1;
    ib.c.status=0;assert(!mnt_context_mount(&ib.c)&&ib.c.status==1);
    assert(count_mounts(tb)==1);assert(!umount2(tb,UMOUNT_NOFOLLOW));
    assert(access(log,F_OK)==-1&&access(p,F_OK)==-1&&count_runtime("slow-")==slow_before);
    test_post_reserve=saved_reserve;
    puts("REAL PRIVATE NAMESPACE #182: two rekordbox sticks inserted together, the second one's cut-short or used-up window is not remembered as a too-large library and it converts on its next insert: PASS");
}
static unsigned count_entries(const char *path) {
    DIR *d=opendir(path);if(!d)return 0;unsigned n=0;struct dirent *e;
    while((e=readdir(d)))if(e->d_name[0]!='.')n++;
    closedir(d);return n;
}
// #182 review: a read-only HFS+ stick with both an Engine Library and a
// rekordbox export had its Engine database read and hashed from the stick twice,
// the second time for the visible mount inside the few seconds kept for it. When
// that ran out the deck got the bare read-only volume and the conversion was not
// shown. The visible mount now takes the library the private conversion used.
// The bridge changes the stick's database behind the mount: a second read would
// find other content and present a fresh, unconverted copy of it.
static void test_hfs_visible_mount_reuses_the_converted_library(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char data[PATH_MAX],disk[PATH_MAX],db[PATH_MAX],target[PATH_MAX],p[PATH_MAX],cache[PATH_MAX],script[PATH_MAX*3];
    join(data,root,"reuse-data");join(p,data,"enginex");assert(!make_dirs(p));
    join(test_library,p,"hfs-libraries");test_legacy_library[0]=0;join(cache,test_library,"uuid-hfs-stick");
    join(disk,root,"reuse-hfs");join(p,disk,".PIONEER/rekordbox");assert(!make_dirs(p));
    join(p,disk,"Engine Library/Database2");assert(!make_dirs(p));join(db,p,"m.db");write_file(db,"made on the Mac");
    join(test_rb_bridge,root,"reuse-bridge");
    snprintf(script,sizeof script,"#!/bin/sh\necho \"read again $(date +%%s%%N)\" > '%s'\n: > \"$2/Engine Library/bridged\"\n",db);
    write_file(test_rb_bridge,script);assert(!chmod(test_rb_bridge,0700));
    join(target,MEDIA_ROOT,"REUSE");assert(!make_dirs(target));
    snprintf(test_disk,sizeof test_disk,"%s",disk);test_disk_ro=1;snprintf(test_hfs_target,sizeof test_hfs_target,"%s",target);
    struct libmnt_context c={.target=target,.source="/dev/sdw1"};
    assert(!mnt_context_mount(&c)&&c.status==1);
    join(p,target,"Engine Library/bridged");assert(!access(p,F_OK)); // the converted library is the one shown
    join(p,target,"Engine Library/Database2/m.db");expect_file(p,"made on the Mac");
    assert(count_entries(cache)==1);
    assert(!unmount_dispatch(&c,real_unmount));
    // The hand-over ends with the mount: a database changed since is seeded anew.
    c.status=0;assert(!mnt_context_mount(&c)&&c.status==1);
    join(p,target,"Engine Library/bridged");assert(!access(p,F_OK));
    assert(count_entries(cache)==2);
    assert(!unmount_dispatch(&c,real_unmount));test_hfs_target[0]=0;test_disk_ro=0;
    puts("REAL PRIVATE NAMESPACE #182: a read-only HFS+ stick with an Engine Library and a rekordbox export is shown the library its private conversion used, without a second read of the stick's database: PASS");
}
// #182 review: what the private pre-mount left in edisksd's own namespace. Its
// libmount record went into the shared /run/mount/utab and was never removed
// (one stale line per insert), and a pre-mount killed at its deadline left its
// HFS+ view's directory and record in the runtime directory.
static void test_private_mount_leaves_nothing_behind(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char data[PATH_MAX],disk[PATH_MAX],target[PATH_MAX],p[PATH_MAX],utab[PATH_MAX],line[PATH_MAX+8];
    join(data,root,"clean-data");join(p,data,"enginex");assert(!make_dirs(p));
    join(test_library,p,"hfs-libraries");test_legacy_library[0]=0;
    join(disk,root,"clean-stick");join(p,disk,".PIONEER/rekordbox");assert(!make_dirs(p));
    join(test_rb_bridge,root,"clean-bridge");write_file(test_rb_bridge,"#!/bin/sh\nexit 0\n");assert(!chmod(test_rb_bridge,0700));
    join(target,MEDIA_ROOT,"CLEAN");assert(!make_dirs(target));join(utab,test_utab,"utab");unlink(utab);
    snprintf(test_disk,sizeof test_disk,"%s",disk);test_disk_ro=0;
    struct libmnt_context c={.target=target,.source="/dev/sdv1"};
    assert(!mnt_context_mount(&c)&&c.status==1);
    snprintf(line,sizeof line,"%s\n",target);expect_file(utab,line); // the visible mount's record only
    assert(!umount2(target,UMOUNT_NOFOLLOW));
    // Read-only HFS+: the pre-mount has its view up when it stops responding and is killed.
    test_disk_ro=1;snprintf(test_hfs_target,sizeof test_hfs_target,"%s",target);
    unsigned views_before=count_views();
    test_bridge_hangs=1;c.status=0;assert(!mnt_context_mount(&c)&&c.status==1);test_bridge_hangs=0;
    assert(count_views()==views_before+1); // the visible mount's view and nothing else
    assert(!unmount_dispatch(&c,real_unmount));test_hfs_target[0]=0;test_disk_ro=0;
    assert(count_views()==views_before);
    puts("REAL PRIVATE NAMESPACE #182: the private pre-mount leaves no utab record, and a killed one no view directory, in the caller's namespace: PASS");
}
int main(int argc,char **argv) {
    char root[]="/tmp/sc5000-edisks-tests-XXXXXX";assert(mkdtemp(root));
    join(test_media,root,"media");join(test_library,root,"libraries");join(test_runtime,root,"runtime");
    join(test_legacy_library,root,"legacy-libraries");join(test_utab,root,"utab");assert(!make_dirs(test_utab));
    assert(!make_dirs(test_media));test_paths_and_copy(root);test_compat_symlink_root(root);test_time();
    if(argc>1&&!strcmp(argv[1],"--mounts")){test_mounts(root);test_foreign_owned_root(root);test_hfs_mount_runs_bridge(root);test_first_insert_converted_before_visible(root);test_library_is_walked_once(root);test_two_sticks_at_once(root);test_hfs_visible_mount_reuses_the_converted_library(root);test_private_mount_leaves_nothing_behind(root);}
    // Mount tests have removed all namespace mounts before deleting fixtures.
    int tmp=open("/tmp",O_RDONLY|O_DIRECTORY);assert(tmp>=0);remove_tree(tmp,strrchr(root,'/')+1);close(tmp);
    puts("edisksd-hook: all requested tests passed");return 0;
}
