package agentruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/release"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLASCredentialRotationChangesRevisionForInPlaceSecretUpdates(t *testing.T) {
	for _, tc := range []struct {
		name       string
		secretName string
		dataKey    string
	}{
		{name: "license", secretName: "las-auth", dataKey: "langgraph_cloud_license_key"},
		{name: "optional LangSmith API key", secretName: "las-auth", dataKey: "api_key"},
		{name: "PostgreSQL", secretName: "las-postgres", dataKey: "postgres_connection_url"},
		{name: "Redis", secretName: "las-redis", dataKey: "redis_connection_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ai, secrets := lasRotationFixture()
			scheme := buildAgentRuntimeTestScheme(t)
			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lasRotationObjects(ai, secrets)...).Build()
			reconciler := &LASReconciler{Client: client}

			before, err := reconciler.credentialRevision(context.Background(), ai)
			require.NoError(t, err)
			repeated, err := reconciler.credentialRevision(context.Background(), ai)
			require.NoError(t, err)
			require.Equal(t, before, repeated, "same Secret identities and resourceVersions must produce a stable revision")

			for _, secret := range secrets {
				if secret.Name == tc.secretName {
					secret.Data[tc.dataKey] = []byte("rotated-payload-marker")
				}
			}
			dataOnlyClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lasRotationObjects(ai, secrets)...).Build()
			reconciler = &LASReconciler{Client: dataOnlyClient}
			dataOnlyRevision, err := reconciler.credentialRevision(context.Background(), ai)
			require.NoError(t, err)
			require.Equal(t, before, dataOnlyRevision, "credential payload changes alone must not affect the metadata-only revision")

			for _, secret := range secrets {
				if secret.Name == tc.secretName {
					secret.ResourceVersion = "2"
				}
			}
			rotatedClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lasRotationObjects(ai, secrets)...).Build()
			reconciler = &LASReconciler{Client: rotatedClient}

			after, err := reconciler.credentialRevision(context.Background(), ai)
			require.NoError(t, err)
			require.NotEqual(t, before, after, "an in-place update under the same Secret name must change the revision")

			beforeValues := lasValues(ai, before)
			require.True(t, sameValues(beforeValues, lasValues(ai, repeated)), "repeated reconciliation of one rotation revision must produce identical Helm values")
			afterValues := lasValues(ai, after)
			require.False(t, sameValues(beforeValues, afterValues), "the changed Secret resourceVersion must change Helm values and trigger rollout")
			for _, workload := range []string{"apiServer", "queue"} {
				deployment := afterValues[workload].(map[string]interface{})["deployment"].(map[string]interface{})
				require.Equal(t, after, deployment["annotations"].(map[string]interface{})[lasCredentialRevisionAnnotation], "%s must receive the new revision", workload)
			}
		})
	}
}

