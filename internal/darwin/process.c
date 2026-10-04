#include "darwin.h"

#include <errno.h>
#include <libproc.h>
#include <mach/mach_time.h>
#include <stdlib.h>
#include <string.h>
#include <sys/proc_info.h>
#include <sys/sysctl.h>

int lw_kinfo_of(int32_t pid, lw_kinfo *out) {
    struct kinfo_proc kp;
    memset(&kp, 0, sizeof(kp));
    size_t size = sizeof(kp);
    int mib[4] = {CTL_KERN, KERN_PROC, KERN_PROC_PID, pid};
    if (sysctl(mib, 4, &kp, &size, NULL, 0) != 0) {
        return errno;
    }
    // A missing pid is not an error to sysctl: it just returns nothing.
    if (size == 0 || kp.kp_proc.p_pid != pid) {
        return ESRCH;
    }
    out->ppid = kp.kp_eproc.e_ppid;
    out->start_sec = kp.kp_proc.p_starttime.tv_sec;
    out->start_usec = kp.kp_proc.p_starttime.tv_usec;
    memset(out->comm, 0, sizeof(out->comm));
    strlcpy(out->comm, kp.kp_proc.p_comm, sizeof(out->comm));
    return 0;
}

int lw_proc_all(int32_t **pids, int32_t **ppids) {
    *pids = NULL;
    *ppids = NULL;
    int mib[4] = {CTL_KERN, KERN_PROC, KERN_PROC_ALL, 0};
    // KERN_PROC_ALL is racy: processes can appear between the size probe and the fetch, which
    // fails with ENOMEM. Over-allocate, and retry a few times if that still was not enough.
    for (int attempt = 0; attempt < 4; attempt++) {
        size_t size = 0;
        if (sysctl(mib, 4, NULL, &size, NULL, 0) != 0) {
            return -errno;
        }
        if (size == 0) {
            return -ESRCH;
        }
        size += size / 8 + sizeof(struct kinfo_proc) * 32;
        struct kinfo_proc *procs = malloc(size);
        if (procs == NULL) {
            return -ENOMEM;
        }
        if (sysctl(mib, 4, procs, &size, NULL, 0) != 0) {
            int err = errno;
            free(procs);
            if (err == ENOMEM) {
                continue;
            }
            return -err;
        }
        size_t n = size / sizeof(struct kinfo_proc);
        int32_t *buf = malloc((n > 0 ? n : 1) * 2 * sizeof(int32_t));
        if (buf == NULL) {
            free(procs);
            return -ENOMEM;
        }
        for (size_t i = 0; i < n; i++) {
            buf[i] = procs[i].kp_proc.p_pid;
            buf[n + i] = procs[i].kp_eproc.e_ppid;
        }
        free(procs);
        *pids = buf;
        *ppids = buf + n;
        return (int)n;
    }
    return -ENOMEM;
}

int lw_proc_path(int32_t pid, char *buf, int cap) {
    errno = 0;
    int n = proc_pidpath(pid, buf, (uint32_t)cap);
    if (n <= 0) {
        return errno != 0 ? -errno : -ESRCH;
    }
    return n;
}

int lw_argmax(void) {
    int argmax = 0;
    size_t size = sizeof(argmax);
    int mib[2] = {CTL_KERN, KERN_ARGMAX};
    if (sysctl(mib, 2, &argmax, &size, NULL, 0) != 0) {
        return -errno;
    }
    return argmax;
}

int lw_procargs2(int32_t pid, void *buf, size_t *size) {
    int mib[3] = {CTL_KERN, KERN_PROCARGS2, pid};
    if (sysctl(mib, 3, buf, size, NULL, 0) != 0) {
        return errno;
    }
    return 0;
}

int lw_task_ticks(int32_t pid, uint64_t *ticks) {
    struct proc_taskinfo info;
    memset(&info, 0, sizeof(info));
    errno = 0;
    int n = proc_pidinfo(pid, PROC_PIDTASKINFO, 0, &info, sizeof(info));
    if (n != (int)sizeof(info)) {
        return errno != 0 ? errno : ESRCH;
    }
    *ticks = info.pti_total_user + info.pti_total_system;
    return 0;
}

int lw_timebase(uint32_t *numer, uint32_t *denom) {
    mach_timebase_info_data_t tb = {0, 0};
    if (mach_timebase_info(&tb) != KERN_SUCCESS || tb.numer == 0 || tb.denom == 0) {
        return -1;
    }
    *numer = tb.numer;
    *denom = tb.denom;
    return 0;
}
