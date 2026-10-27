package controller

import (
	"context"
	"testing"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
)

// SLIM readiness depends on status of other resources whose status-only
// changes the event filter drops, so the controller must poll while SLIM is
// blocked and while it is Ready (to notice a dependency being lost).
func TestAIServiceReconcile_SlimPollsItsDependencies(t *testing.T) {
	t.Setenv("RELATED_IMAGE_SLIM_API", "slim:test")

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, aiv1.AddToScheme(scheme))
	require.NoError(t, rayv1.AddToScheme(scheme))

	platform := &aiv1.AIPlatform{
		ObjectMeta: metav1.ObjectMeta{Name: "platform", Namespace: "ai"},
		Spec:       aiv1.AIPlatformSpec{Features: []aiv1.FeatureSpec{{Name: "slim"}}},
	}
	slim := &aiv1.AIService{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-slim", Namespace: "ai"},
		Spec: aiv1.AIServiceSpec{
			Feature:       aiv1.FeatureSpec{Name: "slim"},
			AIPlatformRef: corev1.ObjectReference{Name: "platform", Namespace: "ai"},
			AIPlatformUrl: "http://models.ai.svc:8000",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform, slim).
		WithStatusSubresource(&aiv1.AIService{}).Build()
	r := &AIServiceReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(50)}

	// The platform is full, there is no RayService: SLIM is blocked, which is
	// reported on its status and must not be a reconcile error.
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "platform-slim", Namespace: "ai"}})
	require.NoError(t, err)
	require.Equal(t, slimDependencyPollInterval, result.RequeueAfter)

	got := &aiv1.AIService{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "platform-slim", Namespace: "ai"}, got))
	var ready *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == "Ready" {
			ready = &got.Status.Conditions[i]
		}
	}
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionFalse, ready.Status)
	require.Equal(t, "DependencyNotReady", ready.Reason)
}

func TestAIServiceReconcile_ReadySlimStillPolls(t *testing.T) {
	t.Setenv("RELATED_IMAGE_SLIM_API", "slim:test")

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, aiv1.AddToScheme(scheme))

	// A CPU-only platform with a configured model URL and no AgentRuntime has no
	// dependency to wait for, so SLIM is Ready, and must still be re-checked.
	platform := &aiv1.AIPlatform{
		ObjectMeta: metav1.ObjectMeta{Name: "platform", Namespace: "ai"},
		Spec:       aiv1.AIPlatformSpec{Profile: aiv1.AIPlatformProfileCPUOnly, Features: []aiv1.FeatureSpec{{Name: "slim"}}},
	}
	slim := &aiv1.AIService{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-slim", Namespace: "ai"},
		Spec: aiv1.AIServiceSpec{
			Feature:       aiv1.FeatureSpec{Name: "slim"},
			AIPlatformRef: corev1.ObjectReference{Name: "platform", Namespace: "ai"},
			AIPlatformUrl: "http://models.ai.svc:8000",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform, slim).
		WithStatusSubresource(&aiv1.AIService{}, &appsv1.Deployment{}).Build()
	r := &AIServiceReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(50)}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "platform-slim", Namespace: "ai"}}

	// First pass creates the Deployment; a cpuOnly SLIM then waits for it.
	_, err := r.Reconcile(context.Background(), req)
	require.Error(t, err)
	dep := &appsv1.Deployment{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "platform-slim-slim-deployment", Namespace: "ai"}, dep))
	dep.Status = appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	require.NoError(t, c.Status().Update(context.Background(), dep))

	result, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, slimDependencyPollInterval, result.RequeueAfter)

	got := &aiv1.AIService{}
	require.NoError(t, c.Get(context.Background(), req.NamespacedName, got))
	var ready *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == "Ready" {
			ready = &got.Status.Conditions[i]
		}
	}
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionTrue, ready.Status)
}
