package agentruntime

import (
	"bytes"
	"strings"
	"testing"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
)

// A CPU-only platform must not need a GPU node, so nothing LAS renders may
// request, tolerate or select GPU capacity.
func TestLASChartRendersNoGPURequirements(t *testing.T) {
	chart, err := loader.LoadArchive(bytes.NewReader(lasChartArchive))
	if err != nil {
		t.Fatal(err)
	}
	ai := &aiv1.AIService{}
	ai.Name = "launchpad-agentruntime-mltk"
	ai.Spec.Feature = aiv1.FeatureSpec{Name: "agentruntime", Provider: "mltk", LAS: &aiv1.LASFeatureSpec{
		Image:       aiv1.LASImageSpec{Repository: "registry.example/las", Tag: "v1"},
		AssistantID: "echo",
		NetworkMode: "airgapped",
	}}
	renderValues, err := chartutil.ToRenderValues(chart, lasValues(ai), chartutil.ReleaseOptions{Name: lasReleaseName(ai.Name), Namespace: "default"}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(chart, renderValues)
	if err != nil {
		t.Fatal(err)
	}

	workloads := 0
	for name, manifest := range rendered {
		if strings.TrimSpace(manifest) == "" {
			continue
		}
		if strings.HasSuffix(name, "api-server/deployment.yaml") || strings.HasSuffix(name, "queue/deployment.yaml") {
			workloads++
		}
		lower := strings.ToLower(manifest)
		for _, forbidden := range []string{"nvidia", "gpu"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("%s mentions %q; a CPU-only LAS must not request GPU resources or scheduling", name, forbidden)
			}
		}
	}
	if workloads != 2 {
		t.Fatalf("rendered %d LAS workload Deployments, want API and queue", workloads)
	}
}
