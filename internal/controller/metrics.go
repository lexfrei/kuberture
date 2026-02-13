// Package controller implements the core reconciliation loop for kuberture.
package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

//nolint:gochecknoglobals // prometheus metrics are registered globally by convention.
var (
	reconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kuberture_reconcile_total",
			Help: "Total number of reconciliations by status.",
		},
		[]string{"status"},
	)

	endpointsResolved = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kuberture_endpoints_resolved",
			Help: "Current number of addresses per output.",
		},
		[]string{"output"},
	)

	lastReconcileTimestamp = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "kuberture_last_reconcile_timestamp",
			Help: "Unix timestamp of the last successful reconciliation.",
		},
	)

	reconcileDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "kuberture_reconcile_duration_seconds",
			Help:    "Duration of reconciliation cycles in seconds.",
			Buckets: prometheus.DefBuckets,
		},
	)
)

//nolint:gochecknoinits // metrics registration is the canonical use of init.
func init() {
	ctrlmetrics.Registry.MustRegister(
		reconcileTotal,
		endpointsResolved,
		lastReconcileTimestamp,
		reconcileDuration,
	)
}
