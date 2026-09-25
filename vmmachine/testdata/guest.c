// Linux guest workload for PMEM/DAX and RAM capture qualification.
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/fiemap.h>
#include <linux/fs.h>
#include <linux/magic.h>
#include <linux/vm_sockets.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/ioctl.h>
#include <sys/mount.h>
#include <sys/resource.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/statvfs.h>
#include <sys/sysmacros.h>
#include <sys/time.h>
#include <sys/vfs.h>
#include <sys/wait.h>
#include <termios.h>
#include <time.h>
#include <unistd.h>

static void fail(const char *what) {
    printf("SPROUTFS_ERROR %s: %s\n", what, strerror(errno));
    for (;;) pause();
}

static void flush_value(int fd, volatile uint64_t *disk) {
    if (msync((void *)disk, 4096, MS_SYNC) || fsync(fd)) fail("PMEM flush");
    struct { struct fiemap map; struct fiemap_extent extent; } query = {0};
    query.map.fm_length = 4096; query.map.fm_extent_count = 1;
    if (ioctl(fd, FS_IOC_FIEMAP, &query) || query.map.fm_mapped_extents != 1) fail("locate DAX extent");
    printf("SPROUTFS_FLUSH disk=%lu offset=%llu\n", *disk, query.extent.fe_physical);
}

static uint64_t nanoseconds(const struct timeval *t) {
    return (uint64_t)t->tv_sec * 1000000000u + (uint64_t)t->tv_usec * 1000u;
}

// monotonic is the guest's CLOCK_MONOTONIC, which starts when the kernel brings
// its clocksource up. Reported at init entry and again at readiness, it splits
// a boot into the kernel's own time and the init's without the host having to
// guess from console timestamps.
static uint64_t monotonic(void) {
    struct timespec now;
    clock_gettime(CLOCK_MONOTONIC, &now);
    return (uint64_t)now.tv_sec * 1000000000u + (uint64_t)now.tv_nsec;
}

// run_command executes one shell command and reports its wall time and the
// rusage the kernel charged it. The child never shares the console's input:
// the supervisor's next command line must not be consumed by the workload.
static void run_command(char *command) {
    struct timespec started, finished;
    clock_gettime(CLOCK_MONOTONIC, &started);
    pid_t child = fork();
    if (child == 0) {
        int null = open("/dev/null", O_RDONLY);
        if (null >= 0) { dup2(null, 0); close(null); }
        execl("/bin/sh", "sh", "-c", command, (char *)NULL);
        _exit(127);
    }
    if (child < 0) fail("fork");
    int status = 0;
    struct rusage usage;
    memset(&usage, 0, sizeof(usage));
    while (wait4(child, &status, 0, &usage) < 0) {
        if (errno != EINTR) fail("wait4");
    }
    clock_gettime(CLOCK_MONOTONIC, &finished);
    uint64_t wall = (uint64_t)(finished.tv_sec - started.tv_sec) * 1000000000u +
                    (uint64_t)finished.tv_nsec - (uint64_t)started.tv_nsec;
    int code = WIFEXITED(status) ? WEXITSTATUS(status) : 128 + WTERMSIG(status);
    printf("SPROUTFS_RUN exit=%d wall_ns=%llu user_ns=%llu sys_ns=%llu\n", code,
           (unsigned long long)wall, (unsigned long long)nanoseconds(&usage.ru_utime),
           (unsigned long long)nanoseconds(&usage.ru_stime));
}

// name_root_device gives /dev/root a node, exactly as deploy/guest/init.c does
// in a deployment's own guests. The kernel mounted the root filesystem itself,
// from the device root= names, and lists it in /proc/mounts as /dev/root — a
// name devtmpfs never creates. A tool that asks which device a filesystem is
// on, the guest witness's grow among them, looks that name up and finds
// nothing. A symlink to the device root= named is what answers the lookup.
static void name_root_device(void) {
    char cmdline[4096];
    FILE *file = fopen("/proc/cmdline", "r");
    if (!file) return;
    size_t n = fread(cmdline, 1, sizeof(cmdline) - 1, file);
    fclose(file);
    cmdline[n] = '\0';
    const char *root = strstr(cmdline, "root=/dev/");
    if (!root) return;
    root += strlen("root=/dev/");
    char name[64];
    size_t len = strcspn(root, " \n");
    if (len == 0 || len >= sizeof(name)) return;
    memcpy(name, root, len);
    name[len] = '\0';
    if (symlink(name, "/dev/root") && errno != EEXIST) {
        printf("SPROUTFS_ERROR /dev/root: %s\n", strerror(errno));
    }
}

