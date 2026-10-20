package slim

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"testing"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAgentRuntimeURLUsesStableServiceBaseURL(t *testing.T) {
	scheme := buildTestScheme(t)
	platform := &aiv1.AIPlatform{
		ObjectMeta: metav1.ObjectMeta{Name: "launchpad", Namespace: "ai-platform"},
		Spec: aiv1.AIPlatformSpec{Features: []aiv1.FeatureSpec{
			{Name: "agentruntime", Provider: "mltk", LAS: &aiv1.LASFeatureSpec{AssistantID: "echo"}},
		}},
	}
	ai := &aiv1.AIService{
		ObjectMeta: metav1.ObjectMeta{Name: "launchpad-slim", Namespace: "ai-platform"},
		Spec: aiv1.AIServiceSpec{
			AIPlatformRef: corev1.ObjectReference{Name: "launchpad", Namespace: "ai-platform"},
			ClusterDomain: "cluster.example",
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform).Build()
	r := &SlimReconciler{Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	got, graphID, err := r.agentRuntimeTarget(context.Background(), ai)
	require.NoError(t, err)
	assert.Equal(t, "http://launchpad-agentruntime-mltk-svc.ai-platform.svc.cluster.example:8080", got)
	assert.Equal(t, "echo", graphID)
}

func TestAgentRuntimeURLUsesBoundedLASServiceName(t *testing.T) {
	scheme := buildTestScheme(t)
	platformName := strings.Repeat("a", 52)
	platform := &aiv1.AIPlatform{
		ObjectMeta: metav1.ObjectMeta{Name: platformName, Namespace: "ai-platform"},
		Spec: aiv1.AIPlatformSpec{Features: []aiv1.FeatureSpec{
			{Name: "agentruntime", Provider: "mltk", LAS: &aiv1.LASFeatureSpec{AssistantID: "echo"}},
		}},
	}
	ai := &aiv1.AIService{Spec: aiv1.AIServiceSpec{
		AIPlatformRef: corev1.ObjectReference{Name: platformName, Namespace: platform.Namespace},
	}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform).Build()
	r := &SlimReconciler{Client: fakeClient, Scheme: scheme}

	// Mirror the AIPlatform child-name and LAS stable-Service naming contracts.
	fullChildName := platformName + "-agentruntime-mltk"
	childName := fullChildName[:54] + "-" + testFNV32(fullChildName)
	fullServiceName := childName + "-svc"
	serviceName := childName[:50] + "-" + testFNV32(fullServiceName) + "-svc"
	got, graphID, err := r.agentRuntimeTarget(context.Background(), ai)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("http://%s.ai-platform.svc.cluster.local:8080", serviceName), got)
	assert.Equal(t, "echo", graphID)
}

func testFNV32(value string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(value))
	return fmt.Sprintf("%08x", h.Sum32())
}

func TestSlimDeploymentSeparatesLASAndModelEndpoints(t *testing.T) {
	scheme := buildTestScheme(t)
	platform := &aiv1.AIPlatform{
		ObjectMeta: metav1.ObjectMeta{Name: "launchpad", Namespace: "ai-platform"},
		Spec:       aiv1.AIPlatformSpec{Features: []aiv1.FeatureSpec{{Name: "agentruntime", Provider: "mltk", LAS: &aiv1.LASFeatureSpec{AssistantID: "echo"}}}},
	}
	for _, tc := range []struct {
		name         string
		modelURL     string
		wantModelURL string
	}{
		{name: "cpu-only"},
		{name: "with-model-serving", modelURL: "http://ray.ai-platform.svc.cluster.local:8000/ai-platform-models/v1", wantModelURL: "http://ray.ai-platform.svc.cluster.local:8000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ai := &aiv1.AIService{
				ObjectMeta: metav1.ObjectMeta{Name: "launchpad-slim", Namespace: "ai-platform", UID: types.UID("slim-uid")},
				Spec: aiv1.AIServiceSpec{
					AIPlatformRef: corev1.ObjectReference{Name: platform.Name, Namespace: platform.Namespace},
					AIPlatformUrl: tc.modelURL,
				},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform, ai).Build()
			r := &SlimReconciler{Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}
			t.Setenv("RELATED_IMAGE_SLIM_API", "slim:test")
			require.NoError(t, r.reconcileSlimConfigMap(context.Background(), ai))
			require.NoError(t, r.reconcileSlimDeployment(context.Background(), ai))
			deployment := &appsv1.Deployment{}
			require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: ai.Name + "-slim-deployment", Namespace: ai.Namespace}, deployment))
			env := map[string]string{}
			for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
				env[variable.Name] = variable.Value
			}
			assert.Equal(t, "http://launchpad-agentruntime-mltk-svc.ai-platform.svc.cluster.local:8080", env["AGENT_SERVER_URL"])
			assert.Equal(t, "echo", env["LAS_GRAPH_ID"])
			if tc.wantModelURL == "" {
				assert.NotContains(t, env, "PLATFORM_URL")
			} else {
				assert.Equal(t, tc.wantModelURL, env["PLATFORM_URL"])
			}
		})
	}
}

