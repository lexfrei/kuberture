package controller

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	applymetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/lexfrei/kuberture/internal/config"
	"github.com/lexfrei/kuberture/internal/resolver"
)

// errNotReconciled is returned by the readiness check before the first
// successful reconciliation has completed.
var errNotReconciled = errors.New("controller has not completed initial reconciliation")

const (
	fieldOwner     = "kuberture"
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "kuberture"
	instanceLabel  = "kuberture/instance"

	// stalenessThreshold is how long since the last successful reconciliation
	// before the readiness check reports unhealthy.
	stalenessThreshold = 5 * time.Minute

	// requeueInterval is how often the controller re-reconciles to prevent
	// the readiness probe from going stale on quiet clusters.
	requeueInterval = 2 * time.Minute
)

// Reconciler watches EndpointSlice and Node resources and maintains headless
// Service objects annotated for external-dns consumption.
type Reconciler struct {
	client       client.Client
	resolver     *resolver.Resolver
	config       *atomic.Pointer[config.Config]
	log          *slog.Logger
	instanceName string
	namespace    string
	ownerRef     *metav1.OwnerReference
	reloadCh     <-chan struct{}

	mu          sync.Mutex
	lastSuccess time.Time
}

// NewReconciler creates a Reconciler with the given dependencies.
// ownerRef may be nil when the owner Deployment cannot be resolved (e.g. dev mode).
// reloadCh may be nil if config hot-reload triggering is not needed.
func NewReconciler(
	cli client.Client,
	res *resolver.Resolver,
	cfg *atomic.Pointer[config.Config],
	log *slog.Logger,
	instanceName string,
	namespace string,
	ownerRef *metav1.OwnerReference,
	reloadCh <-chan struct{},
) *Reconciler {
	return &Reconciler{
		client:       cli,
		resolver:     res,
		config:       cfg,
		log:          log,
		instanceName: instanceName,
		namespace:    namespace,
		ownerRef:     ownerRef,
		reloadCh:     reloadCh,
	}
}

// newTriggerRequest returns a fixed reconcile key used when Node events fire.
func newTriggerRequest() reconcile.Request {
	return reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "trigger",
			Namespace: "default",
		},
	}
}

// SetupWithManager registers the controller with the manager, watching
// EndpointSlice and Node resources. Filtering by service name is done
// dynamically in Reconcile via the List call, so config hot-reload of
// source.serviceName takes effect without restart.
// If a reload channel was provided via NewReconciler, the controller also
// triggers reconciliation when the config watcher signals a successful reload.
func (rec *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	nodeHandler := handler.EnqueueRequestsFromMapFunc(
		func(_ context.Context, _ client.Object) []reconcile.Request {
			return []reconcile.Request{newTriggerRequest()}
		},
	)

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&discoveryv1.EndpointSlice{}).
		Watches(&corev1.Node{}, nodeHandler)

	if rec.reloadCh != nil {
		eventCh := make(chan event.GenericEvent, 1)

		go func() {
			for range rec.reloadCh {
				select {
				case eventCh <- event.GenericEvent{Object: &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "config-reload"},
				}}:
				default:
				}
			}
		}()

		reloadHandler := handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{newTriggerRequest()}
			},
		)

		builder = builder.WatchesRawSource(source.Channel(eventCh, reloadHandler))
	}

	err := builder.Complete(rec)

	return errors.Wrap(err, "setting up controller")
}

