package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"testing"
)

// Exercise main's actual exit behavior, including its error rendering.
func TestTopLevelHelpExit(t *testing.T) {
	if raw := os.Getenv("FLOPWIRE_TEST_TOP_HELP_ARGS"); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			panic(err)
		}
		os.Args = append([]string{"flopwire"}, args...)
		main()
		os.Exit(0)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"long help", []string{"--help"}, 0},
		{"short help", []string{"-h"}, 0},
		{"missing command", nil, 1},
		{"unknown command", []string{"unknown-command"}, 1},
		{"unknown flag", []string{"--unknown-flag"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestTopLevelHelpExit$")
			cmd.Env = append(os.Environ(), "FLOPWIRE_TEST_TOP_HELP_ARGS="+string(raw))
			var out, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &stderr
			code := 0
			if err := cmd.Run(); err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				code = exit.ExitCode()
			}
			if code != tc.code {
				t.Fatalf("exit %d want %d; stdout %q stderr %q", code, tc.code, out.String(), stderr.String())
			}
			if tc.code == 0 {
				if out.String() != usageText+"\n" || stderr.Len() != 0 {
					t.Fatalf("stdout %q stderr %q", out.String(), stderr.String())
				}
			} else if out.Len() != 0 || !bytes.Contains(stderr.Bytes(), []byte(usageText)) {
				t.Fatalf("stdout %q stderr %q", out.String(), stderr.String())
			}
		})
	}
}
