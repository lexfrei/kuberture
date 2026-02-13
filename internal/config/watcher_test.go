package config

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

const (
	validYAML = `
outputs:
  - name: test
    hostname:
      - test.example.com
    serviceName: test-svc
`

	updatedYAML = `
outputs:
  - name: updated
    hostname:
      - updated.example.com
    serviceName: updated-svc
`

	invalidYAML = `{{{not yaml at all`

	outputNameTest    = "test"
	outputNameUpdated = "updated"
)

func loadInitialConfig(t *testing.T, path string) *Config {
	t.Helper()

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("loading initial config: %v", err)
	}

	return cfg
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func noopCancel() {}

func TestWatcher_NewWatcher_ValidPath(t *testing.T) {
	path := writeTestConfig(t, validYAML)
	initial := loadInitialConfig(t, path)
	log := newTestLogger()

	watcher, err := NewWatcher(path, initial, log, noopCancel)
	if err != nil {
		t.Fatalf("unexpected error creating watcher: %v", err)
	}

	defer watcher.Close()

	got := watcher.Config()
	if got == nil {
		t.Fatal("Config() returned nil, want non-nil")
	}

	if got.Outputs[0].Name != outputNameTest {
		t.Errorf("output name = %q, want %q", got.Outputs[0].Name, outputNameTest)
	}
}

func TestWatcher_NewWatcher_InvalidPath(t *testing.T) {
	initial := &Config{}
	log := newTestLogger()

	_, err := NewWatcher("/nonexistent/dir/config.yaml", initial, log, noopCancel)
	if err == nil {
		t.Fatal("expected error for invalid path, got nil")
	}
}

func TestWatcher_Config_ReturnsInitialConfig(t *testing.T) {
	path := writeTestConfig(t, validYAML)
	initial := loadInitialConfig(t, path)
	log := newTestLogger()

	watcher, err := NewWatcher(path, initial, log, noopCancel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	defer watcher.Close()

	got := watcher.Config()
	if got != initial {
		t.Error("Config() did not return the same pointer as the initial config")
	}
}

func TestWatcher_ConfigPointer_NonNil(t *testing.T) {
	path := writeTestConfig(t, validYAML)
	initial := loadInitialConfig(t, path)
	log := newTestLogger()

	watcher, err := NewWatcher(path, initial, log, noopCancel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	defer watcher.Close()

	ptr := watcher.ConfigPointer()
	if ptr == nil {
		t.Fatal("ConfigPointer() returned nil")
	}

	if ptr.Load() != initial {
		t.Error("ConfigPointer().Load() did not return the initial config")
	}
}

func TestWatcher_ReloadOnFileWrite(t *testing.T) {
	path := writeTestConfig(t, validYAML)
	initial := loadInitialConfig(t, path)
	log := newTestLogger()

	watcher, err := NewWatcher(path, initial, log, noopCancel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	defer watcher.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		runDone <- watcher.Run(ctx)
	}()

	// Allow the watcher goroutine to start.
	time.Sleep(50 * time.Millisecond)

	writeErr := os.WriteFile(path, []byte(updatedYAML), 0o600)
	if writeErr != nil {
		t.Fatalf("writing updated config: %v", writeErr)
	}

	// Wait for debounce (100ms) plus processing time.
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)

	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for config reload")
		case <-ticker.C:
			cfg := watcher.Config()
			if len(cfg.Outputs) > 0 && cfg.Outputs[0].Name == outputNameUpdated {
				// Config reloaded successfully.
				cancel()

				return
			}
		}
	}
}

func TestWatcher_InvalidConfigPreservesOld(t *testing.T) {
	path := writeTestConfig(t, validYAML)
	initial := loadInitialConfig(t, path)
	log := newTestLogger()

	watcher, err := NewWatcher(path, initial, log, noopCancel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	defer watcher.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		runDone <- watcher.Run(ctx)
	}()

	// Allow the watcher goroutine to start.
	time.Sleep(50 * time.Millisecond)

	writeErr := os.WriteFile(path, []byte(invalidYAML), 0o600)
	if writeErr != nil {
		t.Fatalf("writing invalid config: %v", writeErr)
	}

	// Wait for debounce plus processing time.
	time.Sleep(300 * time.Millisecond)

	got := watcher.Config()
	if got.Outputs[0].Name != outputNameTest {
		t.Errorf("config changed after invalid write: output name = %q, want %q", got.Outputs[0].Name, outputNameTest)
	}

	cancel()
}

