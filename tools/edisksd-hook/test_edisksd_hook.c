// Unit tests run unprivileged; --mounts is used ONLY under unshare -m on the build host.
#define _GNU_SOURCE
#include <limits.h>
#include <stdio.h>
#include <string.h>
static char test_media[PATH_MAX],test_library[PATH_MAX],test_legacy_library[PATH_MAX],test_runtime[PATH_MAX],test_utab[PATH_MAX];
static char test_prepare_state[PATH_MAX];
#define PREPARE_STATE_PATH test_prepare_state
#define MEDIA_ROOT test_media
#define LIB_ROOT test_library
#define LEGACY_LIB_ROOT test_legacy_library
#define RUN_ROOT test_runtime
#define UTAB_DIR test_utab
// after_mount seams: a bind mount is neither HFS+ nor a block device.
static char test_hfs_target[PATH_MAX],test_rb_bridge[PATH_MAX];
static int test_hfs_readonly(const char *target){return *test_hfs_target&&!strcmp(target,test_hfs_target);}
static void observe_prepare_view(void);
static int test_volume_identity(const char *source,char out[80]){(void)source;observe_prepare_view();snprintf(out,80,"uuid-hfs-stick");return 0;}
static int test_bridge_hangs;
static const char *test_rb_bridge_path(void);
#define HFS_READONLY test_hfs_readonly
#define VOLUME_IDENTITY test_volume_identity
#define RB_BRIDGE_PATH test_rb_bridge_path
// Where engine-guardian is looked for: empty (none, nothing of the host's is run) until a test places one.
static char test_guardian_system[PATH_MAX],test_guardian_bundle[PATH_MAX];
#define GUARDIAN_SYSTEM test_guardian_system
#define GUARDIAN_BUNDLE test_guardian_bundle
// The "real" libmount mount: a bind of test_disk onto the target.
struct libmnt_context;
static int test_real_mount(struct libmnt_context *c);
#define REAL_MNT_CONTEXT_MOUNT() test_real_mount
// Leaves 1.5 s of the 20 s budget for the private conversion.
#include <stdint.h>
static int64_t test_post_reserve=18500;
static int test_expire_gen2_stage;
static int64_t test_post_reserve_fn(void);
#define POST_RESERVE_MS test_post_reserve_fn()
// Free space kept on the cache filesystem: none, except in the test that sets it.
static uint64_t test_cache_reserve;
#define HFS_CACHE_RESERVE test_cache_reserve
// Free space left on a stick after a carry-over: none, so a tiny tmpfs stick fills up mid-copy.
static uint64_t test_carry_slack;
#define CARRY_SLACK test_carry_slack
#include <errno.h>
#include <sys/stat.h>
static char test_eio_path[PATH_MAX];
static int test_eio_err=EIO;
static int test_lstat(const char *path,struct stat *st){if(*test_eio_path&&!strcmp(path,test_eio_path)){errno=test_eio_err;return -1;}return lstat(path,st);}
#define LSTAT test_lstat
#include <sys/utsname.h>
#include <unistd.h>
struct libmnt_context;
static int test_real_mount(struct libmnt_context *c);
static int test_reset_context(struct libmnt_context *c);
static int test_set_source(struct libmnt_context *c,const char *src);
static int test_set_target(struct libmnt_context *c,const char *tgt);
static int test_disable_helpers(struct libmnt_context *c,int disable);
static int test_set_fstype_pattern(struct libmnt_context *c,const char *pat);
static int test_set_fstype(struct libmnt_context *c,const char *ft);
static int test_set_options(struct libmnt_context *c,const char *opts);
static int test_enable_lazy(struct libmnt_context *c,int enable);
static int test_get_mflags(struct libmnt_context *c,unsigned long *flags);
static int test_set_mflags(struct libmnt_context *c,unsigned long flags);
static void test_free_context(struct libmnt_context *c);
static int test_sys_mount(const char *src,const char *tgt,const char *ft,unsigned long flags,const void *data);
static int test_sys_fsopen(const char *fsname,unsigned int flags);
static int test_sys_fsconfig(int fd,unsigned int cmd,const char *key,const void *val,int aux);
#define REAL_MNT_RESET_CONTEXT() test_reset_context
#define REAL_MNT_CONTEXT_SET_SOURCE() test_set_source
#define REAL_MNT_CONTEXT_SET_TARGET() test_set_target
#define REAL_MNT_CONTEXT_DISABLE_HELPERS() test_disable_helpers
#define REAL_MNT_CONTEXT_SET_FSTYPE_PATTERN() test_set_fstype_pattern
#define REAL_MNT_CONTEXT_SET_FSTYPE() test_set_fstype
#define REAL_MNT_CONTEXT_SET_OPTIONS() test_set_options
#define REAL_MNT_CONTEXT_ENABLE_LAZY() test_enable_lazy
#define REAL_MNT_CONTEXT_GET_MFLAGS() test_get_mflags
#define REAL_MNT_CONTEXT_SET_MFLAGS() test_set_mflags
#define REAL_MNT_FREE_CONTEXT() test_free_context
#define REAL_MOUNT() test_sys_mount
#define REAL_FSOPEN() test_sys_fsopen
#define REAL_FSCONFIG() test_sys_fsconfig
static int test_platform_mode_val = 0;
#define HFS_PLATFORM_MODE() test_platform_mode_val
static char test_fsck_bin[PATH_MAX],test_lklfuse_bin[PATH_MAX],test_gen1_marker[PATH_MAX],test_az04_dt[PATH_MAX];
static char test_proc_fs[PATH_MAX],test_proc_mi[PATH_MAX],test_modules_root[PATH_MAX],test_hfs_rw_target[PATH_MAX];
static const char *test_fsck_path(void){return *test_fsck_bin&&!access(test_fsck_bin,X_OK)?test_fsck_bin:NULL;}
static const char *test_lklfuse_path(void){return *test_lklfuse_bin&&!access(test_lklfuse_bin,X_OK)?test_lklfuse_bin:NULL;}
#define FSCK_HFSPLUS_PATH test_fsck_path
#define LKLFUSE_PATH test_lklfuse_path
#ifdef __SANITIZE_ADDRESS__
#define LKLFUSE_AS_LIMIT_BYTES 0
#endif
#define GEN1_ROOTFS_MARKER test_gen1_marker
#define AZ04_UFSD_DT_PATH test_az04_dt
#define PROC_FILESYSTEMS_PATH test_proc_fs
#define PROC_MOUNTINFO_PATH (*test_proc_mi?test_proc_mi:"/proc/self/mountinfo")
static char test_sys_dev_block[PATH_MAX];
#define SYS_DEV_BLOCK_DIR test_sys_dev_block
#define HFSPLUSX_MODULES_ROOT test_modules_root
static char test_uname_release[65] = "6.18.57-az01-2026-10-15-rt6";
static int test_uname(struct utsname *u){memset(u,0,sizeof *u);snprintf(u->release,sizeof u->release,"%s",test_uname_release);return 0;}
#define HOOK_UNAME test_uname
static int test_finit_rc = 0, test_finit_errno = 0, test_finit_calls = 0;
static int test_finit_module(int fd){
    (void)fd;test_finit_calls++;
    if(test_finit_rc){errno=test_finit_errno?test_finit_errno:ENOEXEC;return -1;}
    if(*test_proc_fs){FILE *f=fopen(test_proc_fs,"a");if(f){fprintf(f,"\thfsplusx\n");fclose(f);}}
    return 0;
}
#define FINIT_MODULE(fd) test_finit_module(fd)
static int test_hfs_rw_mounted(const char *target){return *test_hfs_rw_target&&target&&!strcmp(target,test_hfs_rw_target);}
#define HFS_RW_MOUNTED test_hfs_rw_mounted
static int test_dev_present_val = 1;
static int test_hfs_dev_present(const char *target,const char *src){(void)target;(void)src;return test_dev_present_val;}
#define HFS_DEV_PRESENT test_hfs_dev_present
#include "edisksd_hook.c"
static int64_t test_post_reserve_fn(void) {
    if(test_expire_gen2_stage) {
        FILE *f=fopen("/proc/self/mountinfo","r");
        if(f) {
            char line[4096],needle[PATH_MAX+8];
            snprintf(needle,sizeof needle,"%s/lkl-",test_runtime);
            int staged=0;
            while(fgets(line,sizeof line,f))if(strstr(line,needle)){staged=1;break;}
            fclose(f);
            if(staged)return HOOK_BUDGET_MS+1;
        }
    }
    return test_post_reserve;
}
#include <assert.h>
#include <ftw.h>
static int test_mock_hfs_syscalls,test_fsconfig_ro_seen,test_fsconfig_rw_seen;
static unsigned long test_last_sys_flags;
static char test_last_sys_fstype[64];
static int test_sys_mount(const char *src,const char *tgt,const char *ft,unsigned long flags,const void *data){
    if(test_mock_hfs_syscalls&&ft&&(!strcmp(ft,"hfsplus")||!strcmp(ft,"hfsplusx"))){
        test_last_sys_flags=flags;
        snprintf(test_last_sys_fstype,sizeof test_last_sys_fstype,"%s",ft);
        return 0;
    }
    int (*real)(const char *,const char *,const char *,unsigned long,const void *)=dlsym(RTLD_NEXT,"mount");
    return real?real(src,tgt,ft,flags,data):-1;
}
static int test_sys_fsopen(const char *fsname,unsigned int flags){
    if(test_mock_hfs_syscalls){(void)fsname;(void)flags;return 777;}
    int (*real)(const char *,unsigned int)=dlsym(RTLD_NEXT,"fsopen");
    return real?real(fsname,flags):-1;
}
static int test_sys_fsconfig(int fd,unsigned int cmd,const char *key,const void *val,int aux){
    if(test_mock_hfs_syscalls&&fd==777){
        if(cmd==FSCONFIG_SET_FLAG&&key&&!strcmp(key,"ro"))test_fsconfig_ro_seen++;
        if(cmd==FSCONFIG_SET_FLAG&&key&&!strcmp(key,"rw"))test_fsconfig_rw_seen++;
        return 0;
    }
    int (*real)(int,unsigned int,const char *,const void *,int)=dlsym(RTLD_NEXT,"fsconfig");
    return real?real(fd,cmd,key,val,aux):-1;
}
struct libmnt_context {
    const char *target,*source;
    char target_buf[PATH_MAX],source_buf[PATH_MAX];
    int status,fail,fail_rw_once,lazy,disable_helpers,mount_failed,invoke_sys_mount;
    unsigned long mflags,last_mount_mflags;
    const char *disk;
    char fstype_pattern[64],fstype[64],options[512];
    char last_mount_fstype[64],last_mount_opts[512];
    int last_umount_lazy,last_mount_disable_helpers;
    unsigned mount_calls;
};
const char *mnt_context_get_target(struct libmnt_context *c){return c->target;}
const char *mnt_context_get_source(struct libmnt_context *c){return c->source;}
int mnt_context_get_status(struct libmnt_context *c){return c->status;}
int mnt_context_set_syscall_status(struct libmnt_context *c,int status){c->status=status==0?1:status;return 0;}
static int test_reset_context(struct libmnt_context *c){
    c->target=NULL;c->source=NULL;c->target_buf[0]=0;c->source_buf[0]=0;
    c->fstype_pattern[0]=0;c->fstype[0]=0;c->options[0]=0;
    c->status=0;c->lazy=0;c->disable_helpers=0;c->mflags=0;c->mount_failed=0;
    return 0;
}
static int test_set_source(struct libmnt_context *c,const char *src){snprintf(c->source_buf,sizeof c->source_buf,"%s",src?src:"");c->source=src?c->source_buf:NULL;return 0;}
static int test_set_target(struct libmnt_context *c,const char *tgt){snprintf(c->target_buf,sizeof c->target_buf,"%s",tgt?tgt:"");c->target=tgt?c->target_buf:NULL;return 0;}
static int test_disable_helpers(struct libmnt_context *c,int disable){c->disable_helpers=disable;return 0;}
static int test_set_fstype_pattern(struct libmnt_context *c,const char *pat){snprintf(c->fstype_pattern,sizeof c->fstype_pattern,"%s",pat?pat:"");return 0;}
static int test_set_fstype(struct libmnt_context *c,const char *ft){snprintf(c->fstype,sizeof c->fstype,"%s",ft?ft:"");return 0;}
static int test_set_options(struct libmnt_context *c,const char *opts){snprintf(c->options,sizeof c->options,"%s",opts?opts:"");return 0;}
static int test_enable_lazy(struct libmnt_context *c,int enable){c->lazy=enable;return 0;}
static int test_get_mflags(struct libmnt_context *c,unsigned long *flags){*flags=c->mflags;return 0;}
static int test_set_mflags(struct libmnt_context *c,unsigned long flags){c->mflags=flags;return 0;}
static void test_free_context(struct libmnt_context *c){(void)c;}
static const char *test_rb_bridge_path(void){while(test_bridge_hangs)pause();return test_rb_bridge;}
static char test_disk[PATH_MAX];static int test_disk_ro;
// Set around a plain empty stick's mount: the flag must stay unpublished while it runs.
static int observing_not_preparing;
static void expect_not_preparing(void);
static int test_real_mount(struct libmnt_context *c) {
    if(observing_not_preparing)expect_not_preparing();
    c->mount_calls++;
    if(c->mount_failed){c->status=0;return 0;} // real libmount skips mounting if context was not reset after a failed mount
    snprintf(c->last_mount_fstype,sizeof c->last_mount_fstype,"%s",*c->fstype?c->fstype:c->fstype_pattern);
    snprintf(c->last_mount_opts,sizeof c->last_mount_opts,"%s",c->options);
    c->last_mount_disable_helpers=c->disable_helpers;
    c->last_mount_mflags=c->mflags;
    if(c->fail_rw_once&&has_opt_token(c->options,"rw")){c->fail_rw_once--;c->mount_failed=1;c->status=0;errno=EINVAL;return -EINVAL;}
    if(c->invoke_sys_mount) {
        test_mock_hfs_syscalls=1;test_fsconfig_ro_seen=0;test_fsconfig_rw_seen=0;test_last_sys_flags=0;test_last_sys_fstype[0]=0;
        mount(c->source,c->target,c->last_mount_fstype,c->mflags,c->options);
        int fd=fsopen(c->last_mount_fstype,FSOPEN_CLOEXEC);
        if(fd>=0){fsconfig(fd,FSCONFIG_SET_FLAG,"rw",NULL,0);fsconfig(fd,FSCONFIG_CMD_CREATE,NULL,NULL,0);}
        test_mock_hfs_syscalls=0;
    }
    if(mount(c->disk?c->disk:test_disk,c->target,NULL,MS_BIND,NULL)){c->mount_failed=1;return -errno;}
    // As libmount does for a mount with a userspace option: a line in <UTAB_DIR>/utab.
    char utab[PATH_MAX+8];snprintf(utab,sizeof utab,"%s/utab",test_utab);
    int fd=open(utab,O_WRONLY|O_CREAT|O_APPEND|O_CLOEXEC,0600);
    if(fd>=0){if(write(fd,c->target,strlen(c->target))<0||write(fd,"\n",1)<0){}close(fd);}
    if((test_disk_ro||has_opt_token(c->options,"ro"))&&mount(NULL,c->target,NULL,MS_BIND|MS_REMOUNT|MS_RDONLY,NULL))return -errno;
    c->status=1;return 0;
}
static unsigned real_calls;
static int real_unmount(struct libmnt_context *c) {
    real_calls++;
    c->last_umount_lazy=c->lazy;
    if(c->fail){c->status=0;errno=EBUSY;return -EBUSY;}
    int rc=umount2(c->target,UMOUNT_NOFOLLOW);
    c->status=(rc==0)?1:0;
    return rc;
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
    // r2 left-deck bug: an older build made hfs-libraries 0755 and every HFS+ stick failed EACCES (no playlists).
    char loose[PATH_MAX];join(loose,root,"loose-data");join(p,loose,"enginex/hfs-libraries");assert(!make_dirs(p));assert(!chmod(p,0755));
    join(test_library,loose,"enginex/hfs-libraries");test_legacy_library[0]=0;
    assert(!seed_library(volume_path,"uuid-a",again,deadline_from(now_ms())));
    assert(!lstat(p,&st)&&(st.st_mode&0777)==0700);
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
// #182 review: the "too large, skip until re-exported" record is keyed by the
// export files. exportOneLibrary.db, which rb-bridge also converts from, was not
// among them: a stick skipped once stayed skipped after a new export to that file.
static void test_slow_marker_follows_every_export_file(const char *root) {
    const char *names[]={"export.pdb","exportLibrary.db","exportOneLibrary.db"};
    for(unsigned hidden=0;hidden<2;hidden++)for(unsigned i=0;i<3;i++) {
        char stick[PATH_MAX],dir[PATH_MAX],p[PATH_MAX],before[PATH_MAX],after[PATH_MAX],name[32];
        snprintf(name,sizeof name,"marker-%u-%u",hidden,i);join(stick,root,name);
        join(dir,stick,hidden?".PIONEER/rekordbox":"PIONEER/rekordbox");assert(!make_dirs(dir));
        join(p,dir,names[i]);write_file(p,"first export");assert(!slow_marker(stick,before));
        write_file(p,"exported again, larger");assert(!slow_marker(stick,after));
        assert(strcmp(before,after));
    }
    puts("slow-library record changes with export.pdb, exportLibrary.db and exportOneLibrary.db, plain and hidden: PASS");
}
// BH2-A1-7: any PIONEER folder made a stick a rekordbox stick, which is converted
// and not healed. Only an export rb-bridge converts from does.
static void test_only_an_export_makes_a_rekordbox_stick(const char *root) {
    char stick[PATH_MAX],p[PATH_MAX],pdb[PATH_MAX],marker[PATH_MAX],stamp[64];struct stat st;
    join(stick,root,"export-or-not");join(p,stick,"Engine Library/Database2");assert(!make_dirs(p));
    assert(!has_export(stick));
    join(p,stick,"PIONEER");assert(!make_dirs(p));join(p,stick,"PIONEER/MYSETTING.DAT");write_file(p,"player settings");
    assert(!has_export(stick)); // what a CDJ or a mixer leaves on any stick
    join(p,stick,"PIONEER/rekordbox");assert(!make_dirs(p));join(p,stick,"PIONEER/rekordbox/export.pdb");assert(!make_dirs(p));
    assert(!has_export(stick)); // no export file
    assert(!rmdir(p));join(pdb,stick,"PIONEER/rekordbox/export.pdb");write_file(pdb,"written by EngineX for CDJ link");
    assert(has_export(stick));
    // The export EngineX wrote itself (pkg/prolink GeneratedExport): rb-bridge does not convert from it.
    assert(!lstat(pdb,&st));snprintf(stamp,sizeof stamp,"%lld:%lld",(long long)st.st_size,(long long)st.st_mtim.tv_sec*1000000000LL+st.st_mtim.tv_nsec);
    join(marker,stick,"PIONEER/rekordbox/.enginex-generated");write_file(marker,stamp);
    assert(!has_export(stick));
    join(p,stick,"PIONEER/rekordbox/exportOneLibrary.db");write_file(p,"rekordbox 7");assert(has_export(stick));assert(!unlink(p));
    assert(!has_export(stick));
    write_file(pdb,"exported again from rekordbox, the stamp is stale");assert(has_export(stick));
    // Hidden folder and other spellings, as on FAT/exFAT/HFS+ and in rb-bridge's resolver.
    for(unsigned i=0;i<3;i++) {
        const char *dirs[]={".PIONEER/rekordbox","pioneer/Rekordbox",".Pioneer/REKORDBOX"},*files[]={"exportLibrary.db","EXPORT.PDB","ExportOneLibrary.DB"};
        char other[PATH_MAX],name[32];snprintf(name,sizeof name,"export-spelling-%u",i);join(other,root,name);
        join(p,other,dirs[i]);assert(!make_dirs(p));assert(!has_export(other));
        char file[PATH_MAX];join(file,p,files[i]);write_file(file,"export");assert(has_export(other));
    }
    puts("BH2-A1-7: a rekordbox stick is one with exportLibrary.db, exportOneLibrary.db or a rekordbox export.pdb (plain, hidden, any case); a bare PIONEER folder or EngineX's own CDJ-link export is not: PASS");
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
static unsigned count_mounts(const char *),count_views(void);
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
    // edisksd shows Engine only what this returns: it was -1, "Operation not permitted".
    int held=open(song,O_RDONLY);assert(held>=0);real_calls=0;
    assert(unmount_dispatch(&c,real_unmount)==-EBUSY&&errno==EBUSY&&real_calls==0);
    expect_file(lib,"deck cue retained");expect_file(song,"audio");close(held);
    // A busy presentation root also restores all detached children.
    held=open(target,O_RDONLY|O_DIRECTORY);assert(held>=0);real_calls=0;
    assert(unmount_dispatch(&c,real_unmount)==-EBUSY&&errno==EBUSY&&real_calls==0);
    expect_file(lib,"deck cue retained");expect_file(song,"audio");close(held);
    // Real parent failure after removing our view must restore it atomically
    // from the already cloned rescue tree, not reconstruct an empty library (#23),
    // also when Engine holds the library's database and its bind was detached.
    c.fail=1;real_calls=0;held=open(lib,O_RDONLY);assert(held>=0);
    assert(unmount_dispatch(&c,real_unmount)!=0&&errno==EBUSY&&real_calls==1);
    assert(mount_id(target)!=raw_id);expect_file(lib,"deck cue retained");expect_file(song,"audio");close(held);
    struct owned_view *reloaded=load_view(target);assert(reloaded);free(reloaded); // daemon restart ownership recovery
    // Engine keeps its library database open across Eject (left deck, 2026-11-14). That file is
    // the cached library's on /data, not the stick's: the stick is ejected, no view part stays.
    unsigned views=count_views();held=open(lib,O_RDONLY);assert(held>=0);
    c.fail=0;assert(!unmount_dispatch(&c,real_unmount)&&count_mounts(target)==0&&count_views()==views-1);close(held);
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
    join(disk,root,"hfs-disk");assert(!make_dirs(disk));join(p,disk,".PIONEER/rekordbox");assert(!make_dirs(p));join(p,disk,".PIONEER/rekordbox/export.pdb");write_file(p,"library");
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
    join(disk,root,"first-fat");join(p,disk,"PIONEER/rekordbox");assert(!make_dirs(p));join(p,disk,"PIONEER/rekordbox/export.pdb");write_file(p,"library");
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
    char hfs[PATH_MAX];join(hfs,root,"first-hfs");join(p,hfs,".PIONEER/rekordbox");assert(!make_dirs(p));join(p,hfs,".PIONEER/rekordbox/export.pdb");write_file(p,"library");
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
    char twice[sizeof expect*2];snprintf(twice,sizeof twice,"%s%s",expect,expect);expect_file(log,twice); // a new export is tried again
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
    join(disk,root,"once-rekordbox");join(p,disk,"PIONEER/rekordbox");assert(!make_dirs(p));join(p,disk,"PIONEER/rekordbox/export.pdb");write_file(p,"library");
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
    join(a,root,"pair-a");join(p,a,"PIONEER/rekordbox");assert(!make_dirs(p));join(p,a,"PIONEER/rekordbox/export.pdb");write_file(p,"library");
    join(b,root,"pair-b");join(p,b,"PIONEER/rekordbox");assert(!make_dirs(p));join(p,b,"PIONEER/rekordbox/export.pdb");write_file(p,"library");
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
    join(disk,root,"reuse-hfs");join(p,disk,".PIONEER/rekordbox");assert(!make_dirs(p));join(p,disk,".PIONEER/rekordbox/export.pdb");write_file(p,"library");
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
    join(disk,root,"clean-stick");join(p,disk,".PIONEER/rekordbox");assert(!make_dirs(p));join(p,disk,".PIONEER/rekordbox/export.pdb");write_file(p,"library");
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
// BH2-A1-1: a second-generation deck keeps the vendor's rootfs and has
// engine-guardian only in the data bundle; the hook asked for /usr/local/bin
// alone, so an Engine stick was never healed at mount on those decks.
static void test_guardian_from_the_data_bundle(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char disk[PATH_MAX],target[PATH_MAX],p[PATH_MAX],log[PATH_MAX],script[PATH_MAX*2],want[PATH_MAX*2];
    join(disk,root,"heal-native");join(p,disk,"Engine Library/Database2");assert(!make_dirs(p));
    join(p,disk,"Engine Library/Database2/m.db");write_file(p,"native library");
    join(target,MEDIA_ROOT,"HEAL");assert(!make_dirs(target));join(log,root,"heal.log");
    join(test_guardian_system,root,"heal-system-guardian");join(test_guardian_bundle,root,"heal-bundle-guardian");
    snprintf(script,sizeof script,"#!/bin/sh\necho \"bundle $*\" >> '%s'\n",log);write_file(test_guardian_bundle,script);assert(!chmod(test_guardian_bundle,0700));
    for(int rootfs=0;rootfs<2;rootfs++) { // the bundle's alone, then next to the rootfs's own, which is preferred
        if(rootfs){snprintf(script,sizeof script,"#!/bin/sh\necho \"system $*\" >> '%s'\n",log);write_file(test_guardian_system,script);assert(!chmod(test_guardian_system,0700));}
        snprintf(test_disk,sizeof test_disk,"%s",disk);test_disk_ro=0;
        struct libmnt_context c={.target=target,.source="/dev/sdx1"};
        assert(!mnt_context_mount(&c)&&c.status==1);
        snprintf(want,sizeof want,"%s --heal-mount %s --data-only\n",rootfs?"system":"bundle",target);
        expect_file(log,want);assert(!unlink(log));
        assert(!umount2(target,UMOUNT_NOFOLLOW));
    }
    // BH2-A1-7: the same stick after a player left its settings folder on it is still healed, not converted.
    join(p,disk,"PIONEER");assert(!make_dirs(p));join(p,disk,"PIONEER/MYSETTING.DAT");write_file(p,"player settings");
    join(test_rb_bridge,root,"heal-bridge");snprintf(script,sizeof script,"#!/bin/sh\necho \"bridge $*\" >> '%s'\n",log);write_file(test_rb_bridge,script);assert(!chmod(test_rb_bridge,0700));
    struct libmnt_context settings={.target=target,.source="/dev/sdx1"};
    assert(!mnt_context_mount(&settings)&&settings.status==1);
    snprintf(want,sizeof want,"system --heal-mount %s --data-only\n",target);expect_file(log,want);
    assert(!umount2(target,UMOUNT_NOFOLLOW));
    test_guardian_system[0]=test_guardian_bundle[0]=0;
    puts("REAL PRIVATE NAMESPACE BH2-A1-1: an Engine stick is healed by the data bundle's engine-guardian where the rootfs has none, by the rootfs's where it has; BH2-A1-7: also with a player's settings folder on it: PASS");
}
// BH2-A1-2: the "too large" record was keyed by f_fsid, on FAT/exFAT/HFS+ the
// device number. Two mounts of one export under different filesystem ids stand
// for one stick in two ports (sda1, then sdb1).
static void test_slow_marker_survives_another_port(const char *root) {
    char mnt[2][PATH_MAX],marker[2][PATH_MAX],other[PATH_MAX],p[PATH_MAX];struct statvfs v[2];
    struct timespec exported[2]={{.tv_sec=1700000000,.tv_nsec=120000000},{.tv_sec=1700000000,.tv_nsec=120000000}};
    for(int i=0;i<2;i++) {
        join(mnt[i],root,i?"port-b":"port-a");assert(!make_dirs(mnt[i]));
        assert(!mount("stick",mnt[i],"tmpfs",0,"size=1m,mode=0700"));
        join(p,mnt[i],"PIONEER/rekordbox");assert(!make_dirs(p));join(p,mnt[i],"PIONEER/rekordbox/export.pdb");write_file(p,"big library");
        assert(!utimensat(AT_FDCWD,p,exported,0));assert(!statvfs(mnt[i],&v[i]));assert(!slow_marker(mnt[i],marker[i]));
    }
    assert(memcmp(&v[0].f_fsid,&v[1].f_fsid,sizeof v[0].f_fsid)&&v[0].f_blocks==v[1].f_blocks); // what a second port looks like
    assert(!strcmp(marker[0],marker[1]));
    write_file(p,"exported again");assert(!slow_marker(mnt[1],other));assert(strcmp(marker[0],other)); // a new export is still looked at
    for(int i=0;i<2;i++)assert(!umount2(mnt[i],UMOUNT_NOFOLLOW));
    puts("REAL PRIVATE NAMESPACE BH2-A1-2: the too-large record of a stick is found again under another device number, and not after a new export: PASS");
}
// r2 left deck: edisksd announced the bare read-only HFS+ mount before the view
// covered it, Engine chose OneLibrary and its migration failed EROFS. The
// monitor's mountinfo must show a view as the plain device mount, nothing else.
static void test_monitor_sees_plain_device_mount(const char *root) {
    char path[PATH_MAX],line[4096],out[16384];join(path,root,"mountinfo");
    FILE *f=fopen(path,"w");assert(f);
    fprintf(f,"25 1 0:21 / /run rw,nosuid,nodev shared:5 - tmpfs tmpfs rw,mode=755\n");
    fprintf(f,"78 33 8:1 / /media/vivi rw,nosuid,nodev,noexec,relatime,nosymfollow shared:239 - hfsplus /dev/sda1 ro,umask=22\n");
    fprintf(f,"79 33 8:17 / /media/vivid rw,relatime shared:240 - vfat /dev/sdb1 rw\n");
    fprintf(f,"260 25 0:21 /x/view-M8w87C %s/view-M8w87C rw,nosuid,nodev - tmpfs tmpfs rw,mode=755\n",test_runtime);
    fprintf(f,"268 260 8:1 / %s/view-M8w87C/backing rw,nosuid - hfsplus /dev/sda1 ro,umask=22\n",test_runtime);
    fprintf(f,"269 260 0:46 / %s/view-M8w87C/view ro,nosuid - tmpfs sc5000-hfs-view ro,size=16384k,mode=755\n",test_runtime);
    fprintf(f,"274 269 8:1 /.PIONEER %s/view-M8w87C/view/.PIONEER rw,nosuid - hfsplus /dev/sda1 ro,umask=22\n",test_runtime);
    fprintf(f,"303 269 179:39 /enginex/hfs-libraries/u/library-0 %s/view-M8w87C/view/Engine\\040Library rw,relatime shared:43 - ext4 /dev/mmcblk0p7 rw\n",test_runtime);
    fprintf(f,"304 78 0:46 / /media/vivi ro,nosuid,nodev,noexec,relatime shared:272 - tmpfs sc5000-hfs-view ro,size=16384k,mode=755\n");
    fprintf(f,"309 304 8:1 /.PIONEER /media/vivi/.PIONEER rw,nosuid shared:277 - hfsplus /dev/sda1 ro,umask=22\n");
    fprintf(f,"311 304 179:39 /enginex/hfs-libraries/u/library-0 /media/vivi/Engine\\040Library rw,relatime shared:43 - ext4 /dev/mmcblk0p7 rw\n");
    fclose(f);
    int fd=hide_views(path);assert(fd>=0);
    ssize_t n=pread(fd,out,sizeof out-1,0);assert(n>0);out[n]=0;close(fd);
    assert(strstr(out,"78 33 8:1 / /media/vivi rw"));   // the device mount Engine expects
    assert(strstr(out,"79 33 8:17 / /media/vivid "));    // a sibling whose name extends the target is kept
    assert(strstr(out,"25 1 0:21 / /run "));
    assert(!strstr(out,"sc5000-hfs-view")&&!strstr(out,"view-M8w87C")&&!strstr(out,"/media/vivi/"));
    assert(!strstr(out,"mmcblk0p7"));                   // /data never appears as the stick's library mount
    int lines=0;for(char *p=out;*p;p++)lines+=*p=='\n';assert(lines==3);
    // Nothing to hide: the monitor parses the kernel's file itself.
    f=fopen(path,"w");assert(f);fprintf(f,"78 33 8:1 / /media/vivi rw - hfsplus /dev/sda1 ro\n");fclose(f);
    assert(hide_views(path)==-1);
    (void)line;
    puts("r2 HFS view: edisksd's monitor sees only the plain device mount, never the bare mount before the view: PASS");
}
// #234: this hook names its view tmpfs enginex-hfs-view; an in-place update from an older release
// can restart edisksd under it while views the old hook mounted (sc5000-hfs-view, helper mounts in
// /run/sc5000-hfs-views) are still there. The monitor sees neither name, only the device mounts.
static void test_monitor_hides_new_and_old_view_names(const char *root) {
    char path[PATH_MAX],out[16384];join(path,root,"mountinfo-234");
    FILE *f=fopen(path,"w");assert(f);
    fprintf(f,"78 33 8:1 / /media/vivi rw,nosuid,nodev,noexec,relatime,nosymfollow shared:239 - hfsplus /dev/sda1 ro,umask=22\n");
    fprintf(f,"79 33 8:17 / /media/old rw,nosuid,nodev,noexec,relatime,nosymfollow shared:241 - hfsplus /dev/sdb1 ro,umask=22\n");
    fprintf(f,"260 25 0:21 /x/view-AAAAAA %s/view-AAAAAA rw,nosuid,nodev - tmpfs tmpfs rw,mode=755\n",test_runtime);
    fprintf(f,"269 260 0:46 / %s/view-AAAAAA/view ro,nosuid - tmpfs enginex-hfs-view ro,size=16384k,mode=755\n",test_runtime);
    fprintf(f,"304 78 0:46 / /media/vivi ro,nosuid,nodev,noexec,relatime shared:272 - tmpfs enginex-hfs-view ro,size=16384k,mode=755\n");
    fprintf(f,"309 304 8:1 /.PIONEER /media/vivi/.PIONEER rw,nosuid shared:277 - hfsplus /dev/sda1 ro,umask=22\n");
    fprintf(f,"360 25 0:22 /x/view-BBBBBB /run/sc5000-hfs-views/view-BBBBBB rw,nosuid,nodev - tmpfs tmpfs rw,mode=755\n");
    fprintf(f,"361 360 8:17 / /run/sc5000-hfs-views/view-BBBBBB/backing rw,nosuid - hfsplus /dev/sdb1 ro,umask=22\n");
    fprintf(f,"362 79 0:47 / /media/old ro,nosuid,nodev,noexec,relatime shared:273 - tmpfs sc5000-hfs-view ro,size=16384k,mode=755\n");
    fprintf(f,"363 362 8:17 /.PIONEER /media/old/.PIONEER rw,nosuid shared:278 - hfsplus /dev/sdb1 ro,umask=22\n");
    fclose(f);
    int fd=hide_views(path);assert(fd>=0);
    ssize_t n=pread(fd,out,sizeof out-1,0);assert(n>0);out[n]=0;close(fd);
    assert(strstr(out,"78 33 8:1 / /media/vivi rw")&&strstr(out,"79 33 8:17 / /media/old rw"));
    assert(!strstr(out,"hfs-view")&&!strstr(out,"view-AAAAAA")&&!strstr(out,"view-BBBBBB")&&!strstr(out,"/.PIONEER"));
    int lines=0;for(char *p=out;*p;p++)lines+=*p=='\n';assert(lines==2);
    // Stacked direct root-bind overlay (constant-mount RO view) is also hidden while the base device mount stays.
    f=fopen(path,"w");assert(f);
    fprintf(f,"78 33 8:1 / /media/vivi rw,nosuid,nodev,noexec,relatime,nosymfollow shared:239 - hfsplus /dev/sda1 ro,umask=22\n");
    fprintf(f,"79 33 8:17 / /media/vivid rw,relatime shared:240 - vfat /dev/sdb1 rw\n");
    fprintf(f,"260 25 0:21 /x/view-CCCCCC %s/view-CCCCCC rw,nosuid,nodev - tmpfs tmpfs rw,mode=755\n",test_runtime);
    fprintf(f,"268 260 8:1 / %s/view-CCCCCC/backing rw,nosuid - hfsplus /dev/sda1 ro,umask=22\n",test_runtime);
    fprintf(f,"269 260 8:1 / %s/view-CCCCCC/view rw,nosuid - hfsplus /dev/sda1 ro,umask=22\n",test_runtime);
    fprintf(f,"304 78 8:1 / /media/vivi rw,nosuid - hfsplus /dev/sda1 ro,umask=22\n");
    fprintf(f,"305 304 179:39 /enginex/hfs-libraries/u/library-0 /media/vivi/Engine\\040Library rw - ext4 /dev/mmcblk0p7 rw\n");
    fprintf(f,"306 305 8:1 /Engine\\040Library/Music /media/vivi/Engine\\040Library/Music rw,nosuid - hfsplus /dev/sda1 ro\n");
    fclose(f);
    fd=hide_views(path);assert(fd>=0);
    n=pread(fd,out,sizeof out-1,0);assert(n>0);out[n]=0;close(fd);
    assert(strstr(out,"78 33 8:1 / /media/vivi rw")&&strstr(out,"79 33 8:17 / /media/vivid rw"));
    assert(!strstr(out,"304 78")&&!strstr(out,"view-CCCCCC")&&!strstr(out,"Engine\\040Library")&&!strstr(out,"mmcblk0p7"));
    lines=0;for(char *p=out;*p;p++)lines+=*p=='\n';assert(lines==2);
    puts("#234 HFS view: the monitor hides views under the new and the pre-5.3.0.5 name: PASS");
}
static void write_bytes(const char *path,size_t n) {
    int fd=open(path,O_WRONLY|O_CREAT|O_TRUNC|O_NOFOLLOW|O_CLOEXEC,0600);assert(fd>=0);
    static char block[65536];memset(block,'a',sizeof block);
    for(size_t done=0;done<n;){size_t k=n-done<sizeof block?n-done:sizeof block;assert(write(fd,block,k)==(ssize_t)k);done+=k;}
    assert(!close(fd));
}
static uint64_t tree_bytes;
static int add_size(const char *path,const struct stat *st,int type,struct FTW *ftw){(void)path;(void)ftw;if(type==FTW_F)tree_bytes+=(uint64_t)st->st_size;return 0;}
static uint64_t tree_size(const char *path){tree_bytes=0;assert(!nftw(path,add_size,16,FTW_PHYS));return tree_bytes;}
static unsigned count_generations(const char *dir) {
    DIR *d=opendir(dir);if(!d)return 0;unsigned n=0;struct dirent *e;
    while((e=readdir(d)))n+=seeded_ok(e->d_name);
    closedir(d);return n;
}
// BH-O55-1 (BH-F52-3, BH-O55-2, BH-G1-6): Engine DJ puts the audio in
// <stick>/Engine Library/Music. The view copied the whole Engine Library into
// /data on every insert (a real collection never finished inside the budget),
// kept one full copy per PC export for good, and had no free-space floor.
static void test_hfs_view_copies_only_metadata(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char data[PATH_MAX],p[PATH_MAX],disk[PATH_MAX],target[PATH_MAX],cache[PATH_MAX],track[PATH_MAX],db[PATH_MAX];
    char saved_lib[PATH_MAX],saved_legacy[PATH_MAX];
    snprintf(saved_lib,sizeof saved_lib,"%s",test_library);snprintf(saved_legacy,sizeof saved_legacy,"%s",test_legacy_library);
    // A small /data of its own, so the reserve is exact.
    join(data,root,"small-data");assert(!make_dirs(data));assert(!mount("small-data",data,"tmpfs",0,"mode=0700,size=24m"));
    join(p,data,"enginex");assert(!make_dirs(p));join(test_library,p,"hfs-libraries");test_legacy_library[0]=0;
    join(cache,test_library,"uuid-music");
    join(disk,root,"music-hfs");join(p,disk,"Engine Library/Database2");assert(!make_dirs(p));
    join(db,p,"m.db");write_file(db,"engine dj export 1");
    join(p,disk,"Engine Library/Music/Artist");assert(!make_dirs(p));join(track,p,"track.mp3");write_bytes(track,8u<<20);
    join(p,disk,"Engine Library/notes.txt");write_file(p,"pc notes");
    join(target,MEDIA_ROOT,"MUSIC-HFS");mount_ro(disk,target);
    assert(!prepare_view(target,"uuid-music",deadline_from(now_ms())));
    // The audio is shown from the stick, read-only; nothing of it is in /data.
    struct stat st;join(p,target,"Engine Library/Music/Artist/track.mp3");assert(!stat(p,&st)&&st.st_size==(8<<20));
    assert(open(p,O_WRONLY)<0&&errno==EROFS);
    join(p,target,"Engine Library/Music/new.mp3");assert(open(p,O_WRONLY|O_CREAT,0600)<0&&errno==EROFS);
    join(p,target,"Engine Library/notes.txt");expect_file(p,"pc notes");assert(open(p,O_WRONLY)<0&&errno==EROFS);
    assert(tree_size(cache)<4096);
    // Engine's metadata is the writable copy, and Engine may add next to it.
    join(p,target,"Engine Library/Database2/m.db");expect_file(p,"engine dj export 1");write_file(p,"deck cue");
    join(p,target,"Engine Library/deck-made");write_file(p,"x");
    // Eject: a busy track, and a failed real umount while Engine has its database open, put every bind back.
    struct libmnt_context c={.target=target};
    join(p,target,"Engine Library/Music/Artist/track.mp3");int held=open(p,O_RDONLY);assert(held>=0);real_calls=0;
    assert(unmount_dispatch(&c,real_unmount)==-EBUSY&&errno==EBUSY&&real_calls==0);close(held);
    join(p,target,"Engine Library/Database2/m.db");held=open(p,O_RDONLY);assert(held>=0);
    c.fail=1;assert(unmount_dispatch(&c,real_unmount)!=0&&errno==EBUSY&&real_calls==1);c.fail=0;close(held);
    join(p,target,"Engine Library/Music/Artist/track.mp3");assert(!stat(p,&st)&&st.st_size==(8<<20));
    join(p,target,"Engine Library/Database2/m.db");expect_file(p,"deck cue");
    // Engine's open database is the cached library's: the stick and its nested binds still go.
    held=open(p,O_RDONLY);assert(held>=0);assert(!unmount_dispatch(&c,real_unmount)&&count_mounts(target)==0);close(held);
    join(p,disk,"Engine Library/Database2/m.db");expect_file(p,"engine dj export 1");
    join(p,disk,"Engine Library/deck-made");assert(access(p,F_OK)==-1&&errno==ENOENT);
    // An older build's full copy in the cached library is replaced by the stick's own folder.
    char library[PATH_MAX];mount_ro(disk,target);assert(!seed_library(target,"uuid-music",library,deadline_from(now_ms())));
    join(p,library,"Music/Artist");assert(!make_dirs(p));join(p,library,"Music/Artist/track.mp3");write_bytes(p,8u<<20);
    seeded_hint[0]=0;assert(!prepare_view(target,"uuid-music",deadline_from(now_ms())));
    join(p,target,"Engine Library/Database2/m.db");expect_file(p,"deck cue");
    join(p,target,"Engine Library/Music/Artist/track.mp3");assert(!stat(p,&st)&&st.st_size==(8<<20));
    assert(tree_size(cache)<4096);
    assert(!unmount_dispatch(&c,real_unmount));
    // Every PC export is a new generation: the one in use and the one before it stay.
    for(int i=2;i<=4;i++) {
        char text[64];snprintf(text,sizeof text,"engine dj export %d",i);join(p,disk,"Engine Library/Database2/m.db");write_file(p,text);
        mount_ro(disk,target);assert(!prepare_view(target,"uuid-music",deadline_from(now_ms())));
        join(p,target,"Engine Library/Database2/m.db");expect_file(p,text);
        assert(count_generations(cache)==2);assert(!unmount_dispatch(&c,real_unmount));
    }
    // The reserve: another stick's unused 6 MiB library goes for a seed that needs
    // the room, but nothing goes when even that would not make it.
    char old[PATH_MAX];join(old,test_library,"uuid-gone/library-0123456789abcdef/Database2");assert(!make_dirs(old));
    join(p,old,"m.db");write_bytes(p,6u<<20);
    struct timespec long_ago[2]={{.tv_sec=1000000000},{.tv_sec=1000000000}};
    join(p,test_library,"uuid-gone/library-0123456789abcdef");assert(!utimensat(AT_FDCWD,p,long_ago,0));
    join(p,disk,"Engine Library/Database2/big.db");write_bytes(p,3u<<20);
    join(p,disk,"Engine Library/Database2/m.db");write_file(p,"engine dj export 5");
    struct statvfs v;assert(!statvfs(test_library,&v));uint64_t avail=(uint64_t)v.f_bavail*v.f_frsize;
    test_cache_reserve=avail+(64u<<20);mount_ro(disk,target);
    assert(prepare_view(target,"uuid-music",deadline_from(now_ms()))==-1&&errno==ENOSPC);
    assert(!access(old,F_OK)&&count_generations(cache)==2);
    test_cache_reserve=avail-(2u<<20); // 1 MiB short of the 3 MiB seed
    assert(!prepare_view(target,"uuid-music",deadline_from(now_ms())));
    join(p,test_library,"uuid-gone");assert(access(p,F_OK)==-1&&errno==ENOENT);
    join(p,target,"Engine Library/Database2/big.db");assert(!stat(p,&st)&&st.st_size==(3<<20));
    assert(!unmount_dispatch(&c,real_unmount));test_cache_reserve=0;
    assert(!umount2(data,MNT_DETACH));
    snprintf(test_library,sizeof test_library,"%s",saved_lib);snprintf(test_legacy_library,sizeof test_legacy_library,"%s",saved_legacy);
    puts("REAL PRIVATE NAMESPACE BH-O55-1: an HFS+ view copies only Database2, shows Engine Library/Music read-only from the stick, ejects and rolls back with the nested binds, drops an older build's full copy, keeps two generations per stick and a free-space reserve: PASS");
}
// BH-F52-1: engine-guardian healed a plain Engine stick's m.db/hm.db after the
// visible mount, while Engine opened them (lock contention, a hot journal left
// on the stick when killed). The heal runs in the private pre-mount now, once.
static void test_heal_before_visible(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char data[PATH_MAX],disk[PATH_MAX],target[PATH_MAX],p[PATH_MAX],log[PATH_MAX],pid[32],script[PATH_MAX*3],expect[PATH_MAX+64];
    join(data,root,"heal-data");join(p,data,"enginex");assert(!make_dirs(p));
    join(test_library,p,"hfs-libraries");test_legacy_library[0]=0;
    join(log,root,"heal-guardian.log");join(test_guardian_system,root,"heal-guardian");
    snprintf(pid,sizeof pid,"%d",(int)getpid());assert(!setenv("EDISKSD_TEST_PID",pid,1));
    snprintf(script,sizeof script,"#!/bin/sh\nif grep -q \" $2 \" /proc/$EDISKSD_TEST_PID/mountinfo; then v=visible; else v=private; fi\n"
             "echo \"$v $*\" >> '%s'\necho healed > \"$2/Engine Library/Database2/m.db\"\n",log);
    write_file(test_guardian_system,script);assert(!chmod(test_guardian_system,0700));
    // A writable Engine stick: healed once, before Engine can see it.
    join(disk,root,"heal-fat");join(p,disk,"Engine Library/Database2");assert(!make_dirs(p));
    join(p,disk,"Engine Library/Database2/m.db");write_file(p,"engine library\n");
    join(target,MEDIA_ROOT,"HEAL");assert(!make_dirs(target));
    snprintf(test_disk,sizeof test_disk,"%s",disk);test_disk_ro=0;
    struct libmnt_context c={.target=target,.source="/dev/sdu1"};
    assert(!mnt_context_mount(&c)&&c.status==1);
    snprintf(expect,sizeof expect,"private --heal-mount %s --data-only\n",target);expect_file(log,expect);
    join(p,target,"Engine Library/Database2/m.db");expect_file(p,"healed\n");
    assert(count_mounts(target)==1);assert(!umount2(target,UMOUNT_NOFOLLOW));unlink(log);
    // A stick without an Engine database is not healed.
    char plain[PATH_MAX];join(plain,root,"heal-plain");join(p,plain,"Music");assert(!make_dirs(p));
    snprintf(test_disk,sizeof test_disk,"%s",plain);c.status=0;
    assert(!mnt_context_mount(&c)&&c.status==1);assert(access(log,F_OK)==-1);assert(!umount2(target,UMOUNT_NOFOLLOW));
    // Read-only HFS+: the heal works on the view's copy, before the visible mount; the stick is untouched.
    char hfs[PATH_MAX];join(hfs,root,"heal-hfs");join(p,hfs,"Engine Library/Database2");assert(!make_dirs(p));
    join(p,hfs,"Engine Library/Database2/m.db");write_file(p,"made on the Mac\n");
    snprintf(test_disk,sizeof test_disk,"%s",hfs);test_disk_ro=1;snprintf(test_hfs_target,sizeof test_hfs_target,"%s",target);c.status=0;
    unsigned views_before=count_views();
    assert(!mnt_context_mount(&c)&&c.status==1);expect_file(log,expect);
    join(p,target,"Engine Library/Database2/m.db");expect_file(p,"healed\n");
    join(p,hfs,"Engine Library/Database2/m.db");expect_file(p,"made on the Mac\n");
    assert(count_views()==views_before+1);
    assert(!unmount_dispatch(&c,real_unmount));test_hfs_target[0]=0;test_disk_ro=0;unlink(log);
    // A heal that runs out of its whole window: killed once, then skipped, never after the visible mount.
    snprintf(script,sizeof script,"#!/bin/sh\necho \"$*\" >> '%s'\nsleep 5\n",log);write_file(test_guardian_system,script);
    snprintf(test_disk,sizeof test_disk,"%s",disk);
    snprintf(expect,sizeof expect,"--heal-mount %s --data-only\n",target);
    for(int i=0;i<2;i++) {
        int64_t t0=now_ms();c.status=0;assert(!mnt_context_mount(&c)&&c.status==1);
        assert(now_ms()-t0<(i?1000:3500));expect_file(log,expect);
        assert(!umount2(target,UMOUNT_NOFOLLOW));
    }
    unlink(log);unsetenv("EDISKSD_TEST_PID");
    puts("REAL PRIVATE NAMESPACE BH-F52-1: an Engine stick's database is healed once in the private pre-mount (FAT and read-only HFS+), never after the visible mount, a stick without one not at all, a too-slow heal skipped on the next insert: PASS");
}
// BH-O55-3: a stick without a view is ejected at once while another stick is
// being converted; the conversion holds view_lock for up to its whole budget.
static void test_eject_while_another_stick_converts(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char plain[PATH_MAX],tp[PATH_MAX],rb[PATH_MAX],tr[PATH_MAX],p[PATH_MAX];
    join(plain,root,"eject-plain");join(p,plain,"Music");assert(!make_dirs(p));
    join(tp,MEDIA_ROOT,"EJECT-PLAIN");assert(!make_dirs(tp));
    join(rb,root,"eject-rekordbox");join(p,rb,"PIONEER/rekordbox");assert(!make_dirs(p));join(p,rb,"PIONEER/rekordbox/export.pdb");write_file(p,"library");
    join(tr,MEDIA_ROOT,"EJECT-RB");assert(!make_dirs(tr));
    join(test_rb_bridge,root,"eject-bridge");
    write_file(test_rb_bridge,"#!/bin/sh\nsleep 2.5\nmkdir -p \"$2/Engine Library/Database2\" && echo converted > \"$2/Engine Library/Database2/m.db\"\n");
    assert(!chmod(test_rb_bridge,0700));
    int64_t saved_reserve=test_post_reserve;test_post_reserve=16000;test_disk_ro=0;
    struct libmnt_context c={.target=tp,.source="/dev/sdr1",.disk=plain};
    unsigned views=count_views();
    assert(!mnt_context_mount(&c)&&c.status==1&&count_mounts(tp)==1&&count_views()==views);
    struct insert ir={.c={.target=tr,.source="/dev/sds1",.disk=rb}};pthread_t t;
    assert(!pthread_create(&t,NULL,insert_thread,&ir));usleep(500000);
    int64_t t0=now_ms();
    assert(!unmount_dispatch(&c,real_unmount)&&count_mounts(tp)==0);
    int64_t took=now_ms()-t0;
    printf("eject of a plain stick took %lld ms while another stick converted\n",(long long)took);
    assert(took<1000);
    assert(pthread_tryjoin_np(t,NULL)==EBUSY); // the other stick is still being converted
    assert(!pthread_join(t,NULL)&&!ir.rc&&ir.c.status==1);
    join(p,tr,"Engine Library/Database2/m.db");expect_file(p,"converted\n");
    assert(!umount2(tr,UMOUNT_NOFOLLOW));test_post_reserve=saved_reserve;
    puts("REAL PRIVATE NAMESPACE BH-O55-3: a stick without a view is ejected at once while another stick is being converted: PASS");
}
// BH-O55-5: a stick pulled without Eject answers EIO inside; its eject is the
// stock umount, not a refusal that keeps the dead mount until a reboot.
static void test_eject_of_a_pulled_stick(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char plain[PATH_MAX],tp[PATH_MAX],p[PATH_MAX];
    join(plain,root,"pulled");join(p,plain,"Music");assert(!make_dirs(p));
    join(tp,MEDIA_ROOT,"PULLED");assert(!make_dirs(tp));test_disk_ro=0;
    struct libmnt_context c={.target=tp,.source="/dev/sdt1",.disk=plain};
    assert(!mnt_context_mount(&c)&&c.status==1&&count_mounts(tp)==1);
    join(test_eio_path,tp,"Engine Library");unsigned calls=real_calls;
    int rc=unmount_dispatch(&c,real_unmount);test_eio_path[0]=0;
    if(rc)assert(!umount2(tp,UMOUNT_NOFOLLOW)); // leave no mount behind before failing
    assert(!rc&&real_calls==calls+1&&count_mounts(tp)==0);
    puts("REAL PRIVATE NAMESPACE BH-O55-5: a stick whose library answers EIO (pulled without Eject) is still unmounted: PASS");
}
// BH2-A1-2: the "too large to heal" record was keyed by f_fsid, on FAT/exFAT/HFS+
// the device number. Two mounts of one library under different filesystem ids
// stand for one stick in two ports (sda1, then sdb1).
static void test_heal_marker_survives_another_port(const char *root) {
    char mnt[2][PATH_MAX],marker[2][PATH_MAX],other[PATH_MAX],p[PATH_MAX];struct statvfs v[2];
    for(int i=0;i<2;i++) {
        join(mnt[i],root,i?"heal-port-b":"heal-port-a");assert(!make_dirs(mnt[i]));
        assert(!mount("stick",mnt[i],"tmpfs",0,"size=1m,mode=0700"));
        join(p,mnt[i],"Engine Library/Database2");assert(!make_dirs(p));join(p,mnt[i],"Engine Library/Database2/m.db");write_file(p,"big engine library");
        assert(!statvfs(mnt[i],&v[i]));assert(!heal_marker(mnt[i],marker[i]));
    }
    assert(memcmp(&v[0].f_fsid,&v[1].f_fsid,sizeof v[0].f_fsid)&&v[0].f_blocks==v[1].f_blocks); // what a second port looks like
    assert(!strcmp(marker[0],marker[1]));
    write_file(p,"a larger engine library");assert(!heal_marker(mnt[1],other));assert(strcmp(marker[0],other)); // another database is still looked at
    for(int i=0;i<2;i++)assert(!umount2(mnt[i],UMOUNT_NOFOLLOW));
    puts("REAL PRIVATE NAMESPACE BH2-A1-2: the too-large-to-heal record of a stick is found again under another device number, and not for another database: PASS");
}
// BH2-A1-6: the view's record keeps "Engine Library/<name>" in NAME_MAX bytes. One
// entry with a longer (valid) name refused the HFS+ view of the whole stick.
static void test_hfs_view_with_a_long_entry_name(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char disk[PATH_MAX],target[PATH_MAX],p[PATH_MAX],longname[NAME_MAX+1];
    memset(longname,'n',250);longname[250]=0;
    join(disk,root,"long-name-hfs");join(p,disk,"Engine Library/Database2");assert(!make_dirs(p));
    join(p,disk,"Engine Library/Database2/m.db");write_file(p,"engine dj export");
    join(p,disk,"Engine Library/Music");assert(!make_dirs(p));join(p,disk,"Engine Library/Music/song.mp3");write_file(p,"audio");
    char lib[PATH_MAX];join(lib,disk,"Engine Library");join(p,lib,longname);write_file(p,"pc file with a long name");
    join(target,MEDIA_ROOT,"LONG-NAME-HFS");mount_ro(disk,target);
    seeded_hint[0]=0;assert(!prepare_view(target,"uuid-long-name",deadline_from(now_ms())));
    join(p,target,"Engine Library/Music/song.mp3");expect_file(p,"audio");
    join(p,target,"Engine Library/Database2/m.db");expect_file(p,"engine dj export");
    join(lib,target,"Engine Library");join(p,lib,longname);assert(access(p,F_OK)==-1&&errno==ENOENT);
    struct libmnt_context c={.target=target};assert(!unmount_dispatch(&c,real_unmount));
    puts("REAL PRIVATE NAMESPACE BH2-A1-6: an Engine Library entry whose name does not fit the view record is left out, the stick's view is up: PASS");
}
// BH2-A1-4: Engine 5.3.0 and rb-bridge write covers as Engine Library/Artwork/<hash>.jpg.
// The view bound the stick's Artwork read-only, so no cover could be added on an HFS+ stick.
static void test_hfs_view_artwork_is_writable(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char disk[PATH_MAX],target[PATH_MAX],p[PATH_MAX];struct stat st;
    join(disk,root,"artwork-hfs");join(p,disk,"Engine Library/Database2");assert(!make_dirs(p));
    join(p,disk,"Engine Library/Database2/m.db");write_file(p,"engine dj export 1");
    join(p,disk,"Engine Library/Artwork");assert(!make_dirs(p));
    join(p,disk,"Engine Library/Artwork/AAAA.jpg");write_file(p,"cover a");
    join(p,disk,"Engine Library/Artwork/BBBB.jpg");write_file(p,"cover b");
    join(target,MEDIA_ROOT,"ARTWORK-HFS");
    struct libmnt_context c={.target=target};seeded_hint[0]=0;
    #define VIEW(pre) do{mount_ro(disk,target);private_ns=(pre);assert(!prepare_view(target,"uuid-artwork",deadline_from(now_ms())));private_ns=0;}while(0)
    // The pre-mount mirrors the stick's covers; the visible mount shows them, writable.
    VIEW(1);assert(!unmount_dispatch(&c,real_unmount));
    VIEW(0);
    join(p,target,"Engine Library/Artwork/AAAA.jpg");expect_file(p,"cover a");
    join(p,target,"Engine Library/Artwork/deck.jpg");int fd=open(p,O_WRONLY|O_CREAT|O_EXCL|O_CLOEXEC,0600);
    if(fd<0)fprintf(stderr,"a new cover in the view's Artwork: %s\n",strerror(errno));
    assert(fd>=0);assert(write(fd,"deck cover",10)==10);close(fd);
    join(p,disk,"Engine Library/Artwork/deck.jpg");assert(access(p,F_OK)==-1&&errno==ENOENT); // the stick is untouched
    assert(!unmount_dispatch(&c,real_unmount));
    // The next insert shows the deck's cover beside the stick's.
    VIEW(1);assert(!unmount_dispatch(&c,real_unmount));VIEW(0);
    join(p,target,"Engine Library/Artwork/deck.jpg");expect_file(p,"deck cover");
    join(p,target,"Engine Library/Artwork/BBBB.jpg");expect_file(p,"cover b");
    assert(!unmount_dispatch(&c,real_unmount));
    // A new PC export is a new generation: its mirror is linked from the one before, even
    // where nothing may be copied (the visible mount).
    join(p,disk,"Engine Library/Database2/m.db");write_file(p,"engine dj export 2");
    VIEW(0);
    join(p,target,"Engine Library/Artwork/AAAA.jpg");expect_file(p,"cover a");assert(!stat(p,&st)&&st.st_nlink==2);
    join(p,target,"Engine Library/Artwork/deck2.jpg");write_file(p,"deck cover 2");
    assert(!unmount_dispatch(&c,real_unmount));
    // A cover that cannot be mirrored now (added on the PC, no pre-mount): the stick's folder
    // is shown read-only as before, the view is up and the deck's covers are kept for later.
    join(p,disk,"Engine Library/Artwork/CCCC.jpg");write_file(p,"cover c");
    VIEW(0);
    join(p,target,"Engine Library/Artwork/CCCC.jpg");expect_file(p,"cover c");
    join(p,target,"Engine Library/Artwork/later.jpg");assert(open(p,O_WRONLY|O_CREAT|O_CLOEXEC,0600)<0&&errno==EROFS);
    assert(!unmount_dispatch(&c,real_unmount));
    VIEW(1);assert(!unmount_dispatch(&c,real_unmount));VIEW(0);
    join(p,target,"Engine Library/Artwork/CCCC.jpg");expect_file(p,"cover c");
    join(p,target,"Engine Library/Artwork/deck2.jpg");expect_file(p,"deck cover 2");
    join(p,target,"Engine Library/Artwork/later.jpg");write_file(p,"later");
    assert(!unmount_dispatch(&c,real_unmount));
    #undef VIEW
    puts("REAL PRIVATE NAMESPACE BH2-A1-4: an HFS+ view's Engine Library/Artwork is writable: the stick's covers are mirrored in the pre-mount, linked into a new generation, shown read-only until a mirror is complete: PASS");
}
// Review of bdbbda4: seed_library passed the new library's path, which does not exist yet, as
// the one to keep, so making room for a new PC export could delete this stick's own previous
// generation (the cues set on the deck) before the newer unused library of another stick.
static void test_seed_keeps_its_own_previous_generation(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char data[PATH_MAX],p[PATH_MAX],disk[PATH_MAX],target[PATH_MAX],own[PATH_MAX],other[PATH_MAX],saved_lib[PATH_MAX];
    snprintf(saved_lib,sizeof saved_lib,"%s",test_library);
    join(data,root,"keep-data");assert(!make_dirs(data));assert(!mount("keep-data",data,"tmpfs",0,"mode=0700,size=24m"));
    join(p,data,"enginex");assert(!make_dirs(p));join(test_library,p,"hfs-libraries");assert(!make_dirs(test_library));assert(!chmod(test_library,0700));
    // This stick's previous generation, the least recently used, and another stick's newer one.
    join(own,test_library,"uuid-own/library-00000000000000aa/Database2");assert(!make_dirs(own));join(p,own,"m.db");write_bytes(p,2u<<20);
    join(other,test_library,"uuid-other/library-00000000000000bb/Database2");assert(!make_dirs(other));join(p,other,"m.db");write_bytes(p,2u<<20);
    struct timespec older[2]={{.tv_sec=1000000000},{.tv_sec=1000000000}},newer[2]={{.tv_sec=1100000000},{.tv_sec=1100000000}};
    join(p,test_library,"uuid-own/library-00000000000000aa");assert(!utimensat(AT_FDCWD,p,older,0));
    join(p,test_library,"uuid-other/library-00000000000000bb");assert(!utimensat(AT_FDCWD,p,newer,0));
    join(disk,root,"keep-hfs");join(p,disk,"Engine Library/Database2");assert(!make_dirs(p));
    join(p,disk,"Engine Library/Database2/m.db");write_bytes(p,1u<<20);
    join(target,MEDIA_ROOT,"KEEP-HFS");mount_ro(disk,target);
    struct statvfs v;assert(!statvfs(test_library,&v));uint64_t avail=(uint64_t)v.f_bavail*v.f_frsize;
    test_cache_reserve=avail-(1u<<19); // one 2 MiB library must go for the 1 MiB seed
    seeded_hint[0]=0;assert(!prepare_view(target,"uuid-own",deadline_from(now_ms())));test_cache_reserve=0;
    if(access(own,F_OK))fprintf(stderr,"the stick's own previous generation was deleted for its new seed\n");
    assert(!access(own,F_OK));
    join(p,test_library,"uuid-other");assert(access(p,F_OK)==-1&&errno==ENOENT);
    struct libmnt_context c={.target=target};assert(!unmount_dispatch(&c,real_unmount));
    assert(!umount2(data,MNT_DETACH));snprintf(test_library,sizeof test_library,"%s",saved_lib);
    puts("REAL PRIVATE NAMESPACE review bdbbda4: a new seed makes room from other sticks' unused libraries, never from its own previous generation: PASS");
}
static unsigned mounts_below(const char *dir) {
    FILE *f=fopen("/proc/self/mountinfo","r");assert(f);char line[4096];unsigned n=0;size_t len=strlen(dir);
    while(fgets(line,sizeof line,f)){char mp[PATH_MAX];if(sscanf(line,"%*s %*s %*s %*s %4095s",mp)==1&&!strncmp(mp,dir,len)&&mp[len]=='/')n++;}
    fclose(f);return n;
}
// Review of bdbbda4: systemd makes /data MS_SHARED. The view's "Engine Library" bind of the
// cached library was then a peer of /data, and the binds inside it (Engine Library/Music, ...)
// propagated into /data/enginex/hfs-libraries.
static void test_view_binds_stay_out_of_a_shared_data(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char data[PATH_MAX],p[PATH_MAX],disk[PATH_MAX],target[PATH_MAX],saved_lib[PATH_MAX];
    snprintf(saved_lib,sizeof saved_lib,"%s",test_library);
    join(data,root,"shared-data");assert(!make_dirs(data));assert(!mount("shared-data",data,"tmpfs",0,"mode=0700,size=8m"));
    assert(!mount(NULL,data,NULL,MS_SHARED,NULL));
    join(test_library,data,"hfs-libraries");
    join(disk,root,"shared-hfs");join(p,disk,"Engine Library/Database2");assert(!make_dirs(p));
    join(p,disk,"Engine Library/Database2/m.db");write_file(p,"engine dj export");
    join(p,disk,"Engine Library/Music");assert(!make_dirs(p));join(p,disk,"Engine Library/Music/a.mp3");write_file(p,"audio");
    join(target,MEDIA_ROOT,"SHARED-HFS");mount_ro(disk,target);
    seeded_hint[0]=0;assert(!prepare_view(target,"uuid-shared",deadline_from(now_ms())));
    join(p,target,"Engine Library/Music/a.mp3");expect_file(p,"audio");
    unsigned leaked=mounts_below(data);
    if(leaked)fprintf(stderr,"%u view mounts propagated into the shared /data\n",leaked);
    assert(!leaked);
    struct libmnt_context c={.target=target};assert(!unmount_dispatch(&c,real_unmount));
    assert(!umount2(data,MNT_DETACH));snprintf(test_library,sizeof test_library,"%s",saved_lib);
    puts("REAL PRIVATE NAMESPACE review bdbbda4: the HFS+ view's binds stay out of a shared /data: PASS");
}
// Observe the state at the HFS prepare_view boundary, before any view work.
static int observing_prepare_view;
static unsigned observed_prepare_views;
static void observe_prepare_view(void) {
    if(!observing_prepare_view)return;
    struct stat st;assert(!lstat(test_prepare_state,&st));
    assert(S_ISREG(st.st_mode)&&(st.st_mode&0777)==0644);observed_prepare_views++;
}
static void expect_not_preparing(void) {
    struct stat st;errno=0;assert(lstat(test_prepare_state,&st)==-1&&errno==ENOENT);
}
static void test_prepare_state_lifecycle(const char *root) {
    char disk[PATH_MAX],target[PATH_MAX],p[PATH_MAX],marker[PATH_MAX],script[PATH_MAX*6];
    join(disk,root,"prep-rb");join(p,disk,"PIONEER/rekordbox");assert(!make_dirs(p));join(p,disk,"PIONEER/rekordbox/export.pdb");write_file(p,"export"); // BH2-A1-7: only an export makes a rekordbox stick
    join(target,MEDIA_ROOT,"PREPARING");assert(!make_dirs(target));
    join(marker,root,"prep-observed");join(test_rb_bridge,root,"prep-bridge");
    // The subprocess observes the real public file, including type, mode and contents.
    snprintf(script,sizeof script,"#!/bin/sh\n[ -f '%s' ] && [ ! -L '%s' ] && [ \"$(stat -c %%a '%s')\" = 644 ] && [ \"$(cat '%s')\" = 1 ] || exit 9\necho ready > '%s'\nexit 0\n",
             test_prepare_state,test_prepare_state,test_prepare_state,test_prepare_state,marker);
    write_file(test_rb_bridge,script);assert(!chmod(test_rb_bridge,0700));
    snprintf(test_disk,sizeof test_disk,"%s",disk);test_disk_ro=0;
    struct libmnt_context c={.target=target,.source="/dev/preparing"};
    assert(!hooked_mount(&c,test_real_mount));expect_file(marker,"ready\n");expect_not_preparing();
    assert(!umount2(target,UMOUNT_NOFOLLOW));assert(!unlink(marker));

    // A guardian error still clears state before returning to edisksd.
    join(disk,root,"prep-engine");join(p,disk,"Engine Library/Database2");assert(!make_dirs(p));
    join(p,disk,"Engine Library/Database2/m.db");write_file(p,"native\n");
    join(test_guardian_system,root,"prep-guardian");
    char *exit_line=strstr(script,"exit 0");assert(exit_line);memcpy(exit_line,"exit 1",6);
    write_file(test_guardian_system,script);assert(!chmod(test_guardian_system,0700));
    snprintf(test_disk,sizeof test_disk,"%s",disk);c.status=0;
    assert(!hooked_mount(&c,test_real_mount));expect_file(marker,"ready\n");expect_not_preparing();
    assert(!umount2(target,UMOUNT_NOFOLLOW));assert(!unlink(marker));test_guardian_system[0]=0;

    // A hanging bridge records state before sleeping; a timeout must kill it and clear state.
    join(disk,root,"prep-timeout");join(p,disk,"PIONEER/rekordbox");assert(!make_dirs(p));join(p,disk,"PIONEER/rekordbox/export.pdb");write_file(p,"export"); // BH2-A1-7: only an export makes a rekordbox stick
    snprintf(test_disk,sizeof test_disk,"%s",disk);
    snprintf(exit_line,(size_t)(script+sizeof script-exit_line),"echo $$ > '%s.pid'\nexec sleep 10\n",marker);write_file(test_rb_bridge,script);
    int64_t start=now_ms();c.status=0;assert(!hooked_mount(&c,test_real_mount));
    assert(now_ms()-start>=1000&&now_ms()-start<3500);
    expect_file(marker,"ready\n");expect_not_preparing();
    char err_path[PATH_MAX];assert(!prepare_err_path(err_path));
    expect_file(err_path,"{\"state\":\"error\",\"kind\":\"usb\",\"error\":\"USB conversion timed out\"}\n");
    assert(snprintf(p,sizeof p,"%s.pid",marker)<(int)sizeof p);
    FILE *pf=fopen(p,"r");long killed;assert(pf&&fscanf(pf,"%ld",&killed)==1);fclose(pf);
    errno=0;assert(kill((pid_t)killed,0)==-1&&errno==ESRCH);assert(!unlink(p));
    assert(!slow_marker(target,p));assert(!access(p,F_OK));assert(!unlink(p)); // created only for ETIMEDOUT
    assert(!umount2(target,UMOUNT_NOFOLLOW));assert(!unlink(marker));
    // A crashed bridge without its own .err also publishes a failure state, and the next insert clears it.
    join(disk,root,"prep-crash");join(p,disk,"PIONEER/rekordbox");assert(!make_dirs(p));join(p,disk,"PIONEER/rekordbox/export.pdb");write_file(p,"export");
    snprintf(test_disk,sizeof test_disk,"%s",disk);
    snprintf(exit_line,(size_t)(script+sizeof script-exit_line),"kill -SEGV $$\n");write_file(test_rb_bridge,script);
    c.status=0;assert(!hooked_mount(&c,test_real_mount));
    expect_file(marker,"ready\n");expect_not_preparing();
    expect_file(err_path,"{\"state\":\"error\",\"kind\":\"usb\",\"error\":\"USB conversion failed\"}\n");
    assert(!unlink(marker));assert(!umount2(target,UMOUNT_NOFOLLOW));
    snprintf(exit_line,(size_t)(script+sizeof script-exit_line),"exit 0\n");write_file(test_rb_bridge,script);
    join(disk,root,"prep-ok-after");join(p,disk,"PIONEER/rekordbox");assert(!make_dirs(p));join(p,disk,"PIONEER/rekordbox/export.pdb");write_file(p,"export");
    snprintf(test_disk,sizeof test_disk,"%s",disk);
    c.status=0;assert(!hooked_mount(&c,test_real_mount));
    expect_file(marker,"ready\n");expect_not_preparing();
    struct stat err_st;assert(lstat(err_path,&err_st)==-1&&errno==ENOENT);
    assert(!unlink(marker));assert(!umount2(target,UMOUNT_NOFOLLOW));

    // HFS without export or DB prepares a writable view on the visible mount.
    join(disk,root,"prep-hfs-empty");assert(!make_dirs(disk));
    snprintf(test_disk,sizeof test_disk,"%s",disk);test_disk_ro=1;
    snprintf(test_hfs_target,sizeof test_hfs_target,"%s",target);observing_prepare_view=1;
    unsigned before=observed_prepare_views;c.status=0;assert(!hooked_mount(&c,test_real_mount));
    assert(observed_prepare_views>before);expect_not_preparing();observing_prepare_view=0;
    assert(!unmount_dispatch(&c,real_unmount));test_hfs_target[0]=0;test_disk_ro=0;

    // Read-only HFS+ pioneer stick: timeout and crash errors published in MOUNT_PRE
    // must survive MOUNT_POST prepare_view until the next insert of that drive.
    join(disk,root,"prep-hfs-timeout");join(p,disk,"PIONEER/rekordbox");assert(!make_dirs(p));
    join(p,disk,"PIONEER/rekordbox/export.pdb");write_file(p,"pdb\n");
    snprintf(test_disk,sizeof test_disk,"%s",disk);test_disk_ro=1;
    snprintf(test_hfs_target,sizeof test_hfs_target,"%s",target);
    snprintf(exit_line,(size_t)(script+sizeof script-exit_line),"exec sleep 10\n");write_file(test_rb_bridge,script);
    c.status=0;assert(!hooked_mount(&c,test_real_mount));
    expect_not_preparing();
    expect_file(err_path,"{\"state\":\"error\",\"kind\":\"usb\",\"error\":\"USB conversion timed out\"}\n");
    if(!slow_marker(target,p))unlink(p);
    assert(!unmount_dispatch(&c,real_unmount));
    join(disk,root,"prep-hfs-crash");join(p,disk,"PIONEER/rekordbox");assert(!make_dirs(p));
    join(p,disk,"PIONEER/rekordbox/export.pdb");write_file(p,"pdb-crash\n");
    snprintf(test_disk,sizeof test_disk,"%s",disk);
    snprintf(exit_line,(size_t)(script+sizeof script-exit_line),"kill -SEGV $$\n");write_file(test_rb_bridge,script);
    c.status=0;assert(!hooked_mount(&c,test_real_mount));
    expect_not_preparing();
    expect_file(err_path,"{\"state\":\"error\",\"kind\":\"usb\",\"error\":\"USB conversion failed\"}\n");
    assert(!unmount_dispatch(&c,real_unmount));
    join(disk,root,"prep-hfs-ok");join(p,disk,"PIONEER/rekordbox");assert(!make_dirs(p));
    join(p,disk,"PIONEER/rekordbox/export.pdb");write_file(p,"pdb-ok\n");
    snprintf(test_disk,sizeof test_disk,"%s",disk);
    snprintf(exit_line,(size_t)(script+sizeof script-exit_line),"exit 0\n");write_file(test_rb_bridge,script);
    c.status=0;assert(!hooked_mount(&c,test_real_mount));
    expect_not_preparing();
    assert(lstat(err_path,&err_st)==-1&&errno==ENOENT);
    assert(!unmount_dispatch(&c,real_unmount));test_hfs_target[0]=0;test_disk_ro=0;
    // A plain empty stick does not publish the flag even while the real mount runs.
    join(disk,root,"prep-hfs-empty");snprintf(test_disk,sizeof test_disk,"%s",disk);
    observing_not_preparing=1;c.status=0;assert(!hooked_mount(&c,test_real_mount));observing_not_preparing=0;expect_not_preparing();
    assert(!umount2(target,UMOUNT_NOFOLLOW));
    puts("USB preparing: conversion, failed heal, SIGKILL timeout, empty HFS view and empty stick: PASS");
}

static void put_be16(uint8_t *p,uint16_t v){p[0]=(uint8_t)(v>>8);p[1]=(uint8_t)v;}
static void put_be32(uint8_t *p,uint32_t v){p[0]=(uint8_t)(v>>24);p[1]=(uint8_t)(v>>16);p[2]=(uint8_t)(v>>8);p[3]=(uint8_t)v;}
static void put_be64(uint8_t *p,uint64_t v){put_be32(p,(uint32_t)(v>>32));put_be32(p+4,(uint32_t)v);}
static void put_le32(uint8_t *p,uint32_t v){p[0]=(uint8_t)v;p[1]=(uint8_t)(v>>8);p[2]=(uint8_t)(v>>16);p[3]=(uint8_t)(v>>24);}
static void put_le64(uint8_t *p,uint64_t v){put_le32(p,(uint32_t)v);put_le32(p+4,(uint32_t)(v>>32));}

static void write_hfs_img(const char *path,uint32_t attr,uint32_t jflags,int be_jh,uint64_t j_start,uint64_t j_end,int dirty_needinit) {
    uint8_t *img=calloc(65536,1);assert(img);
    uint8_t *vh=img+1024;
    put_be16(vh+0,0x482b);put_be16(vh+2,4);put_be32(vh+4,attr);
    put_be32(vh+12,(attr&(1u<<13))?2:0); // jinfo_blk=2 -> 8192
    put_be32(vh+40,4096);put_be32(vh+44,16); // blocksize=4096, total_blocks=16 (64 KiB)
    if(attr&(1u<<13)) {
        uint8_t *jib=img+8192;
        put_be32(jib+0,jflags);
        put_be64(jib+36,12288); // joff=12288
        put_be64(jib+44,8192);  // jsize=8192
        uint8_t *jh=img+12288;
        if(jflags&4u) {
            if(dirty_needinit)jh[0]=0xaa;
        } else if(be_jh) {
            put_be32(jh+0,0x4a4e4c78u);put_be32(jh+4,0x12345678u);
            put_be64(jh+8,j_start);put_be64(jh+16,j_end);put_be64(jh+24,8192);put_be32(jh+40,4096);
        } else {
            put_le32(jh+0,0x4a4e4c78u);put_le32(jh+4,0x12345678u);
            put_le64(jh+8,j_start);put_le64(jh+16,j_end);put_le64(jh+24,8192);put_le32(jh+40,4096);
        }
    }
    int fd=open(path,O_WRONLY|O_CREAT|O_TRUNC|O_NOFOLLOW|O_CLOEXEC,0600);assert(fd>=0);
    assert(write(fd,img,65536)==65536);close(fd);free(img);
}

static void write_fake_ko(const char *path,uint16_t machine,const char *vermagic,const char *modname) {
    char shstr[32]="\0.shstrtab\0.modinfo";
    char modinfo[256];size_t mi_len=0;
    if(vermagic){int n=snprintf(modinfo+mi_len,sizeof modinfo-mi_len,"vermagic=%s",vermagic);assert(n>0);mi_len+=(size_t)n+1;}
    if(modname){int n=snprintf(modinfo+mi_len,sizeof modinfo-mi_len,"name=%s",modname);assert(n>0);mi_len+=(size_t)n+1;}
    if(!mi_len){modinfo[0]=0;mi_len=1;}
    Elf32_Ehdr eh={0};
    memcpy(eh.e_ident,ELFMAG,SELFMAG);
    eh.e_ident[EI_CLASS]=ELFCLASS32;eh.e_ident[EI_DATA]=ELFDATA2LSB;eh.e_ident[EI_VERSION]=EV_CURRENT;
    eh.e_type=ET_REL;eh.e_machine=machine;eh.e_version=EV_CURRENT;
    eh.e_shentsize=sizeof(Elf32_Shdr);eh.e_shnum=3;eh.e_shstrndx=1;
    uint32_t off_str=sizeof eh,off_mi=off_str+sizeof shstr,off_sh=(off_mi+(uint32_t)mi_len+3u)&~3u;
    eh.e_shoff=off_sh;
    Elf32_Shdr sh[3]={0};
    sh[1].sh_name=1;sh[1].sh_type=SHT_STRTAB;sh[1].sh_offset=off_str;sh[1].sh_size=sizeof shstr;
    sh[2].sh_name=11;sh[2].sh_type=SHT_PROGBITS;sh[2].sh_offset=off_mi;sh[2].sh_size=(uint32_t)mi_len;
    uint8_t buf[1024]={0};
    memcpy(buf,&eh,sizeof eh);memcpy(buf+off_str,shstr,sizeof shstr);
    memcpy(buf+off_mi,modinfo,mi_len);memcpy(buf+off_sh,sh,sizeof sh);
    int fd=open(path,O_WRONLY|O_CREAT|O_TRUNC|O_NOFOLLOW|O_CLOEXEC,0600);assert(fd>=0);
    size_t total=off_sh+sizeof sh;
    assert(write(fd,buf,total)==(ssize_t)total);close(fd);
}

static void test_hfs_rw_unit(const char *root) {
    char img[PATH_MAX],ko[PATH_MAX],mi[PATH_MAX],opts[128],ft[64];
    int jnl=99,nc=99;
    { // fsck -y writes the stick: past the deadline it is waited for, never killed; -n still is.
        char fsck[PATH_MAX],done[PATH_MAX],sh[PATH_MAX*2];
        join(fsck,root,"unit-slow-fsck");join(done,root,"unit-slow-fsck.done");unlink(done);
        snprintf(sh,sizeof sh,"#!/bin/sh\nsleep 0.5\ntouch '%s'\necho '** The volume TEST appears to be OK.'\nexit 0\n",done);
        write_file(fsck,sh);assert(!chmod(fsck,0700));
        join(img,root,"unit-slow.img");write_hfs_img(img,(1u<<8),0,0,0,0,0);
        assert(!run_fsck_hfsplus(fsck,img,1,now_ms()+100)&&!access(done,F_OK));
        unlink(done);assert(run_fsck_hfsplus(fsck,img,0,now_ms()+100)==-1&&errno==ETIMEDOUT&&access(done,F_OK));
    }
    join(img,root,"unit-clean.img");write_hfs_img(img,(1u<<8),0,0,0,0,0);
    assert(!hfs_check_volume_and_journal(img,&jnl,0,&nc)&&jnl==0&&nc==0);
    join(img,root,"unit-jnl-empty-le.img");write_hfs_img(img,(1u<<8)|(1u<<13),1,0,4096,4096,0);
    assert(!hfs_check_volume_and_journal(img,&jnl,0,&nc)&&jnl==1&&nc==0);
    join(img,root,"unit-jnl-empty-be.img");write_hfs_img(img,(1u<<8)|(1u<<13),1,1,4096,4096,0);
    assert(!hfs_check_volume_and_journal(img,&jnl,0,&nc)&&jnl==1&&nc==0);
    join(img,root,"unit-jnl-needinit.img");write_hfs_img(img,(1u<<8)|(1u<<13),5,0,0,0,0);
    assert(!hfs_check_volume_and_journal(img,&jnl,0,&nc)&&jnl==1&&nc==0);
    join(img,root,"unit-jnl-needinit-dirty.img");write_hfs_img(img,(1u<<8)|(1u<<13),5,0,0,0,1);
    assert(hfs_check_volume_and_journal(img,&jnl,1,&nc)!=0);
    join(img,root,"unit-jnl-dirty.img");write_hfs_img(img,(1u<<8)|(1u<<13),1,0,4096,8192,0);
    assert(hfs_check_volume_and_journal(img,&jnl,1,&nc)!=0);
    join(img,root,"unit-jnl-ext.img");write_hfs_img(img,(1u<<8)|(1u<<13),3,0,4096,4096,0);
    assert(hfs_check_volume_and_journal(img,&jnl,1,&nc)!=0);
    join(img,root,"unit-unclean.img");write_hfs_img(img,(1u<<11),0,0,0,0,0);
    assert(hfs_check_volume_and_journal(img,&jnl,0,&nc)!=0);
    assert(!hfs_check_volume_and_journal(img,&jnl,1,&nc)&&jnl==0&&nc==1);
    join(img,root,"unit-b14.img");write_hfs_img(img,(1u<<8)|(1u<<14),0,0,0,0,0);
    assert(hfs_check_volume_and_journal(img,&jnl,0,&nc)!=0);
    assert(!hfs_check_volume_and_journal(img,&jnl,1,&nc)&&jnl==0&&nc==1);
    join(img,root,"unit-linux-fscked.img");write_hfs_img(img,(1u<<8)|(1u<<11),0,0,0,0,0);
    assert(!hfs_check_volume_and_journal(img,&jnl,0,&nc)&&jnl==0&&nc==0);
    join(img,root,"unit-softlock.img");write_hfs_img(img,(1u<<8)|(1u<<15),0,0,0,0,0);
    assert(hfs_check_volume_and_journal(img,&jnl,1,&nc)!=0);

    join(ko,root,"ok.ko");write_fake_ko(ko,EM_ARM,"6.18.57-az01-2026-10-15-rt6 SMP preempt_rt ARMv7 p2v8 ","hfsplusx");
    int fd=open(ko,O_RDONLY);assert(fd>=0);assert(!check_ko_vermagic(fd,"6.18.57-az01-2026-10-15-rt6"));close(fd);
    join(ko,root,"badver.ko");write_fake_ko(ko,EM_ARM,"6.18.57-az01-other SMP preempt_rt ARMv7 p2v8 ","hfsplusx");
    fd=open(ko,O_RDONLY);assert(fd>=0);assert(check_ko_vermagic(fd,"6.18.57-az01-2026-10-15-rt6")!=0);close(fd);
    join(ko,root,"prefixtrap.ko");write_fake_ko(ko,EM_ARM,"6.18.57-az01-2026-10-15-rt6-extra SMP ","hfsplusx");
    fd=open(ko,O_RDONLY);assert(fd>=0);assert(check_ko_vermagic(fd,"6.18.57-az01-2026-10-15-rt6")!=0);close(fd);

    // LoadPin: hfs_ensure_hfsplusx loads only <modules_root>/<release>/extra/hfsplusx.ko (no flat/symlink/traversal fallback).
    char mroot[PATH_MAX],extra[PATH_MAX],ko_target[PATH_MAX],procfs[PATH_MAX];
    join(mroot,root,"unit-mods");assert(!make_dirs(mroot));
    snprintf(test_modules_root,sizeof test_modules_root,"%s",mroot);
    join(procfs,root,"unit-procfs");write_file(procfs,"nodev\ttmpfs\n"); // no hfsplusx loaded yet
    snprintf(test_proc_fs,sizeof test_proc_fs,"%s",procfs);
    join(ko_target,mroot,"hfsplusx.ko");
    write_fake_ko(ko_target,EM_ARM,"6.18.57-az01-2026-10-15-rt6 SMP preempt_rt ARMv7 p2v8 ","hfsplusx");
    join(extra,mroot,"6.18.57-other/extra");assert(!make_dirs(extra));
    join(ko,extra,"hfsplusx.ko");write_fake_ko(ko,EM_ARM,"6.18.57-other SMP preempt_rt ARMv7 p2v8 ","hfsplusx");
    test_finit_rc=0;test_finit_calls=0;
    errno=0;assert(hfs_ensure_hfsplusx()==-1&&errno==ENOENT&&test_finit_calls==0);
    join(extra,mroot,"6.18.57-az01-2026-10-15-rt6/extra");assert(!make_dirs(extra));
    join(ko,extra,"hfsplusx.ko");assert(!symlink(ko_target,ko));
    errno=0;assert(hfs_ensure_hfsplusx()==-1&&errno==ELOOP&&test_finit_calls==0);
    assert(!unlink(ko));
    write_fake_ko(ko,EM_ARM,"6.18.57-az01-2026-10-15-rt6 SMP preempt_rt ARMv7 p2v8 ","hfsplusx");
    snprintf(test_uname_release,sizeof test_uname_release,"../evil");
    errno=0;assert(hfs_ensure_hfsplusx()==-1&&errno==ENOENT&&test_finit_calls==0);
    snprintf(test_uname_release,sizeof test_uname_release,"6.18.57-az01-2026-10-15-rt6");
    assert(!hfs_ensure_hfsplusx()&&test_finit_calls==1);
    test_modules_root[0]=0;test_proc_fs[0]=0;test_finit_calls=0;

    assert(!build_hfs_opts(opts,sizeof opts,"nosuid,ro,utf8,nodev",1,0)&&!strcmp(opts,"nosuid,nodev,rw"));
    assert(!build_hfs_opts(opts,sizeof opts,"nosuid,nodev",1,1)&&!strcmp(opts,"nosuid,nodev,force,rw"));
    assert(!build_hfs_opts(opts,sizeof opts,"nosuid,force,rw",0,0)&&!strcmp(opts,"nosuid,ro"));
    assert(!has_opt_token("errors=remount-ro,nosuid","ro"));
    assert(has_opt_token("nosuid,ro,nodev","ro"));

    join(mi,root,"unit-mountinfo");
    char sdb[PATH_MAX],devfile[PATH_MAX],devblk[PATH_MAX];
    join(sdb,root,"unit-sysblk");assert(!make_dirs(sdb));
    join(devfile,root,"unit-devfile");write_file(devfile,"dev\n");
    snprintf(test_sys_dev_block,sizeof test_sys_dev_block,"%s",sdb);
    FILE *mif=fopen(mi,"w");assert(mif);
    fprintf(mif,"10 1 8:1 / /media/My\\040Stick rw,nosuid - ufsd /dev/nonexistent-hfs-dev rw\n"
                "11 1 8:2 / /media/Other rw,nosuid - hfsplusx %s rw\n"
                "12 1 0:99 / /media/Virt rw,nosuid - hfsplusx %s rw\n",devfile,devfile);
    fclose(mif);
    snprintf(test_proc_mi,sizeof test_proc_mi,"%s",mi);
    assert(!mountinfo_fstype("/media/My Stick",ft,sizeof ft)&&!strcmp(ft,"ufsd"));
    assert(!mountinfo_fstype("/media/Other",ft,sizeof ft)&&!strcmp(ft,"hfsplusx"));
    assert(hfs_dev_present_probe("/media/Missing",NULL)==0);
    assert(hfs_dev_present_probe("/media/Other",NULL)==0); // missing /sys/dev/block/8:2
    join(devblk,sdb,"8:2");write_file(devblk,"1\n");
    assert(hfs_dev_present_probe("/media/Other",NULL)==0); // devfile is regular file, not S_ISBLK
    join(devblk,sdb,"8:1");write_file(devblk,"1\n");
    assert(hfs_dev_present_probe("/media/My Stick",NULL)==0); // missing device node /dev/nonexistent-hfs-dev
    assert(hfs_dev_present_probe("/media/Virt",NULL)==1); // mountinfo parsed maj:min & source
    test_proc_mi[0]=0;test_sys_dev_block[0]=0;
    test_mock_hfs_syscalls=1;test_last_sys_flags=0;test_fsconfig_ro_seen=0;test_fsconfig_rw_seen=0;
    assert(!mount("/dev/sda1","/media/X","hfsplus",0,NULL));
    assert((test_last_sys_flags&MS_RDONLY)&&!strcmp(test_last_sys_fstype,"hfsplus"));
    test_last_sys_flags=0;
    assert(!mount("/dev/sda1","/media/X","hfsplusx",0,NULL));
    assert((test_last_sys_flags&MS_RDONLY)&&!strcmp(test_last_sys_fstype,"hfsplusx"));
    int ffd=fsopen("hfsplus",FSOPEN_CLOEXEC);assert(ffd>=0);
    assert(!fsconfig(ffd,FSCONFIG_SET_FLAG,"rw",NULL,0));
    assert(!fsconfig(ffd,FSCONFIG_CMD_CREATE,NULL,NULL,0));
    assert(test_fsconfig_rw_seen==0&&test_fsconfig_ro_seen==2);
    test_mock_hfs_syscalls=0;
    puts("HFS+ RW unit checks (journal/header/vermagic/opts/mountinfo/MS_RDONLY): PASS");
}

// 5d. A stick whose only library is the one Engine built for it in /data while it was read-only
//     (seed_library's zero-signature generation, in use) gets that library carried over, whole or not
//     at all, before its first rw mount. Called from test_hfs_rw_policy with its Gen-1 fixtures in place.
static struct libmnt_context carry_ctx;
static int carry_mount(const char *target,const char *img,const char *disk,const char *notes) {
    memset(&carry_ctx,0,sizeof carry_ctx);carry_ctx.target=target;carry_ctx.source=img;carry_ctx.disk=disk;carry_ctx.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&carry_ctx,"hfsplus");mnt_context_set_options(&carry_ctx,"nosuid");
    int saved=notes_to(notes);
    assert(!mnt_context_mount(&carry_ctx)&&carry_ctx.status==1);
    notes_done(saved);
    int rw=has_opt_token(carry_ctx.last_mount_opts,"rw")&&!(test_last_sys_flags&MS_RDONLY);
    assert(rw||(has_opt_token(carry_ctx.last_mount_opts,"ro")&&(test_last_sys_flags&MS_RDONLY)));
    struct statvfs v;assert(!statvfs(target,&v)&&(!(v.f_flag&ST_RDONLY))==rw);
    return rw;
}
static void carry_unmount(void){assert(!unmount_dispatch(&carry_ctx,real_unmount));mnt_free_context(&carry_ctx);}
static unsigned carry_stages(const char *disk) {
    DIR *d=opendir(disk);assert(d);unsigned n=0;struct dirent *e;
    while((e=readdir(d)))n+=!strncmp(e->d_name,CARRY_STAGE,strlen(CARRY_STAGE));
    closedir(d);return n;
}
// Read-only with the /data library in the view, nothing left on the stick.
static void expect_kept_read_only(const char *target,const char *disk,const char *db) {
    char p[PATH_MAX];
    join(p,target,"Engine Library/Database2/m.db");expect_file(p,"deck-authored library");
    carry_unmount();
    join(p,disk,"Engine Library");assert(access(p,F_OK)&&errno==ENOENT);
    assert(carry_stages(disk)==0);expect_file(db,"deck-authored library");
}
static void test_hfs_carry_over(const char *root,const char *target,const char *img) {
    char disk[PATH_MAX],small[PATH_MAX],lib[PATH_MAX],db[PATH_MAX],p[PATH_MAX],log[PATH_MAX],notes[PATH_MAX],refuse[PATH_MAX],slow[PATH_MAX],script[PATH_MAX*4];
    join(disk,root,"carry-disk");join(p,disk,"Music");assert(!make_dirs(p));join(p,disk,"Music/a.mp3");write_file(p,"audio");
    join(lib,LIB_ROOT,"uuid-hfs-stick/library-0000000000000000");join(p,lib,"Database2");assert(!make_dirs(p));
    join(db,lib,"Database2/m.db");write_file(db,"deck-authored library");
    join(p,lib,"Artwork");assert(!make_dirs(p));join(p,lib,"Artwork/cover.jpg");write_file(p,"cover");
    assert(!utimensat(AT_FDCWD,lib,NULL,0)); // the generation in use
    join(log,root,"carry-guardian.log");join(refuse,root,"carry-refuse");join(slow,root,"carry-slow");join(notes,root,"carry-notes.log");
    join(test_guardian_system,root,"carry-guardian");
    snprintf(script,sizeof script,"#!/bin/sh\necho \"$*\" >> '%s'\n[ \"$1\" = --check-copy ] || exit 0\n[ -f \"$2/Database2/m.db\" ] || exit 3\n"
                                  "[ -e '%s' ] && exit 1\n[ -e '%s' ] && sleep 3\nexit 0\n",log,refuse,slow);
    write_file(test_guardian_system,script);assert(!chmod(test_guardian_system,0700));
    char want[PATH_MAX+32];snprintf(want,sizeof want,"--check-copy %s/" CARRY_STAGE,target);

    // The check refuses the copy: read-only, the /data library in the view, no stage, nothing renamed.
    write_file(refuse,"");snprintf(test_hfs_target,sizeof test_hfs_target,"%s",target);
    assert(!carry_mount(target,img,disk,notes));
    assert(count_notes(log,want)==1&&count_notes(notes,"copy refused by engine-guardian --check-copy")==1&&
           count_notes(notes,"carry-over failed, keeping read-only view on")==1);
    expect_kept_read_only(target,disk,db);
    assert(!unlink(refuse));test_hfs_target[0]=0;

    // Carried over: rw, the whole library on the stick under its own name, the /data copy kept.
    assert(carry_mount(target,img,disk,notes));
    assert(count_notes(log,want)==2&&count_notes(notes,"library Engine built on /data carried over to")==1);
    carry_unmount();
    join(p,disk,"Engine Library/Database2/m.db");expect_file(p,"deck-authored library");
    join(p,disk,"Engine Library/Artwork/cover.jpg");expect_file(p,"cover");
    assert(carry_stages(disk)==0);expect_file(db,"deck-authored library");

    // The stick has an Engine Library: never overwritten, nothing copied, rw.
    write_file(db,"newer deck library");
    assert(carry_mount(target,img,disk,notes));
    assert(count_notes(log,want)==2&&count_notes(notes,"stick has an Engine Library of its own")==1);
    carry_unmount();
    join(p,disk,"Engine Library/Database2/m.db");expect_file(p,"deck-authored library");
    write_file(db,"deck-authored library");
    int dfd=open(disk,O_RDONLY|O_DIRECTORY);assert(dfd>=0);remove_tree(dfd,"Engine Library");close(dfd);

    // No room on the stick: refused before anything is written.
    join(small,root,"carry-small");assert(!make_dirs(small));
    assert(!mount("carry-small",small,"tmpfs",0,"size=256k"));
    join(p,small,"Music");assert(!make_dirs(p));join(p,small,"Music/a.mp3");write_file(p,"audio");
    char big[PATH_MAX];join(big,lib,"Database2/big.bin");write_bytes(big,1u<<20);
    snprintf(test_hfs_target,sizeof test_hfs_target,"%s",target);
    assert(!carry_mount(target,img,small,notes));
    assert(count_notes(log,want)==2&&count_notes(notes,"no room on the stick")==1);
    expect_kept_read_only(target,small,db);
    assert(!unlink(big));

    // The stick fills up mid-copy (each small file takes a page): the partial stage goes.
    assert(!umount2(small,0));assert(!mount("carry-small",small,"tmpfs",0,"size=64k"));
    join(p,small,"Music");assert(!make_dirs(p));join(p,small,"Music/a.mp3");write_file(p,"audio");
    join(p,lib,"OverviewData");assert(!make_dirs(p));
    for(int i=0;i<40;i++){char f[64];snprintf(f,sizeof f,"OverviewData/%02d.dat",i);join(p,lib,f);write_file(p,"w");}
    assert(!carry_mount(target,img,small,notes));
    assert(count_notes(log,want)==2&&count_notes(notes,"cannot copy the library onto the stick")==1);
    expect_kept_read_only(target,small,db);
    assert(!umount2(small,0));
    int lfd=open(lib,O_RDONLY|O_DIRECTORY);assert(lfd>=0);remove_tree(lfd,"OverviewData");close(lfd);

    // Out of the mount budget: read-only, and not tried again until reboot.
    write_file(slow,"");
    assert(!carry_mount(target,img,disk,notes));
    assert(count_notes(log,want)==3);
    expect_kept_read_only(target,disk,db);
    assert(!carry_mount(target,img,disk,notes));
    assert(count_notes(log,want)==3&&count_notes(notes,"too large to carry over at mount")==1);
    expect_kept_read_only(target,disk,db);
    char marker[PATH_MAX];assert(!carry_marker(lib,marker)&&!unlink(marker));
    assert(!unlink(slow));test_hfs_target[0]=0;

    // No library left in /data: a plain rw mount, nothing carried.
    assert(!unlink(db));
    assert(carry_mount(target,img,disk,notes));
    assert(count_notes(log,want)==3);
    carry_unmount();
    join(p,disk,"Engine Library");assert(access(p,F_OK));
    join(p,lib,"Artwork/cover.jpg");assert(!unlink(p));join(p,lib,"Artwork");assert(!rmdir(p));
    unlink(log);test_guardian_system[0]=0;
    puts("HFS+ carry-over of the library Engine built in /data: refused copy, carried whole, own library never overwritten, no room, full mid-copy, out of budget: PASS");
}
static void test_hfs_rw_policy(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char disk[PATH_MAX],target[PATH_MAX],img[PATH_MAX],fsck[PATH_MAX],mroot[PATH_MAX],extra[PATH_MAX],ko[PATH_MAX],flat_ko[PATH_MAX],marker[PATH_MAX],procfs[PATH_MAX],dt[PATH_MAX],p[PATH_MAX];
    join(disk,root,"hfspol-disk");join(p,disk,"PIONEER/rekordbox");assert(!make_dirs(p));
    join(p,disk,"PIONEER/rekordbox/export.pdb");write_file(p,"pdb\n");
    join(target,MEDIA_ROOT,"HFSPOL");assert(!make_dirs(target));
    join(test_rb_bridge,root,"hfspol-bridge");write_file(test_rb_bridge,"#!/bin/sh\nexit 0\n");assert(!chmod(test_rb_bridge,0700));
    join(fsck,root,"hfspol-fsck");join(mroot,root,"hfspol-mods");join(extra,mroot,"6.18.57-az01-2026-10-15-rt6/extra");assert(!make_dirs(extra));
    join(ko,extra,"hfsplusx.ko");join(flat_ko,mroot,"hfsplusx.ko");
    join(marker,root,"hfspol-gen1");
    join(procfs,root,"hfspol-procfs");join(dt,root,"hfspol-dt");
    snprintf(test_disk,sizeof test_disk,"%s",disk);
    snprintf(test_fsck_bin,sizeof test_fsck_bin,"%s",fsck);
    snprintf(test_modules_root,sizeof test_modules_root,"%s",mroot);
    snprintf(test_gen1_marker,sizeof test_gen1_marker,"%s",marker);
    snprintf(test_proc_fs,sizeof test_proc_fs,"%s",procfs);
    snprintf(test_az04_dt,sizeof test_az04_dt,"%s",dt);

    // 1. Gen-1 clean non-journaled HFS+: fsck -n -f passes -> loads hfsplusx.ko (matching vermagic) -> mounts hfsplusx rw without view.
    write_file(marker,"v5.3.0.5\n");write_file(procfs,"nodev\ttmpfs\n\text4\n");
    write_file(fsck,"#!/bin/sh\n[ \"$1\" = -n ] && [ \"$2\" = -f ] || exit 9\necho '** The volume TEST appears to be OK.'\nexit 0\n");
    assert(!chmod(fsck,0700));
    write_fake_ko(ko,EM_ARM,"6.18.57-az01-2026-10-15-rt6 SMP preempt_rt ARMv7 p2v8 ","hfsplusx");
    join(img,root,"pol-clean.img");write_hfs_img(img,(1u<<8),0,0,0,0,0);
    test_platform_mode_val=1;test_finit_rc=0;test_finit_calls=0;test_disk_ro=0;test_hfs_target[0]=0;
    struct libmnt_context c={.target=target,.source=img,.invoke_sys_mount=1};
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid,nodev,uhelper=edisksd");
    unsigned before_views=observed_prepare_views;
    assert(!mnt_context_mount(&c)&&c.status==1);
    assert(!strcmp(c.last_mount_fstype,"hfsplusx")&&has_opt_token(c.last_mount_opts,"rw")&&!has_opt_token(c.last_mount_opts,"force"));
    assert(!(c.last_mount_mflags&MS_RDONLY)&&!(test_last_sys_flags&MS_RDONLY)&&test_fsconfig_ro_seen==0);
    assert(test_finit_calls==1&&observed_prepare_views==before_views);
    expect_not_preparing();
    // Clean eject on RW HFS+ (device present): MNT_DETACH / lazy is forced off even if edisksd enables lazy.
    test_dev_present_val=1;
    snprintf(test_hfs_rw_target,sizeof test_hfs_rw_target,"%s",target);
    mnt_context_enable_lazy(&c,1);assert(c.lazy==0);
    c.lazy=1;c.fail=1;assert(unmount_dispatch(&c,real_unmount)==-EBUSY&&c.last_umount_lazy==0);
    c.fail=0;c.lazy=1;assert(!unmount_dispatch(&c,real_unmount)&&c.last_umount_lazy==0);
    test_hfs_rw_target[0]=0;mnt_free_context(&c);

    // 2. Gen-1 already-loaded hfsplusx (e.g. insmod /tmp/hfsplusx.ko) works without another finit_module,
    //    and device removal while rw (stick pulled, device gone) keeps lazy unmount so stale mount clears.
    test_modules_root[0]=0;
    memset(&c,0,sizeof c);c.target=target;c.source=img;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1&&test_finit_calls==1&&!strcmp(c.last_mount_fstype,"hfsplusx"));
    test_dev_present_val=0;
    snprintf(test_hfs_rw_target,sizeof test_hfs_rw_target,"%s",target);
    mnt_context_enable_lazy(&c,1);assert(c.lazy==1);
    assert(!unmount_dispatch(&c,real_unmount)&&c.last_umount_lazy==1);
    test_hfs_rw_target[0]=0;test_dev_present_val=1;mnt_free_context(&c);
    snprintf(test_modules_root,sizeof test_modules_root,"%s",mroot);

    // 3. Gen-1 vermagic mismatch & insmod failure both fail closed to ro view.
    write_file(procfs,"nodev\ttmpfs\n");
    write_fake_ko(ko,EM_ARM,"6.18.57-mismatch SMP ARMv7 ","hfsplusx");
    memset(&c,0,sizeof c);c.target=target;c.source=img;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    snprintf(test_hfs_target,sizeof test_hfs_target,"%s",target);
    assert(!mnt_context_mount(&c)&&c.status==1&&test_finit_calls==1);
    assert(!strcmp(c.last_mount_fstype,"hfsplus")&&has_opt_token(c.last_mount_opts,"ro"));
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    write_fake_ko(ko,EM_ARM,"6.18.57-az01-2026-10-15-rt6 SMP preempt_rt ARMv7 p2v8 ","hfsplusx");
    test_finit_rc=-1;test_finit_errno=ENOEXEC;
    memset(&c,0,sizeof c);c.target=target;c.source=img;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1&&test_finit_calls==2);
    assert(!strcmp(c.last_mount_fstype,"hfsplus")&&has_opt_token(c.last_mount_opts,"ro"));
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);
    test_finit_rc=0;test_hfs_target[0]=0;

    // 4. fsck non-zero exit, abnormal exit-0 output, and missing fsck binary all fail closed to ro.
    write_file(fsck,"#!/bin/sh\necho 'Volume corrupt'\nexit 8\n");
    test_platform_mode_val=2;
    memset(&c,0,sizeof c);c.target=target;c.source=img;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&has_opt_token(c.last_mount_opts,"ro"));
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    write_file(fsck,"#!/bin/sh\necho 'fsck_hfs: Volume is journaled.  No checking performed.'\nexit 0\n");
    memset(&c,0,sizeof c);c.target=target;c.source=img;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&has_opt_token(c.last_mount_opts,"ro"));
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    test_fsck_bin[0]=0;
    memset(&c,0,sizeof c);c.target=target;c.source=img;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&has_opt_token(c.last_mount_opts,"ro"));
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    // 5. AZ04 journaled empty -> force,rw; journaled non-empty -> ro; rw mount failure -> ro fallback.
    snprintf(test_fsck_bin,sizeof test_fsck_bin,"%s",fsck);
    write_file(fsck,"#!/bin/sh\necho '** The volume TEST appears to be OK.'\nexit 0\n");
    join(img,root,"pol-jnl-empty.img");write_hfs_img(img,(1u<<8)|(1u<<13),1,0,4096,4096,0);
    memset(&c,0,sizeof c);c.target=target;c.source=img;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&!strcmp(c.last_mount_fstype,"hfsplus")&&
           has_opt_token(c.last_mount_opts,"force")&&has_opt_token(c.last_mount_opts,"rw"));
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    join(img,root,"pol-jnl-dirty.img");write_hfs_img(img,(1u<<8)|(1u<<13),1,0,4096,8192,0);
    memset(&c,0,sizeof c);c.target=target;c.source=img;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&has_opt_token(c.last_mount_opts,"ro")&&!has_opt_token(c.last_mount_opts,"force"));
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    join(img,root,"pol-clean.img");
    memset(&c,0,sizeof c);c.fail_rw_once=1;c.invoke_sys_mount=1;
    test_set_source(&c,img);test_set_target(&c,target);
    mnt_context_disable_helpers(&c,1);
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1&&c.mount_calls>=2&&
           has_opt_token(c.last_mount_opts,"ro")&&(c.last_mount_mflags&MS_RDONLY)&&
           (test_last_sys_flags&MS_RDONLY)&&test_fsconfig_rw_seen==0&&test_fsconfig_ro_seen>=2&&
           c.last_mount_disable_helpers==1&&c.source&&!strcmp(c.source,img)&&c.target&&!strcmp(c.target,target));
    assert(!unmount_dispatch(&c,real_unmount));
    // Reusing the same context via mnt_reset_context must drop prior HFS+ fstype/options state.
    mnt_reset_context(&c);
    test_set_source(&c,img);test_set_target(&c,target);
    mnt_context_set_fstype_pattern(&c,"ext4");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1&&!strcmp(c.last_mount_fstype,"ext4")&&!strcmp(c.last_mount_opts,"nosuid"));
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    // 5b. Unclean pull (UNMNT=0, bit 11=1): if -n -f is OK and journal is clean, mark clean (-y -f), re-check (-n -f), mount rw.
    //     If -n -f finds any error or journal is dirty, never attempt -y -f and stay ro.
    char fsck_log[PATH_MAX],fsck_sh[PATH_MAX*2];
    join(fsck_log,root,"hfspol-fsck.log");
    snprintf(fsck_sh,sizeof fsck_sh,
             "#!/bin/sh\necho \"$1 $2\" >> '%s'\n"
             "if [ \"$1\" = -y ] && [ \"$2\" = -f ]; then printf '\\011' | dd of=\"$3\" bs=1 seek=1030 count=1 conv=notrunc 2>/dev/null; fi\n"
             "echo '** The volume TEST appears to be OK.'\nexit 0\n",fsck_log);
    write_file(fsck,fsck_sh);
    join(img,root,"pol-pulled.img");write_hfs_img(img,(1u<<11),0,0,0,0,0);
    unlink(fsck_log);
    test_platform_mode_val=1;
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&!strcmp(c.last_mount_fstype,"hfsplusx")&&has_opt_token(c.last_mount_opts,"rw")&&!(test_last_sys_flags&MS_RDONLY));
    expect_file(fsck_log,"-n -f\n-y -f\n-n -f\n");
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    // Corrupt volume with UNMNT=0: -n -f reports repair needed -> never runs -y -f, stays ro.
    snprintf(fsck_sh,sizeof fsck_sh,
             "#!/bin/sh\necho \"$1 $2\" >> '%s'\n"
             "echo '** The volume TEST was found corrupt and needs to be repaired.'\nexit 0\n",fsck_log);
    write_file(fsck,fsck_sh);
    write_hfs_img(img,(1u<<11),0,0,0,0,0);
    unlink(fsck_log);
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&has_opt_token(c.last_mount_opts,"ro")&&(test_last_sys_flags&MS_RDONLY));
    expect_file(fsck_log,"-n -f\n");
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    // Unclean pull with dirty journal (start != end): even if -n -f prints OK, never runs -y -f, stays ro.
    snprintf(fsck_sh,sizeof fsck_sh,
             "#!/bin/sh\necho \"$1 $2\" >> '%s'\necho '** The volume TEST appears to be OK.'\nexit 0\n",fsck_log);
    write_file(fsck,fsck_sh);
    write_hfs_img(img,(1u<<13),1,0,4096,8192,0);
    unlink(fsck_log);
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&has_opt_token(c.last_mount_opts,"ro")&&(test_last_sys_flags&MS_RDONLY));
    expect_file(fsck_log,"-n -f\n");
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    // Failed mark-clean (e.g. write-protected device): -y -f fails -> stays ro.
    snprintf(fsck_sh,sizeof fsck_sh,
             "#!/bin/sh\necho \"$1 $2\" >> '%s'\n"
             "[ \"$1\" = -y ] && exit 8\necho '** The volume TEST appears to be OK.'\nexit 0\n",fsck_log);
    write_file(fsck,fsck_sh);
    write_hfs_img(img,(1u<<11),0,0,0,0,0);
    unlink(fsck_log);
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&has_opt_token(c.last_mount_opts,"ro")&&(test_last_sys_flags&MS_RDONLY));
    expect_file(fsck_log,"-n -f\n-y -f\n");
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    // 5c. The -n -f check took most of the budget: -y -f (and the check after it) would not fit, so it
    //     is not started; the stick stays ro and is marked clean on an insert with the time for it.
    snprintf(fsck_sh,sizeof fsck_sh,"#!/bin/sh\necho \"$1 $2\" >> '%s'\nsleep 0.6\necho '** The volume TEST appears to be OK.'\nexit 0\n",fsck_log);
    write_file(fsck,fsck_sh);
    write_hfs_img(img,(1u<<11),0,0,0,0,0);
    unlink(fsck_log);
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&has_opt_token(c.last_mount_opts,"ro")&&(test_last_sys_flags&MS_RDONLY));
    expect_file(fsck_log,"-n -f\n");
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    snprintf(fsck_sh,sizeof fsck_sh,"#!/bin/sh\necho \"$1 $2\" >> '%s'\necho '** The volume TEST appears to be OK.'\nexit 0\n",fsck_log);
    write_file(fsck,fsck_sh);
    join(img,root,"pol-clean.img");
    test_hfs_carry_over(root,target,img);

    // Restore clean image & fsck mock for sections 6-8.
    join(img,root,"pol-clean.img");
    write_file(fsck,"#!/bin/sh\necho '** The volume TEST appears to be OK.'\nexit 0\n");
    test_platform_mode_val=2;

    // 6. AZ04 DT flag coexistence: ufsd mounts stay untouched; hfsplus still mounts stock rw.
    write_file(dt,"1\n");
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"ufsd");mnt_context_set_options(&c,"utf8,nosuid");
    assert(!mnt_context_mount(&c)&&!strcmp(c.last_mount_fstype,"ufsd")&&!strcmp(c.last_mount_opts,"utf8,nosuid"));
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&!strcmp(c.last_mount_fstype,"hfsplus")&&has_opt_token(c.last_mount_opts,"rw")&&
           !(c.last_mount_mflags&MS_RDONLY)&&!(test_last_sys_flags&MS_RDONLY));
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    // 7. Gen-2 armhf (no GEN1_ROOTFS_MARKER) forces MS_RDONLY on non-journaled HFS+ without calling fsck or insmod.
    unlink(marker);
    write_file(fsck,"#!/bin/sh\nexit 99\n");
    test_platform_mode_val=1;int finit_before=test_finit_calls;
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&!strcmp(c.last_mount_fstype,"hfsplus")&&
           has_opt_token(c.last_mount_opts,"ro")&&!has_opt_token(c.last_mount_opts,"rw")&&
           (c.last_mount_mflags&MS_RDONLY)&&(test_last_sys_flags&MS_RDONLY)&&
           test_fsconfig_rw_seen==0&&test_fsconfig_ro_seen>=2);
    struct statvfs sv;assert(!statvfs(target,&sv)&&(sv.f_flag&ST_RDONLY));
    assert(test_finit_calls==finit_before);
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    // 8. Gen-1 without <modules_root>/<rel>/extra/hfsplusx.ko (even if a flat hfsplusx.ko exists) forces MS_RDONLY.
    write_file(marker,"v5.3.0.5\n");write_file(procfs,"nodev\ttmpfs\n");
    assert(!unlink(ko));
    write_fake_ko(flat_ko,EM_ARM,"6.18.57-az01-2026-10-15-rt6 SMP preempt_rt ARMv7 p2v8 ","hfsplusx");
    write_file(fsck,"#!/bin/sh\necho '** The volume TEST appears to be OK.'\nexit 0\n");
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&!strcmp(c.last_mount_fstype,"hfsplus")&&
           has_opt_token(c.last_mount_opts,"ro")&&(c.last_mount_mflags&MS_RDONLY)&&(test_last_sys_flags&MS_RDONLY));
    assert(!statvfs(target,&sv)&&(sv.f_flag&ST_RDONLY));
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    test_platform_mode_val=0;test_fsck_bin[0]=0;test_modules_root[0]=0;test_gen1_marker[0]=0;
    test_proc_fs[0]=0;test_az04_dt[0]=0;test_rb_bridge[0]=0;test_disk[0]=0;
    puts("HFS+ RW policy (Gen-1 hfsplusx, AZ04 hfsplus+ufsd, Gen-2 ro, fsck/journal/eject): PASS");
}

