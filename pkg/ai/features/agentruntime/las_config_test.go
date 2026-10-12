package agentruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

func TestLASRolloutPolicyReachesBothDeployments(t *testing.T) {
	ai := &aiv1.AIService{}
	ai.Name = "fixture"
	ai.Spec.Feature = aiv1.FeatureSpec{
		LicenseSecretRef:  "offline-license",
		PostgresSecretRef: "fixture-postgres",
		RedisSecretRef:    "fixture-redis",
		LAS: &aiv1.LASFeatureSpec{
			Image:       aiv1.LASImageSpec{Repository: "registry.example/fixture", Tag: "candidate-123"},
			AssistantID: "contract_fixture",
		},
	}

	values := lasValues(ai)
	apiValues := values["apiServer"].(map[string]interface{})
	queueValues := values["queue"].(map[string]interface{})
	if apiValues["autoscaling"].(map[string]interface{})["enabled"] != false || apiValues["deployment"].(map[string]interface{})["replicaCount"] != 1 {
		t.Fatal("API is not fixed at one replica")
	}
	apiStrategy := apiValues["deployment"].(map[string]interface{})["strategy"].(map[string]interface{})
	queueStrategy := queueValues["deployment"].(map[string]interface{})["strategy"].(map[string]interface{})
	if apiStrategy["type"] != "RollingUpdate" || queueStrategy["type"] != "RollingUpdate" {
		t.Fatal("both workloads must request RollingUpdate")
	}
	for name, strategy := range map[string]map[string]interface{}{"API": apiStrategy, "queue": queueStrategy} {
		rolling := strategy["rollingUpdate"].(map[string]interface{})
		if rolling["maxSurge"] != 0 || rolling["maxUnavailable"] != 1 {
			t.Errorf("%s requested rollout policy = %#v", name, rolling)
		}
	}
	oldValues := lasValues(ai)
	delete(oldValues["apiServer"].(map[string]interface{})["deployment"].(map[string]interface{}), "strategy")
	delete(oldValues["queue"].(map[string]interface{})["deployment"].(map[string]interface{}), "strategy")
	if sameValues(oldValues, values) || !sameValues(values, lasValues(ai)) {
		t.Fatal("the rollout value change must cause one upgrade, then compare equal")
	}

	digest := sha256.Sum256(lasChartArchive)
	if hex.EncodeToString(digest[:]) != lasChartSHA256 {
		t.Fatal("embedded LAS chart digest differs from pinned identity")
	}
	chart, err := loader.LoadArchive(bytes.NewReader(lasChartArchive))
	if err != nil {
		t.Fatal(err)
	}
	if chart.Metadata.Version != lasChartVersion {
		t.Fatalf("embedded LAS chart version = %q, want %q", chart.Metadata.Version, lasChartVersion)
	}
	renderValues, err := chartutil.ToRenderValues(chart, values, chartutil.ReleaseOptions{Name: "las-fixture", Namespace: "default"}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(chart, renderValues)
	if err != nil {
		t.Fatal(err)
	}
	for name, template := range map[string]string{
		"API":   "langgraph-cloud/templates/api-server/deployment.yaml",
		"queue": "langgraph-cloud/templates/queue/deployment.yaml",
	} {
		var deployment appsv1.Deployment
		if err := yaml.Unmarshal([]byte(rendered[template]), &deployment); err != nil {
			t.Fatalf("parse %s Deployment: %v", name, err)
		}
		strategy := deployment.Spec.Strategy
		if strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || strategy.RollingUpdate == nil || strategy.RollingUpdate.MaxSurge == nil || strategy.RollingUpdate.MaxUnavailable == nil {
			t.Fatalf("%s Deployment has no explicit RollingUpdate strategy: %#v", name, strategy)
		}
		if *strategy.RollingUpdate.MaxSurge != intstr.FromInt32(0) || *strategy.RollingUpdate.MaxUnavailable != intstr.FromInt32(1) {
			t.Errorf("%s Deployment rendered rollout policy = %#v", name, strategy.RollingUpdate)
		}
		if name == "API" && (deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1) {
			t.Errorf("API Deployment replicas = %v, want one", deployment.Spec.Replicas)
		}
	}
}

func TestLASValuesSelectImageAndKeepRunContextOut(t *testing.T) {
	ai := &aiv1.AIService{}
	ai.Name = "fixture"
	ai.Spec.Feature = aiv1.FeatureSpec{
		Name:              "agentruntime",
		LicenseSecretRef:  "offline-license",
		PostgresSecretRef: "fixture-postgres",
		RedisSecretRef:    "fixture-redis",
		LAS: &aiv1.LASFeatureSpec{
			Image:       aiv1.LASImageSpec{Repository: "registry.example/fixture", Tag: "candidate-123"},
			AssistantID: "contract_fixture",
			Queue:       &aiv1.LASQueueSpec{Autoscaling: &aiv1.LASQueueAutoscalingSpec{}},
		},
	}
	values := lasValues(ai)
	images := values["images"].(map[string]interface{})
	image := images["apiServerImage"].(map[string]interface{})
	if image["repository"] != "registry.example/fixture" || image["tag"] != "candidate-123" {
		t.Fatalf("selected image not rendered: %#v", image)
	}
	if _, found := values["llm_config"]; found {
		t.Fatal("per-run context rendered into Helm values")
	}
	if _, found := values["mcp_configs"]; found {
		t.Fatal("per-run context rendered into Helm values")
	}
	if got := values["config"].(map[string]interface{})["existingSecretName"]; got != "offline-license" {
		t.Fatalf("license Secret reference = %v", got)
	}
	if got := values["postgres"].(map[string]interface{})["external"].(map[string]interface{})["existingSecretName"]; got != "fixture-postgres" {
		t.Fatalf("PostgreSQL Secret reference = %v", got)
	}
	if got := values["redis"].(map[string]interface{})["external"].(map[string]interface{})["existingSecretName"]; got != "fixture-redis" {
		t.Fatalf("Redis Secret reference = %v", got)
	}
	serialized, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"llm_config", "mcp_configs", "kb_configs", "assistantId", "api_key", "Authorization"} {
		if bytes.Contains(serialized, []byte(forbidden)) {
			t.Errorf("Helm values contain per-invocation field %q", forbidden)
		}
	}
}

