package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

const kindDeployment = "Deployment"

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()

	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}

	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding appsv1 to scheme: %v", err)
	}

	return scheme
}

func TestResolveOwnerDeployment_HappyPath(t *testing.T) {
	t.Setenv("POD_NAME", "my-pod")

	scheme := newTestScheme(t)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-pod",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "ReplicaSet",
					Name:       "my-rs",
					UID:        types.UID("rs-uid-123"),
				},
			},
		},
	}

	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-rs",
			Namespace: "default",
			UID:       types.UID("rs-uid-123"),
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       kindDeployment,
					Name:       "my-deploy",
					UID:        types.UID("deploy-uid-456"),
				},
			},
		},
	}

	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, replicaSet).Build()

	ref, err := resolveOwnerDeployment(context.Background(), reader, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ref == nil {
		t.Fatal("expected non-nil OwnerReference")
	}

	if ref.Kind != kindDeployment {
		t.Errorf("kind = %q, want Deployment", ref.Kind)
	}

	if ref.Name != "my-deploy" {
		t.Errorf("name = %q, want my-deploy", ref.Name)
	}

	if ref.UID != types.UID("deploy-uid-456") {
		t.Errorf("uid = %q, want deploy-uid-456", ref.UID)
	}
}

func TestResolveOwnerDeployment_NoPodName(t *testing.T) {
	t.Setenv("POD_NAME", "")

	scheme := newTestScheme(t)
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, err := resolveOwnerDeployment(context.Background(), reader, "default")
	if err == nil {
		t.Fatal("expected error for missing POD_NAME, got nil")
	}

	if !strings.Contains(err.Error(), "POD_NAME") {
		t.Errorf("error = %q, want it to mention POD_NAME", err.Error())
	}
}

func TestResolveOwnerDeployment_PodWithoutReplicaSetOwner(t *testing.T) {
	t.Setenv("POD_NAME", "bare-pod")

	scheme := newTestScheme(t)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bare-pod",
			Namespace: "default",
		},
	}

	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	_, err := resolveOwnerDeployment(context.Background(), reader, "default")
	if err == nil {
		t.Fatal("expected error for pod without RS owner, got nil")
	}

	if !strings.Contains(err.Error(), "no ReplicaSet owner") {
		t.Errorf("error = %q, want it to mention no ReplicaSet owner", err.Error())
	}
}

func TestResolveOwnerDeployment_ReplicaSetWithoutDeploymentOwner(t *testing.T) {
	t.Setenv("POD_NAME", "my-pod")

	scheme := newTestScheme(t)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-pod",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "ReplicaSet",
					Name:       "standalone-rs",
					UID:        types.UID("rs-uid-789"),
				},
			},
		},
	}

	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "standalone-rs",
			Namespace: "default",
			UID:       types.UID("rs-uid-789"),
		},
	}

	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, replicaSet).Build()

	_, err := resolveOwnerDeployment(context.Background(), reader, "default")
	if err == nil {
		t.Fatal("expected error for RS without Deployment owner, got nil")
	}

	if !strings.Contains(err.Error(), "no Deployment owner") {
		t.Errorf("error = %q, want it to mention no Deployment owner", err.Error())
	}
}

func TestResolveOwnerDeployment_PodNotFound(t *testing.T) {
	t.Setenv("POD_NAME", "nonexistent-pod")

	scheme := newTestScheme(t)
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, err := resolveOwnerDeployment(context.Background(), reader, "default")
	if err == nil {
		t.Fatal("expected error for pod not found, got nil")
	}

	if !strings.Contains(err.Error(), "getting controller pod") {
		t.Errorf("error = %q, want it to mention getting controller pod", err.Error())
	}
}

func TestSetLogLevel_AllLevels(t *testing.T) {
	tests := []struct {
		level    string
		expected slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"unknown", slog.LevelInfo},
		{"", slog.LevelInfo},
	}

	for _, tc := range tests {
		t.Run(tc.level, func(t *testing.T) {
			lv := &slog.LevelVar{}
			setLogLevel(lv, tc.level)

			if lv.Level() != tc.expected {
				t.Errorf("setLogLevel(%q) = %v, want %v", tc.level, lv.Level(), tc.expected)
			}
		})
	}
}
