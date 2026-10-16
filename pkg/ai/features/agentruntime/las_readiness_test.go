package agentruntime

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func healthyLASReconcileFixture(t *testing.T, queueReady int32, endpoints bool, hpaConditionStatus corev1.ConditionStatus) (*LASReconciler, *aiv1.AIService, *fakeLASHelmActions) {
	t.Helper()
	ctx := context.Background()
	helm := &fakeLASHelmActions{}
	r, ai := testLASReconciler(t, helm, true)
	revision, err := r.credentialRevision(ctx, ai)
	require.NoError(t, err)
	values := lasValues(ai, revision)
	releaseName := lasReleaseName(ai.Name)
	helm.current = testLASRelease(releaseName, string(ai.UID), release.StatusDeployed, 5, metav1.Now().Time)
	helm.current.Chart = &chart.Chart{Metadata: &chart.Metadata{Version: lasChartVersion}}
	helm.current.Config = values

	apiName := releaseName + "-langgraph-cloud-api-server"
	queueName := releaseName + "-langgraph-cloud-queue"
	apiReplicas, queueReplicas := int32(1), int32(3)
	for _, deployment := range []*appsv1.Deployment{
		{
			ObjectMeta: metav1.ObjectMeta{Name: apiName, Namespace: ai.Namespace, Generation: 1},
			Spec:       appsv1.DeploymentSpec{Replicas: &apiReplicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/component": apiName}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{lasCredentialRevisionAnnotation: revision}}}},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: queueName, Namespace: ai.Namespace, Generation: 1},
			Spec:       appsv1.DeploymentSpec{Replicas: &queueReplicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/component": queueName}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{lasCredentialRevisionAnnotation: revision}}}},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 3, ReadyReplicas: queueReady, AvailableReplicas: queueReady},
		},
	} {
		require.NoError(t, r.Create(ctx, deployment))
	}

	minReplicas, maxReplicas, cpuTarget := int32(1), int32(3), int32(80)
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: queueName, Namespace: ai.Namespace, Generation: 1, Labels: map[string]string{"app.kubernetes.io/instance": releaseName, "app.kubernetes.io/managed-by": "Helm"}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: queueName},
			MinReplicas:    &minReplicas,
			MaxReplicas:    maxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{
				Name:   corev1.ResourceCPU,
				Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: &cpuTarget},
			}}},
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{
			CurrentReplicas: 3,
			DesiredReplicas: 3,
			Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
				{Type: autoscalingv2.AbleToScale, Status: corev1.ConditionTrue, Reason: "ReadyForNewScale"},
				{Type: autoscalingv2.ScalingActive, Status: hpaConditionStatus, Reason: hpaMetricReason(hpaConditionStatus), Message: hpaMetricMessage(hpaConditionStatus)},
			},
		},
	}
	require.NoError(t, r.Create(ctx, hpa))

	if endpoints {
		require.NoError(t, r.Create(ctx, &corev1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{Name: agentRuntimeServiceName(ai.Name), Namespace: ai.Namespace},
			Subsets:    []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: "10.0.0.10"}}, Ports: []corev1.EndpointPort{{Name: "http", Port: defaultAgentRuntimeTargetPort}}}},
		}))
	}
	return r, ai, helm
}

func hpaMetricReason(status corev1.ConditionStatus) string {
	if status == corev1.ConditionTrue {
		return "ValidMetricFound"
	}
	return "FailedGetResourceMetric"
}

func hpaMetricMessage(status corev1.ConditionStatus) string {
	if status == corev1.ConditionTrue {
		return "queue CPU metric was observed"
	}
	return "unable to read queue CPU metric"
}

func TestLASReconcileKeepsReadyAtHPASelectedReplicaCountAndChecksAPIHealth(t *testing.T) {
	r, ai, helm := healthyLASReconcileFixture(t, 1, true, corev1.ConditionTrue)
	var requestedURL string
	r.healthClient = &http.Client{Transport: lasRoundTripper(func(request *http.Request) (*http.Response, error) {
		requestedURL = request.URL.String()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header), Request: request}, nil
	})}

	require.NoError(t, r.Reconcile(context.Background(), ai))
	ready := meta.FindStatusCondition(ai.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionTrue, ready.Status)
	scaling := meta.FindStatusCondition(ai.Status.Conditions, lasScalingReadyCondition)
	require.NotNil(t, scaling)
	require.Equal(t, metav1.ConditionUnknown, scaling.Status, "scaling is still converging to the HPA-selected replica count")
	require.Equal(t, "http://fixture-svc.default.svc:8080/ok", requestedURL)
	require.Zero(t, helm.upgradeCalls, "a healthy minimum worker pool must not trigger a Helm upgrade")
	require.Zero(t, helm.installCalls, "a stable release must not be reinstalled")
	require.Equal(t, 5, helm.current.Version, "a no-op reconcile must not create a new Helm revision")
	queue := &appsv1.Deployment{}
	queueName := lasReleaseName(ai.Name) + "-langgraph-cloud-queue"
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: ai.Namespace, Name: queueName}, queue))
	require.Equal(t, int32(3), *queue.Spec.Replicas, "the HPA-selected queue Deployment may have three desired replicas while one worker serves")
	require.Equal(t, int32(1), queue.Status.ReadyReplicas, "the minimum available worker still serves requests")
}

