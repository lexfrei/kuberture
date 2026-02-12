// Package main is the entrypoint for the kuberture controller.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/cockroachdb/errors"
	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/lexfrei/kuberture/internal/config"
	"github.com/lexfrei/kuberture/internal/controller"
	"github.com/lexfrei/kuberture/internal/resolver"
)

const defaultConfigPath = "/etc/kuberture/config.yaml"

// version and revision are set at build time via ldflags.
var (
	version  = "dev"
	revision = "unknown"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	configPath, leaderElect, instanceName := parseFlags()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

	logger.Info("kuberture starting",
		slog.String("version", version),
		slog.String("revision", revision),
	)

	cfg, err := config.Load(configPath)
	if err != nil {
		return errors.Wrap(err, "loading config")
	}

	logger.Info("config loaded", slog.String("path", configPath))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Metrics: metricsserver.Options{
			BindAddress: cfg.MetricsBindAddress,
		},
		HealthProbeBindAddress: cfg.HealthProbeBindAddress,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "kuberture-leader",
	})
	if err != nil {
		return errors.Wrap(err, "creating manager")
	}

	res := resolver.NewResolver(mgr.GetClient(), logger)

	watcher, err := config.NewWatcher(configPath, cfg, logger)
	if err != nil {
		return errors.Wrap(err, "creating config watcher")
	}

	defer func() {
		if closeErr := watcher.Close(); closeErr != nil {
			logger.Error("closing config watcher", slog.String("error", closeErr.Error()))
		}
	}()

	reconciler := controller.NewReconciler(
		mgr.GetClient(),
		res,
		watcher.ConfigPointer(),
		logger,
		instanceName,
	)

	err = reconciler.SetupWithManager(mgr)
	if err != nil {
		return errors.Wrap(err, "setting up controller")
	}

	err = mgr.AddHealthzCheck("healthz", healthz.Ping)
	if err != nil {
		return errors.Wrap(err, "adding healthz check")
	}

	err = mgr.AddReadyzCheck("readyz", reconciler.ReadyzCheck)
	if err != nil {
		return errors.Wrap(err, "adding readyz check")
	}

	signalCtx := ctrl.SetupSignalHandler()
	watcherCtx, watcherCancel := context.WithCancel(signalCtx)

	defer watcherCancel()

	watcherErrCh := make(chan error, 1)

	go func() {
		runErr := watcher.Run(watcherCtx)
		if runErr != nil && !errors.Is(runErr, watcherCtx.Err()) {
			logger.Error("config watcher failed, shutting down", slog.String("error", runErr.Error()))

			watcherErrCh <- runErr

			watcherCancel()
		}
	}()

	logger.Info("starting manager")

	startErr := mgr.Start(watcherCtx)

	// Check if a watcher error triggered the shutdown.
	select {
	case watcherErr := <-watcherErrCh:
		return errors.Wrap(watcherErr, "config watcher failed")
	default:
	}

	if startErr != nil && !errors.Is(startErr, context.Canceled) {
		return errors.Wrap(startErr, "running manager")
	}

	return nil
}

// parseFlags parses CLI flags and environment variables, returning the config
// file path, leader election flag, and instance name.
func parseFlags() (string, bool, string) {
	cfgFlag := flag.String("config", "", "path to configuration file")
	leaderFlag := flag.Bool("leader-elect", true, "enable leader election for high availability")
	instanceFlag := flag.String("instance", "", "unique instance name for multi-instance deployments")

	flag.Parse()

	cfgPath := *cfgFlag
	if cfgPath == "" {
		if envPath := os.Getenv("KUBERTURE_CONFIG"); envPath != "" {
			cfgPath = envPath
		} else {
			cfgPath = defaultConfigPath
		}
	}

	instance := *instanceFlag
	if instance == "" {
		if envInstance := os.Getenv("KUBERTURE_INSTANCE"); envInstance != "" {
			instance = envInstance
		} else {
			instance = "kuberture"
		}
	}

	return cfgPath, *leaderFlag, instance
}
