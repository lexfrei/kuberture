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
	path        string
	config      *atomic.Pointer[Config]
	log         *slog.Logger
	fsWatcher   *fsnotify.Watcher
	cancelFunc  context.CancelFunc // called when a restart-requiring change is detected
	reloadCh    chan struct{}      // signals successful config reloads
	logLevelVar *slog.LevelVar     // optional; updated on reload when logLevel changes
}

// NewWatcher creates a config file watcher with the given initial config.
// The cancelFunc is invoked when a config change requires a process restart
// (e.g. source.namespace or bind address changes). In production this should
// be the signal context's cancel so the manager shuts down gracefully.
func NewWatcher(
	path string,
	initial *Config,
	log *slog.Logger,
	cancelFunc context.CancelFunc,
) (*Watcher, error) {
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
		path:       path,
		config:     ptr,
		log:        log,
		fsWatcher:  fsw,
		cancelFunc: cancelFunc,
		reloadCh:   make(chan struct{}, 1),
	}, nil
}

// SetLogLevelVar sets the slog.LevelVar that will be updated when
// the config logLevel changes during a hot-reload.
// Must be called before Run() starts; not safe for concurrent use.
func (w *Watcher) SetLogLevelVar(lv *slog.LevelVar) {
	w.logLevelVar = lv
}

// Config returns the current configuration via atomic load.
func (w *Watcher) Config() *Config {
	return w.config.Load()
}

// ConfigPointer returns the atomic pointer used for live config reloads.
func (w *Watcher) ConfigPointer() *atomic.Pointer[Config] {
	return w.config
}

// ReloadChannel returns a channel that receives a signal after each
// successful config reload. This allows controllers to trigger
// reconciliation immediately when config changes.
func (w *Watcher) ReloadChannel() <-chan struct{} {
	return w.reloadCh
}

// Close stops the file system watcher and releases resources.
// The reload channel is intentionally NOT closed here because Close()
// may race with reload() in the Run goroutine. The channel goroutine
// in SetupWithManager terminates when the process exits.
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
func restartReason(old, updated *Config) string {
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

// logConfigDiff logs the differences between old and new configuration.
func (w *Watcher) logConfigDiff(old, updated *Config) {
	if old.LogLevel != updated.LogLevel {
		w.log.Info("config change: logLevel",
			slog.String("old", old.LogLevel),
			slog.String("new", updated.LogLevel),
		)
	}

	if len(old.Outputs) != len(updated.Outputs) {
		w.log.Info("config change: outputs count",
			slog.Int("old", len(old.Outputs)),
			slog.Int("new", len(updated.Outputs)),
		)
	}

	oldNames := make(map[string]struct{}, len(old.Outputs))
	for idx := range old.Outputs {
		oldNames[old.Outputs[idx].Name] = struct{}{}
	}

	for idx := range updated.Outputs {
		name := updated.Outputs[idx].Name
		if _, exists := oldNames[name]; !exists {
			w.log.Info("config change: output added", slog.String("name", name))
		}
	}

	newNames := make(map[string]struct{}, len(updated.Outputs))
	for idx := range updated.Outputs {
		newNames[updated.Outputs[idx].Name] = struct{}{}
	}

	for idx := range old.Outputs {
		name := old.Outputs[idx].Name
		if _, exists := newNames[name]; !exists {
			w.log.Info("config change: output removed", slog.String("name", name))
		}
	}
}

// ParseSlogLevel maps a config log level string to the corresponding slog.Level.
func ParseSlogLevel(level string) slog.Level {
	switch level {
	case logLevelDebug:
		return slog.LevelDebug
	case logLevelWarn:
		return slog.LevelWarn
	case logLevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func isReloadEvent(evt fsnotify.Event) bool {
	return evt.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove) != 0
}

func (w *Watcher) reload() {
	cfg, err := Load(w.path)
	if err != nil {
		configReloadTotal.WithLabelValues("error").Inc()
		w.log.Error("failed to reload config", slog.String("error", err.Error()))

		return
	}

	old := w.config.Load()

	if reason := restartReason(old, cfg); reason != "" {
		configReloadTotal.WithLabelValues("restart").Inc()
		w.log.Info("config change requires restart, shutting down gracefully",
			slog.String("reason", reason),
			slog.String("path", w.path),
		)

		w.cancelFunc()

		return
	}

	configReloadTotal.WithLabelValues("success").Inc()
	w.logConfigDiff(old, cfg)
	w.config.Store(cfg)

	if w.logLevelVar != nil && old.LogLevel != cfg.LogLevel {
		w.logLevelVar.Set(ParseSlogLevel(cfg.LogLevel))
	}

	w.log.Info("config reloaded successfully", slog.String("path", w.path))

	// Signal controllers to reconcile with the new config.
	select {
	case w.reloadCh <- struct{}{}:
	default:
	}
}
