#include "darwin.h"

#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>
#include <bsm/libbsm.h>
#include <errno.h>
#include <limits.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

// kSecCodeSignatureRuntime: the code was signed with the hardened runtime.
#define LW_HARDENED_RUNTIME_FLAG 0x10000u

static char *lw_cfstring_dup(CFTypeRef value) {
    if (value == NULL || CFGetTypeID(value) != CFStringGetTypeID()) {
        return NULL;
    }
    CFStringRef s = (CFStringRef)value;
    CFIndex max = CFStringGetMaximumSizeForEncoding(CFStringGetLength(s), kCFStringEncodingUTF8) + 1;
    char *buf = malloc((size_t)max);
    if (buf == NULL) {
        return NULL;
    }
    if (!CFStringGetCString(s, buf, max, kCFStringEncodingUTF8)) {
        free(buf);
        return NULL;
    }
    return buf;
}

// Fills identifier, team, flags and path from the static code.
static int lw_fill_from_static(SecStaticCodeRef sc, lw_peer_code *out, int32_t *status) {
    CFDictionaryRef info = NULL;
    OSStatus st = SecCodeCopySigningInformation(sc, (SecCSFlags)kSecCSSigningInformation, &info);
    if (st != errSecSuccess || info == NULL) {
        *status = st;
        return LW_PEER_SIGNING;
    }
    out->identifier = lw_cfstring_dup(CFDictionaryGetValue(info, kSecCodeInfoIdentifier));
    if (out->identifier == NULL) {
        // Unsigned code has no identifier; there is nothing to anchor trust in.
        CFRelease(info);
        *status = 0;
        return LW_PEER_IDENTIFIER;
    }
    out->team = lw_cfstring_dup(CFDictionaryGetValue(info, kSecCodeInfoTeamIdentifier));
    CFTypeRef flags = CFDictionaryGetValue(info, kSecCodeInfoFlags);
    if (flags != NULL && CFGetTypeID(flags) == CFNumberGetTypeID()) {
        int64_t f = 0;
        if (CFNumberGetValue((CFNumberRef)flags, kCFNumberSInt64Type, &f)) {
            out->hardened = ((uint64_t)f & LW_HARDENED_RUNTIME_FLAG) != 0;
        }
    }
    CFRelease(info);

    CFURLRef url = NULL;
    if (SecCodeCopyPath(sc, kSecCSDefaultFlags, &url) == errSecSuccess && url != NULL) {
        char path[PATH_MAX];
        if (CFURLGetFileSystemRepresentation(url, true, (UInt8 *)path, sizeof(path))) {
            out->path = strdup(path);
        }
        CFRelease(url);
    }
    *status = 0;
    return LW_PEER_OK;
}

// Resolves a dynamic code object: validity, then everything the static code says.
static int lw_fill_from_code(SecCodeRef code, lw_peer_code *out, int32_t *status) {
    out->valid = SecCodeCheckValidity(code, kSecCSDefaultFlags, NULL) == errSecSuccess;
    SecStaticCodeRef sc = NULL;
    OSStatus st = SecCodeCopyStaticCode(code, kSecCSDefaultFlags, &sc);
    if (st != errSecSuccess || sc == NULL) {
        *status = st;
        return LW_PEER_STATIC;
    }
    int rc = lw_fill_from_static(sc, out, status);
    CFRelease(sc);
    return rc;
}

int lw_peer_code_of_fd(int fd, lw_peer_code *out, int32_t *status) {
    memset(out, 0, sizeof(*out));
    // The audit token, not the PID: a PID can be recycled between the connect and this check, the
    // token names exactly one process instance.
    audit_token_t token;
    memset(&token, 0, sizeof(token));
    socklen_t len = sizeof(token);
    if (getsockopt(fd, SOL_LOCAL, LOCAL_PEERTOKEN, &token, &len) != 0) {
        *status = errno;
        return LW_PEER_TOKEN;
    }
    if (len != sizeof(token)) {
        *status = EINVAL;
        return LW_PEER_TOKEN;
    }
    out->pid = audit_token_to_pid(token);
    out->uid = (int32_t)audit_token_to_euid(token);

    CFDataRef data = CFDataCreate(kCFAllocatorDefault, (const UInt8 *)&token, sizeof(token));
    if (data == NULL) {
        *status = ENOMEM;
        return LW_PEER_GUEST;
    }
    const void *keys[] = {kSecGuestAttributeAudit};
    const void *values[] = {data};
    CFDictionaryRef attrs = CFDictionaryCreate(kCFAllocatorDefault, keys, values, 1,
                                               &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    CFRelease(data);
    if (attrs == NULL) {
        *status = ENOMEM;
        return LW_PEER_GUEST;
    }
    SecCodeRef code = NULL;
    OSStatus st = SecCodeCopyGuestWithAttributes(NULL, attrs, kSecCSDefaultFlags, &code);
    CFRelease(attrs);
    if (st != errSecSuccess || code == NULL) {
        *status = st;
        return LW_PEER_GUEST;
    }
    int rc = lw_fill_from_code(code, out, status);
    CFRelease(code);
    return rc;
}

int lw_self_code(lw_peer_code *out, int32_t *status) {
    memset(out, 0, sizeof(*out));
    out->pid = getpid();
    out->uid = (int32_t)geteuid();
    SecCodeRef code = NULL;
    OSStatus st = SecCodeCopySelf(kSecCSDefaultFlags, &code);
    if (st != errSecSuccess || code == NULL) {
        *status = st;
        return LW_PEER_GUEST;
    }
    int rc = lw_fill_from_code(code, out, status);
    CFRelease(code);
    return rc;
}

void lw_peer_code_free(lw_peer_code *c) {
    free(c->path);
    free(c->identifier);
    free(c->team);
    c->path = NULL;
    c->identifier = NULL;
    c->team = NULL;
}
