/*
File: controllers/validate.go
*/
package ai_platform

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	//"k8s.io/apimachinery/pkg/api/resource"
	aiApi "github.com/splunk/splunk-ai-operator/api/v1"
	splunkutils "github.com/splunk/splunk-ai-operator/pkg/splunkutils"
)

// Validate checks required fields and backfills defaults on the AIPlatform spec.
func (r *AIPlatformReconciler) validate(ctx context.Context, p *aiApi.AIPlatform) error {
	if p.Spec.Profile != "" && p.Spec.Profile != aiApi.AIPlatformProfileFull && p.Spec.Profile != aiApi.AIPlatformProfileCPUOnly {
		return fmt.Errorf("unsupported AIPlatform profile %q", p.Spec.Profile)
	}
	if p.Spec.Profile == aiApi.AIPlatformProfileCPUOnly {
		if p.Status.RayServiceName != "" || p.Status.VectorDbServiceName != "" || p.Status.RayServiceStatus != "" {
			return fmt.Errorf("switching an existing model-serving AIPlatform to cpuOnly is not supported")
		}
		if p.Spec.GpuInstanceType != "" || p.Spec.DefaultAcceleratorType != "" || p.Spec.GPUSchedulingSpec != nil || p.Spec.WorkerGroupConfig != nil ||
			(p.Spec.ScaleFactor != nil && *p.Spec.ScaleFactor > 1) || p.Spec.Images.RayHeadGroupImage != "" || p.Spec.Images.RayWorkerGroupImage != "" ||
			p.Spec.Images.WeaviateImage != "" || p.Spec.Images.SAIAImage != "" || p.Spec.Ingress != nil && p.Spec.Ingress.Enabled {
			return fmt.Errorf("cpuOnly profile does not support Ray, Weaviate, SAIA, GPU, or Ray-backed ingress settings")
		}
		if p.Spec.Storage.VectorDB.Size != "" || p.Spec.Storage.VectorDB.PVCName != "" || p.Spec.Storage.VectorDB.StorageClassName != "" {
			return fmt.Errorf("cpuOnly profile does not support vectorDB storage settings")
		}
		for _, feature := range p.Spec.Features {
			if feature.Name == "saia" || feature.Name == "seca" {
				return fmt.Errorf("cpuOnly profile does not support %s feature", feature.Name)
			}
		}
		if p.Spec.Sidecars.Envoy || p.Spec.Sidecars.Otel || p.Spec.Sidecars.PrometheusOperator {
			return fmt.Errorf("cpuOnly profile does not support sidecars")
		}
	}
	// Required volume paths
	if p.Spec.ObjectStorage.Path == "" {
		return fmt.Errorf("object storage is required")
	}
	if p.Spec.CPUSchedulingSpec == nil {
		p.Spec.CPUSchedulingSpec = &aiApi.SchedulingSpec{
			NodeSelector: map[string]string{},
			Tolerations:  []corev1.Toleration{},
			Affinity:     &corev1.Affinity{},
		}
	}
	if p.Spec.Profile != aiApi.AIPlatformProfileCPUOnly && p.Spec.GPUSchedulingSpec == nil {
		p.Spec.GPUSchedulingSpec = &aiApi.SchedulingSpec{
			NodeSelector: map[string]string{},
			Tolerations:  []corev1.Toleration{},
			Affinity:     &corev1.Affinity{},
		}
	}

	sc := p.Spec.SplunkConfiguration
	if sc.Endpoint == "" &&
		sc.SplunkCustomResourceRef.Name == "" &&
		sc.SecretRef.Name == "" &&
		sc.VaultFilePath == "" {
		r.Recorder.Event(p, corev1.EventTypeWarning, "SplunkConfigMissing",
			"Splunk configuration is missing; assuming no telemetry")
		return nil
	}

	var resolver splunkutils.SplunkSecretResolver
	if p.Spec.SplunkConfiguration.SecretSource == aiApi.SecretSourceVault {
		resolver = &splunkutils.VaultFileResolver{}
	} else {
		resolver = &splunkutils.KubernetesSecretResolver{Client: r.Client}
	}

	return splunkutils.ValidateAndEnrichSplunkConfig(
		ctx,
		r.Client,
		p.Namespace,
		p.Spec.ClusterDomain,
		&p.Spec.SplunkConfiguration,
		resolver,
	)

}

func SetImageRegistry(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultValue
}
