package ipc

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxFrame bounds a frame so a corrupt or hostile length prefix can't make a peer allocate
// gigabytes, while leaving room for a status listing every hold the registry allows.
const MaxFrame = 8 << 20

var ErrFrameTooLarge = errors.New("ipc: frame exceeds 8 MiB")

// WriteFrame encodes v as JSON and writes it with its length prefix in a single write.
func WriteFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(body) > MaxFrame {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[4:], body)
	_, err = w.Write(buf)
	return err
}

// ReadFrame reads one length-prefixed frame and decodes it into v.
func ReadFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return ErrFrameTooLarge
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("ipc: bad frame: %w", err)
	}
	return nil
}