static void test_hfs_view_scale_and_47879b10_253(const char *root) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char disk[PATH_MAX],target[PATH_MAX],p[PATH_MAX],track[PATH_MAX],db[PATH_MAX],err_path[PATH_MAX];
    join(disk,root,"scale-disk");
    join(p,disk,"PIONEER/rekordbox");assert(!make_dirs(p));
    join(p,disk,"PIONEER/rekordbox/export.pdb");write_file(p,"pdb-scale\n");
    join(p,disk,"Engine Library/Database2");assert(!make_dirs(p));
    join(p,disk,"Engine Library/Database2/m.db");write_file(p,"native scale db\n");
    join(p,disk,"Engine Library/Music");assert(!make_dirs(p));
    join(p,disk,"Engine Library/Music/native.mp3");write_file(p,"native audio\n");
    for(int i=0;i<1300;i++) {
        char d[64];snprintf(d,sizeof d,"dir-%04d",i);
        join(p,disk,d);assert(!mkdir(p,0700));
    }
    join(track,disk,"dir-0500/track.mp3");write_file(track,"root audio\n");
    join(target,MEDIA_ROOT,"SCALE");assert(!make_dirs(target));
    join(test_rb_bridge,root,"scale-bridge");
    write_file(test_rb_bridge,"#!/bin/sh\nexit 1\n");assert(!chmod(test_rb_bridge,0700));
    snprintf(test_disk,sizeof test_disk,"%s",disk);test_disk_ro=1;
    snprintf(test_hfs_target,sizeof test_hfs_target,"%s",target);
    struct libmnt_context c={.target=target,.source="/dev/sdscale1"};
    // #253: bridge failure in MOUNT_PRE publishes .err, surviving MOUNT_POST prepare_view; state clears on exit.
    assert(!hooked_mount(&c,test_real_mount));
    expect_not_preparing();
    assert(!prepare_err_path(err_path));
    expect_file(err_path,"{\"state\":\"error\",\"kind\":\"usb\",\"error\":\"USB conversion failed\"}\n");
    assert(!unlink(err_path));
    // Constant mount count regardless of 1300 root folders.
    assert(count_mounts(target)<=6&&count_mounts(test_runtime)<=8);
    // hide_views hides stacked overlay + Engine Library + helper mounts, keeping only the base mount.
    int hfd=hide_views("/proc/self/mountinfo");assert(hfd>=0);
    char *mbuf=malloc(65536);assert(mbuf);
    ssize_t mn=pread(hfd,mbuf,65535,0);assert(mn>0);mbuf[mn]=0;close(hfd);
    unsigned hits=0;for(char *pos=mbuf;(pos=strstr(pos,target));pos+=strlen(target))hits++;
    assert(hits==1&&!strstr(mbuf,test_runtime)&&!strstr(mbuf,"Engine\\040Library"));
    free(mbuf);
    // Root track is readable and read-only; Engine Library/Database2/m.db is writable; native.mp3 is RO.
    join(p,target,"dir-0500/track.mp3");expect_file(p,"root audio\n");
    assert(open(p,O_WRONLY)<0&&errno==EROFS);
    int held=open(p,O_RDONLY);assert(held>=0);
    assert(unmount_dispatch(&c,real_unmount)==-EBUSY&&errno==EBUSY);
    close(held);
    join(p,target,"Engine Library/Music/native.mp3");expect_file(p,"native audio\n");
    assert(open(p,O_WRONLY)<0&&errno==EROFS);
    join(db,target,"Engine Library/Database2/m.db");write_file(db,"updated cue\n");
    // 47879b10: open database on /data does not block eject.
    held=open(db,O_RDONLY);assert(held>=0);
    assert(!unmount_dispatch(&c,real_unmount)&&count_mounts(target)==0);
    close(held);
    test_hfs_target[0]=0;test_disk_ro=0;test_disk[0]=0;test_rb_bridge[0]=0;
    puts("REAL PRIVATE NAMESPACE Scale + 47879b10 + #253: 1300 root folders use O(1) mounts, hide_views/eject/errors intact: PASS");
}

