package main

import (
	"strings"
	"testing"
)

// TestRun_MissingConfigFile verifies that run() returns an error when the
// config file specified via KUBERTURE_CONFIG does not exist.
//
// NOTE: run() calls resolveConfigPath() which registers a "config" flag via
// flag.String and then calls flag.Parse. Registering the same flag name twice
// causes a panic, so run() can only be called once per test binary execution.
// That is why this file contains a single test for run(). The config loading
// logic itself is thoroughly covered in internal/config/config_test.go.
func TestRun_MissingConfigFile(t *testing.T) {
	t.Setenv("KUBERTURE_CONFIG", "/tmp/kuberture-nonexistent-test-config-xyz.yaml")

	runErr := run()
	if runErr == nil {
		t.Fatal("expected error for missing config file, got nil")
	}

	if !strings.Contains(runErr.Error(), "loading config") {
		t.Errorf("error = %q, want it to contain %q", runErr.Error(), "loading config")
	}
}
