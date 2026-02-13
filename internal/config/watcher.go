package config

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/fsnotify/fsnotify"
)

// Watcher monitors a config file for changes and reloads it atomically.
type Watcher struct {
	path      string
	config    *atomic.Pointer[Config]
	log       *slog.Logger
	fsWatcher *fsnotify.Watcher
	exitFunc  func(int) // overridable for testing; defaults to os.Exit
}

// NewWatcher creates a config file watcher with the given initial config.
func NewWatcher(path string, initial *Config, log *slog.Logger) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, errors.Wrap(err, "creating fsnotify watcher")
	}

	dir := filepath.Dir(path)

	addErr := fsw.Add(dir)
	if addErr != nil {
		fsw.Close()

		return nil, errors.Wrapf(addErr, "watching directory %s", dir)
	}

	ptr := &atomic.Pointer[Config]{}
	ptr.Store(initial)

	return &Watcher{
		path:      path,
		config:    ptr,
		log:       log,
		fsWatcher: fsw,
		exitFunc:  os.Exit,
	}, nil
}

// Config returns the current configuration via atomic load.
func (w *Watcher) Config() *Config {
	return w.config.Load()
}

// ConfigPointer returns the atomic pointer used for live config reloads.
func (w *Watcher) ConfigPointer() *atomic.Pointer[Config] {
	return w.config
}

// Close stops the file system watcher and releases resources.
func (w *Watcher) Close() error {
	err := w.fsWatcher.Close()
	if err != nil {
		return errors.Wrap(err, "closing fsnotify watcher")
	}

	return nil
}

// Run starts the main watch loop, reloading config on file changes.
func (w *Watcher) Run(ctx context.Context) error {
	debounce := debounceDuration
	timer := time.NewTimer(debounce)
	timer.Stop()

	defer timer.Stop()

	filename := filepath.Base(w.path)

	for {
		select {
		case <-ctx.Done():
			return errors.Wrap(ctx.Err(), "watcher context cancelled")
		case evt, open := <-w.fsWatcher.Events:
			if !open {
				return errors.New("fsnotify event channel closed")
			}

			if filepath.Base(evt.Name) == filename && isReloadEvent(evt) {
				timer.Reset(debounce)
			}
		case fsErr, open := <-w.fsWatcher.Errors:
			if !open {
				return errors.New("fsnotify error channel closed")
			}

			w.log.Error("fsnotify error", slog.String("error", fsErr.Error()))
		case <-timer.C:
			w.reload()
		}
	}
}

// restartReason returns a non-empty string describing the first field that
// changed and requires a process restart, or "" if a hot-reload is sufficient.
func (w *Watcher) restartReason(old, updated *Config) string {
	if old.MetricsBindAddress != updated.MetricsBindAddress {
		return "metricsBindAddress changed"
	}

	if old.HealthProbeBindAddress != updated.HealthProbeBindAddress {
		return "healthProbeBindAddress changed"
	}

	if old.Source.Namespace != updated.Source.Namespace {
		return "source.namespace changed"
	}

	if old.Source.ServiceName != updated.Source.ServiceName {
		return "source.serviceName changed"
	}

	return ""
}

func isReloadEvent(evt fsnotify.Event) bool {
	return evt.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove) != 0
}

func (w *Watcher) reload() {
	cfg, err := Load(w.path)
	if err != nil {
		w.log.Error("failed to reload config", slog.String("error", err.Error()))

		return
	}

	old := w.config.Load()

	if reason := w.restartReason(old, cfg); reason != "" {
		w.log.Info("config change requires restart, exiting gracefully",
			slog.String("reason", reason),
			slog.String("path", w.path),
		)

		w.exitFunc(0)

		return
	}

	w.config.Store(cfg)
	w.log.Info("config reloaded successfully", slog.String("path", w.path))
}