static int mock_mount_present(const char *a,const char *b) {
    FILE *f=fopen("/proc/self/mountinfo","r");if(!f)return 0;
    char *line=NULL;size_t cap=0;int hit=0;
    while(getline(&line,&cap,f)>0) {
        char *sp[5]={0};char *p=line;int idx=0;
        for(;idx<5;idx++){sp[idx]=p;char *q=strchr(p,' ');if(!q)break;p=q+1;}
        if(idx<5)continue;
        char *mpe=strchr(sp[4],' ');if(!mpe)continue;*mpe=0;
        if((a&&!strcmp(sp[4],a))||(b&&*b&&!strcmp(sp[4],b))){hit=1;break;}
    }
    free(line);fclose(f);return hit;
}
static int run_mock_lklfuse(int argc,char **argv) {
    if(argc<4)return 2;
    const char *stage=argv[3];
    const char *logf=getenv("TEST_LKL_LOG");
    if(logf) {
        FILE *f=fopen(logf,"w");
        if(f) {
            struct rlimit as_rl={0},st_rl={0};
            getrlimit(RLIMIT_AS,&as_rl);getrlimit(RLIMIT_STACK,&st_rl);
            const char *ma=getenv("MALLOC_ARENA_MAX");
            fprintf(f,"sched=%d nice=%d arena=%s as=%llu stack=%llu",
                    sched_getscheduler(0),getpriority(PRIO_PROCESS,0),ma?ma:"",
                    (unsigned long long)as_rl.rlim_cur,(unsigned long long)st_rl.rlim_cur);
            for(int i=1;i<argc;i++)fprintf(f," arg%d=%s",i,argv[i]);
            fputc('\n',f);fclose(f);
        }
    }
    const char *fail=getenv("TEST_LKL_FAIL");
    if(fail&&!strcmp(fail,"1"))return 1;
    const char *disk=getenv("TEST_LKL_DISK");
    if(!disk||mount(disk,stage,NULL,MS_BIND,NULL))return 1;
    const char *pidf=getenv("TEST_LKL_PIDFILE");
    if(pidf){FILE *pf=fopen(pidf,"w");if(pf){fprintf(pf,"%d\n",(int)getpid());fclose(pf);}}
    const char *tgt=getenv("TEST_LKL_TARGET");
    while(mock_mount_present(stage,tgt))usleep(10000);
    usleep(350000);
    const char *exf=getenv("TEST_LKL_EXITED");
    if(exf){FILE *ef=fopen(exf,"w");if(ef){fputs("exited\n",ef);fclose(ef);}}
    return 0;
}
static void test_hfs_gen2_unit(const char *root) {
    char img[PATH_MAX],mi[PATH_MAX],opts[1024],out[8192];
    int jnl=99,unclean=99;
    join(img,root,"g2-unclean.img");write_hfs_img(img,0u,0,0,0,0,0);
    assert(hfs_check_volume_and_journal(img,&jnl,0,NULL)==-1&&errno==EIO);
    assert(!hfs_check_volume_and_journal(img,&jnl,1,&unclean)&&jnl==0&&unclean==1);

    join(img,root,"g2-unclean-jnl.img");write_hfs_img(img,(1u<<13),1,0,4096,4096,0);
    assert(hfs_check_volume_and_journal(img,&jnl,0,NULL)==-1);
    assert(!hfs_check_volume_and_journal(img,&jnl,1,&unclean)&&jnl==1&&unclean==1);

    join(img,root,"g2-softlock.img");write_hfs_img(img,(1u<<15),0,0,0,0,0);
    assert(hfs_check_volume_and_journal(img,&jnl,1,&unclean)==-1);

    join(img,root,"g2-dirty-jnl.img");write_hfs_img(img,(1u<<13),1,0,4096,8192,0);
    assert(hfs_check_volume_and_journal(img,&jnl,1,&unclean)==-1);

    assert(!hfs_gen2_build_opts(opts,sizeof opts,"/dev/sda1","nosuid,nodev,uhelper=edisksd,ro",MS_RELATIME,0));
    assert(has_opt_token(opts,"type=hfsplus")&&has_opt_token(opts,"mb=32")&&has_opt_token(opts,"fsname=/dev/sda1")&&
           has_opt_token(opts,"subtype=hfsplus")&&has_opt_token(opts,"allow_other")&&has_opt_token(opts,"default_permissions")&&
           has_opt_token(opts,"nosuid")&&has_opt_token(opts,"nodev")&&has_opt_token(opts,"relatime")&&
           !has_opt_token(opts,"opts=force")&&!has_opt_token(opts,"uhelper=edisksd")&&!has_opt_token(opts,"ro"));
    assert(!hfs_gen2_build_opts(opts,sizeof opts,"/dev/a,b","nosuid",0,1));
    assert(strstr(opts,"fsname=/dev/a\\,b")&&has_opt_token(opts,"opts=force"));

    // Verify hide_views rewrites 0:anon -> block dev major:minor for active lklfuse mounts.
    assert(!make_dirs(RUN_ROOT));
    struct hfs_gen2_mount rec={0};
    rec.magic=HFS_GEN2_MAGIC;rec.dev_maj=8;rec.dev_min=1;rec.fuse_maj=0;rec.fuse_min=59;
    join(rec.target,MEDIA_ROOT,"MY USB");
    snprintf(rec.source,sizeof rec.source,"/dev/sda1");
    assert(!hfs_gen2_save_record(&rec));
    join(mi,root,"g2-mountinfo");
    FILE *f=fopen(mi,"w");assert(f);
    fprintf(f,"274 198 0:59 / %s/MY\\040USB rw,nosuid - fuse.hfsplus /dev/sda1 rw,allow_other\n",MEDIA_ROOT);
    fprintf(f,"275 198 8:17 / %s/OTHER rw - vfat /dev/sdb1 rw\n",MEDIA_ROOT);
    fclose(f);
    int fd=hide_views(mi);assert(fd>=0);
    ssize_t n=pread(fd,out,sizeof out-1,0);assert(n>0);out[n]=0;close(fd);
    char expect[PATH_MAX*2];
    snprintf(expect,sizeof expect,"274 198 8:1 / %s/MY\\040USB rw,nosuid - fuse.hfsplus /dev/sda1 rw",MEDIA_ROOT);
    assert(strstr(out,expect));
    assert(strstr(out,"275 198 8:17 / "));
    hfs_gen2_remove_record(rec.target);
    puts("Gen-2 HFS+ unit checks (unclean/journal/opts/mountinfo-devno): PASS");
}
struct gen2_unmount_args {
    struct libmnt_context *ctx;
    int rc;
};
static void *gen2_unmount_thread(void *arg) {
    struct gen2_unmount_args *a=arg;
    a->rc=unmount_dispatch(a->ctx,real_unmount);
    return NULL;
}
static void test_hfs_gen2_lkl_policy(const char *root,const char *self_exe) {
    assert(!mount(NULL,"/",NULL,MS_REC|MS_PRIVATE,NULL));
    char disk[PATH_MAX],target[PATH_MAX],img[PATH_MAX],fsck[PATH_MAX],lkl[PATH_MAX],p[PATH_MAX];
    char logf[PATH_MAX],pidf[PATH_MAX],exf[PATH_MAX];
    join(disk,root,"g2-disk");join(p,disk,"PIONEER/rekordbox");assert(!make_dirs(p));
    join(p,disk,"PIONEER/rekordbox/export.pdb");write_file(p,"pdb\n");
    join(target,MEDIA_ROOT,"HFSG2");assert(!make_dirs(target));
    join(test_rb_bridge,root,"g2-bridge");
    char bsh[512];
    snprintf(bsh,sizeof bsh,"#!/bin/sh\nmkdir -p \"$2/Engine Library/Database2\"\n: > \"$2/Engine Library/Database2/m.db\"\nexit 0\n");
    write_file(test_rb_bridge,bsh);assert(!chmod(test_rb_bridge,0700));
    join(fsck,root,"g2-fsck");join(lkl,root,"g2-lklfuse");
    join(logf,root,"g2-lkl.log");join(pidf,root,"g2-lkl.pid");join(exf,root,"g2-lkl.exited");
    char exe_real[PATH_MAX];ssize_t er=readlink("/proc/self/exe",exe_real,sizeof exe_real-1);
    if(er>0)exe_real[er]=0;else snprintf(exe_real,sizeof exe_real,"%s",self_exe);
    char lsh[PATH_MAX+128];
    snprintf(lsh,sizeof lsh,"#!/bin/sh\nexec \"%s\" --mock-lklfuse \"$@\"\n",exe_real);
    write_file(lkl,lsh);assert(!chmod(lkl,0700));
    snprintf(test_disk,sizeof test_disk,"%s",disk);
    snprintf(test_fsck_bin,sizeof test_fsck_bin,"%s",fsck);
    snprintf(test_lklfuse_bin,sizeof test_lklfuse_bin,"%s",lkl);
    test_gen1_marker[0]=0;test_proc_fs[0]=0;test_proc_mi[0]=0;test_az04_dt[0]=0;test_modules_root[0]=0;
    test_platform_mode_val=1;test_disk_ro=0;
    setenv("TEST_LKL_LOG",logf,1);setenv("TEST_LKL_DISK",disk,1);
    setenv("TEST_LKL_TARGET",target,1);setenv("TEST_LKL_PIDFILE",pidf,1);setenv("TEST_LKL_EXITED",exf,1);
    unsetenv("TEST_LKL_FAIL");

    // 1. fsck fail -> kernel hfsplus stays RO, lklfuse never started.
    unlink(logf);
    write_file(fsck,"#!/bin/sh\necho 'Volume corrupt'\nexit 8\n");assert(!chmod(fsck,0700));
    join(img,root,"g2-pol-clean.img");write_hfs_img(img,(1u<<8),0,0,0,0,0);
    struct libmnt_context c={.target=target,.source=img,.invoke_sys_mount=1};
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1&&!strcmp(c.last_mount_fstype,"hfsplus")&&
           has_opt_token(c.last_mount_opts,"ro")&&(test_last_sys_flags&MS_RDONLY)&&access(logf,F_OK)==-1);
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    // 2. Clean HFS+ -> lklfuse RW + pre-visibility bridge + sched/mem limits + unmount waits for lklfuse exit.
    write_file(fsck,"#!/bin/sh\n[ \"$1\" = -n ] && [ \"$2\" = -f ] || exit 9\necho '** The volume TEST appears to be OK.'\nexit 0\n");
    unlink(exf);unlink(pidf);unlink(logf);
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid,nodev,uhelper=edisksd");
    assert(!mnt_context_mount(&c)&&c.status==1);
    struct statvfs sv;assert(!statvfs(target,&sv)&&!(sv.f_flag&ST_RDONLY));
    join(p,target,"Engine Library/Database2/m.db");assert(!access(p,F_OK));
    char lbuf[2048];int lfd=open(logf,O_RDONLY);assert(lfd>=0);
    ssize_t ln=read(lfd,lbuf,sizeof lbuf-1);assert(ln>0);lbuf[ln]=0;close(lfd);
    assert(strstr(lbuf,"sched=0 ")&&strstr(lbuf,"nice=2 ")&&strstr(lbuf,"arena=1 ")&&
           strstr(lbuf,"type=hfsplus,mb=32")&&strstr(lbuf,"subtype=hfsplus")&&strstr(lbuf,"nosuid,nodev"));
