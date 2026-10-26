package slim

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A CPU-only platform must not need a GPU node, so the SLIM pod, including the
// resource defaults validation applies, may not request or select GPU capacity.
func TestSlimDeploymentForCPUOnlyRequestsNoGPU(t *testing.T) {
	ctx := context.Background()
	scheme := buildTestScheme(t)
	platform := &aiv1.AIPlatform{
		ObjectMeta: metav1.ObjectMeta{Name: "launchpad", Namespace: "ai-platform"},
		Spec: aiv1.AIPlatformSpec{
			Profile:  aiv1.AIPlatformProfileCPUOnly,
			Features: []aiv1.FeatureSpec{{Name: "agentruntime", Provider: "mltk", LAS: &aiv1.LASFeatureSpec{AssistantID: "echo"}}},
		},
	}
	ai := &aiv1.AIService{
		ObjectMeta: metav1.ObjectMeta{Name: "launchpad-slim", Namespace: "ai-platform", UID: types.UID("slim-uid")},
		Spec:       aiv1.AIServiceSpec{AIPlatformRef: corev1.ObjectReference{Name: platform.Name, Namespace: platform.Namespace}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform, ai).Build()
	r := &SlimReconciler{Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}
	t.Setenv("RELATED_IMAGE_SLIM_API", "slim:test")

	require.NoError(t, r.validateAIService(ctx, ai))
	require.NoError(t, r.reconcileSlimConfigMap(ctx, ai))
	require.NoError(t, r.reconcileSlimDeployment(ctx, ai))

	deployment := &appsv1.Deployment{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: ai.Name + "-slim-deployment", Namespace: ai.Namespace}, deployment))
	pod := deployment.Spec.Template.Spec
	require.NotEmpty(t, pod.Containers)
	for _, container := range pod.Containers {
		require.NotEmpty(t, container.Resources.Requests, "container %s has no resource requests", container.Name)
		for _, resources := range []corev1.ResourceList{container.Resources.Requests, container.Resources.Limits} {
			for name := range resources {
				require.Contains(t, []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory}, name,
					"container %s requests non-CPU/memory resource %s", container.Name, name)
			}
		}
	}
	podSpec, err := json.Marshal(pod)
	require.NoError(t, err)
	lower := strings.ToLower(string(podSpec))
	require.NotContains(t, lower, "nvidia")
	require.NotContains(t, lower, "gpu")
}
