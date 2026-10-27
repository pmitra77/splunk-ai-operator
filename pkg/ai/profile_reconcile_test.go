package ai_platform

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	aiApi "github.com/splunk/splunk-ai-operator/api/v1"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func profileTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, aiApi.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
	require.NoError(t, networkingv1.AddToScheme(s))
	require.NoError(t, rayv1.AddToScheme(s))
	return s
}

func profileTestPlatform(profile aiApi.AIPlatformProfile, features ...aiApi.FeatureSpec) *aiApi.AIPlatform {
	return &aiApi.AIPlatform{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ai", Namespace: "default", UID: "platform-uid"},
		Spec: aiApi.AIPlatformSpec{
			Profile:       profile,
			ObjectStorage: aiApi.ObjectStorageSpec{Path: "s3://fixture/tasks", Region: "us-west-2"},
			Features:      features,
		},
	}
}

func launchpadFeatures() []aiApi.FeatureSpec {
	return []aiApi.FeatureSpec{
		{Name: "slim", Version: "1.0.0"},
		{
			Name: "agentruntime", Provider: "mltk", Version: "1.0.0",
			LicenseSecretRef: "las-auth", PostgresSecretRef: "las-postgres", RedisSecretRef: "las-redis",
			LAS: &aiApi.LASFeatureSpec{
				Image:       aiApi.LASImageSpec{Repository: "registry.example/las", Tag: "v1"},
				AssistantID: "echo",
			},
		},
	}
}

// The fake client has no AIService controller, so children never become Ready.
func profileTestReconciler(t *testing.T, platform *aiApi.AIPlatform) (*AIPlatformReconciler, client.Client) {
	t.Helper()
	scheme := profileTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&aiApi.AIPlatform{}, &aiApi.AIService{}).
		WithObjects(platform).
		WithIndex(&aiApi.AIService{}, ownerKey, func(obj client.Object) []string {
			if owner := metav1.GetControllerOfNoCopy(obj); owner != nil {
				return []string{owner.Name}
			}
			return nil
		}).Build()
	return &AIPlatformReconciler{p: platform, Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(100)}, c
}

func stageNames(stages []reconcileStage) []string {
	names := make([]string, 0, len(stages))
	for _, s := range stages {
		names = append(names, s.name)
	}
	return names
}

// Stage names become "<name>Ready" status conditions, and SLIM reads
// RayServiceStatusReady, so the lists are pinned exactly.
func TestStagesFor_PinsStagesPerProfile(t *testing.T) {
	fullStages := []string{
		"Validate", "Sidecars", "rayAutoscalerRBAC", "RayService", "ActiveClusterScale",
		"WeaviateDatabase", "Ingress", "RayServiceStatus", "WeaviateDatabaseStatus", "IngressStatus",
		"AIService", "AIServiceStatus",
	}
	for _, test := range []struct {
		name    string
		profile aiApi.AIPlatformProfile
		want    []string
	}{
		{name: "profile omitted selects full", profile: "", want: fullStages},
		{name: "explicit full", profile: aiApi.AIPlatformProfileFull, want: fullStages},
		{name: "cpuOnly", profile: aiApi.AIPlatformProfileCPUOnly, want: []string{"Validate", "AIService", "AIServiceStatus"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := &AIPlatformReconciler{}
			stages := r.stagesFor(&aiApi.AIPlatform{Spec: aiApi.AIPlatformSpec{Profile: test.profile}}, nil)
			require.Equal(t, test.want, stageNames(stages))
		})
	}
}

func TestReconcile_CPUOnlyCreatesOnlyLaunchpadResources(t *testing.T) {
	ctx := context.Background()
	platform := profileTestPlatform(aiApi.AIPlatformProfileCPUOnly, launchpadFeatures()...)
	r, c := profileTestReconciler(t, platform)

	_, err := r.Reconcile(ctx, platform)
	require.ErrorContains(t, err, "LAS Ready condition", "children are never Ready under the fake client")

	var services aiApi.AIServiceList
	require.NoError(t, c.List(ctx, &services))
	names := make([]string, 0, len(services.Items))
	for _, s := range services.Items {
		names = append(names, s.Name)
	}
	require.ElementsMatch(t, []string{"my-ai-slim", "my-ai-agentruntime-mltk"}, names, "only SLIM and AgentRuntime are created")

	// Nothing for model serving, a vector database, ingress or sidecars exists.
	for _, list := range []client.ObjectList{
		&rayv1.RayServiceList{}, &rayv1.RayClusterList{}, &appsv1.StatefulSetList{}, &appsv1.DeploymentList{},
		&networkingv1.IngressList{}, &corev1.ServiceList{}, &corev1.ConfigMapList{}, &corev1.SecretList{},
		&corev1.ServiceAccountList{}, &corev1.PersistentVolumeClaimList{},
	} {
		require.NoError(t, c.List(ctx, list))
		require.Zero(t, apimeta.LenList(list), "unexpected %T items from a cpuOnly platform reconcile", list)
	}

	// The status records only the stages cpuOnly runs.
	latest := &aiApi.AIPlatform{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(platform), latest))
	types := map[string]bool{}
	for _, cond := range latest.Status.Conditions {
		types[cond.Type] = true
	}
	for _, want := range []string{"ValidateReady", "AIServiceReady", "AIServiceStatusReady"} {
		require.True(t, types[want], "missing condition %s in %v", want, types)
	}
	for _, forbidden := range []string{"Sidecars", "rayAutoscalerRBAC", "RayService", "ActiveClusterScale", "Weaviate", "Ingress"} {
		for condType := range types {
			require.False(t, strings.HasPrefix(condType, forbidden), "cpuOnly recorded a %s condition", condType)
		}
	}

	// No GPU resources, tolerations, selectors or scheduling reach any child.
	require.Nil(t, platform.Spec.GPUSchedulingSpec, "cpuOnly must not default GPU scheduling")
	for _, s := range services.Items {
		spec, err := json.Marshal(s.Spec)
		require.NoError(t, err)
		lower := strings.ToLower(string(spec))
		require.NotContains(t, lower, "nvidia", "AIService %s", s.Name)
		require.NotContains(t, lower, "gpu", "AIService %s", s.Name)
	}
}

