package agentruntime

import (
	"context"
	"fmt"
	"net/http"
	"time"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	lasScalingReadyCondition       = "ScalingReady"
	defaultAgentRuntimeTargetPort  = int32(8000)
	defaultAgentRuntimeHealthLimit = 5 * time.Second
)

func (r *LASReconciler) checkStableServiceEndpoints(ctx context.Context, ai *aiv1.AIService, releaseName string) (*corev1.Service, error) {
	service := &corev1.Service{}
	serviceKey := client.ObjectKey{Namespace: ai.Namespace, Name: agentRuntimeServiceName(ai.Name)}
	if err := r.Get(ctx, serviceKey, service); err != nil {
		return nil, fmt.Errorf("LAS compatibility Service %s unavailable: %w", serviceKey.Name, err)
	}
	if !metav1.IsControlledBy(service, ai) {
		return nil, fmt.Errorf("LAS compatibility Service %s is not controlled by this AIService", service.Name)
	}
	expectedSelector := map[string]string{
		"app.kubernetes.io/name":      "langgraph-cloud",
		"app.kubernetes.io/instance":  releaseName,
		"app.kubernetes.io/component": releaseName + "-langgraph-cloud-api-server",
	}
	if service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.Ports == nil || len(service.Spec.Ports) != 1 ||
		service.Spec.Ports[0].Port != defaultAgentRuntimeHTTPPort || service.Spec.Ports[0].TargetPort.IntVal != defaultAgentRuntimeTargetPort ||
		!sameStringMap(service.Spec.Selector, expectedSelector) {
		return nil, fmt.Errorf("LAS compatibility Service %s does not match the expected ClusterIP API endpoint", service.Name)
	}

	endpoints := &corev1.Endpoints{}
	if err := r.Get(ctx, serviceKey, endpoints); err != nil {
		return nil, fmt.Errorf("LAS Service endpoints %s unavailable: %w", service.Name, err)
	}
	for _, subset := range endpoints.Subsets {
		readyAddresses := len(subset.Addresses) > 0
		hasAPIEndpointPort := false
		for _, port := range subset.Ports {
			if port.Name == "http" && port.Port == defaultAgentRuntimeTargetPort {
				hasAPIEndpointPort = true
				break
			}
		}
		if readyAddresses && hasAPIEndpointPort {
			return service, nil
		}
	}
	return nil, fmt.Errorf("LAS Service %s has no ready API endpoint on port %d", service.Name, defaultAgentRuntimeTargetPort)
}

func sameStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range right {
		if left[key] != value {
			return false
		}
	}
	return true
}

func (r *LASReconciler) checkAPIHealth(ctx context.Context, ai *aiv1.AIService, service *corev1.Service) error {
	url := fmt.Sprintf("http://%s.%s.svc:%d/ok", service.Name, ai.Namespace, defaultAgentRuntimeHTTPPort)
	requestCtx, cancel := context.WithTimeout(ctx, defaultAgentRuntimeHealthLimit)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build LAS API health request: %w", err)
	}
	client := r.healthClient
	if client == nil {
		client = &http.Client{Timeout: defaultAgentRuntimeHealthLimit}
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("LAS API health check GET /ok failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("LAS API health check GET /ok returned HTTP %d, want 200", response.StatusCode)
	}
	return nil
}

func lasQueueAutoscalingEnabled(ai *aiv1.AIService) bool {
	var autoscaling *aiv1.LASQueueAutoscalingSpec
	if ai.Spec.Feature.LAS != nil && ai.Spec.Feature.LAS.Queue != nil {
		autoscaling = ai.Spec.Feature.LAS.Queue.Autoscaling
	}
	return lasQueueAutoscalingValues(autoscaling)["enabled"].(bool)
}

func lasQueueMinReplicas(ai *aiv1.AIService) int32 {
	if !lasQueueAutoscalingEnabled(ai) {
		return 1
	}
	var autoscaling *aiv1.LASQueueAutoscalingSpec
	if ai.Spec.Feature.LAS != nil && ai.Spec.Feature.LAS.Queue != nil {
		autoscaling = ai.Spec.Feature.LAS.Queue.Autoscaling
	}
	return lasQueueAutoscalingValues(autoscaling)["minReplicas"].(int32)
}

func lasQueueMaxReplicas(ai *aiv1.AIService) int32 {
	var autoscaling *aiv1.LASQueueAutoscalingSpec
	if ai.Spec.Feature.LAS != nil && ai.Spec.Feature.LAS.Queue != nil {
		autoscaling = ai.Spec.Feature.LAS.Queue.Autoscaling
	}
	return lasQueueAutoscalingValues(autoscaling)["maxReplicas"].(int32)
}

