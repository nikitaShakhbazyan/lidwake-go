#include "darwin.h"

#include <CoreFoundation/CoreFoundation.h>
#include <CoreGraphics/CoreGraphics.h>
#include <IOKit/IOKitLib.h>
#include <IOKit/ps/IOPSKeys.h>
#include <IOKit/ps/IOPowerSources.h>
#include <errno.h>
#include <math.h>
#include <sys/sysctl.h>
#include <sys/time.h>

// Service matching rather than a registry path: "IOService:/IOResources/IOPMrootDomain" does not
// resolve on every macOS release, the matching lookup does.
static io_service_t lw_root_domain(void) {
    return IOServiceGetMatchingService(MACH_PORT_NULL, IOServiceMatching("IOPMrootDomain"));
}

int lw_root_bool_property(const char *key, bool *value) {
    io_service_t root = lw_root_domain();
    if (root == 0) {
        return -1;
    }
    CFStringRef name = CFStringCreateWithCString(kCFAllocatorDefault, key, kCFStringEncodingUTF8);
    CFTypeRef prop = name ? IORegistryEntryCreateCFProperty(root, name, kCFAllocatorDefault, 0) : NULL;
    if (name) {
        CFRelease(name);
    }
    IOObjectRelease(root);
    if (prop == NULL) {
        return 0;
    }
    *value = false;
    if (CFGetTypeID(prop) == CFBooleanGetTypeID()) {
        *value = CFBooleanGetValue((CFBooleanRef)prop);
    } else if (CFGetTypeID(prop) == CFNumberGetTypeID()) {
        int64_t n = 0;
        if (CFNumberGetValue((CFNumberRef)prop, kCFNumberSInt64Type, &n)) {
            *value = n != 0;
        }
    }
    CFRelease(prop);
    return 1;
}

static bool lw_dict_int(CFDictionaryRef dict, CFStringRef key, int *out) {
    CFTypeRef v = CFDictionaryGetValue(dict, key);
    if (v == NULL || CFGetTypeID(v) != CFNumberGetTypeID()) {
        return false;
    }
    return CFNumberGetValue((CFNumberRef)v, kCFNumberIntType, out);
}

static bool lw_dict_string_equals(CFDictionaryRef dict, CFStringRef key, CFStringRef want) {
    CFTypeRef v = CFDictionaryGetValue(dict, key);
    return v != NULL && CFGetTypeID(v) == CFStringGetTypeID() &&
           CFStringCompare((CFStringRef)v, want, 0) == kCFCompareEqualTo;
}

// Walks the power sources; for each InternalBattery description calls visit, stopping when it
// returns true. Returns whether a visit returned true.
static bool lw_each_internal_battery(bool (*visit)(CFDictionaryRef, void *), void *ctx) {
    CFTypeRef snapshot = IOPSCopyPowerSourcesInfo();
    if (snapshot == NULL) {
        return false;
    }
    bool found = false;
    CFArrayRef sources = IOPSCopyPowerSourcesList(snapshot);
    if (sources != NULL) {
        CFIndex n = CFArrayGetCount(sources);
        for (CFIndex i = 0; i < n && !found; i++) {
            CFDictionaryRef desc = IOPSGetPowerSourceDescription(snapshot, CFArrayGetValueAtIndex(sources, i));
            if (desc == NULL || CFGetTypeID(desc) != CFDictionaryGetTypeID()) {
                continue;
            }
            if (!lw_dict_string_equals(desc, CFSTR(kIOPSTypeKey), CFSTR(kIOPSInternalBatteryType))) {
                continue;
            }
            found = visit(desc, ctx);
        }
        CFRelease(sources);
    }
    CFRelease(snapshot);
    return found;
}

typedef struct {
    int percent;
    bool on_battery;
} lw_battery_reading;

static bool lw_visit_battery(CFDictionaryRef desc, void *ctx) {
    int current = 0, max = 0;
    if (!lw_dict_int(desc, CFSTR(kIOPSCurrentCapacityKey), &current) ||
        !lw_dict_int(desc, CFSTR(kIOPSMaxCapacityKey), &max) || max <= 0) {
        return false;
    }
    lw_battery_reading *r = ctx;
    r->percent = (int)round((double)current / (double)max * 100.0);
    r->on_battery = lw_dict_string_equals(desc, CFSTR(kIOPSPowerSourceStateKey), CFSTR(kIOPSBatteryPowerValue));
    return true;
}

int lw_battery(int *percent, bool *onBattery) {
    lw_battery_reading r = {0};
    if (!lw_each_internal_battery(lw_visit_battery, &r)) {
        return 0;
    }
    *percent = r.percent;
    *onBattery = r.on_battery;
    return 1;
}

static bool lw_visit_any(CFDictionaryRef desc, void *ctx) {
    (void)desc;
    (void)ctx;
    return true;
}

bool lw_has_internal_battery(void) {
    return lw_each_internal_battery(lw_visit_any, NULL);
}

int lw_boot_time(int64_t *sec, int32_t *usec) {
    struct timeval tv = {0};
    size_t size = sizeof(tv);
    int mib[2] = {CTL_KERN, KERN_BOOTTIME};
    if (sysctl(mib, 2, &tv, &size, NULL, 0) != 0) {
        return errno;
    }
    if (tv.tv_sec <= 0) {
        return EINVAL;
    }
    *sec = tv.tv_sec;
    *usec = tv.tv_usec;
    return 0;
}

bool lw_display_asleep(void) {
    return CGDisplayIsAsleep(CGMainDisplayID()) != 0;
}

int lw_display_counts(uint32_t *total, uint32_t *external) {
    CGDirectDisplayID displays[16];
    uint32_t count = 0;
    if (CGGetOnlineDisplayList(16, displays, &count) != kCGErrorSuccess) {
        return -1;
    }
    uint32_t ext = 0;
    for (uint32_t i = 0; i < count && i < 16; i++) {
        if (CGDisplayIsBuiltin(displays[i]) == 0) {
            ext++;
        }
    }
    *total = count;
    *external = ext;
    return 0;
}
