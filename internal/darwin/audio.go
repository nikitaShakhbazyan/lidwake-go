package darwin

/*
#cgo LDFLAGS: -framework CoreAudio
#include "darwin.h"
*/
import "C"

// OutputMuted reports whether the default output device is muted (CoreAudio
// kAudioDevicePropertyMute). False on any error or when the device has no mute control.
func OutputMuted() bool { return bool(C.lw_output_muted()) }
