package darwin

/*
#include <stdlib.h>
#include "darwin.h"
*/
import "C"

import (
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"
)

// ioReturn is an IOKit status code (IOReturn / kern_return_t).
type ioReturn int32

func (r ioReturn) Error() string { return fmt.Sprintf("IOReturn 0x%08x", uint32(r)) }

func createAssertion(t AssertionType, name string) (*PowerAssertion, error) {
	ct := C.CString(string(t))
	defer C.free(unsafe.Pointer(ct))
	cn := C.CString(name)
	defer C.free(unsafe.Pointer(cn))
	var id C.uint32_t
	if kr := C.lw_assertion_create(ct, cn, &id); kr != 0 {
		return nil, fmt.Errorf("darwin: IOPMAssertionCreateWithName(%s): %w", t, ioReturn(kr))
	}
	return &PowerAssertion{id: uint32(id)}, nil
}

func (a *PowerAssertion) release() {
	if a == nil {
		return
	}
	if id := atomic.SwapUint32(&a.id, 0); id != 0 {
		C.lw_assertion_release(C.uint32_t(id))
	}
}

// userActivity is the id IOPMAssertionDeclareUserActivity hands back; passing it in again makes
// the system refresh that assertion instead of minting a new one.
var userActivity struct {
	sync.Mutex
	id C.uint32_t
}

func declareUserActivity(name string) error {
	if !displayAsleep() {
		return nil
	}
	cn := C.CString(name)
	defer C.free(unsafe.Pointer(cn))
	userActivity.Lock()
	defer userActivity.Unlock()
	if kr := C.lw_declare_user_activity(cn, &userActivity.id); kr != 0 {
		return fmt.Errorf("darwin: IOPMAssertionDeclareUserActivity: %w", ioReturn(kr))
	}
	return nil
}

func pidsPreventingSystemSleep() map[int]bool {
	out := map[int]bool{}
	var pids *C.int32_t
	n := C.lw_pids_preventing_system_sleep(&pids)
	if n < 0 {
		return out
	}
	defer C.free(unsafe.Pointer(pids))
	for _, pid := range unsafe.Slice(pids, int(n)) {
		out[int(pid)] = true
	}
	return out
}
