// Package main is the entrypoint for the kuberture controller.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	logLevel := &slog.LevelVar{}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

	logger.Info("kuberture starting",
		slog.String("version", version),
		slog.String("revision", revision),
	)

	cfg, err := config.Load(configPath)
	if err != nil {
		return errors.Wrap(err, "loading config")
	}

	logLevel.Set(config.ParseSlogLevel(cfg.LogLevel))

	logger.Info("config loaded", slog.String("path", configPath))

	podNamespace := os.Getenv("POD_NAMESPACE")
	if podNamespace == "" {
		podNamespace = "default"
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Metrics: metricsserver.Options{
			BindAddress: cfg.MetricsBindAddress,
		},
		HealthProbeBindAddress:  cfg.HealthProbeBindAddress,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "kuberture-leader",
		LeaderElectionNamespace: podNamespace,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Service{}: {
					Namespaces: map[string]cache.Config{
						podNamespace: {},
					},
				},
				&discoveryv1.EndpointSlice{}: {
					Namespaces: map[string]cache.Config{
						cfg.Source.Namespace: {},
					},
				},
			},
		},
	})
	if err != nil {
		return errors.Wrap(err, "creating manager")
	}

	res := resolver.NewResolver(mgr.GetClient(), logger)

	ownerCtx, ownerCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ownerCancel()

	ownerRef, ownerErr := resolveOwnerDeployment(ownerCtx, mgr.GetAPIReader(), podNamespace)
	if ownerErr != nil {
		logger.Warn("could not resolve owner deployment; managed Services will not have ownerReferences",
			slog.String("error", ownerErr.Error()),
		)
	} else {
		logger.Info("resolved owner deployment",
			slog.String("name", ownerRef.Name),
			slog.String("uid", string(ownerRef.UID)),
		)
	}

	signalCtx := ctrl.SetupSignalHandler()
	watcherCtx, watcherCancel := context.WithCancel(signalCtx)

	watcher, err := config.NewWatcher(configPath, cfg, logger, watcherCancel)
	if err != nil {
		watcherCancel()

		return errors.Wrap(err, "creating config watcher")
	}

	watcher.SetLogLevelVar(logLevel)

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
		podNamespace,
		ownerRef,
		watcher.ReloadChannel(),
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

// resolveOwnerDeployment discovers the Deployment that owns the current pod
// by walking the owner chain: Pod → ReplicaSet → Deployment. Returns nil if
// the owner cannot be determined (e.g. running locally, bare pod, StatefulSet).
func resolveOwnerDeployment(
	ctx context.Context,
	reader client.Reader,
	namespace string,
) (*metav1.OwnerReference, error) {
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		return nil, errors.New("POD_NAME environment variable not set")
	}

	var pod corev1.Pod

	err := reader.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &pod)
	if err != nil {
		return nil, errors.Wrap(err, "getting controller pod")
	}

	var rsName string

	for idx := range pod.OwnerReferences {
		if pod.OwnerReferences[idx].Kind == "ReplicaSet" {
			rsName = pod.OwnerReferences[idx].Name

			break
		}
	}

	if rsName == "" {
		return nil, errors.New("pod has no ReplicaSet owner")
	}

	var replicaSet appsv1.ReplicaSet

	err = reader.Get(ctx, types.NamespacedName{Name: rsName, Namespace: namespace}, &replicaSet)
	if err != nil {
		return nil, errors.Wrap(err, "getting owner ReplicaSet")
	}

	for idx := range replicaSet.OwnerReferences {
		ref := &replicaSet.OwnerReferences[idx]
		if ref.Kind == "Deployment" {
			return &metav1.OwnerReference{
				APIVersion:         ref.APIVersion,
				Kind:               ref.Kind,
				Name:               ref.Name,
				UID:                ref.UID,
				BlockOwnerDeletion: new(true),
			}, nil
		}
	}

	return nil, errors.New("ReplicaSet has no Deployment owner")
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
