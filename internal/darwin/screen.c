#include "darwin.h"

#include <dlfcn.h>
#include <pthread.h>
#include <stddef.h>

typedef void (*lw_lock_fn)(void);

static lw_lock_fn lw_lock_function = NULL;
static pthread_once_t lw_lock_once = PTHREAD_ONCE_INIT;

// SACLockScreenImmediate has no header and the on-disk framework is a stub: the symbol lives in
// the dyld shared cache, so it is bound at runtime. Both install paths are tried.
static void lw_lock_resolve(void) {
    static const char *paths[] = {
        "/System/Library/PrivateFrameworks/login.framework/Versions/Current/login",
        "/System/Library/PrivateFrameworks/login.framework/login",
    };
    for (size_t i = 0; i < sizeof(paths) / sizeof(paths[0]); i++) {
        void *handle = dlopen(paths[i], RTLD_NOW);
        if (handle == NULL) {
            continue;
        }
        void *sym = dlsym(handle, "SACLockScreenImmediate");
        if (sym != NULL) {
            lw_lock_function = (lw_lock_fn)sym;
            return;
        }
    }
}

bool lw_lock_screen_available(void) {
    pthread_once(&lw_lock_once, lw_lock_resolve);
    return lw_lock_function != NULL;
}

int lw_lock_screen(void) {
    if (!lw_lock_screen_available()) {
        return -1;
    }
    lw_lock_function();
    return 0;
}