func TestLASCredentialRotationDoesNotExposeCredentialPayloads(t *testing.T) {
	ai, secrets := lasRotationFixture()
	scheme := buildAgentRuntimeTestScheme(t)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&aiv1.AIService{}).WithObjects(lasRotationObjects(ai, secrets)...).Build()
	reconciler := &LASReconciler{Client: kubeClient, Scheme: scheme}

	revision, err := reconciler.credentialRevision(context.Background(), ai)
	require.NoError(t, err)
	require.Len(t, revision, 64)
	require.NotContains(t, revision, "license-payload-marker")
	require.NotContains(t, revision, "postgres-payload-marker")
	require.NotContains(t, revision, "redis-payload-marker")
	for _, payload := range [][]byte{
		[]byte("license-payload-marker"),
		[]byte("langsmith-api-key-marker"),
		[]byte("postgres-payload-marker"),
		[]byte("redis-payload-marker"),
	} {
		digest := sha256.Sum256(payload)
		require.NotContains(t, revision, hex.EncodeToString(digest[:]), "the revision must not hash credential payloads")
	}

	aiJSON, err := json.Marshal(ai)
	require.NoError(t, err)
	valuesJSON, err := json.Marshal(lasValues(ai, revision))
	require.NoError(t, err)
	chart, err := loader.LoadArchive(bytes.NewReader(lasChartArchive))
	require.NoError(t, err)
	renderValues, err := chartutil.ToRenderValues(chart, lasValues(ai, revision), chartutil.ReleaseOptions{Name: "las-rotation-test", Namespace: ai.Namespace}, chartutil.DefaultCapabilities)
	require.NoError(t, err)
	rendered, err := engine.Render(chart, renderValues)
	require.NoError(t, err)
	manifests := strings.Join(mapValues(rendered), "\n")
	for label, output := range map[string]string{
		"AIService":      string(aiJSON),
		"Helm values":    string(valuesJSON),
		"Helm manifests": manifests,
	} {
		for _, payload := range []string{"license-payload-marker", "langsmith-api-key-marker", "postgres-payload-marker", "redis-payload-marker"} {
			require.NotContains(t, output, payload, "%s must contain Secret references/metadata only", label)
			require.NotContains(t, output, base64.StdEncoding.EncodeToString([]byte(payload)), "%s must not contain base64-encoded Secret data", label)
		}
	}

	for _, secret := range secrets {
		if secret.Name == "las-postgres" {
			delete(secret.Data, "postgres_connection_url")
		}
	}
	diagnosticClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&aiv1.AIService{}).WithObjects(lasRotationObjects(ai, secrets)...).Build()
	reconciler = &LASReconciler{Client: diagnosticClient, Scheme: scheme}
	rotationErr := func() error {
		_, err := reconciler.credentialRevision(context.Background(), ai)
		return err
	}()
	require.Error(t, rotationErr)
	require.NotContains(t, rotationErr.Error(), "license-payload-marker")
	require.NotContains(t, rotationErr.Error(), "postgres-payload-marker")
	require.NotContains(t, rotationErr.Error(), "redis-payload-marker")
	require.Error(t, reconciler.fail(context.Background(), ai, "DependenciesUnavailable", rotationErr))
	updated := &aiv1.AIService{}
	require.NoError(t, diagnosticClient.Get(context.Background(), types.NamespacedName{Namespace: ai.Namespace, Name: ai.Name}, updated))
	require.Len(t, updated.Status.Conditions, 1)
	for _, payload := range []string{"license-payload-marker", "langsmith-api-key-marker", "postgres-payload-marker", "redis-payload-marker"} {
		require.NotContains(t, updated.Status.Conditions[0].Message, payload, "AIService diagnostics must not contain credential values")
	}
}

func TestLASRepeatedCredentialRevisionDoesNotRequireHelmUpgrade(t *testing.T) {
	ai, secrets := lasRotationFixture()
	scheme := buildAgentRuntimeTestScheme(t)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lasRotationObjects(ai, secrets)...).Build()
	reconciler := &LASReconciler{Client: kubeClient}

	revision, err := reconciler.credentialRevision(context.Background(), ai)
	require.NoError(t, err)
	desiredValues := lasValues(ai, revision)
	current := &release.Release{
		Config: desiredValues,
		Chart:  &chart.Chart{Metadata: &chart.Metadata{Version: lasChartVersion}},
		Info:   &release.Info{Status: release.StatusDeployed},
	}
	require.True(t, lasDesiredReleaseApplied(current, lasValues(ai, revision)), "the same rotation revision must preserve the Helm release")

	secrets[0].ResourceVersion = "2"
	rotatedClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lasRotationObjects(ai, secrets)...).Build()
	rotatedReconciler := &LASReconciler{Client: rotatedClient}
	rotatedRevision, err := rotatedReconciler.credentialRevision(context.Background(), ai)
	require.NoError(t, err)
	require.False(t, lasDesiredReleaseApplied(current, lasValues(ai, rotatedRevision)), "a changed Secret revision must request a Helm upgrade")
}

func TestLASCredentialRevisionChangesWhenSecretIsRecreatedWithSameName(t *testing.T) {
	ai, secrets := lasRotationFixture()
	scheme := buildAgentRuntimeTestScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lasRotationObjects(ai, secrets)...).Build()
	reconciler := &LASReconciler{Client: client}

	before, err := reconciler.credentialRevision(context.Background(), ai)
	require.NoError(t, err)
	secrets[1].UID = types.UID("replacement-postgres-uid")
	replacementClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lasRotationObjects(ai, secrets)...).Build()
	reconciler = &LASReconciler{Client: replacementClient}
	after, err := reconciler.credentialRevision(context.Background(), ai)
	require.NoError(t, err)
	require.NotEqual(t, before, after, "replacing a Secret under the same name must change the revision through its UID")
}

