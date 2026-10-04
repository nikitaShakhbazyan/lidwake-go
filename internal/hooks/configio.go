package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"
)

// Config file I/O shared by the integrations. These files are owned by other programs and hold
// user content, so the helpers are deliberately defensive: a file that exists but can't be parsed
// is an error (writing a fresh object over it would destroy the user's content), a file that
// changed since it was read is not overwritten, writes go through symlinks rather than replacing
// them (dotfile managers like stow/chezmoi symlink these configs), and the original permissions
// survive the atomic replace.
//
// JSON is decoded with json.Number, so numbers the user (or the agent) wrote are written back
// digit for digit.

type readStatus int

const (
	readMissing readStatus = iota
	// readUnparseable: the file exists but couldn't be read or parsed as a JSON object — a
	// comment-bearing (jsonc) file, a syntax error mid-edit, an array root, invalid UTF-8 or a
	// permission error.
	readUnparseable
	readObject
)

// readConfig reads the JSON object at path.
func readConfig(path string) (map[string]any, readStatus) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, readMissing
		}
		return nil, readUnparseable
	}
	obj, ok := decodeObject(data)
	if !ok {
		return nil, readUnparseable
	}
	return obj, readObject
}

// decodeObject parses data as exactly one JSON object.
func decodeObject(data []byte) (map[string]any, bool) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	// The decoder would silently replace invalid UTF-8 with U+FFFD, and writing that back would
	// corrupt the user's bytes; refuse instead.
	if !utf8.Valid(data) {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	obj, ok := v.(map[string]any)
	if !ok || obj == nil {
		return nil, false
	}
	return obj, true
}

// readJSONForUpdate reads a JSON object for a read-modify-write cycle. A missing file is (nil,
// nil): the caller starts fresh. An existing but unparseable file is a SkipConfigUnreadable
// error, because the write that follows would replace the user's content with only ours.
func readJSONForUpdate(path string) (map[string]any, error) {
	obj, status := readConfig(path)
	switch status {
	case readMissing:
		return nil, nil
	case readObject:
		return obj, nil
	default:
		return nil, &SkipError{Reason: SkipConfigUnreadable, Detail: path}
	}
}

// writeJSON writes obj to path atomically, pretty-printed with sorted keys for stable diffs.
//
// before is the object the caller read at the start of its read-modify-write cycle (nil when the
// file didn't exist). The file is re-read just before writing: if another process changed it in
// between — agents rewrite their own configs during sessions — the write is refused instead of
// silently dropping that change.
func writeJSON(obj map[string]any, path string, before map[string]any) error {
	current, status := readConfig(path)
	conflict := &SkipError{Reason: SkipConcurrentModification, Detail: path}
	switch status {
	case readMissing:
		if before != nil {
			return conflict
		}
	case readUnparseable:
		return conflict
	case readObject:
		if before == nil || !reflect.DeepEqual(current, before) {
			return conflict
		}
	}
	data, err := marshalPretty(obj)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return writeThroughSymlinks(data, path)
}

// saveJSON finishes a read-modify-write cycle: nothing is written when the change is a no-op
// (diff is "(unchanged)"), so an untouched file keeps its formatting and its mtime.
func saveJSON(obj map[string]any, path string, before map[string]any, diff string) error {
	if diff == unchangedDiff {
		return nil
	}
	if err := ensureParentDir(path); err != nil {
		return err
	}
	return writeJSON(obj, path, before)
}

// writeString writes a text config (YAML, rc files, plugin sources) through the same safe path.
func writeString(s, path string) error {
	return writeThroughSymlinks([]byte(s), path)
}

// readText reads a text config for a read-modify-write cycle: "" for a missing file, an error for
// one that exists but can't be read. Treating an unreadable file as empty would make the write
// that follows replace the user's whole file with only our lines. The bytes are kept as they are,
// so an rc file with non-UTF-8 comments round-trips untouched.
func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}

