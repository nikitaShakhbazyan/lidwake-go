package darwin

/*
#include "darwin.h"
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"unsafe"
)

// smcTransport is the three SMC calls the sensor logic needs; a fake stands in for AppleSMC in
// tests.
type smcTransport interface {
	keyInfo(key string) (size uint32, typ string, ok bool)
	readKey(key string, size uint32) (b [4]byte, ok bool)
	keyAt(index uint32) (key string, ok bool)
}

// smcConn is an open AppleSMC user client.
type smcConn uint32

func (c smcConn) keyInfo(key string) (uint32, string, bool) {
	code, ok := fourCharCode(key)
	if !ok {
		return 0, "", false
	}
	var size, typ C.uint32_t
	if C.lw_smc_key_info(C.uint32_t(c), C.uint32_t(code), &size, &typ) != 0 {
		return 0, "", false
	}
	return uint32(size), decodeFourCC(uint32(typ)), true
}

func (c smcConn) readKey(key string, size uint32) ([4]byte, bool) {
	var b [4]byte
	code, ok := fourCharCode(key)
	if !ok {
		return b, false
	}
	if C.lw_smc_read(C.uint32_t(c), C.uint32_t(code), C.uint32_t(size), (*C.uint8_t)(unsafe.Pointer(&b[0]))) != 0 {
		return b, false
	}
	return b, true
}

func (c smcConn) keyAt(index uint32) (string, bool) {
	var key C.uint32_t
	if C.lw_smc_key_at(C.uint32_t(c), C.uint32_t(index), &key) != 0 {
		return "", false
	}
	return decodeFourCC(uint32(key)), true
}

func openSMC() (*SMC, error) {
	var conn C.uint32_t
	if kr := C.lw_smc_open(&conn); kr != 0 {
		return nil, fmt.Errorf("darwin: open AppleSMC: %w", ioReturn(kr))
	}
	return &SMC{conn: uint32(conn)}, nil
}

func (s *SMC) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != 0 {
		C.lw_smc_close(C.uint32_t(s.conn))
		s.conn = 0
	}
}

func (s *SMC) cpuTemperature() (float64, bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == 0 {
		return 0, false
	}
	return s.cpuTemperatureVia(smcConn(s.conn))
}

// cpuTemperatureVia is the read with the sensor cache; the caller holds s.mu.
func (s *SMC) cpuTemperatureVia(t smcTransport) (float64, bool) {
	keys := s.keys
	if keys == nil {
		// Cache only a successful discovery: a transient SMC hiccup yielding zero sensors must
		// retry on the next read — caching the empty list would silently kill the thermal
		// cutout for the daemon's whole lifetime.
		keys = discoverCPUSensors(t)
		if len(keys) > 0 {
			s.keys = keys
		}
	}
	return averageTemperature(t, keys)
}

// plausibleCelsius is the range a real CPU reading falls in; anything else is a dead or
// mislabeled sensor.
func plausibleCelsius(t float64) bool { return t > 5 && t < 120 }

// averageTemperature is the mean of the plausible readings among keys. Averaging the per-core
// sensors gives a smooth, package-like value that doesn't trip the cutout when a single core
// briefly spikes under normal load.
func averageTemperature(t smcTransport, keys []string) (float64, bool) {
	sum, n := 0.0, 0
	for _, k := range keys {
		if c, ok := readTemperature(t, k); ok && plausibleCelsius(c) {
			sum += c
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// readTemperature reads a four-character key as °C (sp78 or flt formats).
func readTemperature(t smcTransport, key string) (float64, bool) {
	if len(key) != 4 {
		return 0, false
	}
	size, typ, ok := t.keyInfo(key)
	if !ok {
		return 0, false
	}
	b, ok := t.readKey(key, size)
	if !ok {
		return 0, false
	}
	return decodeTemperature(b, typ)
}

// intelCPUKeys are the classic CPU proximity/die sensors (sp78), in preference order.
var intelCPUKeys = []string{"TC0P", "TC0D", "TC0E", "TC0F", "TCXC"}

// discoverCPUSensors finds the CPU temperature sensors. Apple Silicon: every Tp… (performance
// core) / Te… (efficiency core) key reporting a plausible `flt ` temperature, found by walking
// the whole key list (~2k keys, so callers cache the result). Intel: the classic proximity/die
// keys that read at all.
func discoverCPUSensors(t smcTransport) []string {
	var keys []string
	if count, ok := smcKeyCount(t); ok {
		for i := uint32(0); i < count; i++ {
			name, ok := t.keyAt(i)
			if !ok || !(strings.HasPrefix(name, "Tp") || strings.HasPrefix(name, "Te")) {
				continue
			}
			size, typ, ok := t.keyInfo(name)
			if !ok || typ != "flt " {
				continue
			}
			b, ok := t.readKey(name, size)
			if !ok {
				continue
			}
			if c, ok := decodeTemperature(b, typ); ok && plausibleCelsius(c) {
				keys = append(keys, name)
			}
		}
	}
	if len(keys) == 0 {
		for _, k := range intelCPUKeys {
			if _, ok := readTemperature(t, k); ok {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// smcKeyCount is the number of SMC keys, from the synthetic `#KEY` key (a big-endian ui32).
func smcKeyCount(t smcTransport) (uint32, bool) {
	size, _, ok := t.keyInfo("#KEY")
	if !ok {
		return 0, false
	}
	b, ok := t.readKey("#KEY", size)
	if !ok {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[:]), true
}

// decodeTemperature decodes the two temperature formats: `sp78` (Intel, signed 8.8 fixed point,
// big-endian) and `flt ` (Apple Silicon, IEEE 754 float, little-endian).
func decodeTemperature(b [4]byte, typ string) (float64, bool) {
	switch typ {
	case "sp78":
		raw := int16(uint16(b[0])<<8 | uint16(b[1]))
		return float64(raw) / 256, true
	case "flt ":
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(b[:]))), true
	default:
		return 0, false
	}
}

// fourCharCode packs a four-character key big-endian, as the SMC expects.
func fourCharCode(s string) (uint32, bool) {
	if len(s) != 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32([]byte(s)), true
}

// decodeFourCC unpacks a key or type code; "" when it isn't ASCII.
func decodeFourCC(v uint32) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	for _, c := range b {
		if c > 0x7f {
			return ""
		}
	}
	return string(b[:])
}
