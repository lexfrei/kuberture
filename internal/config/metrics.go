package config

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

//nolint:gochecknoglobals // prometheus metrics are registered globally by convention.
var configReloadTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "kuberture_config_reload_total",
		Help: "Total number of config reload attempts by result.",
	},
	[]string{"result"},
)

//nolint:gochecknoinits // metrics registration is the canonical use of init.
func init() {
	ctrlmetrics.Registry.MustRegister(configReloadTotal)
}
