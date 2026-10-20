package v1

import (
	"testing"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func boolPtrLAS(v bool) *bool { return &v }

func fixtureLASForWebhookTest() *aiv1.LASFeatureSpec {
	return &aiv1.LASFeatureSpec{
		Image:       aiv1.LASImageSpec{Repository: "registry.example/fixture", Tag: "candidate-123"},
		AssistantID: "contract_fixture",
		NetworkMode: "connected",
		Queue: &aiv1.LASQueueSpec{
			LASWorkloadSpec: aiv1.LASWorkloadSpec{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}},
			Autoscaling:     &aiv1.LASQueueAutoscalingSpec{Enabled: boolPtrLAS(true)},
		},
	}
}

func TestLASFeatureConfigValidation(t *testing.T) {
	path := field.NewPath("spec", "features").Index(0).Child("las")
	if errs := validateLASFeature(nil, path); len(errs) != 1 || errs[0].Field != "spec.features[0].las" {
		t.Fatalf("missing LAS config errors: %v", errs)
	}
	if errs := validateLASFeature(fixtureLASForWebhookTest(), path); len(errs) != 0 {
		t.Fatalf("valid LAS config errors: %v", errs)
	}
	bad := &aiv1.LASFeatureSpec{}
	if errs := validateLASFeature(bad, path); len(errs) != 3 {
		t.Fatalf("incomplete LAS config errors: %v", errs)
	}
}

func TestLASFeatureAcceptsAIP5391ConfigurationWithoutNewFields(t *testing.T) {
	base := &aiv1.LASFeatureSpec{
		Image:       aiv1.LASImageSpec{Repository: "registry.example/fixture", Tag: "candidate-123"},
		AssistantID: "contract_fixture",
		API:         aiv1.LASWorkloadSpec{},
	}
	for _, tt := range []struct {
		name  string
		queue *aiv1.LASQueueSpec
	}{
		{name: "queue omitted", queue: nil},
		{name: "queue without autoscaling", queue: &aiv1.LASQueueSpec{LASWorkloadSpec: aiv1.LASWorkloadSpec{}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			las := base.DeepCopy()
			las.Queue = tt.queue
			if errs := validateLASFeature(las, field.NewPath("spec", "features").Index(0).Child("las")); len(errs) != 0 {
				t.Fatalf("AIP-5391 LAS configuration without AIP-5392 fields must remain valid: %v", errs)
			}
		})
	}
}

func TestLASFeatureRejectsInvalidRuntimeSettings(t *testing.T) {
	tests := []struct {
		name string
		edit func(*aiv1.LASFeatureSpec)
		want string
	}{
		{"unsupported network mode", func(las *aiv1.LASFeatureSpec) { las.NetworkMode = "offline" }, "networkMode"},
		{"tracing requires an endpoint", func(las *aiv1.LASFeatureSpec) { las.Tracing = &aiv1.LASTracingSpec{Enabled: true} }, "tracing.endpoint"},
		{"tracing endpoint must be absolute", func(las *aiv1.LASFeatureSpec) {
			las.Tracing = &aiv1.LASTracingSpec{Enabled: true, Endpoint: "langsmith.internal"}
		}, "tracing.endpoint"},
		{"minimum replicas are positive", func(las *aiv1.LASFeatureSpec) { v := int32(0); las.Queue.Autoscaling.MinReplicas = &v }, "minReplicas"},
		{"maximum replicas cannot be below minimum", func(las *aiv1.LASFeatureSpec) {
			min, max := int32(4), int32(3)
			las.Queue.Autoscaling.MinReplicas, las.Queue.Autoscaling.MaxReplicas = &min, &max
		}, "maxReplicas"},
		{"CPU target is bounded", func(las *aiv1.LASFeatureSpec) {
			v := int32(101)
			las.Queue.Autoscaling.TargetCPUUtilizationPercentage = &v
		}, "targetCPUUtilizationPercentage"},
		{"CPU request required for HPA", func(las *aiv1.LASFeatureSpec) { las.Queue.Resources.Requests = nil }, "resources.requests.cpu"},
		{"CPU request must be positive", func(las *aiv1.LASFeatureSpec) {
			las.Queue.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("0")
		}, "resources.requests.cpu"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			las := fixtureLASForWebhookTest()
			tt.edit(las)
			errs := validateLASFeature(las, field.NewPath("spec", "features").Index(0).Child("las"))
			for _, err := range errs {
				if err.Field == "" || !containsString(err.Field, tt.want) {
					continue
				}
				return
			}
			t.Fatalf("expected validation error containing field %q, got %v", tt.want, errs)
		})
	}
}

func TestLASFeatureTracingFollowsNetworkMode(t *testing.T) {
	path := field.NewPath("spec", "features").Index(0).Child("las")
	for _, tt := range []struct {
		name        string
		networkMode string
		tracing     *aiv1.LASTracingSpec
		wantReject  bool
	}{
		{name: "airgapped tracing omitted", networkMode: "airgapped"},
		{name: "airgapped tracing disabled", networkMode: "airgapped", tracing: &aiv1.LASTracingSpec{Enabled: false}},
		{name: "airgapped disabled endpoint ignored", networkMode: "airgapped", tracing: &aiv1.LASTracingSpec{Enabled: false, Endpoint: "https://unused.internal"}},
		{name: "airgapped public tracing rejected", networkMode: "airgapped", tracing: &aiv1.LASTracingSpec{Enabled: true, Endpoint: "https://api.smith.langchain.com"}, wantReject: true},
		{name: "airgapped internal tracing rejected", networkMode: "airgapped", tracing: &aiv1.LASTracingSpec{Enabled: true, Endpoint: "https://langsmith.internal"}, wantReject: true},
		{name: "connected tracing allowed", networkMode: "connected", tracing: &aiv1.LASTracingSpec{Enabled: true, Endpoint: "https://langsmith.internal"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			las := fixtureLASForWebhookTest()
			las.NetworkMode = tt.networkMode
			las.Tracing = tt.tracing
			errs := validateLASFeature(las, path)
			if !tt.wantReject {
				if len(errs) != 0 {
					t.Fatalf("unexpected validation errors: %v", errs)
				}
				return
			}
			if len(errs) != 1 || errs[0].Field != "spec.features[0].las.tracing.enabled" || errs[0].Type != field.ErrorTypeForbidden {
				t.Fatalf("airgapped tracing errors = %v, want one forbidden tracing.enabled error", errs)
			}
		})
	}
}

func TestLASFeatureAllowsQueueWithoutCPURequestWhenAutoscalingDisabled(t *testing.T) {
	las := fixtureLASForWebhookTest()
	las.Queue.Autoscaling.Enabled = boolPtrLAS(false)
	las.Queue.Resources.Requests = nil
	if errs := validateLASFeature(las, field.NewPath("spec", "features").Index(0).Child("las")); len(errs) != 0 {
		t.Fatalf("disabled queue autoscaling should not require CPU requests: %v", errs)
	}
}

func containsString(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
