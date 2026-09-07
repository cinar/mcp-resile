package logging_test

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cinar/mcp-resile/internal/config"
	"github.com/cinar/mcp-resile/internal/logging"
)

// captureStdout redirects os.Stdout for the duration of fn, returning
// everything written to it. logging.New always writes to os.Stdout by
// design (it's the gateway's own output stream), so this is what it takes
// to test its actual behavior rather than reimplementing its level/format
// selection logic in the test and asserting against that instead.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("closing pipe writer: %v", err)
	}

	var sb strings.Builder
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		sb.WriteString(scanner.Text())
		sb.WriteByte('\n')
	}
	return sb.String()
}

// TestNewJSONFormat proves cfg.Format: "json" produces one JSON object per
// log line, with the standard "msg" key and any attached attributes.
func TestNewJSONFormat(t *testing.T) {
	out := captureStdout(t, func() {
		logging.New(config.Logging{Level: "info", Format: "json"}).Info("hello", "tool", "echo")
	})

	var decoded map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &decoded); err != nil {
		t.Fatalf("output %q did not parse as JSON: %v", out, err)
	}
	if decoded["msg"] != "hello" {
		t.Errorf(`decoded["msg"] = %v, want "hello"`, decoded["msg"])
	}
	if decoded["tool"] != "echo" {
		t.Errorf(`decoded["tool"] = %v, want "echo"`, decoded["tool"])
	}
}

// TestNewTextFormat proves cfg.Format: "text" produces slog's key=value
// text form, not JSON.
func TestNewTextFormat(t *testing.T) {
	out := captureStdout(t, func() {
		logging.New(config.Logging{Level: "info", Format: "text"}).Info("hello", "tool", "echo")
	})

	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("text-format output looks like JSON: %q", out)
	}
	if !strings.Contains(out, "msg=hello") || !strings.Contains(out, "tool=echo") {
		t.Errorf("text-format output = %q, want it to contain msg=hello and tool=echo", out)
	}
}

// TestNewLevelFiltering proves cfg.Level gates which records actually reach
// the log, and that an unrecognized level falls back to info per New's doc
// comment (config.Parse rejects anything else at load time, but New itself
// stays defensive).
func TestNewLevelFiltering(t *testing.T) {
	cases := []struct {
		level     string
		wantDebug bool
		wantWarn  bool
	}{
		{"debug", true, true},
		{"info", false, true},
		{"warn", false, true},
		{"error", false, false},
		{"unrecognized", false, true},
	}

	for _, tc := range cases {
		t.Run(tc.level, func(t *testing.T) {
			out := captureStdout(t, func() {
				logger := logging.New(config.Logging{Level: tc.level, Format: "json"})
				logger.Debug("debug line")
				logger.Warn("warn line")
			})

			if strings.Contains(out, "debug line") != tc.wantDebug {
				t.Errorf("debug line present = %v, want %v (output: %q)", strings.Contains(out, "debug line"), tc.wantDebug, out)
			}
			if strings.Contains(out, "warn line") != tc.wantWarn {
				t.Errorf("warn line present = %v, want %v (output: %q)", strings.Contains(out, "warn line"), tc.wantWarn, out)
			}
		})
	}
}