func TestLASTracingSettingsReachAPIAndQueueWorkloads(t *testing.T) {
	ai := &aiv1.AIService{}
	ai.Name = "fixture"
	ai.Spec.Feature = aiv1.FeatureSpec{
		LicenseSecretRef:  "las-credentials",
		PostgresSecretRef: "fixture-postgres",
		RedisSecretRef:    "fixture-redis",
		LAS: &aiv1.LASFeatureSpec{
			Image:       aiv1.LASImageSpec{Repository: "registry.example/fixture", Tag: "candidate-123"},
			AssistantID: "contract_fixture",
			Tracing:     &aiv1.LASTracingSpec{Enabled: false},
		},
	}
	disabledValues := lasValues(ai)
	ai.Spec.Feature.LAS.Tracing.Endpoint = "https://langsmith.internal"
	endpointValues := lasValues(ai)
	if sameValues(disabledValues, endpointValues) {
		t.Fatal("changing the tracing endpoint must change the Helm values even when tracing is disabled")
	}
	ai.Spec.Feature.LAS.Tracing.Enabled = true
	values := lasValues(ai)
	if sameValues(disabledValues, values) {
		t.Fatal("changing tracing settings must change the Helm values and trigger an upgrade")
	}
	for _, name := range []string{"apiServer", "queue"} {
		deployment := values[name].(map[string]interface{})["deployment"].(map[string]interface{})
		env := deployment["extraEnv"].([]map[string]interface{})
		if len(env) != 2 || env[0]["name"] != "LANGSMITH_TRACING" || env[0]["value"] != "true" || env[1]["name"] != "LANGSMITH_ENDPOINT" || env[1]["value"] != "https://langsmith.internal" {
			t.Fatalf("%s tracing env = %#v", name, env)
		}
	}
	if !sameValues(values, lasValues(ai)) {
		t.Fatal("tracing values must be stable for Helm release comparison")
	}

	chart, err := loader.LoadArchive(bytes.NewReader(lasChartArchive))
	if err != nil {
		t.Fatal(err)
	}
	renderValues, err := chartutil.ToRenderValues(chart, values, chartutil.ReleaseOptions{Name: "las-fixture", Namespace: "default"}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(chart, renderValues)
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range []string{
		"langgraph-cloud/templates/api-server/deployment.yaml",
		"langgraph-cloud/templates/queue/deployment.yaml",
	} {
		manifest := rendered[template]
		if !strings.Contains(manifest, "name: LANGSMITH_TRACING\n              value: \"true\"") || !strings.Contains(manifest, "name: LANGSMITH_ENDPOINT\n              value: https://langsmith.internal") {
			t.Errorf("%s does not render tracing configuration", template)
		}
		if !strings.Contains(manifest, "name: LANGSMITH_API_KEY\n              valueFrom:") {
			t.Errorf("%s does not source the LangSmith credential from the existing Secret", template)
		}
	}
}

func TestLASQueueAutoscalingValuesUseDefaultsAndOverrides(t *testing.T) {
	defaults := lasQueueAutoscalingValues(nil)
	if defaults["enabled"] != false || defaults["minReplicas"] != int32(1) || defaults["maxReplicas"] != int32(3) || defaults["targetCPUUtilizationPercentage"] != int32(80) {
		t.Fatalf("unexpected queue autoscaling defaults: %#v", defaults)
	}
	enabled := false
	minReplicas, maxReplicas, targetCPU := int32(2), int32(6), int32(65)
	configured := lasQueueAutoscalingValues(&aiv1.LASQueueAutoscalingSpec{
		Enabled: &enabled, MinReplicas: &minReplicas, MaxReplicas: &maxReplicas,
		TargetCPUUtilizationPercentage: &targetCPU,
	})
	if configured["enabled"] != false || configured["minReplicas"] != minReplicas || configured["maxReplicas"] != maxReplicas || configured["targetCPUUtilizationPercentage"] != targetCPU {
		t.Fatalf("queue autoscaling overrides were not mapped: %#v", configured)
	}
}

func TestAIP5391QueueConfigKeepsHPAOptIn(t *testing.T) {
	ai := &aiv1.AIService{}
	ai.Spec.Feature.LAS = &aiv1.LASFeatureSpec{
		Image:       aiv1.LASImageSpec{Repository: "registry.example/fixture", Tag: "candidate-123"},
		AssistantID: "contract_fixture",
		Queue:       &aiv1.LASQueueSpec{LASWorkloadSpec: aiv1.LASWorkloadSpec{}},
	}
	values := lasValues(ai)
	queue := values["queue"].(map[string]interface{})
	if queue["autoscaling"].(map[string]interface{})["enabled"] != false {
		t.Fatal("AIP-5391 queue configuration without autoscaling must not create an HPA")
	}
}

func TestLASChartRendersSelectedCandidateForBothWorkloads(t *testing.T) {
	ai := &aiv1.AIService{}
	ai.Name = "fixture"
	ai.Spec.Feature = aiv1.FeatureSpec{
		Name:              "agentruntime",
		LicenseSecretRef:  "offline-license",
		PostgresSecretRef: "fixture-postgres",
		RedisSecretRef:    "fixture-redis",
		LAS: &aiv1.LASFeatureSpec{
			Image:       aiv1.LASImageSpec{Repository: "registry.example/fixture", Tag: "candidate-123"},
			AssistantID: "contract_fixture",
		},
	}
	chart, err := loader.LoadArchive(bytes.NewReader(lasChartArchive))
	if err != nil {
		t.Fatal(err)
	}
	renderValues, err := chartutil.ToRenderValues(chart, lasValues(ai), chartutil.ReleaseOptions{Name: "las-fixture", Namespace: "default"}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(chart, renderValues)
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range []string{
		"langgraph-cloud/templates/api-server/deployment.yaml",
		"langgraph-cloud/templates/queue/deployment.yaml",
	} {
		manifest := rendered[template]
		if !strings.Contains(manifest, `image: "registry.example/fixture:candidate-123"`) {
			t.Errorf("%s does not use the selected candidate image", template)
		}
		for _, forbidden := range []string{"llm_config", "mcp_configs", "kb_configs"} {
			if strings.Contains(manifest, forbidden) {
				t.Errorf("%s contains per-invocation field %q", template, forbidden)
			}
		}
		if !strings.Contains(manifest, "timeoutSeconds: 5") {
			t.Errorf("%s does not use the documented five-second probe timeout", template)
		}
	}
	if !strings.Contains(rendered["langgraph-cloud/templates/api-server/deployment.yaml"], "cpu: 250m") {
		t.Error("API workload does not use the documented 250m default CPU request")
	}
}

func TestLASChartMapsWorkloadDefaultsOverridesSchedulingAndReferences(t *testing.T) {
	ai := &aiv1.AIService{}
	ai.Name = "fixture"
	ai.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "registry-auth"}}
	ai.Spec.Feature = aiv1.FeatureSpec{
		Name:              "agentruntime",
		LicenseSecretRef:  "offline-license",
		PostgresSecretRef: "fixture-postgres",
		RedisSecretRef:    "fixture-redis",
		LAS: &aiv1.LASFeatureSpec{
			Image:       aiv1.LASImageSpec{Repository: "registry.example/fixture", Tag: "candidate-123"},
			AssistantID: "contract_fixture",
			API: aiv1.LASWorkloadSpec{
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				},
				SchedulingSpec: aiv1.SchedulingSpec{
					NodeSelector: map[string]string{"node-role": "api"},
					Tolerations:  []corev1.Toleration{{Key: "las", Operator: corev1.TolerationOpEqual, Value: "api", Effect: corev1.TaintEffectNoSchedule}},
				},
				StartupProbe: &corev1.Probe{
					ProbeHandler:   corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "echo api"}}},
					TimeoutSeconds: 7,
				},
			},
			Queue: &aiv1.LASQueueSpec{
				LASWorkloadSpec: aiv1.LASWorkloadSpec{
					SchedulingSpec: aiv1.SchedulingSpec{
						NodeSelector: map[string]string{"node-role": "queue"},
						Tolerations:  []corev1.Toleration{{Key: "las", Operator: corev1.TolerationOpEqual, Value: "queue", Effect: corev1.TaintEffectNoSchedule}},
						Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/os", Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}}}}},
							},
						}},
					},
				},
				Autoscaling: &aiv1.LASQueueAutoscalingSpec{},
			},
		},
	}

	chart, err := loader.LoadArchive(bytes.NewReader(lasChartArchive))
	if err != nil {
		t.Fatal(err)
	}
	renderValues, err := chartutil.ToRenderValues(chart, lasValues(ai), chartutil.ReleaseOptions{Name: "las-fixture", Namespace: "default"}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(chart, renderValues)
	if err != nil {
		t.Fatal(err)
	}
	api := rendered["langgraph-cloud/templates/api-server/deployment.yaml"]
	queue := rendered["langgraph-cloud/templates/queue/deployment.yaml"]
	for name, manifest := range map[string]string{"API": api, "queue": queue} {
		for _, expected := range []string{
			"name: registry-auth",
			"name: offline-license",
			"key: langgraph_cloud_license_key",
			"name: LANGSMITH_API_KEY",
			"key: api_key",
			"name: fixture-postgres",
			"key: postgres_connection_url",
			"name: fixture-redis",
			"key: redis_connection_url",
			"timeoutSeconds: 5",
			"memory: 2Gi",
			"memory: 4Gi",
			`cpu: "2"`,
		} {
			if !strings.Contains(manifest, expected) {
				t.Errorf("%s workload missing %q", name, expected)
			}
		}
		for _, forbidden := range []string{"llm_config", "mcp_configs", "kb_configs", "postgresUri", "redisUri", "licenseKey"} {
			if strings.Contains(manifest, forbidden) {
				t.Errorf("%s workload contains forbidden configuration %q", name, forbidden)
			}
		}
	}
	for _, expected := range []string{"cpu: 500m", "node-role: api", "value: api", "timeoutSeconds: 7", "exec:", "echo api"} {
		if !strings.Contains(api, expected) {
			t.Errorf("API workload missing configured override %q", expected)
		}
	}
	for _, expected := range []string{`cpu: "1"`, "node-role: queue", "value: queue", "kubernetes.io/os", "operator: In", "linux", "httpGet:", "path: /ok"} {
		if !strings.Contains(queue, expected) {
			t.Errorf("queue workload missing configured/default value %q", expected)
		}
	}

	manifests := make([]string, 0, len(rendered))
	for _, manifest := range rendered {
		manifests = append(manifests, manifest)
	}
	allManifests := strings.Join(manifests, "\n")
	for _, forbidden := range []string{
		"kind: StatefulSet", "kind: Secret", "kind: PersistentVolumeClaim",
		"las-fixture-langgraph-cloud-postgres", "las-fixture-langgraph-cloud-redis", "las-fixture-langgraph-cloud-mongo",
	} {
		if strings.Contains(allManifests, forbidden) {
			t.Errorf("rendered chart unexpectedly contains %q", forbidden)
		}
	}
}
