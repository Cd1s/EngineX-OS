// drmgrab: dump the framebuffer currently scanned out on a DRM plane as raw
// pixels on stdout. Engine renders through KMS, so /dev/fb0 only holds the
// boot splash; this reads what the panel actually shows.
//
//   drmgrab [/dev/dri/card0] > shot.raw     (stderr: "WxH pitch fourcc")
//
// Needs root (GETFB returns a handle only to CAP_SYS_ADMIN). No libdrm.
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <unistd.h>

#define DRM_IOCTL_BASE 'd'
#define DRM_IOWR(nr, type) _IOWR(DRM_IOCTL_BASE, nr, type)

struct drm_set_client_cap { uint64_t capability, value; };
struct drm_mode_get_plane_res { uint64_t plane_id_ptr; uint32_t count_planes; };
struct drm_mode_get_plane {
    uint32_t plane_id, crtc_id, fb_id, possible_crtcs, gamma_size, count_format_types;
    uint64_t format_type_ptr;
};
struct drm_mode_fb_cmd { uint32_t fb_id, width, height, pitch, bpp, depth, handle; };
struct drm_mode_map_dumb { uint32_t handle, pad; uint64_t offset; };
struct drm_gem_close { uint32_t handle, pad; };

#define SET_CLIENT_CAP DRM_IOWR(0x0D, struct drm_set_client_cap)
#define GETPLANERESOURCES DRM_IOWR(0xB5, struct drm_mode_get_plane_res)
#define GETPLANE DRM_IOWR(0xB6, struct drm_mode_get_plane)
#define GETFB DRM_IOWR(0xAD, struct drm_mode_fb_cmd)
#define MAP_DUMB DRM_IOWR(0xB3, struct drm_mode_map_dumb)
#define GEM_CLOSE _IOW(DRM_IOCTL_BASE, 0x09, struct drm_gem_close)

int main(int argc, char **argv) {
    const char *dev = argc > 1 ? argv[1] : "/dev/dri/card0";
    int fd = open(dev, O_RDWR | O_CLOEXEC);
    if (fd < 0) { perror(dev); return 1; }
    struct drm_set_client_cap cap = {2 /* UNIVERSAL_PLANES */, 1};
    ioctl(fd, SET_CLIENT_CAP, &cap);
    struct drm_mode_get_plane_res res = {0};
    if (ioctl(fd, GETPLANERESOURCES, &res) || !res.count_planes) { perror("planes"); return 1; }
    uint32_t *ids = calloc(res.count_planes, sizeof *ids);
    res.plane_id_ptr = (uintptr_t)ids;
    if (ioctl(fd, GETPLANERESOURCES, &res)) { perror("planes"); return 1; }
    struct drm_mode_fb_cmd fb = {0};
    for (uint32_t i = 0; i < res.count_planes; i++) {
        struct drm_mode_get_plane p = {.plane_id = ids[i]};
        if (ioctl(fd, GETPLANE, &p) || !p.fb_id || !p.crtc_id) continue;
        fb.fb_id = p.fb_id;
        if (ioctl(fd, GETFB, &fb) == 0 && fb.handle) break;
        fb.handle = 0;
    }
    if (!fb.handle) { fprintf(stderr, "no scanned-out framebuffer with a handle\n"); return 1; }
    struct drm_mode_map_dumb map = {.handle = fb.handle};
    if (ioctl(fd, MAP_DUMB, &map)) { perror("map"); return 1; }
    size_t size = (size_t)fb.pitch * fb.height;
    void *px = mmap(NULL, size, PROT_READ, MAP_SHARED, fd, (off_t)map.offset);
    if (px == MAP_FAILED) { perror("mmap"); return 1; }
    fprintf(stderr, "%ux%u pitch=%u bpp=%u fb=%u\n", fb.width, fb.height, fb.pitch, fb.bpp, fb.fb_id);
    for (size_t off = 0; off < size;) {
        ssize_t n = write(STDOUT_FILENO, (char *)px + off, size - off);
        if (n <= 0) { perror("write"); return 1; }
        off += (size_t)n;
    }
    munmap(px, size);
    struct drm_gem_close gc = {.handle = fb.handle};
    ioctl(fd, GEM_CLOSE, &gc);
    return 0;
}
