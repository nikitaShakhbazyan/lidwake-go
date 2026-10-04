// C helpers behind package darwin. Each returns plain C values (or malloc'd buffers the Go side
// frees with free()) so no CoreFoundation object ever crosses into Go.
#ifndef LIDWAKE_DARWIN_H
#define LIDWAKE_DARWIN_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

// ---- power.c -----------------------------------------------------------------------------------

// Reads a boolean property of IOPMrootDomain. Returns 1 and sets *value when the property exists,
// 0 when it does not, -1 when IOPMrootDomain cannot be found.
int lw_root_bool_property(const char *key, bool *value);

// The internal battery: 1 when found (percent and onBattery set), 0 otherwise.
int lw_battery(int *percent, bool *onBattery);

// Whether a power source of type InternalBattery exists.
bool lw_has_internal_battery(void);

// kern.boottime: 0 on success.
int lw_boot_time(int64_t *sec, int32_t *usec);

// Whether the main display is asleep.
bool lw_display_asleep(void);

// Online displays: 0 on success, with the total count and how many are not built in.
int lw_display_counts(uint32_t *total, uint32_t *external);

// ---- thermal.m ---------------------------------------------------------------------------------

// NSProcessInfo.processInfo.thermalState.
int lw_thermal_state(void);

// ---- assertions.c ------------------------------------------------------------------------------

// IOPMAssertionCreateWithName at kIOPMAssertionLevelOn. Returns the IOReturn.
int lw_assertion_create(const char *type, const char *name, uint32_t *id);

// IOPMAssertionRelease. Returns the IOReturn.
int lw_assertion_release(uint32_t id);

// IOPMAssertionDeclareUserActivity(kIOPMUserActiveLocal), reusing *id. Returns the IOReturn.
int lw_declare_user_activity(const char *name, uint32_t *id);

// PIDs holding a system-sleep-preventing assertion. Returns the count written to *pids (malloc'd,
// caller frees), or -1 on failure.
int lw_pids_preventing_system_sleep(int32_t **pids);

// ---- smc.c -------------------------------------------------------------------------------------

// Opens AppleSMC. Returns the IOReturn (kIOReturnNotFound when there is no AppleSMC service).
int lw_smc_open(uint32_t *conn);
void lw_smc_close(uint32_t conn);

// kSMCGetKeyInfo. Returns the IOReturn of the call.
int lw_smc_key_info(uint32_t conn, uint32_t key, uint32_t *size, uint32_t *type);

// kSMCReadKey: the first four data bytes. Returns the IOReturn of the call.
int lw_smc_read(uint32_t conn, uint32_t key, uint32_t size, uint8_t *out4);

// kSMCGetKeyFromIndex. Returns the IOReturn of the call.
int lw_smc_key_at(uint32_t conn, uint32_t index, uint32_t *key);

// ---- process.c ---------------------------------------------------------------------------------

typedef struct {
    int32_t ppid;
    int64_t start_sec;
    int32_t start_usec;
    char comm[17];
} lw_kinfo;

// sysctl KERN_PROC_PID. Returns 0 on success, ESRCH when there is no such process, or errno.
int lw_kinfo_of(int32_t pid, lw_kinfo *out);

// One KERN_PROC_ALL snapshot as parallel pid/ppid arrays (malloc'd, caller frees *pids only — both
// arrays share one allocation). Returns the count, or -errno on failure.
int lw_proc_all(int32_t **pids, int32_t **ppids);

// proc_pidpath into buf (cap bytes). Returns the length, or -errno.
int lw_proc_path(int32_t pid, char *buf, int cap);

// kern.argmax. Returns it, or -errno.
int lw_argmax(void);

// sysctl KERN_PROCARGS2 into buf; *size in: capacity, out: length. Returns 0 or errno.
int lw_procargs2(int32_t pid, void *buf, size_t *size);

// proc_pidinfo PROC_PIDTASKINFO: total user+system time in Mach absolute-time ticks. Returns 0 or
// errno (ESRCH when the info could not be read).
int lw_task_ticks(int32_t pid, uint64_t *ticks);

// mach_timebase_info. Returns 0 on success.
int lw_timebase(uint32_t *numer, uint32_t *denom);

// ---- codesign.c --------------------------------------------------------------------------------

typedef struct {
    int32_t pid;
    int32_t uid;
    bool valid;
    bool hardened;
    char *path;       // malloc'd or NULL
    char *identifier; // malloc'd or NULL
    char *team;       // malloc'd or NULL
} lw_peer_code;

// Stages lw_peer_code_of_fd can fail at; the OSStatus or errno is written to *status.
enum {
    LW_PEER_OK = 0,
    LW_PEER_TOKEN = 1,       // getsockopt(LOCAL_PEERTOKEN)
    LW_PEER_GUEST = 2,       // SecCodeCopyGuestWithAttributes
    LW_PEER_STATIC = 3,      // SecCodeCopyStaticCode
    LW_PEER_SIGNING = 4,     // SecCodeCopySigningInformation
    LW_PEER_IDENTIFIER = 5,  // no signing identifier
};

// Resolves the code on the other end of a connected Unix socket from its audit token.
int lw_peer_code_of_fd(int fd, lw_peer_code *out, int32_t *status);

// The signing info of this process; same stages and ownership as lw_peer_code_of_fd.
int lw_self_code(lw_peer_code *out, int32_t *status);

void lw_peer_code_free(lw_peer_code *c);

// ---- screen.c ----------------------------------------------------------------------------------

// Whether SACLockScreenImmediate could be resolved.
bool lw_lock_screen_available(void);

// Calls SACLockScreenImmediate. Returns 0, or -1 when it is unavailable.
int lw_lock_screen(void);

// ---- audio.c -----------------------------------------------------------------------------------

// Whether the default output device is muted; false on any error.
bool lw_output_muted(void);

#endif
