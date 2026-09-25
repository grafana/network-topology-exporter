package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestRunVersionFlag exercises the --version short-circuit in run().
func TestRunVersionFlag(t *testing.T) {
	code := run(context.Background(), []string{"--version"})
	if code != 0 {
		t.Errorf("--version: exit code = %d, want 0", code)
	}
}

// TestRunUnknownFlag verifies that an unrecognised flag causes run() to
// return 1.
func TestRunUnknownFlag(t *testing.T) {
	code := run(context.Background(), []string{"--no-such-flag"})
	if code != 1 {
		t.Errorf("unknown flag: exit code = %d, want 1", code)
	}
}

// TestRunMissingConfigFile verifies that run() returns 1 when the config
// file does not exist.
func TestRunMissingConfigFile(t *testing.T) {
	code := run(context.Background(), []string{"--config.file=/nonexistent/path.yaml"})
	if code != 1 {
		t.Errorf("missing config: exit code = %d, want 1", code)
	}
}

// TestRunWrongRoleRejected is the regression test for the crash-loop this
// binary hits when handed a config written for any role other than hub
// (e.g. config/example.yaml's federation.role: standalone default, which is
// what Dockerfile.hub used to bake in — see Dockerfile.hub and
// config/hub-example.yaml). run() must fail fast with exit code 1 rather
// than proceeding to build the hub server against a non-hub config.
func TestRunWrongRoleRejected(t *testing.T) {
	for _, role := range []string{"standalone", "uncoordinated", "spoke", ""} {
		t.Run(role, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.yaml")
			body := "discovery:\n  interval: 60s\n"
			if role != "" {
				body += fmt.Sprintf("federation:\n  role: %s\n", role)
			}
			if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			code := run(context.Background(), []string{"--config.file=" + cfgPath})
			if code != 1 {
				t.Errorf("role %q: exit code = %d, want 1 (only federation.role: hub is accepted)", role, code)
			}
		})
	}
}

// TestRunInvalidYAMLConfig verifies that run() returns 1 when the config
// file contains invalid YAML, before any role check runs.
func TestRunInvalidYAMLConfig(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "bad-config-*.yaml")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	_, _ = fmt.Fprint(f, "discovery:\n  interval: [this is not a duration\n")
	_ = f.Close()

	code := run(context.Background(), []string{"--config.file=" + f.Name()})
	if code != 1 {
		t.Errorf("invalid YAML: exit code = %d, want 1", code)
	}
}

// TestRunLogLevelFlag exercises the --log.level flag, covering newLogger's
// switch branches. A nonexistent config makes run() return immediately after
// logging, which is what we care about here.
func TestRunLogLevelFlag(t *testing.T) {
	for _, level := range []string{"debug", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			code := run(context.Background(), []string{
				"--log.level=" + level,
				"--config.file=/nonexistent/path.yaml",
			})
			if code != 1 {
				t.Errorf("--log.level=%s: exit code = %d, want 1 (config not found)", level, code)
			}
		})
	}
}