func TestWatcher_ContextCancellationStopsRun(t *testing.T) {
	path := writeTestConfig(t, validYAML)
	initial := loadInitialConfig(t, path)
	log := newTestLogger()

	watcher, err := NewWatcher(path, initial, log, noopCancel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	defer watcher.Close()

	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan error, 1)
	go func() {
		runDone <- watcher.Run(ctx)
	}()

	// Allow the watcher goroutine to start.
	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case runErr := <-runDone:
		if runErr == nil {
			t.Fatal("expected error from cancelled context, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Run() to return after context cancellation")
	}
}

func TestWatcher_CloseStopsFSWatcher(t *testing.T) {
	path := writeTestConfig(t, validYAML)
	initial := loadInitialConfig(t, path)
	log := newTestLogger()

	watcher, err := NewWatcher(path, initial, log, noopCancel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	closeErr := watcher.Close()
	if closeErr != nil {
		t.Fatalf("unexpected error closing watcher: %v", closeErr)
	}

	// After Close(), Run() should exit because channels are closed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		runDone <- watcher.Run(ctx)
	}()

	select {
	case runErr := <-runDone:
		if runErr == nil {
			t.Fatal("expected error from Run() after Close(), got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Run() to return after Close()")
	}
}

func TestWatcher_WatchesParentDirectory(t *testing.T) {
	// Verify the watcher works with ConfigMap-style symlink updates:
	// writing a new file in the parent directory triggers a reload.
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	writeErr := os.WriteFile(configPath, []byte(validYAML), 0o600)
	if writeErr != nil {
		t.Fatalf("writing initial config: %v", writeErr)
	}

	initial := loadInitialConfig(t, configPath)
	log := newTestLogger()

	watcher, err := NewWatcher(configPath, initial, log, noopCancel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	defer watcher.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		runDone <- watcher.Run(ctx)
	}()

	// Allow the watcher goroutine to start.
	time.Sleep(50 * time.Millisecond)

	// Simulate a ConfigMap update: remove old file, write new one.
	removeErr := os.Remove(configPath)
	if removeErr != nil {
		t.Fatalf("removing config file: %v", removeErr)
	}

	writeErr = os.WriteFile(configPath, []byte(updatedYAML), 0o600)
	if writeErr != nil {
		t.Fatalf("writing updated config: %v", writeErr)
	}

	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)

	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for config reload via parent directory watch")
		case <-ticker.C:
			cfg := watcher.Config()
			if len(cfg.Outputs) > 0 && cfg.Outputs[0].Name == outputNameUpdated {
				cancel()

				return
			}
		}
	}
}

func newCapturingLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestReload_CancelsOnSourceNamespaceChange(t *testing.T) {
	initialYAML := `
source:
  namespace: kube-system
  serviceName: kubernetes
outputs:
  - name: test
    hostname:
      - test.example.com
    serviceName: test-svc
`
	updatedSourceYAML := `
source:
  namespace: monitoring
  serviceName: kubernetes
outputs:
  - name: test
    hostname:
      - test.example.com
    serviceName: test-svc
`

	path := writeTestConfig(t, initialYAML)
	initial := loadInitialConfig(t, path)

	var logBuf bytes.Buffer
	log := newCapturingLogger(&logBuf)

	cancelCalled := make(chan struct{}, 1)
	testCancel := func() { cancelCalled <- struct{}{} }

	watcher, err := NewWatcher(path, initial, log, testCancel)
	if err != nil {
		t.Fatalf("unexpected error creating watcher: %v", err)
	}

	defer watcher.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = watcher.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	writeErr := os.WriteFile(path, []byte(updatedSourceYAML), 0o600)
	if writeErr != nil {
		t.Fatalf("writing updated config: %v", writeErr)
	}

	select {
	case <-cancelCalled:
		// cancelFunc was invoked — graceful shutdown triggered.
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for cancel call; log output:\n%s", logBuf.String())
	}

	if !strings.Contains(logBuf.String(), "source.namespace changed") {
		t.Errorf("log output missing reason; got:\n%s", logBuf.String())
	}

	cancel()
}