#ifndef __SANITIZE_ADDRESS__
    assert(strstr(lbuf,"as=134217728 stack=1048576"));
#endif
    char pbuf[64];int pfd=open(pidf,O_RDONLY);assert(pfd>=0);
    ssize_t pn=read(pfd,pbuf,sizeof pbuf-1);assert(pn>0);pbuf[pn]=0;close(pfd);
    pid_t lpid=(pid_t)atoi(pbuf);assert(lpid>0);
    // Verify eject while another stick holds view_lock does not block (BH-O55-3).
    pthread_mutex_lock(&view_lock);
    struct gen2_unmount_args ua={.ctx=&c,.rc=-1};
    pthread_t ut;assert(!pthread_create(&ut,NULL,gen2_unmount_thread,&ua));
    int64_t u0=now_ms();
    assert(!pthread_join(ut,NULL)&&ua.rc==0);
    int64_t uel=now_ms()-u0;
    pthread_mutex_unlock(&view_lock);
    assert(uel>=280&&!access(exf,F_OK));
    assert(kill(lpid,0)==-1&&errno==ESRCH);
    mnt_free_context(&c);

    // 2b. Clean eject while a track fd is open (not pulled) must fail EBUSY and keep lklfuse alive; closing fd then allows clean eject.
    unlink(exf);unlink(pidf);unlink(logf);
    memset(&c,0,sizeof c);c.target=target;c.source=img;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1);
    pfd=open(pidf,O_RDONLY);assert(pfd>=0);pn=read(pfd,pbuf,sizeof pbuf-1);pbuf[pn]=0;close(pfd);
    lpid=(pid_t)atoi(pbuf);assert(lpid>0);
    int busy_fd=open(p,O_RDONLY);assert(busy_fd>=0);
    assert(unmount_dispatch(&c,real_unmount)!=0);
    assert(kill(lpid,0)==0&&access(exf,F_OK)==-1&&count_mounts(target)==1);
    close(busy_fd);
    u0=now_ms();assert(!unmount_dispatch(&c,real_unmount));uel=now_ms()-u0;
    assert(uel>=280&&!access(exf,F_OK)&&kill(lpid,0)==-1&&errno==ESRCH);
    mnt_free_context(&c);

    // 2c. SOFTLOCK or dirty journal fails volume/journal check -> RO guard without calling fsck -y or lklfuse.
    join(img,root,"g2-pol-dirty-jnl.img");write_hfs_img(img,(1u<<8)|(1u<<13),1,0,4096,8192,0);
    unlink(logf);
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1&&has_opt_token(c.last_mount_opts,"ro")&&(test_last_sys_flags&MS_RDONLY)&&access(logf,F_OK)==-1);
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    // 3. Unclean (!UNMNT) but fsck-clean + journal-clean -> fsck -y marks clean, re-checks -n -f, then lklfuse RW (with opts=force for journaled).
    join(img,root,"g2-pol-unclean-jnl.img");write_hfs_img(img,(1u<<13),1,0,4096,4096,0);
    char ylog[PATH_MAX];join(ylog,root,"g2-y.log");
    setenv("TEST_G2_YLOG",ylog,1);setenv("TEST_G2_YIMG",img,1);
    const char *fsh="#!/bin/sh\necho \"$1 $2\" >> \"$TEST_G2_YLOG\"\nif [ \"$1\" = -y ]; then\n"
                    "  python3 -c 'import os,struct; f=open(os.environ[\"TEST_G2_YIMG\"],\"r+b\"); f.seek(1028); a=struct.unpack(\">I\",f.read(4))[0]; f.seek(1028); f.write(struct.pack(\">I\",a|(1<<8)))'\n"
                    "fi\necho '** The volume TEST appears to be OK.'\nexit 0\n";
    write_file(fsck,fsh);assert(!chmod(fsck,0700));
    unlink(exf);unlink(pidf);unlink(logf);unlink(ylog);
    memset(&c,0,sizeof c);c.target=target;c.source=img;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1);
    assert(!statvfs(target,&sv)&&!(sv.f_flag&ST_RDONLY));
    lfd=open(logf,O_RDONLY);assert(lfd>=0);ln=read(lfd,lbuf,sizeof lbuf-1);lbuf[ln]=0;close(lfd);
    assert(strstr(lbuf,"opts=force"));
    expect_file(ylog,"-n -f\n-y -f\n-n -f\n");
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    // 4. Device pulled while mounted (open fd keeps normal umount EBUSY) -> lazy detach + kill lklfuse, no hang.
    unlink(exf);unlink(pidf);unlink(logf);
    memset(&c,0,sizeof c);c.target=target;c.source=img;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1);
    pfd=open(pidf,O_RDONLY);assert(pfd>=0);pn=read(pfd,pbuf,sizeof pbuf-1);pbuf[pn]=0;close(pfd);
    lpid=(pid_t)atoi(pbuf);assert(lpid>0);
    int held=open(p,O_RDONLY);assert(held>=0);
    unlink(img); // device node removed while mounted
    u0=now_ms();
    assert(!unmount_dispatch(&c,real_unmount));
    uel=now_ms()-u0;
    assert(uel<250&&count_mounts(target)==0&&access(exf,F_OK)==-1);
    assert(kill(lpid,0)==-1&&errno==ESRCH);
    close(held);mnt_free_context(&c);

    // 4b. udev remove (enable_lazy=1 while node still exists and fd held) -> lazy detach + kill lklfuse, no hang.
    join(img,root,"g2-pol-clean.img");
    unlink(exf);unlink(pidf);unlink(logf);
    memset(&c,0,sizeof c);c.target=target;c.source=img;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1);
    pfd=open(pidf,O_RDONLY);assert(pfd>=0);pn=read(pfd,pbuf,sizeof pbuf-1);pbuf[pn]=0;close(pfd);
    lpid=(pid_t)atoi(pbuf);assert(lpid>0);
    held=open(p,O_RDONLY);assert(held>=0);
    mnt_context_enable_lazy(&c,1);
    u0=now_ms();assert(!unmount_dispatch(&c,real_unmount));uel=now_ms()-u0;
    assert(uel<250&&count_mounts(target)==0&&access(exf,F_OK)==-1&&kill(lpid,0)==-1&&errno==ESRCH);
    close(held);mnt_free_context(&c);

    // 4c. Dead lklfuse (target lstat ENOTCONN while an fd is open) -> still detaches and cleans up record.
    join(img,root,"g2-pol-clean.img");
    unlink(exf);unlink(pidf);unlink(logf);
    memset(&c,0,sizeof c);c.target=target;c.source=img;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1);
    pfd=open(pidf,O_RDONLY);assert(pfd>=0);pn=read(pfd,pbuf,sizeof pbuf-1);pbuf[pn]=0;close(pfd);
    lpid=(pid_t)atoi(pbuf);assert(lpid>0);
    held=open(p,O_RDONLY);assert(held>=0);
    assert(!kill(lpid,SIGKILL));
    while(waitpid(lpid,NULL,0)<0&&errno==EINTR){}
    snprintf(test_eio_path,sizeof test_eio_path,"%s",target);test_eio_err=ENOTCONN;
    assert(!unmount_dispatch(&c,real_unmount));
    test_eio_path[0]=0;test_eio_err=EIO;
    assert(count_mounts(target)==0);
    close(held);mnt_free_context(&c);

    // 5. lklfuse failure -> clean RO fallback.
    join(img,root,"g2-pol-clean.img");
    setenv("TEST_LKL_FAIL","1",1);
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1&&!strcmp(c.last_mount_fstype,"hfsplus")&&
           has_opt_token(c.last_mount_opts,"ro")&&(test_last_sys_flags&MS_RDONLY));
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    unsetenv("TEST_LKL_FAIL");
    // 6. Gen-2 carry-over via lklfuse: refused copy cleanly tears down lklfuse and falls back RO; valid copy lands on stick RW.
    char cdisk[PATH_MAX],clib[PATH_MAX],cdb[PATH_MAX],crefuse[PATH_MAX],cgsh[PATH_MAX*2];
    join(cdisk,root,"g2-carry-disk");join(p,cdisk,"Music");assert(!make_dirs(p));
    join(p,cdisk,"Music/a.mp3");write_file(p,"audio");
    join(clib,LIB_ROOT,"uuid-hfs-stick/library-0000000000000000");
    join(p,clib,"Database2");assert(!make_dirs(p));
    join(cdb,clib,"Database2/m.db");write_file(cdb,"g2-deck-lib");
    assert(!utimensat(AT_FDCWD,clib,NULL,0));
    join(crefuse,root,"g2-carry-refuse");join(test_guardian_system,root,"g2-carry-guardian");
    snprintf(cgsh,sizeof cgsh,"#!/bin/sh\n[ \"$1\" = --check-copy ] || exit 0\n[ -e '%s' ] && exit 1\nexit 0\n",crefuse);
    write_file(test_guardian_system,cgsh);assert(!chmod(test_guardian_system,0700));
    setenv("TEST_LKL_DISK",cdisk,1);snprintf(test_disk,sizeof test_disk,"%s",cdisk);
    write_file(crefuse,"");snprintf(test_hfs_target,sizeof test_hfs_target,"%s",target);
    unlink(exf);unlink(pidf);unlink(logf);
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1&&has_opt_token(c.last_mount_opts,"ro")&&(test_last_sys_flags&MS_RDONLY));
    assert(!access(exf,F_OK));
    join(p,cdisk,"Engine Library");assert(access(p,F_OK)&&errno==ENOENT);
    assert(!unmount_dispatch(&c,real_unmount));mnt_free_context(&c);

    assert(!unlink(crefuse));test_hfs_target[0]=0;unlink(exf);unlink(pidf);unlink(logf);
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1&&!statvfs(target,&sv)&&!(sv.f_flag&ST_RDONLY));
    join(p,cdisk,"Engine Library/Database2/m.db");expect_file(p,"g2-deck-lib");
    assert(!unmount_dispatch(&c,real_unmount)&&!access(exf,F_OK));mnt_free_context(&c);
    assert(!unlink(cdb));

    // 7. Out of budget once lklfuse has staged: mount RW succeeds without running rb-bridge or writing slow-.
    char odisk[PATH_MAX];
    join(odisk,root,"g2-oob-disk");join(p,odisk,"PIONEER/rekordbox");assert(!make_dirs(p));
    join(p,odisk,"PIONEER/rekordbox/export.pdb");write_file(p,"pdb\n");
    test_guardian_system[0]=0;
    setenv("TEST_LKL_DISK",odisk,1);snprintf(test_disk,sizeof test_disk,"%s",odisk);
    unsigned slow_before=count_runtime("slow-");
    test_expire_gen2_stage=1;unlink(exf);unlink(pidf);unlink(logf);
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1&&!statvfs(target,&sv)&&!(sv.f_flag&ST_RDONLY));
    join(p,odisk,"Engine Library");assert(access(p,F_OK)==-1&&errno==ENOENT);
    assert(count_runtime("slow-")==slow_before);
    assert(!unmount_dispatch(&c,real_unmount)&&!access(exf,F_OK));mnt_free_context(&c);
    test_expire_gen2_stage=0;unlink(exf);unlink(pidf);unlink(logf);
    memset(&c,0,sizeof c);c.target=target;c.source=img;c.invoke_sys_mount=1;
    mnt_context_set_fstype_pattern(&c,"hfsplus");mnt_context_set_options(&c,"nosuid");
    assert(!mnt_context_mount(&c)&&c.status==1&&!statvfs(target,&sv)&&!(sv.f_flag&ST_RDONLY));
    join(p,odisk,"Engine Library/Database2/m.db");expect_file(p,"");
    assert(!unmount_dispatch(&c,real_unmount)&&!access(exf,F_OK));mnt_free_context(&c);

    unsetenv("TEST_LKL_FAIL");unsetenv("TEST_LKL_LOG");unsetenv("TEST_LKL_DISK");
    unsetenv("TEST_LKL_TARGET");unsetenv("TEST_LKL_PIDFILE");unsetenv("TEST_LKL_EXITED");unsetenv("TEST_G2_YLOG");unsetenv("TEST_G2_YIMG");
    test_platform_mode_val=0;test_fsck_bin[0]=0;test_lklfuse_bin[0]=0;test_rb_bridge[0]=0;test_guardian_system[0]=0;test_hfs_target[0]=0;test_disk[0]=0;test_modules_root[0]=0;
    puts("Gen-2 lklfuse policy (fsck-fail->ro, clean->rw+wait, unclean->fsck-y->rw, pull->lazy+kill, lkl-fail->ro, carry-over): PASS");
}
int main(int argc,char **argv) {
    if(argc>1&&!strcmp(argv[1],"--mock-lklfuse"))return run_mock_lklfuse(argc,argv);
    char root[]="/tmp/enginex-edisks-tests-XXXXXX";assert(mkdtemp(root));
    join(test_prepare_state,root,"run/enginex-usb-preparing");char prep_run[PATH_MAX];join(prep_run,root,"run");assert(!make_dirs(prep_run));
    join(test_media,root,"media");join(test_library,root,"libraries");join(test_runtime,root,"runtime");
    join(test_legacy_library,root,"legacy-libraries");join(test_utab,root,"utab");assert(!make_dirs(test_utab));
    assert(!make_dirs(test_media));test_paths_and_copy(root);test_compat_symlink_root(root);test_time();test_slow_marker_follows_every_export_file(root);test_only_an_export_makes_a_rekordbox_stick(root);test_monitor_sees_plain_device_mount(root);test_monitor_hides_new_and_old_view_names(root);test_hfs_rw_unit(root);test_hfs_gen2_unit(root);
    if(argc>1&&!strcmp(argv[1],"--mounts")){test_prepare_state_lifecycle(root);test_hfs_rw_policy(root);test_hfs_gen2_lkl_policy(root,argv[0]);test_mounts(root);test_foreign_owned_root(root);test_hfs_mount_runs_bridge(root);test_first_insert_converted_before_visible(root);test_library_is_walked_once(root);test_two_sticks_at_once(root);test_hfs_visible_mount_reuses_the_converted_library(root);test_private_mount_leaves_nothing_behind(root);test_hfs_view_copies_only_metadata(root);test_heal_before_visible(root);test_eject_while_another_stick_converts(root);test_eject_of_a_pulled_stick(root);test_heal_marker_survives_another_port(root);test_hfs_view_with_a_long_entry_name(root);test_hfs_view_artwork_is_writable(root);test_seed_keeps_its_own_previous_generation(root);test_view_binds_stay_out_of_a_shared_data(root);test_guardian_from_the_data_bundle(root);test_slow_marker_survives_another_port(root);test_hfs_view_scale_and_47879b10_253(root);}
    // Mount tests have removed all namespace mounts before deleting fixtures.
    int tmp=open("/tmp",O_RDONLY|O_DIRECTORY);assert(tmp>=0);remove_tree(tmp,strrchr(root,'/')+1);close(tmp);
    puts("edisksd-hook: all requested tests passed");return 0;
}