func TestLASReconcileSeparatesQueueMetricFailureFromServingReadiness(t *testing.T) {
	r, ai, _ := healthyLASReconcileFixture(t, 1, true, corev1.ConditionFalse)
	require.NoError(t, r.Reconcile(context.Background(), ai))
	ready := meta.FindStatusCondition(ai.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionTrue, ready.Status)
	scaling := meta.FindStatusCondition(ai.Status.Conditions, lasScalingReadyCondition)
	require.NotNil(t, scaling)
	require.Equal(t, metav1.ConditionFalse, scaling.Status)
	require.Equal(t, "HPAMetricsUnavailable", scaling.Reason)
}

func TestLASReconcileReportsScalingAndOwnedResourceDrift(t *testing.T) {
	t.Run("HPA configuration mismatch is scaling diagnostic", func(t *testing.T) {
		r, ai, _ := healthyLASReconcileFixture(t, 1, true, corev1.ConditionTrue)
		hpa := &autoscalingv2.HorizontalPodAutoscaler{}
		name := lasReleaseName(ai.Name) + "-langgraph-cloud-queue"
		require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: ai.Namespace, Name: name}, hpa))
		hpa.Spec.Metrics[0].Resource.Name = corev1.ResourceMemory
		require.NoError(t, r.Update(context.Background(), hpa))

		require.NoError(t, r.Reconcile(context.Background(), ai))
		require.Equal(t, metav1.ConditionTrue, meta.FindStatusCondition(ai.Status.Conditions, "Ready").Status)
		scaling := meta.FindStatusCondition(ai.Status.Conditions, lasScalingReadyCondition)
		require.Equal(t, metav1.ConditionFalse, scaling.Status)
		require.Equal(t, "HPAConfigurationMismatch", scaling.Reason)
	})

	t.Run("foreign compatibility Service is rejected", func(t *testing.T) {
		r, ai, _ := healthyLASReconcileFixture(t, 1, true, corev1.ConditionTrue)
		service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: agentRuntimeServiceName(ai.Name), Namespace: ai.Namespace}}
		require.NoError(t, r.Create(context.Background(), service))

		require.ErrorContains(t, r.Reconcile(context.Background(), ai), "is not controlled by this AIService")
		ready := meta.FindStatusCondition(ai.Status.Conditions, "Ready")
		require.Equal(t, metav1.ConditionFalse, ready.Status)
		require.Equal(t, "ServiceFailed", ready.Reason)
	})

	t.Run("missing API Deployment reports useful Ready failure", func(t *testing.T) {
		r, ai, helm := healthyLASReconcileFixture(t, 1, true, corev1.ConditionTrue)
		name := lasReleaseName(ai.Name) + "-langgraph-cloud-api-server"
		deployment := &appsv1.Deployment{}
		require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: ai.Namespace, Name: name}, deployment))
		require.NoError(t, r.Delete(context.Background(), deployment))

		require.ErrorContains(t, r.Reconcile(context.Background(), ai), "LAS Deployment "+name+" unavailable")
		require.Equal(t, 1, helm.upgradeCalls, "missing owned workload triggers a Helm repair")
		ready := meta.FindStatusCondition(ai.Status.Conditions, "Ready")
		require.Equal(t, metav1.ConditionFalse, ready.Status)
		require.Equal(t, "WorkloadsNotReady", ready.Reason)
	})
}