func TestReload_CancelsOnSourceServiceNameChange(t *testing.T) {
	initialYAML := `
source:
  namespace: default
  serviceName: kubernetes
outputs:
  - name: test
    hostname:
      - test.example.com
    serviceName: test-svc
`
	updatedSourceYAML := `
source:
  namespace: default
  serviceName: kube-apiserver
outputs:
  - name: test
    hostname:
      - test.example.com
    serviceName: test-svc
`

	path := writeTestConfig(t, initialYAML)
	initial := loadInitialConfig(t, path)

	var logBuf bytes.Buffer
	log := newCapturingLogger(&logBuf)

	cancelCalled := make(chan struct{}, 1)
	testCancel := func() { cancelCalled <- struct{}{} }

	watcher, err := NewWatcher(path, initial, log, testCancel)
	if err != nil {
		t.Fatalf("unexpected error creating watcher: %v", err)
	}

	defer watcher.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = watcher.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	writeErr := os.WriteFile(path, []byte(updatedSourceYAML), 0o600)
	if writeErr != nil {
		t.Fatalf("writing updated config: %v", writeErr)
	}

	select {
	case <-cancelCalled:
		// cancelFunc was invoked — graceful shutdown triggered.
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for cancel call; log output:\n%s", logBuf.String())
	}

	if !strings.Contains(logBuf.String(), "source.serviceName changed") {
		t.Errorf("log output missing reason; got:\n%s", logBuf.String())
	}

	cancel()
}

func TestWatcher_ReloadChannel_SendsOnSuccess(t *testing.T) {
	path := writeTestConfig(t, validYAML)
	initial := loadInitialConfig(t, path)
	log := newTestLogger()

	watcher, err := NewWatcher(path, initial, log, noopCancel)
	if err != nil {
		t.Fatalf("unexpected error creating watcher: %v", err)
	}

	defer watcher.Close()

	reloadCh := watcher.ReloadChannel()
	if reloadCh == nil {
		t.Fatal("ReloadChannel() returned nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = watcher.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	writeErr := os.WriteFile(path, []byte(updatedYAML), 0o600)
	if writeErr != nil {
		t.Fatalf("writing updated config: %v", writeErr)
	}

	select {
	case <-reloadCh:
		// Reload signal received.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reload signal on channel")
	}

	cancel()
}

func TestWatcher_ReloadChannel_NoSendOnError(t *testing.T) {
	path := writeTestConfig(t, validYAML)
	initial := loadInitialConfig(t, path)
	log := newTestLogger()

	watcher, err := NewWatcher(path, initial, log, noopCancel)
	if err != nil {
		t.Fatalf("unexpected error creating watcher: %v", err)
	}

	defer watcher.Close()

	reloadCh := watcher.ReloadChannel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = watcher.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// Write invalid YAML — reload should fail, no signal on channel.
	writeErr := os.WriteFile(path, []byte(invalidYAML), 0o600)
	if writeErr != nil {
		t.Fatalf("writing invalid config: %v", writeErr)
	}

	// Wait for debounce + processing, then verify no signal was sent.
	time.Sleep(300 * time.Millisecond)

	select {
	case <-reloadCh:
		t.Fatal("received reload signal after invalid config write, expected none")
	default:
		// No signal — correct behavior.
	}

	cancel()
}

func TestReload_IncrementsSuccessMetric(t *testing.T) {
	path := writeTestConfig(t, validYAML)
	initial := loadInitialConfig(t, path)
	log := newTestLogger()

	before := testutil.ToFloat64(configReloadTotal.WithLabelValues("success"))

	watcher, err := NewWatcher(path, initial, log, noopCancel)
	if err != nil {
		t.Fatalf("unexpected error creating watcher: %v", err)
	}

	defer watcher.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = watcher.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	writeErr := os.WriteFile(path, []byte(updatedYAML), 0o600)
	if writeErr != nil {
		t.Fatalf("writing updated config: %v", writeErr)
	}

	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)

	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for config reload success metric increment")
		case <-ticker.C:
			after := testutil.ToFloat64(configReloadTotal.WithLabelValues("success"))
			if after > before {
				cancel()

				return
			}
		}
	}
}

func TestReload_IncrementsErrorMetric(t *testing.T) {
	path := writeTestConfig(t, validYAML)
	initial := loadInitialConfig(t, path)
	log := newTestLogger()

	before := testutil.ToFloat64(configReloadTotal.WithLabelValues("error"))

	watcher, err := NewWatcher(path, initial, log, noopCancel)
	if err != nil {
		t.Fatalf("unexpected error creating watcher: %v", err)
	}

	defer watcher.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = watcher.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	writeErr := os.WriteFile(path, []byte(invalidYAML), 0o600)
	if writeErr != nil {
		t.Fatalf("writing invalid config: %v", writeErr)
	}

	// Wait for debounce + processing.
	time.Sleep(300 * time.Millisecond)

	after := testutil.ToFloat64(configReloadTotal.WithLabelValues("error"))
	if after <= before {
		t.Errorf("error counter did not increment: before=%f, after=%f", before, after)
	}

	cancel()
}
