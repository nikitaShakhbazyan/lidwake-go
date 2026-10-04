package darwin

/*
#include "darwin.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"syscall"
)

// peerStageError names the step that failed to resolve a peer's code.
func peerStageError(stage C.int, status C.int32_t) error {
	switch stage {
	case C.LW_PEER_TOKEN:
		return fmt.Errorf("getsockopt LOCAL_PEERTOKEN: %w", syscall.Errno(status))
	case C.LW_PEER_GUEST:
		return fmt.Errorf("SecCodeCopyGuestWithAttributes: OSStatus %d", int32(status))
	case C.LW_PEER_STATIC:
		return fmt.Errorf("SecCodeCopyStaticCode: OSStatus %d", int32(status))
	case C.LW_PEER_SIGNING:
		return fmt.Errorf("SecCodeCopySigningInformation: OSStatus %d", int32(status))
	case C.LW_PEER_IDENTIFIER:
		return errors.New("code has no signing identifier")
	default:
		return fmt.Errorf("code signing lookup failed at stage %d", int(stage))
	}
}

func peerCodeFromC(c *C.lw_peer_code) PeerCode {
	pc := PeerCode{
		PID:             int(c.pid),
		UID:             int(c.uid),
		HardenedRuntime: bool(c.hardened),
		Valid:           bool(c.valid),
	}
	if c.path != nil {
		pc.Path = C.GoString(c.path)
	}
	if c.identifier != nil {
		pc.Identifier = C.GoString(c.identifier)
	}
	if c.team != nil {
		pc.Team = C.GoString(c.team)
	}
	return pc
}

func peerCodeOf(conn *net.UnixConn) (PeerCode, error) {
	if conn == nil {
		return PeerCode{}, errors.New("darwin: peer code: nil connection")
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return PeerCode{}, fmt.Errorf("darwin: peer code: %w", err)
	}
	var (
		pc     PeerCode
		resErr error
	)
	ctlErr := raw.Control(func(fd uintptr) {
		var c C.lw_peer_code
		var status C.int32_t
		stage := C.lw_peer_code_of_fd(C.int(fd), &c, &status)
		defer C.lw_peer_code_free(&c)
		if stage != C.LW_PEER_OK {
			resErr = peerStageError(stage, status)
			return
		}
		pc = peerCodeFromC(&c)
	})
	if ctlErr != nil {
		return PeerCode{}, fmt.Errorf("darwin: peer code: %w", ctlErr)
	}
	if resErr != nil {
		return PeerCode{}, fmt.Errorf("darwin: peer code: %w", resErr)
	}
	return pc, nil
}

// selfCode is this process's own signing information; it can't change while the process runs.
var selfCode = sync.OnceValues(func() (PeerCode, error) {
	var c C.lw_peer_code
	var status C.int32_t
	stage := C.lw_self_code(&c, &status)
	defer C.lw_peer_code_free(&c)
	if stage != C.LW_PEER_OK {
		return PeerCode{}, fmt.Errorf("darwin: own code: %w", peerStageError(stage, status))
	}
	return peerCodeFromC(&c), nil
})

func selfTeam() string {
	pc, err := selfCode()
	if err != nil {
		return ""
	}
	return pc.Team
}