// writeThroughSymlinks replaces the file atomically, writing to the symlink's target (so a
// stow/chezmoi-managed config keeps its link and the dotfiles repo keeps tracking reality) and
// keeping the original permissions (an atomic replace would otherwise reset a 0600 settings file
// holding keys to the default mode).
func writeThroughSymlinks(data []byte, path string) error {
	target := resolveSymlinks(path)
	perm := fs.FileMode(0o644)
	if fi, err := os.Stat(target); err == nil {
		perm = fi.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".lidwake-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	ok = true
	return nil
}

// resolveSymlinks returns the file a write to path should replace: path with every symlink
// resolved, or — for a dangling link whose target doesn't exist yet — the end of its link chain,
// so the write creates the target instead of replacing the link.
func resolveSymlinks(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	p := path
	for range 40 {
		target, err := os.Readlink(p)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		p = target
	}
	return p
}

// ensureParentDir creates the parent directory of path (and any intermediates) if needed.
func ensureParentDir(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	return nil
}

// marshalPretty renders v as two-space-indented JSON with sorted object keys, without HTML or
// slash escaping and without a trailing newline.
func marshalPretty(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func serialized(v map[string]any) string {
	data, err := marshalPretty(v)
	if err != nil {
		return ""
	}
	return string(data)
}

// makeDiff renders a BEFORE/AFTER diff of two JSON objects, or "(unchanged)" when they serialize
// equal.
func makeDiff(before, after map[string]any) string {
	b, a := serialized(before), serialized(after)
	if b == a {
		return unchangedDiff
	}
	return "BEFORE:\n" + b + "\n---\nAFTER:\n" + a
}

// commandInvokesLidwakeCLI reports whether a hook command calls the lidwake CLI. It recognizes
// entries written before the _lidwake tag existed, or where a tag isn't allowed (Codex rejects
// unknown keys), even when the embedded CLI path has since drifted. The command must call a
// lidwake binary with one of our verbs and --tool — merely containing the word (a user's own
// lidwake-notify.sh) doesn't make it ours to rewrite or delete.
func commandInvokesLidwakeCLI(command string) bool {
	if !strings.Contains(command, "lidwake") || !strings.Contains(command, "--tool") {
		return false
	}
	for _, verb := range []string{" acquire", " release", " hold"} {
		if strings.Contains(command, verb) {
			return true
		}
	}
	return false
}

// ---- Generic JSON-value helpers ---------------------------------------------------------------
//
// Configs decode to map[string]any / []any. Arrays are walked element by element: an element that
// isn't an object is never ours, so it is kept in place, never dropped.

// lidwakeTag marks the hook entries lidwake owns.
const lidwakeTag = "_lidwake"

// orEmpty returns m, or a new empty object for nil.
func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// objectField returns m[key] when it is an object, else a new empty object.
func objectField(m map[string]any, key string) map[string]any {
	if o, ok := m[key].(map[string]any); ok {
		return o
	}
	return map[string]any{}
}

// stringField returns m[key] when it is a string.
func stringField(m map[string]any, key string) (string, bool) {
	s, ok := m[key].(string)
	return s, ok
}

// isTagged reports whether an entry carries "_lidwake": true.
func isTagged(m map[string]any) bool {
	b, ok := m[lidwakeTag].(bool)
	return ok && b
}

// taggedOrInvokesCLI is the ownership test for entries that may carry the tag: tagged, or a
// command calling the lidwake CLI (an entry written before the tag existed).
func taggedOrInvokesCLI(m map[string]any) bool {
	cmd, _ := stringField(m, "command")
	return isTagged(m) || commandInvokesLidwakeCLI(cmd)
}

// firstObject returns the index of the first object element of arr satisfying pred, or -1.
func firstObject(arr []any, pred func(map[string]any) bool) int {
	for i, v := range arr {
		if o, ok := v.(map[string]any); ok && pred(o) {
			return i
		}
	}
	return -1
}

// anyObject reports whether an object element of arr satisfies pred.
func anyObject(arr []any, pred func(map[string]any) bool) bool {
	return firstObject(arr, pred) >= 0
}

// withoutObjects returns arr minus the object elements satisfying pred (never nil).
func withoutObjects(arr []any, pred func(map[string]any) bool) []any {
	out := make([]any, 0, len(arr))
	for _, v := range arr {
		if o, ok := v.(map[string]any); ok && pred(o) {
			continue
		}
		out = append(out, v)
	}
	return out
}

// deepCopy copies maps and slices so the copy can be mutated without touching the original.
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyObject(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopy(e)
		}
		return out
	default:
		return v
	}
}

func deepCopyObject(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopy(v)
	}
	return out
}