func TestLASCredentialRevisionRemainsStableAfterControllerRestart(t *testing.T) {
	helperHelm := &fakeLASHelmActions{current: testLASRelease("las-fixture", "service-uid", release.StatusDeployed, 1, time.Now())}
	reconciler, ai := testLASReconciler(t, helperHelm, true)
	ctx := context.Background()

	// Keep the restart fixture independent of HPA metrics; it exercises whether
	// an unchanged Secret revision causes a Helm upgrade after controller restart.
	ai.Spec.Feature.LAS.Queue = &aiv1.LASQueueSpec{Autoscaling: &aiv1.LASQueueAutoscalingSpec{Enabled: ptrBool(false)}}
	require.NoError(t, reconciler.Update(ctx, ai))
	revision, err := reconciler.credentialRevision(ctx, ai)
	require.NoError(t, err)
	helperHelm.current.Config = lasValues(ai, revision)

	replicas := int32(1)
	for _, suffix := range []string{"-langgraph-cloud-api-server", "-langgraph-cloud-queue"} {
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: lasReleaseName(ai.Name) + suffix, Namespace: ai.Namespace, Generation: 1},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{lasCredentialRevisionAnnotation: revision}}},
			},
			Status: appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1},
		}
		require.NoError(t, reconciler.Create(ctx, deployment))
	}
	endpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: agentRuntimeServiceName(ai.Name), Namespace: ai.Namespace},
		Subsets: []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{{IP: "10.0.0.10"}},
			Ports:     []corev1.EndpointPort{{Name: "http", Port: defaultAgentRuntimeTargetPort}},
		}},
	}
	require.NoError(t, reconciler.Create(ctx, endpoints))

	// Recreate the controller while retaining the fake Kubernetes state and
	// Helm release, then exercise the real reconcile decision path.
	restarted := &LASReconciler{
		Client: reconciler.Client, Scheme: reconciler.Scheme, Recorder: reconciler.Recorder,
		healthClient: reconciler.healthClient, helmActionsFactory: reconciler.helmActionsFactory,
	}
	require.NoError(t, restarted.Reconcile(ctx, ai))
	require.Zero(t, helperHelm.upgradeCalls, "unchanged Secret versions after restart must not trigger Helm upgrade")
	require.Equal(t, 1, helperHelm.current.Version)
}

func TestLASInvalidSecretDoesNotUpgradeExistingRelease(t *testing.T) {
	helm := &fakeLASHelmActions{current: testLASRelease("las-fixture", "service-uid", release.StatusDeployed, 1, time.Now())}
	reconciler, ai := testLASReconciler(t, helm, true)
	secret := &corev1.Secret{}
	require.NoError(t, reconciler.Get(context.Background(), types.NamespacedName{Namespace: ai.Namespace, Name: ai.Spec.Feature.PostgresSecretRef}, secret))
	delete(secret.Data, "postgres_connection_url")
	require.NoError(t, reconciler.Update(context.Background(), secret))

	require.Error(t, reconciler.Reconcile(context.Background(), ai))
	require.Equal(t, 1, helm.current.Version, "invalid credentials must not upgrade the existing release")
	require.Zero(t, helm.upgradeCalls)
	condition := meta.FindStatusCondition(ai.Status.Conditions, "Ready")
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, "DependenciesUnavailable", condition.Reason)
}

func lasRotationFixture() (*aiv1.AIService, []*corev1.Secret) {
	ai := &aiv1.AIService{ObjectMeta: metav1.ObjectMeta{Name: "rotation-test", Namespace: "default", UID: types.UID("rotation-test-uid")}}
	ai.Spec.Feature = aiv1.FeatureSpec{
		Name:              "agentruntime",
		LicenseSecretRef:  "las-auth",
		PostgresSecretRef: "las-postgres",
		RedisSecretRef:    "las-redis",
		LAS: &aiv1.LASFeatureSpec{
			Image:       aiv1.LASImageSpec{Repository: "registry.example/agent", Tag: "test"},
			AssistantID: "rotation_test",
		},
	}
	secrets := []*corev1.Secret{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "las-auth", Namespace: ai.Namespace, UID: types.UID("auth-uid"), ResourceVersion: "1"},
			Data:       map[string][]byte{"langgraph_cloud_license_key": []byte("license-payload-marker"), "api_key": []byte("langsmith-api-key-marker")},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "las-postgres", Namespace: ai.Namespace, UID: types.UID("postgres-uid"), ResourceVersion: "1"},
			Data:       map[string][]byte{"postgres_connection_url": []byte("postgres-payload-marker")},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "las-redis", Namespace: ai.Namespace, UID: types.UID("redis-uid"), ResourceVersion: "1"},
			Data:       map[string][]byte{"redis_connection_url": []byte("redis-payload-marker")},
		},
	}
	return ai, secrets
}

func lasRotationObjects(ai *aiv1.AIService, secrets []*corev1.Secret) []client.Object {
	objects := []client.Object{ai}
	for _, secret := range secrets {
		objects = append(objects, secret)
	}
	return objects
}

// mapValues converts rendered chart templates to a deterministic list for test assertions.
func mapValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
