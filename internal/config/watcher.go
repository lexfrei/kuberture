package config

import (
	"context"
	"log/slog"
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
	if old.MetricsBindAddress != cfg.MetricsBindAddress {
		w.log.Warn("metricsBindAddress changed but requires restart to take effect",
			slog.String("old", old.MetricsBindAddress),
			slog.String("new", cfg.MetricsBindAddress),
		)
	}

	if old.HealthProbeBindAddress != cfg.HealthProbeBindAddress {
		w.log.Warn("healthProbeBindAddress changed but requires restart to take effect",
			slog.String("old", old.HealthProbeBindAddress),
			slog.String("new", cfg.HealthProbeBindAddress),
		)
	}

	if old.Source.Namespace != cfg.Source.Namespace {
		w.log.Warn("source.namespace changed but requires restart to take effect",
			slog.String("old", old.Source.Namespace),
			slog.String("new", cfg.Source.Namespace),
		)
	}

	if old.Source.ServiceName != cfg.Source.ServiceName {
		w.log.Warn("source.serviceName changed but requires restart to take effect",
			slog.String("old", old.Source.ServiceName),
			slog.String("new", cfg.Source.ServiceName),
		)
	}

	w.config.Store(cfg)
	w.log.Info("config reloaded successfully", slog.String("path", w.path))
}
