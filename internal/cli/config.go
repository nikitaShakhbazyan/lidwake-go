package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

// config is `lidwake config [<key> [<value>]]`: read or change a setting. A value is typed after
// the key's current one (true/false, a number, text) and goes through the same decoding and
// clamping as config.json itself, then the daemon reloads the file.
func (a *app) config(args []string) int {
	current := settings.Load(a.configPath)
	values, err := settingsValues(current)
	if err != nil {
		return a.fail("could not read settings: %v", err)
	}

	switch len(args) {
	case 0:
		for _, key := range sortedKeys(values) {
			a.printf("%s = %s\n", key, formatValue(values[key]))
		}
		return 0
	case 1:
		value, ok := values[args[0]]
		if !ok {
			return a.unknownSetting(args[0], values)
		}
		a.printf("%s\n", formatValue(value))
		return 0
	case 2:
	default:
		return a.fail("usage: lidwake config [<key> [<value>]]")
	}

	key, text := args[0], args[1]
	old, ok := values[key]
	if !ok {
		return a.unknownSetting(key, values)
	}
	value, ok := parseValue(text, old)
	if !ok {
		return a.fail("%s expects %s, got '%s'", key, valueKind(old), text)
	}
	values[key] = value
	data, err := json.Marshal(values)
	if err != nil {
		return a.fail("could not save settings: %v", err)
	}
	var updated settings.Settings
	if err := json.Unmarshal(data, &updated); err != nil {
		return a.fail("could not save settings: %v", err)
	}
	if err := updated.Save(a.configPath); err != nil {
		return a.fail("could not save settings: %v", err)
	}
	// Report what was applied after clamping, not what was typed.
	applied := text
	if after, err := settingsValues(updated); err == nil {
		if v, ok := after[key]; ok {
			applied = formatValue(v)
		}
	}
	a.printf("%s = %s\n", key, applied)

	if _, err := a.send(ipc.Request{Op: ipc.OpReloadSettings}); err != nil {
		a.warnf("saved; the daemon is not running, so it applies on next start")
	}
	return 0
}

func (a *app) unknownSetting(key string, values map[string]any) int {
	return a.fail("unknown setting '%s'. Known: %s", key, strings.Join(sortedKeys(values), ", "))
}

// settingsValues is s as its JSON object: bools, json.Numbers and strings by key.
func settingsValues(s settings.Settings) (map[string]any, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encode settings: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var values map[string]any
	if err := dec.Decode(&values); err != nil {
		return nil, fmt.Errorf("decode settings: %w", err)
	}
	return values, nil
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// parseValue reads text as the same JSON type as current. A number without a decimal point
// that is a whole value becomes an integer, so integer settings accept it.
func parseValue(text string, current any) (any, bool) {
	switch current.(type) {
	case bool:
		switch strings.ToLower(text) {
		case "true", "on", "yes", "1":
			return true, true
		case "false", "off", "no", "0":
			return false, true
		}
		return nil, false
	case json.Number:
		f, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, false
		}
		if f == math.Trunc(f) && !strings.Contains(text, ".") && math.Abs(f) < 1<<53 {
			return int64(f), true
		}
		return f, true
	default:
		return text, true
	}
}

func valueKind(v any) string {
	switch v.(type) {
	case bool:
		return "true or false"
	case json.Number:
		return "a number"
	default:
		return "text"
	}
}

func formatValue(v any) string {
	switch v := v.(type) {
	case bool:
		return strconv.FormatBool(v)
	case json.Number:
		return v.String()
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}