// start_agent runs the guest agent, which serves the host over this VM's vsock
// rather than over this console. That is the channel a deployment reaches a
// guest on, so a suite that exercises it needs the real agent running in a real
// guest; a root image built without one boots exactly as it always did.
static void start_agent(void) {
    if (access("/agent", X_OK)) return;
    pid_t child = fork();
    if (child < 0) fail("fork agent");
    if (child == 0) {
        execl("/agent", "/agent", (char *)NULL);
        _exit(127);
    }
}

// hog runs one hostile load in a child until the VM ends, so the console loop
// goes on answering. It stands in for untrusted code that uses as much of the
// host as its guest can reach:
//   ram N    stores into every 4 KiB page of N MiB of RAM, over and over;
//   disk N   does the same over an N MiB file on the DAX root;
//   sync N   writes one 4 KiB block of an N MiB file on the root and fsyncs it,
//            over and over, which is a virtio-pmem flush each time;
//   vsock N  connects to the host over the vsock, where nothing listens, over
//            and over, N connections to a pass.
// The child says when it has been over the whole of it once.
static void hog(const char *kind, unsigned long size) {
    if (!size || (strcmp(kind, "vsock") && size > 1024)) { errno = EINVAL; fail("hog size"); }
    pid_t child = fork();
    if (child < 0) fail("fork hog");
    if (child > 0) { printf("SPROUTFS_HOG kind=%s size=%lu\n", kind, size); return; }
    int null = open("/dev/null", O_RDONLY);
    if (null >= 0) { dup2(null, 0); close(null); }
    if (!strcmp(kind, "vsock")) {
        struct sockaddr_vm host = {.svm_family = AF_VSOCK, .svm_cid = VMADDR_CID_HOST, .svm_port = 1024};
        for (unsigned long round = 1;; round++) {
            int s = socket(AF_VSOCK, SOCK_STREAM, 0);
            if (s < 0) fail("hog vsock");
            // Refused: the host serves nothing a guest can connect to.
            if (!connect(s, (struct sockaddr *)&host, sizeof host)) { errno = EISCONN; fail("hog vsock connect"); }
            close(s);
            if (round == size) printf("SPROUTFS_HOG_PASS kind=%s\n", kind);
        }
    }
    size_t bytes = (size_t)size << 20;
    int file = -1;
    if (strcmp(kind, "ram")) {
        file = open("/hog", O_CREAT | O_RDWR, 0600);
        if (file < 0 || fallocate(file, 0, 0, (off_t)bytes)) fail("hog file");
    }
    if (!strcmp(kind, "sync")) {
        char block[4096];
        for (unsigned long round = 1;; round++) {
            memset(block, (int)round, sizeof block);
            off_t at = (off_t)(round % (bytes / sizeof block)) * (off_t)sizeof block;
            if (pwrite(file, block, sizeof block, at) != (ssize_t)sizeof block || fsync(file)) fail("hog sync");
            if (round == bytes / sizeof block) printf("SPROUTFS_HOG_PASS kind=%s\n", kind);
        }
    }
    volatile unsigned char *region = file < 0
        ? mmap(NULL, bytes, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0)
        : mmap(NULL, bytes, PROT_READ | PROT_WRITE, MAP_SHARED, file, 0);
    if (region == MAP_FAILED) fail("hog mmap");
    for (unsigned long round = 1;; round++) {
        for (size_t at = 0; at < bytes; at += 4096) region[at] = (unsigned char)round;
        if (round == 1) printf("SPROUTFS_HOG_PASS kind=%s\n", kind);
    }
}