func lasQueueTargetCPU(ai *aiv1.AIService) int32 {
	var autoscaling *aiv1.LASQueueAutoscalingSpec
	if ai.Spec.Feature.LAS != nil && ai.Spec.Feature.LAS.Queue != nil {
		autoscaling = ai.Spec.Feature.LAS.Queue.Autoscaling
	}
	return lasQueueAutoscalingValues(autoscaling)["targetCPUUtilizationPercentage"].(int32)
}

func (r *LASReconciler) queueScalingCondition(ctx context.Context, ai *aiv1.AIService, releaseName string) metav1.Condition {
	condition := func(status metav1.ConditionStatus, reason, message string) metav1.Condition {
		return metav1.Condition{Type: lasScalingReadyCondition, Status: status, Reason: reason, Message: message, ObservedGeneration: ai.Generation}
	}
	hpaName := releaseName + "-langgraph-cloud-queue"
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ai.Namespace, Name: hpaName}, hpa)
	if !lasQueueAutoscalingEnabled(ai) {
		if err == nil {
			return condition(metav1.ConditionFalse, "UnexpectedHPA", fmt.Sprintf("queue HPA %s remains after autoscaling was disabled", hpaName))
		}
		if !apierrors.IsNotFound(err) {
			return condition(metav1.ConditionFalse, "HPAObservationFailed", fmt.Sprintf("observe disabled queue HPA %s: %v", hpaName, err))
		}
		deployment := &appsv1.Deployment{}
		deploymentName := hpaName
		if getErr := r.Get(ctx, client.ObjectKey{Namespace: ai.Namespace, Name: deploymentName}, deployment); getErr != nil {
			return condition(metav1.ConditionUnknown, "QueueTopologyPending", fmt.Sprintf("waiting for fixed queue Deployment %s: %v", deploymentName, getErr))
		}
		if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
			return condition(metav1.ConditionFalse, "QueueReplicaOwnershipMismatch", fmt.Sprintf("autoscaling is disabled but queue Deployment %s does not request exactly one worker", deploymentName))
		}
		if deployment.Status.ObservedGeneration < deployment.Generation || deployment.Status.ReadyReplicas < 1 {
			return condition(metav1.ConditionUnknown, "QueueTopologyPending", fmt.Sprintf("fixed one-worker queue Deployment %s is converging", deploymentName))
		}
		return condition(metav1.ConditionTrue, "AutoscalingDisabled", "queue CPU autoscaling is disabled; the fixed one-worker queue is ready")
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			return condition(metav1.ConditionFalse, "HpaMissing", fmt.Sprintf("queue HPA %s is missing while autoscaling is enabled", hpaName))
		}
		return condition(metav1.ConditionFalse, "HPAObservationFailed", fmt.Sprintf("observe queue HPA %s: %v", hpaName, err))
	}

	if hpa.Labels["app.kubernetes.io/instance"] != releaseName || hpa.Labels["app.kubernetes.io/managed-by"] != "Helm" {
		return condition(metav1.ConditionFalse, "HPAOwnershipMismatch", fmt.Sprintf("queue HPA %s is not labeled as part of Helm release %s", hpaName, releaseName))
	}
	minReplicas, maxReplicas, targetCPU := lasQueueMinReplicas(ai), lasQueueMaxReplicas(ai), lasQueueTargetCPU(ai)
	if hpa.Spec.ScaleTargetRef.APIVersion != "apps/v1" || hpa.Spec.ScaleTargetRef.Kind != "Deployment" || hpa.Spec.ScaleTargetRef.Name != hpaName ||
		hpa.Spec.MinReplicas == nil || *hpa.Spec.MinReplicas != minReplicas || hpa.Spec.MaxReplicas != maxReplicas ||
		len(hpa.Spec.Metrics) != 1 || hpa.Spec.Metrics[0].Type != autoscalingv2.ResourceMetricSourceType || hpa.Spec.Metrics[0].Resource == nil ||
		hpa.Spec.Metrics[0].Resource.Name != corev1.ResourceCPU || hpa.Spec.Metrics[0].Resource.Target.Type != autoscalingv2.UtilizationMetricType ||
		hpa.Spec.Metrics[0].Resource.Target.AverageUtilization == nil || *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization != targetCPU {
		return condition(metav1.ConditionFalse, "HPAConfigurationMismatch", fmt.Sprintf("queue HPA %s does not match the configured CPU-only target and replica bounds", hpaName))
	}

	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ai.Namespace, Name: hpaName}, deployment); err != nil {
		return condition(metav1.ConditionFalse, "QueueDeploymentUnavailable", fmt.Sprintf("observe HPA target Deployment %s: %v", hpaName, err))
	}
	if reason, message := queueCapacityFailure(ctx, r.Client, deployment); reason != "" {
		return condition(metav1.ConditionFalse, reason, message)
	}
	if deployment.Status.ObservedGeneration < deployment.Generation || hpa.Status.DesiredReplicas == 0 {
		return condition(metav1.ConditionUnknown, "ScalingObservationPending", fmt.Sprintf("queue HPA %s is waiting for current-generation scaling metrics", hpaName))
	}
	for _, hpaCondition := range hpa.Status.Conditions {
		if hpaCondition.Type == autoscalingv2.AbleToScale && hpaCondition.Status == corev1.ConditionFalse {
			return condition(metav1.ConditionFalse, "HPAUnableToScale", hpaCondition.Message)
		}
		if hpaCondition.Type == autoscalingv2.ScalingActive && hpaCondition.Status == corev1.ConditionFalse {
			message := hpaCondition.Message
			if message == "" {
				message = hpaCondition.Reason
			}
			return condition(metav1.ConditionFalse, "HPAMetricsUnavailable", message)
		}
	}
	if !hpaConditionIsTrue(hpa.Status.Conditions, autoscalingv2.AbleToScale) || !hpaConditionIsTrue(hpa.Status.Conditions, autoscalingv2.ScalingActive) {
		return condition(metav1.ConditionUnknown, "ScalingObservationPending", fmt.Sprintf("queue HPA %s has not reported usable scaling metrics", hpaName))
	}
	if hpa.Status.DesiredReplicas > int32(deployment.Status.ReadyReplicas) {
		return condition(metav1.ConditionUnknown, "ScalingTransition", fmt.Sprintf("queue HPA requests %d workers; %d are ready (minimum %d)", hpa.Status.DesiredReplicas, deployment.Status.ReadyReplicas, minReplicas))
	}
	for _, hpaCondition := range hpa.Status.Conditions {
		if hpaCondition.Type == autoscalingv2.ScalingLimited && hpaCondition.Status == corev1.ConditionTrue && hpaCondition.Reason == "TooManyReplicas" {
			return condition(metav1.ConditionTrue, "MaximumReplicasReached", fmt.Sprintf("queue HPA is healthy at its configured maximum of %d workers", maxReplicas))
		}
	}
	return condition(metav1.ConditionTrue, "QueueAutoscalingReady", fmt.Sprintf("CPU queue HPA is active; %d to %d workers target %d%% average CPU", minReplicas, maxReplicas, targetCPU))
}