func TestReconcile_CPUOnlyRepeatDoesNotRewriteChildren(t *testing.T) {
	ctx := context.Background()
	platform := profileTestPlatform(aiApi.AIPlatformProfileCPUOnly, launchpadFeatures()...)
	r, c := profileTestReconciler(t, platform)

	_, _ = r.Reconcile(ctx, platform)
	versions := map[string]string{}
	var services aiApi.AIServiceList
	require.NoError(t, c.List(ctx, &services))
	require.Len(t, services.Items, 2)
	for _, s := range services.Items {
		versions[s.Name] = s.ResourceVersion
	}

	_, _ = r.Reconcile(ctx, platform)
	require.NoError(t, c.List(ctx, &services))
	require.Len(t, services.Items, 2)
	for _, s := range services.Items {
		require.Equal(t, versions[s.Name], s.ResourceVersion, "unchanged cpuOnly config rewrote AIService %s", s.Name)
	}
}

func TestReconcileFeatures_FullProfileStillCreatesSAIA(t *testing.T) {
	ctx := context.Background()
	features := append([]aiApi.FeatureSpec{{Name: "saia", Version: "1.0.0"}}, launchpadFeatures()...)
	platform := profileTestPlatform("", features...)
	r, c := profileTestReconciler(t, platform)

	require.NoError(t, r.ReconcileFeatures(ctx, platform))
	var services aiApi.AIServiceList
	require.NoError(t, c.List(ctx, &services))
	names := make([]string, 0, len(services.Items))
	for _, s := range services.Items {
		names = append(names, s.Name)
	}
	require.ElementsMatch(t, []string{"my-ai-saia", "my-ai-slim", "my-ai-agentruntime-mltk"}, names)
}

func TestValidate_ProfileGuardsAndGPUDefaults(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		status aiApi.AIPlatformStatus
	}{
		{name: "ray service name recorded", status: aiApi.AIPlatformStatus{RayServiceName: "my-ai-head-svc"}},
		{name: "vector database recorded", status: aiApi.AIPlatformStatus{VectorDbServiceName: "my-ai-weaviate"}},
		{name: "ray service status recorded", status: aiApi.AIPlatformStatus{RayServiceStatus: rayv1.Running}},
	} {
		t.Run("cpuOnly rejects a platform that already ran model serving: "+test.name, func(t *testing.T) {
			platform := profileTestPlatform(aiApi.AIPlatformProfileCPUOnly, launchpadFeatures()...)
			platform.Status = test.status
			r, _ := profileTestReconciler(t, platform)
			require.ErrorContains(t, r.validate(ctx, platform), "switching an existing model-serving AIPlatform to cpuOnly is not supported")
		})
		t.Run("full accepts the same history: "+test.name, func(t *testing.T) {
			platform := profileTestPlatform("", launchpadFeatures()...)
			platform.Status = test.status
			r, _ := profileTestReconciler(t, platform)
			require.NoError(t, r.validate(ctx, platform))
		})
	}

	t.Run("a fresh cpuOnly platform is accepted and gets no GPU scheduling", func(t *testing.T) {
		platform := profileTestPlatform(aiApi.AIPlatformProfileCPUOnly, launchpadFeatures()...)
		r, _ := profileTestReconciler(t, platform)
		require.NoError(t, r.validate(ctx, platform))
		require.Nil(t, platform.Spec.GPUSchedulingSpec)
		require.NotNil(t, platform.Spec.CPUSchedulingSpec)
	})
	t.Run("a full platform defaults GPU scheduling", func(t *testing.T) {
		platform := profileTestPlatform("", launchpadFeatures()...)
		r, _ := profileTestReconciler(t, platform)
		require.NoError(t, r.validate(ctx, platform))
		require.NotNil(t, platform.Spec.GPUSchedulingSpec)
	})
}