func TestSlimDeploymentPreservesServerDefaultsAndRevisionWithoutRollout(t *testing.T) {
	ctx := context.Background()
	scheme := buildTestScheme(t)
	platform := &aiv1.AIPlatform{
		ObjectMeta: metav1.ObjectMeta{Name: "launchpad", Namespace: "ai-platform"},
		Spec: aiv1.AIPlatformSpec{Features: []aiv1.FeatureSpec{
			{Name: "agentruntime", Provider: "mltk", LAS: &aiv1.LASFeatureSpec{AssistantID: "echo"}},
		}},
	}
	ai := &aiv1.AIService{
		ObjectMeta: metav1.ObjectMeta{Name: "launchpad-slim", Namespace: "ai-platform", UID: types.UID("slim-uid")},
		Spec: aiv1.AIServiceSpec{
			AIPlatformRef:      corev1.ObjectReference{Name: platform.Name, Namespace: platform.Namespace},
			ServiceAccountName: "launchpad-slim-sa",
			Replicas:           1,
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform, ai).Build()
	r := &SlimReconciler{Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}
	t.Setenv("RELATED_IMAGE_SLIM_API", "slim:test")
	require.NoError(t, r.reconcileSlimConfigMap(ctx, ai))
	require.NoError(t, r.reconcileSlimDeployment(ctx, ai))

	key := types.NamespacedName{Name: ai.Name + "-slim-deployment", Namespace: ai.Namespace}
	deployment := &appsv1.Deployment{}
	require.NoError(t, fakeClient.Get(ctx, key, deployment))
	if deployment.Annotations == nil {
		deployment.Annotations = map[string]string{}
	}
	deployment.Annotations["deployment.kubernetes.io/revision"] = "7"
	// Simulate the defaults supplied by the API server to a stored Deployment.
	template := &deployment.Spec.Template.Spec
	template.DNSPolicy = corev1.DNSClusterFirst
	template.RestartPolicy = corev1.RestartPolicyAlways
	template.SchedulerName = "default-scheduler"
	grace := int64(30)
	template.TerminationGracePeriodSeconds = &grace
	serviceLinks := true
	template.EnableServiceLinks = &serviceLinks
	container := &template.Containers[0]
	container.TerminationMessagePath = "/dev/termination-log"
	container.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	for i := range container.Ports {
		container.Ports[i].Protocol = corev1.ProtocolTCP
	}
	for _, probe := range []*corev1.Probe{container.LivenessProbe, container.ReadinessProbe, container.StartupProbe} {
		probe.TimeoutSeconds = 1
		probe.SuccessThreshold = 1
		probe.HTTPGet.Scheme = corev1.URISchemeHTTP
	}
	defaultMode := int32(420)
	template.Volumes[0].ConfigMap.DefaultMode = &defaultMode
	require.NoError(t, fakeClient.Update(ctx, deployment))
	require.NoError(t, fakeClient.Get(ctx, key, deployment))
	storedVersion := deployment.ResourceVersion
	storedTemplate := deployment.Spec.Template.DeepCopy()

	require.NoError(t, r.reconcileSlimDeployment(ctx, ai))
	require.NoError(t, fakeClient.Get(ctx, key, deployment))
	require.Equal(t, *storedTemplate, deployment.Spec.Template)
	require.Equal(t, storedVersion, deployment.ResourceVersion, "unchanged desired config must not write Deployment")
	require.Equal(t, "7", deployment.Annotations["deployment.kubernetes.io/revision"])

	ai.Annotations = map[string]string{"example.com/rollout": "next"}
	require.NoError(t, r.reconcileSlimDeployment(ctx, ai))
	require.NoError(t, fakeClient.Get(ctx, key, deployment))
	require.NotEqual(t, storedVersion, deployment.ResourceVersion, "changed desired annotations must roll Deployment")
	require.Equal(t, "next", deployment.Spec.Template.Annotations["example.com/rollout"])
}

func TestAgentRuntimeURLRejectsMissingOrAmbiguousFeature(t *testing.T) {
	scheme := buildTestScheme(t)
	for _, tc := range []struct {
		name     string
		features []aiv1.FeatureSpec
		wantErr  string
	}{
		{name: "missing", wantErr: "has no AgentRuntime feature"},
		{
			name: "ambiguous",
			features: []aiv1.FeatureSpec{
				{Name: "agentruntime", Provider: "mltk"},
				{Name: "agentruntime", Provider: "seca"},
			},
			wantErr: "has multiple AgentRuntime features",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			platform := &aiv1.AIPlatform{
				ObjectMeta: metav1.ObjectMeta{Name: "launchpad", Namespace: "ai-platform"},
				Spec:       aiv1.AIPlatformSpec{Features: tc.features},
			}
			ai := &aiv1.AIService{Spec: aiv1.AIServiceSpec{AIPlatformRef: corev1.ObjectReference{Name: platform.Name, Namespace: platform.Namespace}}}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform).Build()
			r := &SlimReconciler{Client: fakeClient, Scheme: scheme}

			_, _, err := r.agentRuntimeTarget(context.Background(), ai)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestValidateAIService_CPUOnlyRouteDoesNotRequireRay(t *testing.T) {
	scheme := buildTestScheme(t)
	platform := &aiv1.AIPlatform{
		ObjectMeta: metav1.ObjectMeta{Name: "cpu-platform", Namespace: "ai-platform"},
		Spec:       aiv1.AIPlatformSpec{Features: []aiv1.FeatureSpec{{Name: "agentruntime", Provider: "mltk"}}},
	}
	ai := &aiv1.AIService{
		ObjectMeta: metav1.ObjectMeta{Name: "cpu-platform-slim", Namespace: "ai-platform"},
		Spec:       aiv1.AIServiceSpec{AIPlatformRef: corev1.ObjectReference{Name: platform.Name, Namespace: platform.Namespace}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform, ai).Build()
	r := &SlimReconciler{Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}
	t.Setenv("RELATED_IMAGE_SLIM_API", "slim:test")

	require.NoError(t, r.validateAIService(context.Background(), ai))
}

func TestValidateAIService_FullProfileStillRequiresRay(t *testing.T) {
	scheme := buildTestScheme(t)
	platform := &aiv1.AIPlatform{
		ObjectMeta: metav1.ObjectMeta{Name: "full-platform", Namespace: "ai-platform"},
		Spec: aiv1.AIPlatformSpec{Features: []aiv1.FeatureSpec{
			{Name: "saia"}, {Name: "agentruntime", Provider: "mltk"},
		}},
	}
	ai := &aiv1.AIService{
		ObjectMeta: metav1.ObjectMeta{Name: "full-platform-slim", Namespace: "ai-platform"},
		Spec:       aiv1.AIServiceSpec{AIPlatformRef: corev1.ObjectReference{Name: platform.Name, Namespace: platform.Namespace}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform, ai).Build()
	r := &SlimReconciler{Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}
	t.Setenv("RELATED_IMAGE_SLIM_API", "slim:test")

	require.ErrorContains(t, r.validateAIService(context.Background(), ai), "RayService is not ready")
}

func TestSlimConfigMapEnforcesProductionCallerAuthentication(t *testing.T) {
	scheme := buildTestScheme(t)
	ai := &aiv1.AIService{ObjectMeta: metav1.ObjectMeta{
		Name: "launchpad-slim", Namespace: "ai-platform", UID: types.UID("slim-uid"),
	}}
	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "launchpad-slim-slim-config", Namespace: ai.Namespace},
		Data:       map[string]string{"ENABLE_AUTHN": "false", "ENABLE_AUTHZ": "false"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ai, config).Build()
	r := &SlimReconciler{Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	require.NoError(t, r.reconcileSlimConfigMap(context.Background(), ai))
	updated := &corev1.ConfigMap{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: config.Name, Namespace: config.Namespace}, updated))
	assert.Equal(t, "true", updated.Data["ENABLE_AUTHN"])
	assert.Equal(t, "true", updated.Data["ENABLE_AUTHZ"])
	assert.NotContains(t, updated.Data, "APP_ENV")
}

func TestSlimAuthenticationConfigCorrectionRollsDeployment(t *testing.T) {
	scheme := buildTestScheme(t)
	platform := &aiv1.AIPlatform{
		ObjectMeta: metav1.ObjectMeta{Name: "launchpad", Namespace: "ai-platform"},
		Spec: aiv1.AIPlatformSpec{Features: []aiv1.FeatureSpec{
			{Name: "agentruntime", Provider: "mltk", LAS: &aiv1.LASFeatureSpec{AssistantID: "echo"}},
		}},
	}
	ai := &aiv1.AIService{
		ObjectMeta: metav1.ObjectMeta{Name: "launchpad-slim", Namespace: "ai-platform", UID: types.UID("slim-uid")},
		Spec: aiv1.AIServiceSpec{
			AIPlatformRef: corev1.ObjectReference{Name: platform.Name, Namespace: platform.Namespace},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform, ai).Build()
	r := &SlimReconciler{Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}
	t.Setenv("RELATED_IMAGE_SLIM_API", "slim:test")
	ctx := context.Background()

	require.NoError(t, r.reconcileSlimConfigMap(ctx, ai))
	require.NoError(t, r.reconcileSlimDeployment(ctx, ai))
	deployment := &appsv1.Deployment{}
	key := types.NamespacedName{Name: ai.Name + "-slim-deployment", Namespace: ai.Namespace}
	require.NoError(t, fakeClient.Get(ctx, key, deployment))
	initialRevision := deployment.Spec.Template.Annotations["splunk-ai-operator/slim-config-revision"]
	require.NotEmpty(t, initialRevision)

	config := &corev1.ConfigMap{}
	configKey := types.NamespacedName{Name: ai.Name + "-slim-config", Namespace: ai.Namespace}
	require.NoError(t, fakeClient.Get(ctx, configKey, config))
	config.Data["ENABLE_AUTHN"] = "false"
	config.Data["ENABLE_AUTHZ"] = "false"
	require.NoError(t, fakeClient.Update(ctx, config))
	require.NoError(t, r.reconcileSlimConfigMap(ctx, ai))
	require.NoError(t, fakeClient.Get(ctx, configKey, config))
	assert.Equal(t, "true", config.Data["ENABLE_AUTHN"])
	assert.Equal(t, "true", config.Data["ENABLE_AUTHZ"])

	require.NoError(t, r.reconcileSlimDeployment(ctx, ai))
	require.NoError(t, fakeClient.Get(ctx, key, deployment))
	assert.NotEqual(t, initialRevision, deployment.Spec.Template.Annotations["splunk-ai-operator/slim-config-revision"])
}
