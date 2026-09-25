// PID 1 for the sproutfs demo guest.
//
// The qualification init, vmmachine/testdata/guest.c, owns the serial console
// for its own line protocol: it turns echo off and consumes every line. The
// demo needs the opposite, a console a person types into, so it gets this init
// instead. It mounts what a shell expects, prints a banner, and keeps an
// interactive /bin/sh on ttyS0 forever, restarting it whenever it exits, so a
// demo console session can never end up attached to a guest with no shell.
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

// The kernel is booted with console=ttyS0, which is the serial device
// Firecracker exposes and the demo's console API reads and writes.
static const char console_device[] = "/dev/ttyS0";
// The agent serves the host's exec and HTTP requests over the VM's vsock. It is
// started before the shell and restarted whenever it exits, so a guest is
// reachable for as long as it is running, whoever is or is not at the console.
static const char agent_path[] = "/usr/local/bin/sproutfs-guest-agent";
// The shell's prompt is the first thing a demo audience reads, so the guest
// names itself rather than showing the kernel's "(none)".
static const char host_name[] = "sproutfs-demo";

static void mount_at(const char *source, const char *target, const char *type) {
    mkdir(target, 0755);
    if (mount(source, target, type, 0, "") && errno != EBUSY) {
        fprintf(stderr, "sproutfs-init: mount %s: %s\n", target, strerror(errno));
    }
}

// open_console returns a descriptor on the serial device, or on /dev/console if
// the serial device is absent, so that a misconfigured boot still says so.
static int open_console(void) {
    int fd = open(console_device, O_RDWR | O_NOCTTY);
    if (fd < 0) fd = open("/dev/console", O_RDWR | O_NOCTTY);
    return fd;
}

// attach_console points this process's own stdio at the console. It is called
// again after every shell exit: the shell owns the console as the leader of its
// own session, and the kernel hangs that terminal up when it dies, which leaves
// init's descriptors writing into an error and its stdout stream stuck in it.
static void attach_console(void) {
    int fd = open_console();
    if (fd < 0) return;
    dup2(fd, 0);
    dup2(fd, 1);
    dup2(fd, 2);
    if (fd > 2) close(fd);
    clearerr(stdout);
    setvbuf(stdout, NULL, _IOLBF, 0);
}

// start_agent runs the guest agent. It keeps the console for its own few log
// lines — a guest that cannot start its agent should say so where a person
// watching the console will see it — and takes no input: the host talks to it
// over the vsock and never over this terminal.
static pid_t start_agent(void) {
    pid_t child = fork();
    if (child != 0) return child;
    setsid();
    int null = open("/dev/null", O_RDONLY);
    if (null >= 0) {
        dup2(null, 0);
        if (null > 2) close(null);
    }
    execl(agent_path, agent_path, (char *)NULL);
    fprintf(stderr, "sproutfs-init: %s: %s\n", agent_path, strerror(errno));
    _exit(127);
}

// start_shell execs an interactive login shell owning the console as its
// controlling terminal, which is what gives the demo's session job control and
// a prompt. It only returns on failure to fork.
static pid_t start_shell(void) {
    pid_t child = fork();
    if (child != 0) return child;
    setsid();
    int fd = open_console();
    if (fd >= 0) {
        ioctl(fd, TIOCSCTTY, 0);
        dup2(fd, 0);
        dup2(fd, 1);
        dup2(fd, 2);
        if (fd > 2) close(fd);
    }
    // A login shell reads /etc/profile, which is where Alpine's prompt lives.
    execl("/bin/sh", "-sh", (char *)NULL);
    perror("sproutfs-init: /bin/sh");
    _exit(127);
}

// name_root_device gives /dev/root a node. The kernel mounted the root
// filesystem itself, from the device root= names, and lists it in /proc/mounts
// as /dev/root — a name devtmpfs never creates. A tool that asks whether a
// device is mounted, resize2fs among them, looks that name up and, finding
// nothing, treats the device as free and opens it exclusively, which the
// mounted filesystem refuses. A symlink to the device root= named is what
// answers the lookup.
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
        fprintf(stderr, "sproutfs-init: /dev/root: %s\n", strerror(errno));
    }
}

// stop_access_times remounts the root noatime. The kernel mounted it relatime,
// and an image's files all have an access time no newer than their modification
// time, so a fork that only reads would write the inode of every file it reads
// and dirty pages it shares with its parent — guest memory for the inode, root
// pages when the journal commits. It is a flag of the mount rather than of
// ext4, which rootflags= cannot carry: ext4 refuses it and the root does not
// mount.
static void stop_access_times(void) {
    if (mount(NULL, "/", NULL, MS_REMOUNT | MS_BIND | MS_NOATIME, NULL)) {
        fprintf(stderr, "sproutfs-init: remount / noatime: %s\n", strerror(errno));
    }
}

int main(void) {
    stop_access_times();
    mount_at("devtmpfs", "/dev", "devtmpfs");
    mount_at("proc", "/proc", "proc");
    mount_at("sysfs", "/sys", "sysfs");
    mount_at("devpts", "/dev/pts", "devpts");
    mount_at("tmpfs", "/run", "tmpfs");
    name_root_device();

    attach_console();
    if (sethostname(host_name, sizeof(host_name) - 1)) {
        fprintf(stderr, "sproutfs-init: sethostname: %s\n", strerror(errno));
    }
    setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", 1);
    setenv("HOME", "/root", 1);
    setenv("TERM", "linux", 1);
    setenv("SHELL", "/bin/sh", 1);
    printf("\n"
           "================================================\n"
           " sproutfs demo guest is up on %s\n"
           " type a command; the shell restarts if it exits\n"
           "================================================\n",
           console_device);

    pid_t agent = start_agent();
    for (;;) {
        pid_t shell = start_shell();
        if (shell < 0) {
            perror("sproutfs-init: fork");
            // Backing off keeps a machine that cannot fork from spinning on it.
            sleep(1);
            continue;
        }
        // PID 1 also reaps whatever the shell orphaned, so the wait continues
        // until the shell itself is the process that was reaped.
        for (;;) {
            int status = 0;
            pid_t done = waitpid(-1, &status, 0);
            if (done == shell) break;
            if (done < 0 && errno != EINTR) break;
            // The agent outlives every shell: a guest that stopped answering
            // the host because its agent crashed would be a VM nothing can
            // reach until someone types at its console. Backing off keeps an
            // agent that cannot start at all from spinning on it.
            if (done == agent) {
                sleep(1);
                agent = start_agent();
            }
        }
        attach_console();
        printf("\n[sproutfs] shell exited; starting a new one\n");
    }
}
