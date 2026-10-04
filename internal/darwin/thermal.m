#include "darwin.h"

#import <Foundation/Foundation.h>

int lw_thermal_state(void) {
    @autoreleasepool {
        return (int)[[NSProcessInfo processInfo] thermalState];
    }
}
