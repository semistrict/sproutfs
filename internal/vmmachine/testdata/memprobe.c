// Guest memory microbenchmark. It separates the cost of touching guest memory
// for the first time, which faults all the way to whatever backs guest RAM on
// the host, from the cost of touching memory that is already mapped. A guest
// whose RAM is served by the pager and one whose RAM is plain host memory should
// differ only in the first pass if faults are the whole cost; if the later
// passes differ too, mapped memory itself is slower to reach.
#define _GNU_SOURCE
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <time.h>

static uint64_t now(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (uint64_t)t.tv_sec * 1000000000u + (uint64_t)t.tv_nsec;
}

int main(int argc, char **argv) {
    size_t mib = argc > 1 ? strtoul(argv[1], NULL, 10) : 512;
    size_t bytes = mib << 20, pages = bytes / 4096;
    unsigned char *p = mmap(NULL, bytes, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (p == MAP_FAILED) {
        perror("mmap");
        return 1;
    }
    uint64_t t0 = now();
    memset(p, 1, bytes); // first touch: guest and host page faults
    uint64_t t1 = now();
    memset(p, 2, bytes); // rewrite: every page already mapped everywhere
    uint64_t t2 = now();
    uint64_t sum = 0;
    for (size_t i = 0; i < bytes; i += 64) sum += p[i]; // one load per cache line
    uint64_t t3 = now();
    uint64_t x = 88172645463325252ull;
    size_t ops = pages * 4;
    for (size_t i = 0; i < ops; i++) { // one load per random page: TLB bound
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        sum += p[(x % pages) * 4096 + ((x >> 52) & 4032)];
    }
    uint64_t t4 = now();
    printf("SPROUTFS_MEMPROBE mib=%zu fresh_ns=%llu rewrite_ns=%llu read_ns=%llu random_ns=%llu random_ops=%zu sum=%llu\n",
           mib, (unsigned long long)(t1 - t0), (unsigned long long)(t2 - t1), (unsigned long long)(t3 - t2),
           (unsigned long long)(t4 - t3), ops, (unsigned long long)sum);
    return 0;
}