int main(void) {
    uint64_t entered = monotonic();
    mkdir("/dev", 0755); mkdir("/proc", 0755); mkdir("/sys", 0755); mkdir("/mnt", 0755);
    mount("devtmpfs", "/dev", "devtmpfs", 0, "");
    int console = open("/dev/console", O_RDWR);
    if (console >= 0) { dup2(console, 0); dup2(console, 1); dup2(console, 2); close(console); }
    setvbuf(stdout, NULL, _IOLBF, 0);
    struct termios term;
    if (!tcgetattr(0, &term)) { term.c_lflag &= ~ECHO; tcsetattr(0, TCSANOW, &term); }
    mount("proc", "/proc", "proc", 0, ""); mount("sysfs", "/sys", "sysfs", 0, "");
    name_root_device();
    // The kernel mounted the root relatime, and an image's files all have an
    // access time no newer than their modification time, so a fork that only
    // reads would write the inode of every file it reads and dirty pages it
    // shares. noatime is a flag of the mount rather than of ext4, which
    // rootflags= cannot carry: ext4 refuses it and the root does not mount.
    if (mount(NULL, "/", NULL, MS_REMOUNT | MS_BIND | MS_NOATIME, NULL)) fail("remount root noatime");
    struct statfs rootfs;
    if (statfs("/", &rootfs)) fail("root filesystem");
    if (!(rootfs.f_flags & ST_NOATIME)) { errno = EINVAL; fail("root keeps access times"); }
    // The plain-Firecracker baseline boots the same image over virtio-block,
    // where there is no PMEM device and therefore no DAX to require.
    int have_pmem = access("/dev/pmem0", F_OK) == 0;
    int pmem_root = have_pmem && rootfs.f_type == EXT4_SUPER_MAGIC;
    if (have_pmem && !pmem_root && mount("/dev/pmem0", "/mnt", "ext4", MS_NOATIME, "dax=always")) fail("mount PMEM/DAX");
    int fd = open(have_pmem && !pmem_root ? "/mnt/value" : "/value", O_CREAT | O_RDWR, 0600);
    if (fd < 0 || ftruncate(fd, 4096)) fail("open value");
    struct statx st;
    int dax = !statx(fd, "", AT_EMPTY_PATH, STATX_ALL, &st) && (st.stx_attributes & STATX_ATTR_DAX) != 0;
    if (have_pmem && !dax) { errno = EOPNOTSUPP; fail("file is not DAX"); }
    volatile uint64_t *disk = mmap(NULL, 4096, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    if (disk == MAP_FAILED) fail("mmap DAX");
    volatile uint64_t *ram = mmap(NULL, 4096, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (ram == MAP_FAILED) fail("mmap RAM");
    *ram = 7;
    // A workload image carries a real userland; the supervisor's `run` command
    // needs the environment a login shell would otherwise have supplied.
    setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", 1);
    setenv("HOME", "/root", 1);
    setenv("TERM", "dumb", 1);
    setenv("SHELL", "/bin/sh", 1);
    start_agent();
    // entry_ns is how long the kernel took to reach this init; ready_ns how
    // long the init itself took, both on the guest's own monotonic clock.
    printf("SPROUTFS_READY ram=%lu disk=%lu dax=%d root=%s entry_ns=%llu ready_ns=%llu\n",
           *ram, *disk, dax, !have_pmem ? "block" : pmem_root ? "pmem" : "initrd",
           (unsigned long long)entered, (unsigned long long)monotonic());
    char line[8192];
    volatile unsigned char *pressure = NULL;
    size_t pressure_bytes = 0;
    for (;;) {
        if (!fgets(line, sizeof(line), stdin)) { clearerr(stdin); continue; }
        unsigned long value;
        if (sscanf(line, "write %lu", &value) == 1) {
            *disk = value;
            flush_value(fd, disk);
        } else if (sscanf(line, "dirty %lu", &value) == 1) {
            *disk = value; printf("SPROUTFS_DIRTY disk=%lu\n", *disk);
        } else if (!strncmp(line, "flush", 5)) {
            flush_value(fd, disk);
        } else if (sscanf(line, "ram %lu", &value) == 1) {
            *ram = value; printf("SPROUTFS_RAM ram=%lu\n", *ram);
        } else if (!strncmp(line, "read", 4)) {
            printf("SPROUTFS_VALUE ram=%lu disk=%lu\n", *ram, *disk);
        } else if (sscanf(line, "pressure %lu", &value) == 1) {
            if (!value || value > 64 || pressure) { errno = EINVAL; fail("pressure size"); }
            pressure_bytes = (size_t)value << 20;
            pressure = mmap(NULL, pressure_bytes, PROT_READ | PROT_WRITE,
                            MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
            if (pressure == MAP_FAILED) fail("pressure mmap");
            // Distinct markers in every guest 4 KiB subpage exercise the
            // complete managed page, including its last byte.
            for (size_t offset = 0; offset < pressure_bytes; offset += 4096) {
                pressure[offset] = (unsigned char)(offset / 4096);
                pressure[offset + 1] = (unsigned char)((offset / 4096) >> 8);
                pressure[offset + 4095] = (unsigned char)(offset / 4096 + 1);
            }
            printf("SPROUTFS_PRESSURE bytes=%zu\n", pressure_bytes);
        } else if (sscanf(line, "touch %lu", &value) == 1) {
            // Store the same markers into the first `value` MiB of the
            // pressure region again. It dirties pages a checkpoint has already
            // published without changing what checkpressure expects, which is
            // what a guest does between one checkpoint and the next.
            size_t touched = (size_t)value << 20;
            if (!pressure || !value || touched > pressure_bytes) { errno = EINVAL; fail("touch size"); }
            for (size_t offset = 0; offset < touched; offset += 4096) {
                pressure[offset] = (unsigned char)(offset / 4096);
                pressure[offset + 1] = (unsigned char)((offset / 4096) >> 8);
                pressure[offset + 4095] = (unsigned char)(offset / 4096 + 1);
            }
            printf("SPROUTFS_TOUCH bytes=%zu\n", touched);
        } else if (!strncmp(line, "checkpressure", 13)) {
            if (!pressure) { errno = EINVAL; fail("pressure missing"); }
            for (size_t offset = 0; offset < pressure_bytes; offset += 4096) {
                if (pressure[offset] != (unsigned char)(offset / 4096) ||
                    pressure[offset + 1] != (unsigned char)((offset / 4096) >> 8) ||
                    pressure[offset + 4095] != (unsigned char)(offset / 4096 + 1)) {
                    errno = EIO; fail("pressure contents");
                }
            }
            printf("SPROUTFS_PRESSURE_OK bytes=%zu\n", pressure_bytes);
        } else if (!strncmp(line, "scan", 4)) {
            // Read every page of the PMEM device, which is the whole of the
            // root volume behind it. A child of a fork reaches every page of
            // that volume this way — through the host's pager, from whichever
            // host still holds it — rather than only the few its filesystem
            // happened to touch.
            static char block[1 << 20];
            int dev = open("/dev/pmem0", O_RDONLY);
            if (dev < 0) fail("open PMEM device");
            unsigned long long scanned = 0, sum = 0;
            for (;;) {
                ssize_t got = read(dev, block, sizeof block);
                if (got < 0) fail("read PMEM device");
                if (got == 0) break;
                for (ssize_t at = 0; at < got; at += 4096) sum += (unsigned char)block[at];
                scanned += (unsigned long long)got;
            }
            close(dev);
            printf("SPROUTFS_SCAN bytes=%llu sum=%llu\n", scanned, sum);
        } else if (!strncmp(line, "run ", 4)) {
            line[strcspn(line, "\n")] = '\0';
            run_command(line + 4);
        } else if (!strncmp(line, "hog ", 4)) {
            char kind[8];
            if (sscanf(line, "hog %7s %lu", kind, &value) != 2 ||
                (strcmp(kind, "ram") && strcmp(kind, "disk") && strcmp(kind, "sync") && strcmp(kind, "vsock"))) {
                errno = EINVAL; fail("hog command");
            }
            hog(kind, value);
        } else if (!strncmp(line, "sync", 4)) {
            sync();
            printf("SPROUTFS_SYNC\n");
        } else if (!strncmp(line, "exit", 4)) {
            printf("SPROUTFS_EXIT\n"); for (;;) pause();
        }
    }
}