func TestLASReconcileRequiresAPIHealthAndServiceEndpoints(t *testing.T) {
	t.Run("API health", func(t *testing.T) {
		r, ai, _ := healthyLASReconcileFixture(t, 1, true, corev1.ConditionTrue)
		r.healthClient = lasHealthClient(http.StatusServiceUnavailable)
		require.ErrorContains(t, r.Reconcile(context.Background(), ai), "returned HTTP 503")
		require.Equal(t, "APIHealthCheckFailed", meta.FindStatusCondition(ai.Status.Conditions, "Ready").Reason)
	})

	t.Run("missing Service endpoints", func(t *testing.T) {
		r, ai, _ := healthyLASReconcileFixture(t, 1, false, corev1.ConditionTrue)
		require.ErrorContains(t, r.Reconcile(context.Background(), ai), "endpoints fixture-svc unavailable")
		require.Equal(t, "ServiceEndpointsNotReady", meta.FindStatusCondition(ai.Status.Conditions, "Ready").Reason)
	})

	t.Run("no ready Service endpoint", func(t *testing.T) {
		r, ai, _ := healthyLASReconcileFixture(t, 1, true, corev1.ConditionTrue)
		endpoints := &corev1.Endpoints{}
		require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: ai.Namespace, Name: agentRuntimeServiceName(ai.Name)}, endpoints))
		endpoints.Subsets = nil
		require.NoError(t, r.Update(context.Background(), endpoints))
		require.ErrorContains(t, r.Reconcile(context.Background(), ai), "has no ready API endpoint")
		require.Equal(t, "ServiceEndpointsNotReady", meta.FindStatusCondition(ai.Status.Conditions, "Ready").Reason)
	})
}

func TestLASReconcileRequiresQueueMinimumAvailability(t *testing.T) {
	r, ai, _ := healthyLASReconcileFixture(t, 0, true, corev1.ConditionTrue)
	require.ErrorContains(t, r.Reconcile(context.Background(), ai), "minimum required is 1")
	ready := meta.FindStatusCondition(ai.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionFalse, ready.Status)
}

func TestLASReconcileDisabledAutoscalingUsesOneFixedWorker(t *testing.T) {
	r, ai, helm := healthyLASReconcileFixture(t, 1, true, corev1.ConditionTrue)
	disabled := false
	ai.Spec.Feature.LAS.Queue = &aiv1.LASQueueSpec{Autoscaling: &aiv1.LASQueueAutoscalingSpec{Enabled: &disabled}}
	revision, err := r.credentialRevision(context.Background(), ai)
	require.NoError(t, err)
	helm.current.Config = lasValues(ai, revision)
	queueName := lasReleaseName(ai.Name) + "-langgraph-cloud-queue"
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: ai.Namespace, Name: queueName}, hpa))
	require.NoError(t, r.Delete(context.Background(), hpa))
	queue := &appsv1.Deployment{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: ai.Namespace, Name: queueName}, queue))
	one := int32(1)
	queue.Spec.Replicas = &one
	require.NoError(t, r.Update(context.Background(), queue))
	queue.Status.ObservedGeneration = queue.Generation
	queue.Status.ReadyReplicas = 1
	require.NoError(t, r.Status().Update(context.Background(), queue))

	require.NoError(t, r.Reconcile(context.Background(), ai))
	require.Zero(t, helm.upgradeCalls)
	require.Equal(t, "AutoscalingDisabled", meta.FindStatusCondition(ai.Status.Conditions, lasScalingReadyCondition).Reason)
}

func TestLASReconcileReadinessRecoversAfterUnusableEndpoint(t *testing.T) {
	r, ai, _ := healthyLASReconcileFixture(t, 1, true, corev1.ConditionTrue)
	ctx := context.Background()
	name := agentRuntimeServiceName(ai.Name)
	endpoints := &corev1.Endpoints{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: ai.Namespace, Name: name}, endpoints))
	endpoints.Subsets[0].Ports[0].Port = defaultAgentRuntimeTargetPort + 1
	require.NoError(t, r.Update(ctx, endpoints))

	require.ErrorContains(t, r.Reconcile(ctx, ai), "has no ready API endpoint")
	ready := meta.FindStatusCondition(ai.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionFalse, ready.Status)
	require.Equal(t, "ServiceEndpointsNotReady", ready.Reason)

	endpoints.Subsets[0].Ports[0].Port = defaultAgentRuntimeTargetPort
	require.NoError(t, r.Update(ctx, endpoints))
	require.NoError(t, r.Reconcile(ctx, ai))
	ready = meta.FindStatusCondition(ai.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionTrue, ready.Status)
	require.Equal(t, "LASReady", ready.Reason)
}

func TestLASReconcileReadinessRecoversAfterAPIHealthFailure(t *testing.T) {
	r, ai, _ := healthyLASReconcileFixture(t, 1, true, corev1.ConditionTrue)
	r.healthClient = lasHealthClient(http.StatusServiceUnavailable)
	ctx := context.Background()

	require.ErrorContains(t, r.Reconcile(ctx, ai), "returned HTTP 503")
	ready := meta.FindStatusCondition(ai.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionFalse, ready.Status)
	require.Equal(t, "APIHealthCheckFailed", ready.Reason)

	r.healthClient = lasHealthClient(http.StatusOK)
	require.NoError(t, r.Reconcile(ctx, ai))
	ready = meta.FindStatusCondition(ai.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionTrue, ready.Status)
	require.Equal(t, "LASReady", ready.Reason)
}