func hpaConditionIsTrue(conditions []autoscalingv2.HorizontalPodAutoscalerCondition, conditionType autoscalingv2.HorizontalPodAutoscalerConditionType) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func queueCapacityFailure(ctx context.Context, c client.Client, deployment *appsv1.Deployment) (string, string) {
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue {
			return "QueueCapacityUnavailable", fmt.Sprintf("queue Deployment %s cannot create workers: %s", deployment.Name, condition.Message)
		}
		if condition.Type == appsv1.DeploymentProgressing && condition.Status == corev1.ConditionFalse && condition.Reason == "ProgressDeadlineExceeded" {
			return "QueueCapacityUnavailable", fmt.Sprintf("queue Deployment %s failed to make scaling progress: %s", deployment.Name, condition.Message)
		}
	}
	selector, err := metav1.LabelSelectorAsMap(deployment.Spec.Selector)
	if err != nil || len(selector) == 0 {
		return "", ""
	}
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(deployment.Namespace), client.MatchingLabels(selector)); err != nil {
		return "", ""
	}
	for _, pod := range pods.Items {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && condition.Reason == "Unschedulable" {
				return "QueueCapacityUnavailable", fmt.Sprintf("queue Pod %s cannot be scheduled: %s", pod.Name, condition.Message)
			}
		}
	}
	return "", ""
}

func (r *LASReconciler) setScalingCondition(ctx context.Context, ai *aiv1.AIService, condition metav1.Condition) error {
	before := ai.DeepCopy()
	keepLASConditions(ai)
	meta.SetStatusCondition(&ai.Status.Conditions, condition)
	ai.Status.ObservedGeneration = ai.Generation
	return r.Status().Patch(ctx, ai, client.MergeFrom(before))
}

func keepLASConditions(ai *aiv1.AIService) {
	conditions := make([]metav1.Condition, 0, 2)
	for _, condition := range ai.Status.Conditions {
		if condition.Type == "Ready" || condition.Type == lasScalingReadyCondition {
			conditions = append(conditions, condition)
		}
	}
	ai.Status.Conditions = conditions
}
