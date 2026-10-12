package ai_platform

import (
	"context"
	"testing"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func boolPtrForLAS(v bool) *bool { return &v }

func TestBuildAIServicePropagatesLASImageAndAssistant(t *testing.T) {
	platform := &aiv1.AIPlatform{ObjectMeta: metav1.ObjectMeta{Name: "platform", Namespace: "default"}}
	feature := aiv1.FeatureSpec{
		Name:              "agentruntime",
		Provider:          "mltk",
		LicenseSecretRef:  "offline-license",
		PostgresSecretRef: "fixture-postgres",
		RedisSecretRef:    "fixture-redis",
		LAS: &aiv1.LASFeatureSpec{
			Image:       aiv1.LASImageSpec{Repository: "registry.example/fixture", Tag: "candidate-1"},
			AssistantID: "contract_fixture",
			NetworkMode: "airgapped",
			Tracing:     &aiv1.LASTracingSpec{Enabled: false},
			Queue: &aiv1.LASQueueSpec{
				LASWorkloadSpec: aiv1.LASWorkloadSpec{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}},
				Autoscaling:     &aiv1.LASQueueAutoscalingSpec{Enabled: boolPtrForLAS(true)},
			},
		},
	}
	r := &AIPlatformReconciler{}
	first := r.buildAIService(context.Background(), platform, feature, "platform-agentruntime-mltk")
	if first.Spec.Feature.LAS == nil || first.Spec.Feature.LAS.Image != feature.LAS.Image || first.Spec.Feature.LAS.AssistantID != "contract_fixture" {
		t.Fatalf("LAS selection was not propagated to AIService: %#v", first.Spec.Feature.LAS)
	}
	if first.Spec.Feature.LAS.NetworkMode != feature.LAS.NetworkMode || first.Spec.Feature.LAS.Tracing == nil || first.Spec.Feature.LAS.Tracing.Enabled || first.Spec.Feature.LAS.Queue == nil || first.Spec.Feature.LAS.Queue.Autoscaling.Enabled == nil || !*first.Spec.Feature.LAS.Queue.Autoscaling.Enabled {
		t.Fatalf("LAS network, tracing, or queue settings were not propagated: %#v", first.Spec.Feature.LAS)
	}
	if got := first.Spec.Feature.LAS.Queue.Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Fatalf("queue CPU request was not propagated: %s", got.String())
	}
	if first.Spec.Feature.LicenseSecretRef != "offline-license" || first.Spec.Feature.PostgresSecretRef != "fixture-postgres" || first.Spec.Feature.RedisSecretRef != "fixture-redis" {
		t.Fatalf("LAS deployment Secret references were not propagated: %#v", first.Spec.Feature)
	}
	feature.LAS.Image.Tag = "candidate-2"
	updated := r.buildAIService(context.Background(), platform, feature, "platform-agentruntime-mltk")
	if updated.Spec.Feature.LAS.Image.Tag != "candidate-2" {
		t.Fatalf("changed image tag was not propagated: %#v", updated.Spec.Feature.LAS)
	}
}
