#include "darwin.h"

#include <IOKit/IOKitLib.h>
#include <mach/mach.h>
#include <stddef.h>
#include <string.h>

// The SMC transport struct. The kernel accepts only exactly this layout: 80 bytes with these field
// offsets (key@0, vers@4, pLimitData@12, keyInfo@28, result@40, data32@44, bytes@48). Anything
// else makes every IOConnectCallStructMethod fail and the temperature silently unreadable, which
// would leave the thermal cutout dead — hence the static asserts.
typedef struct {
    uint8_t major;
    uint8_t minor;
    uint8_t build;
    uint8_t reserved;
    uint16_t release;
} lw_smc_vers;

typedef struct {
    uint16_t version;
    uint16_t length;
    uint32_t cpuPLimit;
    uint32_t gpuPLimit;
    uint32_t memPLimit;
} lw_smc_plimit;

typedef struct {
    uint32_t dataSize;
    uint32_t dataType;
    uint8_t dataAttributes;
} lw_smc_keyinfo;

typedef struct {
    uint32_t key;
    lw_smc_vers vers;
    lw_smc_plimit pLimitData;
    lw_smc_keyinfo keyInfo;
    uint8_t result;
    uint8_t status;
    uint8_t data8;
    uint32_t data32;
    uint8_t bytes[32];
} lw_smc_param;

_Static_assert(sizeof(lw_smc_param) == 80, "SMC param struct must be 80 bytes");
_Static_assert(offsetof(lw_smc_param, vers) == 4, "vers offset");
_Static_assert(offsetof(lw_smc_param, pLimitData) == 12, "pLimitData offset");
_Static_assert(offsetof(lw_smc_param, keyInfo) == 28, "keyInfo offset");
_Static_assert(offsetof(lw_smc_param, result) == 40, "result offset");
_Static_assert(offsetof(lw_smc_param, data8) == 42, "data8 offset");
_Static_assert(offsetof(lw_smc_param, data32) == 44, "data32 offset");
_Static_assert(offsetof(lw_smc_param, bytes) == 48, "bytes offset");

enum {
    lw_kSMCHandleYPCEvent = 2,
    lw_kSMCReadKey = 5,
    lw_kSMCGetKeyFromIndex = 8,
    lw_kSMCGetKeyInfo = 9,
};

int lw_smc_open(uint32_t *conn) {
    io_service_t service = IOServiceGetMatchingService(MACH_PORT_NULL, IOServiceMatching("AppleSMC"));
    if (service == 0) {
        return kIOReturnNotFound;
    }
    io_connect_t c = 0;
    kern_return_t kr = IOServiceOpen(service, mach_task_self(), 0, &c);
    IOObjectRelease(service);
    if (kr == KERN_SUCCESS) {
        *conn = c;
    }
    return kr;
}

void lw_smc_close(uint32_t conn) {
    if (conn != 0) {
        IOServiceClose(conn);
    }
}

static kern_return_t lw_smc_call(uint32_t conn, lw_smc_param *in, lw_smc_param *out) {
    size_t outSize = sizeof(lw_smc_param);
    return IOConnectCallStructMethod(conn, lw_kSMCHandleYPCEvent, in, sizeof(lw_smc_param), out, &outSize);
}

int lw_smc_key_info(uint32_t conn, uint32_t key, uint32_t *size, uint32_t *type) {
    lw_smc_param in, out;
    memset(&in, 0, sizeof(in));
    memset(&out, 0, sizeof(out));
    in.key = key;
    in.data8 = lw_kSMCGetKeyInfo;
    kern_return_t kr = lw_smc_call(conn, &in, &out);
    if (kr == KERN_SUCCESS) {
        *size = out.keyInfo.dataSize;
        *type = out.keyInfo.dataType;
    }
    return kr;
}

int lw_smc_read(uint32_t conn, uint32_t key, uint32_t size, uint8_t *out4) {
    lw_smc_param in, out;
    memset(&in, 0, sizeof(in));
    memset(&out, 0, sizeof(out));
    in.key = key;
    in.keyInfo.dataSize = size;
    in.data8 = lw_kSMCReadKey;
    kern_return_t kr = lw_smc_call(conn, &in, &out);
    if (kr == KERN_SUCCESS) {
        memcpy(out4, out.bytes, 4);
    }
    return kr;
}

int lw_smc_key_at(uint32_t conn, uint32_t index, uint32_t *key) {
    lw_smc_param in, out;
    memset(&in, 0, sizeof(in));
    memset(&out, 0, sizeof(out));
    in.data8 = lw_kSMCGetKeyFromIndex;
    in.data32 = index;
    kern_return_t kr = lw_smc_call(conn, &in, &out);
    if (kr == KERN_SUCCESS) {
        *key = out.key;
    }
    return kr;
}