// Reconcile resolves addresses from EndpointSlices and patches headless
// Service objects for each configured output.
func (rec *Reconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	cfg := rec.config.Load()

	sliceList, err := rec.listEndpointSlices(ctx, cfg)
	if err != nil {
		reconcileTotal.WithLabelValues("error").Inc()

		return ctrl.Result{}, err
	}

	outputErr := rec.reconcileAllOutputs(ctx, cfg, sliceList.Items)
	if outputErr != nil {
		reconcileTotal.WithLabelValues("error").Inc()

		return ctrl.Result{}, outputErr
	}

	reconcileTotal.WithLabelValues("success").Inc()
	lastReconcileTimestamp.Set(float64(time.Now().Unix()))
	rec.markReady()

	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// ReadyzCheck reports whether the controller has completed at least one
// successful reconciliation and has not become stale.
func (rec *Reconciler) ReadyzCheck(_ *http.Request) error {
	rec.mu.Lock()
	defer rec.mu.Unlock()

	if rec.lastSuccess.IsZero() {
		return errNotReconciled
	}

	since := time.Since(rec.lastSuccess)
	if since > stalenessThreshold {
		return errors.Newf("last successful reconciliation was %s ago (stale threshold: %s)",
			since.Round(time.Second), stalenessThreshold)
	}

	return nil
}

// listEndpointSlices fetches all EndpointSlices matching the source config.
func (rec *Reconciler) listEndpointSlices(
	ctx context.Context,
	cfg *config.Config,
) (*discoveryv1.EndpointSliceList, error) {
	var sliceList discoveryv1.EndpointSliceList

	err := rec.client.List(ctx, &sliceList,
		client.InNamespace(cfg.Source.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: cfg.Source.ServiceName},
	)
	if err != nil {
		return nil, errors.Wrap(err, "listing endpoint slices")
	}

	return &sliceList, nil
}

// reconcileAllOutputs processes every configured output, collecting errors
// so that one broken output does not block the others.
func (rec *Reconciler) reconcileAllOutputs(
	ctx context.Context,
	cfg *config.Config,
	slices []discoveryv1.EndpointSlice,
) error {
	var errs []error

	for idx := range cfg.Outputs {
		err := rec.reconcileOutput(ctx, slices, &cfg.Outputs[idx])
		if err != nil {
			rec.log.ErrorContext(ctx, "output reconciliation failed",
				slog.String("output", cfg.Outputs[idx].Name),
				slog.String("error", err.Error()),
			)

			errs = append(errs, err)
		}
	}

	cleanupErr := rec.cleanupStaleServices(ctx, cfg)
	if cleanupErr != nil {
		errs = append(errs, cleanupErr)
	}

	return errors.Join(errs...)
}

// reconcileOutput resolves addresses and applies the headless Service for a
// single output configuration.
func (rec *Reconciler) reconcileOutput(
	ctx context.Context,
	slices []discoveryv1.EndpointSlice,
	output *config.OutputConfig,
) error {
	addresses, err := rec.resolver.ResolveAddresses(
		ctx, slices, output.AddressSource, discoveryv1.AddressType(output.AddressType),
	)
	if err != nil {
		return errors.Wrapf(err, "resolving addresses for output %q", output.Name)
	}

	if len(addresses) == 0 {
		rec.log.WarnContext(ctx, "no addresses resolved, skipping output to protect against split-brain",
			slog.String("output", output.Name),
		)

		return nil
	}

	svc := rec.buildService(output, addresses)

	applyErr := rec.client.Apply(ctx, svc,
		client.FieldOwner(fieldOwner),
		client.ForceOwnership,
	)
	if applyErr != nil {
		return errors.Wrapf(applyErr, "applying service for output %q", output.Name)
	}

	endpointsResolved.WithLabelValues(output.Name).Set(float64(len(addresses)))

	return nil
}

// buildService constructs a Service apply configuration for server-side apply.
func (rec *Reconciler) buildService(
	output *config.OutputConfig,
	addresses []string,
) *applycorev1.ServiceApplyConfiguration {
	svc := applycorev1.Service(output.ServiceName, rec.namespace).
		WithLabels(map[string]string{
			managedByLabel: managedByValue,
			instanceLabel:  rec.instanceName,
		}).
		WithAnnotations(map[string]string{
			output.AnnotationPrefix + "hostname": strings.Join(output.Hostnames, ","),
			output.AnnotationPrefix + "target":   strings.Join(addresses, ","),
			output.AnnotationPrefix + "ttl":      strconv.Itoa(output.RecordTTL),
		})

	if rec.ownerRef != nil {
		svc = svc.WithOwnerReferences(
			applymetav1.OwnerReference().
				WithAPIVersion(rec.ownerRef.APIVersion).
				WithKind(rec.ownerRef.Kind).
				WithName(rec.ownerRef.Name).
				WithUID(rec.ownerRef.UID).
				WithBlockOwnerDeletion(true),
		)
	}

	return svc.WithSpec(applycorev1.ServiceSpec().
		WithType(corev1.ServiceTypeClusterIP).
		WithClusterIP(corev1.ClusterIPNone),
	)
}

// cleanupStaleServices removes Services with the managed-by label that no
// longer correspond to any configured output.
func (rec *Reconciler) cleanupStaleServices(ctx context.Context, cfg *config.Config) error {
	expected := make(map[string]struct{}, len(cfg.Outputs))
	for idx := range cfg.Outputs {
		expected[cfg.Outputs[idx].ServiceName] = struct{}{}
	}

	var svcList corev1.ServiceList

	err := rec.client.List(ctx, &svcList,
		client.InNamespace(rec.namespace),
		client.MatchingLabels{
			managedByLabel: managedByValue,
			instanceLabel:  rec.instanceName,
		},
	)
	if err != nil {
		return errors.Wrap(err, "listing managed services for cleanup")
	}

	var errs []error

	for idx := range svcList.Items {
		svc := &svcList.Items[idx]

		if _, ok := expected[svc.Name]; ok {
			continue
		}

		rec.log.InfoContext(ctx, "deleting stale service",
			slog.String("name", svc.Name),
			slog.String("namespace", svc.Namespace),
		)

		delErr := rec.client.Delete(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      svc.Name,
				Namespace: svc.Namespace,
			},
		})
		if delErr != nil {
			errs = append(errs, errors.Wrapf(delErr, "deleting stale service %s/%s", svc.Namespace, svc.Name))
		}
	}

	return errors.Join(errs...)
}

// markReady records that a reconciliation has succeeded.
func (rec *Reconciler) markReady() {
	rec.mu.Lock()
	defer rec.mu.Unlock()

	rec.lastSuccess = time.Now()
}
