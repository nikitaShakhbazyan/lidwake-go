#include "darwin.h"

#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/pwr_mgt/IOPMLib.h>
#include <stdlib.h>
#include <string.h>

static CFStringRef lw_cfstr(const char *s) {
    return CFStringCreateWithCString(kCFAllocatorDefault, s, kCFStringEncodingUTF8);
}

int lw_assertion_create(const char *type, const char *name, uint32_t *id) {
    CFStringRef t = lw_cfstr(type);
    CFStringRef n = lw_cfstr(name);
    IOReturn kr = kIOReturnNoMemory;
    if (t != NULL && n != NULL) {
        IOPMAssertionID out = kIOPMNullAssertionID;
        kr = IOPMAssertionCreateWithName(t, kIOPMAssertionLevelOn, n, &out);
        if (kr == kIOReturnSuccess) {
            *id = out;
        }
    }
    if (t) CFRelease(t);
    if (n) CFRelease(n);
    return kr;
}

int lw_assertion_release(uint32_t id) {
    return IOPMAssertionRelease(id);
}

int lw_declare_user_activity(const char *name, uint32_t *id) {
    CFStringRef n = lw_cfstr(name);
    if (n == NULL) {
        return kIOReturnNoMemory;
    }
    IOPMAssertionID aid = *id;
    IOReturn kr = IOPMAssertionDeclareUserActivity(n, kIOPMUserActiveLocal, &aid);
    if (kr == kIOReturnSuccess) {
        *id = aid;
    }
    CFRelease(n);
    return kr;
}

// Assertion types that keep the whole system awake. Display-only types are left out: they do not
// keep a lid-closed, displayless Mac running, so they do not mean "agent working".
static bool lw_prevents_system_sleep(CFTypeRef type) {
    if (type == NULL || CFGetTypeID(type) != CFStringGetTypeID()) {
        return false;
    }
    CFStringRef s = (CFStringRef)type;
    return CFStringCompare(s, CFSTR("PreventUserIdleSystemSleep"), 0) == kCFCompareEqualTo || // caffeinate -i
           CFStringCompare(s, CFSTR("PreventSystemSleep"), 0) == kCFCompareEqualTo ||         // caffeinate -s
           CFStringCompare(s, CFSTR("NoIdleSleepAssertion"), 0) == kCFCompareEqualTo;         // legacy alias
}

int lw_pids_preventing_system_sleep(int32_t **pids) {
    *pids = NULL;
    CFDictionaryRef byPID = NULL;
    if (IOPMCopyAssertionsByProcess(&byPID) != kIOReturnSuccess || byPID == NULL) {
        return -1;
    }
    if (CFGetTypeID(byPID) != CFDictionaryGetTypeID()) {
        CFRelease(byPID);
        return -1;
    }
    CFIndex n = CFDictionaryGetCount(byPID);
    const void **keys = calloc(n > 0 ? (size_t)n : 1, sizeof(void *));
    const void **values = calloc(n > 0 ? (size_t)n : 1, sizeof(void *));
    int32_t *out = calloc(n > 0 ? (size_t)n : 1, sizeof(int32_t));
    if (keys == NULL || values == NULL || out == NULL) {
        free(keys);
        free(values);
        free(out);
        CFRelease(byPID);
        return -1;
    }
    CFDictionaryGetKeysAndValues(byPID, keys, values);
    int count = 0;
    for (CFIndex i = 0; i < n; i++) {
        int64_t pid = 0;
        if (keys[i] == NULL || CFGetTypeID(keys[i]) != CFNumberGetTypeID() ||
            !CFNumberGetValue((CFNumberRef)keys[i], kCFNumberSInt64Type, &pid) || pid <= 0) {
            continue;
        }
        if (values[i] == NULL || CFGetTypeID(values[i]) != CFArrayGetTypeID()) {
            continue;
        }
        CFArrayRef list = (CFArrayRef)values[i];
        CFIndex m = CFArrayGetCount(list);
        for (CFIndex j = 0; j < m; j++) {
            CFTypeRef a = CFArrayGetValueAtIndex(list, j);
            if (a == NULL || CFGetTypeID(a) != CFDictionaryGetTypeID()) {
                continue;
            }
            if (lw_prevents_system_sleep(CFDictionaryGetValue((CFDictionaryRef)a, CFSTR("AssertType")))) {
                out[count++] = (int32_t)pid;
                break;
            }
        }
    }
    free(keys);
    free(values);
    CFRelease(byPID);
    *pids = out;
    return count;
}
