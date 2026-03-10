package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mumoshu/arc-detective/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func createTestNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-",
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ns))
	t.Cleanup(func() {
		k8sClient.Delete(context.Background(), ns)
	})
	return ns.Name
}

func newRunnerPod(ns, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:    "runner",
				Image:   "busybox",
				Command: []string{"sh", "-c", "echo running && sleep 3600"},
			}},
		},
	}
}

func arcLabels(scaleSetName string) map[string]string {
	return map[string]string{
		"actions-ephemeral-runner":          "True",
		"actions.github.com/scale-set-name": scaleSetName,
	}
}

func newEphemeralRunner(ns, name string, status map[string]interface{}) *unstructured.Unstructured {
	er := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "actions.github.com/v1alpha1",
			"kind":       "EphemeralRunner",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"githubConfigUrl":    "https://github.com/myorg/myrepo",
				"githubConfigSecret": "fake-secret",
				"runnerScaleSetId":   int64(1),
			},
		},
	}
	er.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "actions.github.com",
		Version: "v1alpha1",
		Kind:    "EphemeralRunner",
	})
	if status != nil {
		er.Object["status"] = status
	}
	return er
}

func waitForInvestigation(t *testing.T, ns string, timeout time.Duration) *v1alpha1.Investigation {
	t.Helper()
	var inv v1alpha1.Investigation
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		list := &v1alpha1.InvestigationList{}
		if err := k8sClient.List(ctx, list, client.InNamespace(ns)); err == nil && len(list.Items) > 0 {
			inv = list.Items[0]
			return &inv
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("Timed out waiting for Investigation to be created")
	return nil
}

func waitForInvestigationPhase(t *testing.T, ns string, phase string, timeout time.Duration) *v1alpha1.Investigation {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		list := &v1alpha1.InvestigationList{}
		if err := k8sClient.List(ctx, list, client.InNamespace(ns)); err == nil {
			for _, inv := range list.Items {
				if inv.Status.Phase == phase {
					return &inv
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("Timed out waiting for Investigation in phase %q", phase)
	return nil
}

func waitForFinalizer(t *testing.T, ns, name, finalizerName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod := &corev1.Pod{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, pod); err == nil {
			for _, f := range pod.Finalizers {
				if f == finalizerName {
					return
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("Timed out waiting for finalizer %q on pod %s/%s", finalizerName, ns, name)
}

func updatePodStatus(t *testing.T, ns, name string, updateFn func(*corev1.PodStatus)) {
	t.Helper()
	pod := &corev1.Pod{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, pod))
	updateFn(&pod.Status)
	require.NoError(t, k8sClient.Status().Update(ctx, pod))
}

func updateERStatus(t *testing.T, ns, name string, status map[string]interface{}) {
	t.Helper()
	er := &unstructured.Unstructured{}
	er.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "actions.github.com",
		Version: "v1alpha1",
		Kind:    "EphemeralRunner",
	})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, er))
	er.Object["status"] = status
	require.NoError(t, k8sClient.Status().Update(ctx, er))
}

func createSecret(t *testing.T, ns, name string, data map[string][]byte) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       data,
	}
	require.NoError(t, k8sClient.Create(ctx, secret))
}

func newDetectiveConfig(ns, name string, spec v1alpha1.DetectiveConfigSpec) *v1alpha1.DetectiveConfig {
	return &v1alpha1.DetectiveConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spec,
	}
}

func _() {
	// prevent unused import
	_ = fmt.Sprintf
}
