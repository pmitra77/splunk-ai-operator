package controller

import (
	"testing"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAIServiceReadyChangesWakeOnlyLASParentAggregation(t *testing.T) {
	oldService := &aiv1.AIService{Spec: aiv1.AIServiceSpec{Feature: aiv1.FeatureSpec{Name: "agentruntime", LAS: &aiv1.LASFeatureSpec{}}}, Status: aiv1.AIServiceStatus{Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Installing"}}}}
	newService := oldService.DeepCopy()
	newService.Status.Conditions[0] = metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "LASReady"}
	if !aiServiceReadyChanged(oldService, newService) {
		t.Fatal("LAS Ready transition must enqueue its parent AIPlatform")
	}

	newService = oldService.DeepCopy()
	newService.Status.Conditions = append(newService.Status.Conditions, metav1.Condition{Type: "ScalingReady", Status: metav1.ConditionFalse, Reason: "HPAMetricsUnavailable"})
	if aiServiceReadyChanged(oldService, newService) {
		t.Fatal("queue scaling diagnostics must not change serving-readiness aggregation")
	}

	oldService.Spec.Feature.LAS = nil
	newService.Spec.Feature.LAS = nil
	if aiServiceReadyChanged(oldService, newService) {
		t.Fatal("non-LAS child readiness must not wake the LAS aggregation watch")
	}
}
