#include <CoreAudio/CoreAudio.h>

#include "darwin.h"

bool lw_output_muted(void) {
    AudioDeviceID device = 0;
    UInt32 size = sizeof(device);
    AudioObjectPropertyAddress defaultAddr = {
        kAudioHardwarePropertyDefaultOutputDevice,
        kAudioObjectPropertyScopeGlobal,
        0, // kAudioObjectPropertyElementMain
    };
    if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &defaultAddr, 0, NULL, &size, &device) != noErr ||
        device == 0) {
        return false;
    }
    AudioObjectPropertyAddress muteAddr = {
        kAudioDevicePropertyMute,
        kAudioObjectPropertyScopeOutput,
        0,
    };
    if (!AudioObjectHasProperty(device, &muteAddr)) {
        return false;
    }
    UInt32 muted = 0;
    UInt32 mutedSize = sizeof(muted);
    if (AudioObjectGetPropertyData(device, &muteAddr, 0, NULL, &mutedSize, &muted) != noErr) {
        return false;
    }
    return muted != 0;
}
