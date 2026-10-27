package slim

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"reflect"
	"sort"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	common "github.com/splunk/splunk-ai-operator/pkg/ai/features/common"
	"github.com/splunk/splunk-ai-operator/pkg/splunkutils"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type SlimReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

const (
	defaultAgentRuntimeHTTPPort int32 = 8080
	maxDNSLabelLength                 = 63
)

func (r *SlimReconciler) Reconcile(ctx context.Context, aiservice *aiv1.AIService) error {
	log := log.FromContext(ctx)

	previous := append([]metav1.Condition(nil), aiservice.Status.Conditions...)
	var conditions []metav1.Condition
	// Events are emitted only once the status that justifies them is stored, so
	// a failed write does not repeat them on every poll.
	var emitEvents func()
	defer func() {
		aiservice.Status.Conditions = conditions
		aiservice.Status.ObservedGeneration = aiservice.Generation
		if err := r.Status().Update(ctx, aiservice); err == nil && emitEvents != nil {
			emitEvents()
		}
	}()

	// The periodic requeue re-runs this method, so a condition keeps its
	// LastTransitionTime until its status really changes.
	newCondition := func(condType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
		cond := metav1.Condition{
			Type:               condType,
			Status:             status,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		}
		if old := meta.FindStatusCondition(previous, condType); old != nil && old.Status == status {
			cond.LastTransitionTime = old.LastTransitionTime
		}
		return cond
	}

	stages := []struct {
		name string
		fn   func(context.Context, *aiv1.AIService) error
	}{
		{"Validate", r.validateAIService},
		{"ServiceAccount", r.reconcileServiceAccount},
		{"SlimConfigMap", r.reconcileSlimConfigMap},
		{"FeatureConfigMap", r.reconcileFeatureConfigMap},
		{"Certificate", r.reconcileCertificate},
		{"SlimDeployment", r.reconcileSlimDeployment},
		{"SlimService", r.reconcileSlimService},
		{"SlimMetricsService", r.reconcileSlimMetricsService},
		{"ServiceMonitor", r.reconcileServiceMonitor},
		{"SlimDeploymentAvailable", r.checkSlimDeploymentAvailable},
	}

	var held *dependencyPendingError
	for _, stage := range stages {
		err := stage.fn(ctx, aiservice)
		if errors.As(err, &held) {
			// A dependency the workload cannot be configured without is not
			// ready yet. Hold the workload and report the dependency below; this
			// is not a failure, so it must not back off.
			break
		}

		status, reason, message := metav1.ConditionTrue, "Reconciled", "stage succeeded"
		if err != nil {
			status, reason, message = metav1.ConditionFalse, "Error", err.Error()
		}
		conditions = append(conditions, newCondition(stage.name+"Ready", status, reason, message))
		if err != nil {
			log.Error(err, "stage failed", "stage", stage.name)
			return err
		}
	}

	states, err := r.dependencyStates(ctx, aiservice)
	if err != nil {
		// A lookup failure says nothing about the dependencies, so the last
		// known readiness is kept rather than flipping SLIM and waking the platform.
		log.Error(err, "evaluating SLIM dependencies failed")
		for _, cond := range previous {
			if cond.Type == "Ready" || cond.Type == dependencyAgentRuntime+"Ready" || cond.Type == dependencyModelServing+"Ready" {
				conditions = append(conditions, cond)
			}
		}
		return err
	}
	if held != nil {
		states = withHeldDependency(states, held)
	}

	var blocked []string
	for _, state := range states {
		status, reason, message := metav1.ConditionTrue, "Reconciled", state.dependency+" is ready"
		if !state.ready {
			status, reason, message = metav1.ConditionFalse, reasonDependencyNotReady, state.message
			blocked = append(blocked, state.dependency+": "+state.message)
		}
		conditions = append(conditions, newCondition(state.dependency+"Ready", status, reason, message))
	}

	ready := newCondition("Ready", metav1.ConditionTrue, "AllReconciled", "all stages completed successfully")
	if len(blocked) > 0 {
		ready = newCondition("Ready", metav1.ConditionFalse, reasonDependencyNotReady, "blocked by "+strings.Join(blocked, "; "))
	}
	conditions = append(conditions, ready)
	previousReady := meta.FindStatusCondition(previous, "Ready")
	emitEvents = func() { r.recordReadyTransition(aiservice, previousReady, ready) }

	return nil
}

// recordReadyTransition emits an Event when SLIM becomes blocked, is blocked by
// a different dependency, or recovers.
func (r *SlimReconciler) recordReadyTransition(ai *aiv1.AIService, previous *metav1.Condition, current metav1.Condition) {
	if r.Recorder == nil {
		return
	}
	switch {
	case current.Status == metav1.ConditionFalse && (previous == nil || previous.Status != metav1.ConditionFalse || previous.Message != current.Message):
		r.Recorder.Event(ai, corev1.EventTypeWarning, reasonDependencyNotReady, current.Message)
	case current.Status == metav1.ConditionTrue && previous != nil && previous.Status == metav1.ConditionFalse:
		r.Recorder.Event(ai, corev1.EventTypeNormal, "DependenciesReady", "SLIM dependencies are ready")
	}
}

func (r *SlimReconciler) validateAIService(ctx context.Context, ai *aiv1.AIService) error {
	cleanServiceTemplate(&ai.Spec.ServiceTemplate)

	if os.Getenv("RELATED_IMAGE_SLIM_API") == "" {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "InvalidSpec", "RELATED_IMAGE_SLIM_API must be set")
		return fmt.Errorf("RELATED_IMAGE_SLIM_API must be set")
	}

	if ai.Spec.AIPlatformRef.Name == "" {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "InvalidSpec", "AIPlatformRef.Name is required for SLIM configuration")
		return fmt.Errorf("AIPlatformRef.Name is required for SLIM configuration")
	}

	aiPlatform, err := r.getAIPlatform(ctx, ai.Spec.AIPlatformRef)
	if err != nil {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "InvalidSpec", "fetching AIPlatform failed")
		return fmt.Errorf("fetching AIPlatform: %w", err)
	}
	// A CPU-only platform has no model serving: nothing below this guard runs
	// for it, so SLIM never reads Ray state or needs a model endpoint there.
	if aiPlatform.Spec.Profile != aiv1.AIPlatformProfileCPUOnly {
		saia := platformHasFeature(aiPlatform, "saia")
		// A SLIM workload that does not exist yet, or whose model URL cannot be
		// derived from the Ray service, is held (not failed) until model
		// serving is ready, so its first pods do not start against an endpoint
		// that is not serving. An existing workload keeps reconciling through a
		// Ray outage; readiness is reported by the dependency gates.
		state, err := r.modelServingState(ctx, aiPlatform)
		if err != nil {
			return fmt.Errorf("checking model serving: %w", err)
		}
		if !state.ready {
			urlUnknown := saia && ai.Spec.AIPlatformUrl == "" && aiPlatform.Status.RayServiceName == ""
			exists, err := r.slimDeploymentExists(ctx, ai)
			if err != nil {
				return err
			}
			if urlUnknown || !exists {
				return &dependencyPendingError{state: state}
			}
		}
		if saia && ai.Spec.AIPlatformUrl == "" {
			clusterDomain := ai.Spec.ClusterDomain
			if clusterDomain == "" {
				clusterDomain = "cluster.local"
			}
			scheme := ai.Spec.AIPlatformScheme
			if scheme == "" {
				scheme = "http"
			}
			ai.Spec.AIPlatformUrl = fmt.Sprintf("%s://%s.%s.svc.%s:8000",
				scheme, aiPlatform.Status.RayServiceName, ai.Spec.AIPlatformRef.Namespace, clusterDomain)
		}
	}
	if slimPlatformURL(ai.Spec.AIPlatformUrl) == "" && !platformHasFeature(aiPlatform, "agentruntime") {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "InvalidSpec", "SLIM requires AIPlatformUrl or an AgentRuntime-backed AgentServerUrl")
		return fmt.Errorf("SLIM requires AIPlatformUrl or an AgentRuntime-backed AgentServerUrl")
	}

	if ai.Spec.Resources.Requests == nil {
		ai.Spec.Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("5Gi"),
		}
	}
	if ai.Spec.Resources.Limits == nil {
		ai.Spec.Resources.Limits = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("5Gi"),
		}
	}
	if ai.Spec.Replicas == 0 {
		ai.Spec.Replicas = 1
	}

	return nil
}

func (r *SlimReconciler) slimDeploymentExists(ctx context.Context, ai *aiv1.AIService) (bool, error) {
	err := r.Get(ctx, types.NamespacedName{Name: ai.Name + "-slim-deployment", Namespace: ai.Namespace}, &appsv1.Deployment{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("observe SLIM Deployment: %w", err)
	}
	return true, nil
}

func (r *SlimReconciler) getAIPlatform(ctx context.Context, ref corev1.ObjectReference) (*aiv1.AIPlatform, error) {
	var aiPlatform aiv1.AIPlatform
	key := types.NamespacedName{
		Name:      ref.Name,
		Namespace: ref.Namespace,
	}
	if err := r.Client.Get(ctx, key, &aiPlatform); err != nil {
		return nil, err
	}
	return &aiPlatform, nil
}

func platformHasFeature(platform *aiv1.AIPlatform, name string) bool {
	for _, feature := range platform.Spec.Features {
		if feature.Name == name {
			return true
		}
	}
	return false
}

func slimPlatformURL(raw string) string {
	url := strings.TrimRight(strings.TrimSpace(raw), "/")
	return strings.TrimSuffix(url, "/ai-platform-models/v1")
}

func (r *SlimReconciler) reconcileServiceAccount(ctx context.Context, ai *aiv1.AIService) error {
	defaultName := ai.Name + "-sa"
	if ai.Spec.ServiceAccountName == "" {
		cleanServiceTemplate(&ai.Spec.ServiceTemplate)

		ai.Spec.ServiceAccountName = defaultName
		if err := r.Update(ctx, ai); err != nil {
			return fmt.Errorf("updating SA name in spec: %w", err)
		}
	}
	// A custom name refers to a caller-managed account. For the generated name,
	// ensure the account exists even when the parent supplied the default in
	// the initial AIService spec.
	if ai.Spec.ServiceAccountName != defaultName {
		return nil
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: defaultName, Namespace: ai.Namespace,
	}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		return controllerutil.SetControllerReference(ai, sa, r.Scheme)
	}); err != nil {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "InvalidSpec", "create/update SA failed")
		return fmt.Errorf("create/update SA: %w", err)
	}
	return nil
}

// buildSplunkIssuersVal computes the comma-separated SPLUNK_ISSUERS value from the AIService spec.
// The JWT issuer is the Splunk management endpoint (port 8089).
// Priority: CRRef-derived service FQDN → explicit Endpoint → TrustedIssuers only.
//
// This mirrors saia.buildSplunkIssuersVal; slim keeps its own copy so the two
// feature packages stay decoupled.
func buildSplunkIssuersVal(ai *aiv1.AIService) string {
	var issuers []string
	sc := ai.Spec.SplunkConfiguration
	switch {
	case sc.SplunkCustomResourceRef.Name != "":
		kind := sc.SplunkCustomResourceRef.Kind
		if kind == "" {
			kind = "Standalone"
		}
		instanceType := "standalone"
		if kind == "IndexerCluster" {
			instanceType = "indexer"
		}
		refNS := sc.SplunkCustomResourceRef.Namespace
		if refNS == "" {
			refNS = ai.Namespace
		}
		clusterDomain := ai.Spec.ClusterDomain
		if clusterDomain == "" {
			clusterDomain = "cluster.local"
		}
		svc := fmt.Sprintf("splunk-%s-%s-service.%s.svc.%s", sc.SplunkCustomResourceRef.Name, instanceType, refNS, clusterDomain)
		issuers = append(issuers, fmt.Sprintf("https://%s:%d", svc, splunkutils.SplunkMgmtPort))
	case sc.Endpoint != "":
		issuers = append(issuers, sc.Endpoint)
	}
	issuers = append(issuers, sc.TrustedIssuers...)
	return strings.Join(issuers, ",")
}

// reconcileSlimConfigMap manages the ConfigMap holding the slim-api boot
// configuration. The keys mirror the AITIER-mode env vars from the slim-api
// repo's `run-slim-aipod` Makefile target, trimmed to only those the service
// actually reads in an in-cluster (non-SCS) deployment:
//
//   - DEPLOYMENT_TYPE=AITIER selects the AI Tier access integration.
//   - SPLUNK_ISSUERS is the JWT issuer whitelist used by CMP auth; it is derived
//     from the AIService spec (CRRef → endpoint → trustedIssuers), matching saia.
//   - SERVICE_NAME / SERVICE_INTERNAL_NAME identify the service.
//
// AGENT_SERVER_URL is injected on the Deployment when this AIPlatform has an
// AgentRuntime feature. PLATFORM_URL is the separate model-serving endpoint
// when AIPlatformUrl is set. SLIM needs at least one of these routes.
func (r *SlimReconciler) reconcileSlimConfigMap(ctx context.Context, ai *aiv1.AIService) error {
	cmName := fmt.Sprintf("%s-slim-config", ai.Name)

	splunkIssuersVal := buildSplunkIssuersVal(ai)

	defaults := map[string]string{
		"DEPLOYMENT_TYPE":       "AITIER",
		"SERVICE_NAME":          "slim-api",
		"SERVICE_INTERNAL_NAME": "SLIM",
		"SPLUNK_ISSUERS":        splunkIssuersVal,
		"ENABLE_AUTHN":          "true",
		"ENABLE_AUTHZ":          "true",
		"API_VERSION":           "v1alpha1",
	}

	found := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: ai.Namespace}, found)
	if apierrors.IsNotFound(err) {
		return r.createOrUpdateConfigMap(ctx, cmName, defaults, ai)
	} else if err != nil {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "InvalidSpec",
			fmt.Sprintf("failed to retrieve ConfigMap %q: %v", cmName, err))
		return fmt.Errorf("fetching Slim ConfigMap %q: %w", cmName, err)
	}

	if found.Data == nil {
		found.Data = map[string]string{}
	}
	// Merge defaults: SPLUNK_ISSUERS is always recomputed from spec so that
	// removing or changing a CRRef/endpoint/trustedIssuers is reflected immediately
	// (including clearing to empty when Splunk is disabled).
	// All other keys use fill-if-missing to preserve manual overrides.
	needsUpdate := false
	for k, v := range defaults {
		if k == "SPLUNK_ISSUERS" || k == "ENABLE_AUTHN" || k == "ENABLE_AUTHZ" {
			if found.Data[k] != v {
				found.Data[k] = v
				needsUpdate = true
			}
			continue
		}
		if _, ok := found.Data[k]; !ok || found.Data[k] == "" {
			found.Data[k] = v
			needsUpdate = true
		}
	}
	if needsUpdate {
		if err := controllerutil.SetControllerReference(ai, found, r.Scheme); err != nil {
			return fmt.Errorf("ownerref on ConfigMap: %w", err)
		}
		return r.Update(ctx, found)
	}
	return nil
}

func (r *SlimReconciler) reconcileFeatureConfigMap(ctx context.Context, ai *aiv1.AIService) error {
	cmName := fmt.Sprintf("splunk-%s-feature-config", ai.Name)

	found := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: ai.Namespace}, found)

	if err == nil {
		if !hasOwnerReference(found, ai) {
			if err := controllerutil.SetControllerReference(ai, found, r.Scheme); err != nil {
				r.Recorder.Event(ai, corev1.EventTypeWarning, "FeatureConfigMapError",
					fmt.Sprintf("Failed to set owner reference on ConfigMap %q", cmName))
				return fmt.Errorf("failed to set owner reference on ConfigMap %q: %w", cmName, err)
			}
			if err := r.Update(ctx, found); err != nil {
				return fmt.Errorf("failed to update owner reference on ConfigMap %q: %w", cmName, err)
			}
			r.Recorder.Event(ai, corev1.EventTypeNormal, "FeatureConfigMapUpdated",
				fmt.Sprintf("Added owner reference to existing ConfigMap %q", cmName))
		}
		return nil
	}

	if !apierrors.IsNotFound(err) {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "FeatureConfigMapError",
			fmt.Sprintf("Failed to retrieve ConfigMap %q", cmName))
		return fmt.Errorf("failed to get ConfigMap %q: %w", cmName, err)
	}

	defaultData := map[string]string{
		"features_config.yaml": `customization:
  enabled_by_default: true
`,
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: ai.Namespace,
		},
		Data: defaultData,
	}

	if err := controllerutil.SetControllerReference(ai, cm, r.Scheme); err != nil {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "FeatureConfigMapError",
			fmt.Sprintf("Failed to set owner reference on ConfigMap %q", cmName))
		return fmt.Errorf("failed to set owner reference on ConfigMap %q: %w", cmName, err)
	}

	if err := r.Create(ctx, cm); err != nil {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "FeatureConfigMapError",
			fmt.Sprintf("Failed to create ConfigMap %q", cmName))
		return fmt.Errorf("failed to create ConfigMap %q: %w", cmName, err)
	}

	r.Recorder.Event(ai, corev1.EventTypeNormal, "FeatureConfigMapCreated",
		fmt.Sprintf("Created feature-config ConfigMap %q with default content", cmName))

	return nil
}

func hasOwnerReference(obj metav1.Object, owner metav1.Object) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == owner.GetUID() {
			return true
		}
	}
	return false
}

func (r *SlimReconciler) reconcileCertificate(ctx context.Context, ai *aiv1.AIService) error {
	if !ai.Spec.MTLS.Enabled || ai.Spec.MTLS.Termination != "operator" {
		return nil
	}

	certExists := true
	existingCert := &certmanagerv1.Certificate{}
	certKey := types.NamespacedName{Name: ai.Name + "-tls", Namespace: ai.Namespace}
	if err := r.Get(ctx, certKey, existingCert); err != nil {
		if apierrors.IsNotFound(err) {
			certExists = false
			r.Recorder.Event(ai, corev1.EventTypeNormal, "MTLSCertificateCreating", "Creating mTLS certificate")
		}
	}

	cert := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ai.Name + "-tls",
			Namespace: ai.Namespace,
		},
		Spec: certmanagerv1.CertificateSpec{
			SecretName: ai.Spec.MTLS.SecretName,
			IssuerRef:  ai.Spec.MTLS.IssuerRef,
			DNSNames:   ai.Spec.MTLS.DNSNames,
			Usages: []certmanagerv1.KeyUsage{
				certmanagerv1.UsageServerAuth,
				certmanagerv1.UsageClientAuth,
			},
		},
	}
	if err := controllerutil.SetControllerReference(ai, cert, r.Scheme); err != nil {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "MTLSCertificateError", "Failed to set owner reference on Certificate")
		return fmt.Errorf("ownerref on Certificate: %w", err)
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
		cert.Spec = certmanagerv1.CertificateSpec{
			SecretName: ai.Spec.MTLS.SecretName,
			IssuerRef:  ai.Spec.MTLS.IssuerRef,
			DNSNames:   ai.Spec.MTLS.DNSNames,
			Usages: []certmanagerv1.KeyUsage{
				certmanagerv1.UsageServerAuth,
				certmanagerv1.UsageClientAuth,
			},
		}
		return nil
	}); err != nil {
		r.Recorder.Eventf(ai, corev1.EventTypeWarning, "MTLSCertificateCreationFailed", "Failed to create/update Certificate: %v", err)
		return fmt.Errorf("create/update Certificate: %w", err)
	}

	if !certExists {
		r.Recorder.Event(ai, corev1.EventTypeNormal, "MTLSCertificateCreated", "mTLS Certificate created successfully")
	}

	certReady := false
	for _, cond := range cert.Status.Conditions {
		if cond.Type == certmanagerv1.CertificateConditionReady && cond.Status == cmmeta.ConditionTrue {
			certReady = true
			break
		}
	}

	if !certReady {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "MTLSCertificateNotReady", "Waiting for cert-manager to issue certificate")
		return fmt.Errorf("waiting for Certificate %q to become Ready", cert.Name)
	}

	r.Recorder.Event(ai, corev1.EventTypeNormal, "MTLSCertificateReady", "mTLS certificate issued successfully")
	return nil
}

func (r *SlimReconciler) reconcileSlimDeployment(ctx context.Context, ai *aiv1.AIService) error {
	featureConfigName := fmt.Sprintf("splunk-%s-feature-config", ai.Name)
	slimConfig := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: ai.Name + "-slim-config", Namespace: ai.Namespace}, slimConfig); err != nil {
		return fmt.Errorf("fetching Slim ConfigMap for Deployment rollout: %w", err)
	}

	volumes := []corev1.Volume{
		{
			Name: "config-volume",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: featureConfigName},
					DefaultMode:          slimInt32(420),
				},
			},
		},
	}

	ports := []corev1.ContainerPort{
		{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
		{Name: "metrics", ContainerPort: 8088, Protocol: corev1.ProtocolTCP},
	}
	mounts := []corev1.VolumeMount{
		{Name: "config-volume", MountPath: "/etc/config"},
	}

	aiPlatform, err := r.getAIPlatform(ctx, ai.Spec.AIPlatformRef)
	if err != nil {
		return fmt.Errorf("fetching AIPlatform for SLIM route: %w", err)
	}

	env := []corev1.EnvVar{}
	if platformHasFeature(aiPlatform, "agentruntime") {
		agentServerURL, graphID, err := r.agentRuntimeTarget(ctx, ai)
		if err != nil {
			return err
		}
		env = append(env,
			corev1.EnvVar{Name: "AGENT_SERVER_URL", Value: agentServerURL},
			corev1.EnvVar{Name: "LAS_GRAPH_ID", Value: graphID},
		)
	}
	if platformURL := slimPlatformURL(ai.Spec.AIPlatformUrl); platformURL != "" {
		env = append(env, corev1.EnvVar{Name: "PLATFORM_URL", Value: platformURL})
	}
	if len(env) == 0 {
		return fmt.Errorf("SLIM requires AIPlatformUrl or an AgentRuntime-backed AgentServerUrl")
	}

	if ai.Spec.MTLS.Enabled && ai.Spec.MTLS.Termination == "operator" {
		volumes = append(volumes, corev1.Volume{
			Name: "tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: ai.Spec.MTLS.SecretName},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "tls", MountPath: "/etc/tls", ReadOnly: true})
		env = append(env,
			corev1.EnvVar{Name: "TLS_CERT_FILE", Value: "/etc/tls/tls.crt"},
			corev1.EnvVar{Name: "TLS_KEY_FILE", Value: "/etc/tls/tls.key"},
		)
		ports = append(ports, corev1.ContainerPort{Name: "https", ContainerPort: 8443, Protocol: corev1.ProtocolTCP})
	} else {
		env = append(env, corev1.EnvVar{Name: "TLS_DISABLED", Value: "true"})
	}

	envFrom := []corev1.EnvFromSource{
		{
			ConfigMapRef: &corev1.ConfigMapEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: fmt.Sprintf("%s-slim-config", ai.Name),
				},
			},
		},
	}

	sort.Slice(env, func(i, j int) bool { return env[i].Name < env[j].Name })

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ai.Name + "-slim-deployment",
			Namespace: ai.Namespace,
		},
	}

	labels := map[string]string{
		"app":       ai.Name,
		"component": ai.Name,
		"area":      "ml",
		"team":      "ml",
	}
	for k, v := range ai.Labels {
		labels[k] = v
	}

	// The issuers hash rolls the pods whenever the derived SPLUNK_ISSUERS value
	// changes. The ConfigMap revision also rolls pods after any correction to
	// authentication flags: environment variables do not change in running pods.
	issuersVal := buildSplunkIssuersVal(ai)
	issuersChecksum := fmt.Sprintf("%x", sha256.Sum256([]byte(issuersVal)))
	annotations := map[string]string{
		"prometheus.io/port":                     "8088",
		"prometheus.io/path":                     "/metrics",
		"prometheus.io/scheme":                   "http",
		"splunk-ai-operator/splunk-issuers-hash": issuersChecksum,
	}
	for k, v := range common.FilterPropagatedAnnotations(ai.Annotations) {
		annotations[k] = v
	}
	annotations["splunk-ai-operator/slim-config-revision"] = string(slimConfig.UID) + "/" + slimConfig.ResourceVersion

	if err := controllerutil.SetControllerReference(ai, deployment, r.Scheme); err != nil {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "InvalidSpec", "ownerref on Deployment failed")
		return fmt.Errorf("ownerref on Deployment: %w", err)
	}

	containers := []corev1.Container{{
		Name:            ai.Name,
		Image:           os.Getenv("RELATED_IMAGE_SLIM_API"),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Ports:           ports,
		VolumeMounts:    mounts,
		Resources:       ai.Spec.Resources,
		Env:             env,
		EnvFrom:         envFrom,
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt(8080), Scheme: corev1.URISchemeHTTP},
			},
			PeriodSeconds:    30,
			FailureThreshold: 10,
			TimeoutSeconds:   1,
			SuccessThreshold: 1,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt(8080), Scheme: corev1.URISchemeHTTP},
			},
			PeriodSeconds:    30,
			FailureThreshold: 10,
			TimeoutSeconds:   1,
			SuccessThreshold: 1,
		},
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt(8080), Scheme: corev1.URISchemeHTTP},
			},
			InitialDelaySeconds: 30,
			PeriodSeconds:       30,
			FailureThreshold:    10,
			TimeoutSeconds:      1,
			SuccessThreshold:    1,
		},
		TerminationMessagePath:   "/dev/termination-log",
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}}

	desiredTemplate := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      map[string]string{"app": ai.Name, "component": ai.Name},
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:            ai.Spec.ServiceAccountName,
			Containers:                    containers,
			Volumes:                       volumes,
			Tolerations:                   ai.Spec.Tolerations,
			ImagePullSecrets:              ai.Spec.ImagePullSecrets,
			RestartPolicy:                 corev1.RestartPolicyAlways,
			DNSPolicy:                     corev1.DNSClusterFirst,
			SchedulerName:                 "default-scheduler",
			TerminationGracePeriodSeconds: slimInt64(30),
			EnableServiceLinks:            slimBool(true),
		},
	}
	if !reflect.DeepEqual(ai.Spec.Affinity, corev1.Affinity{}) {
		desiredTemplate.Spec.Affinity = ai.Spec.Affinity.DeepCopy()
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		deployment.ObjectMeta.Labels = labels
		// The Deployment controller writes its revision here. Clearing it on
		// every reconcile causes another update and can keep generation moving.
		revision := deployment.Annotations["deployment.kubernetes.io/revision"]
		deployment.ObjectMeta.Annotations = make(map[string]string, len(annotations)+1)
		for key, value := range annotations {
			deployment.ObjectMeta.Annotations[key] = value
		}
		if revision != "" {
			deployment.ObjectMeta.Annotations["deployment.kubernetes.io/revision"] = revision
		}
		deployment.Spec.Replicas = &ai.Spec.Replicas

		if deployment.Spec.Selector == nil {
			deployment.Spec.Selector = &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": ai.Name, "component": ai.Name},
			}
		}

		// Kubernetes fills omitted Pod and container defaults in the stored
		// template. Replacing it unconditionally drops those defaults and
		// triggers a fresh rollout even when our managed fields are unchanged.
		if !slimTemplateMatches(deployment.Spec.Template, desiredTemplate) {
			deployment.Spec.Template = desiredTemplate
		}
		return nil
	}); err != nil {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "InvalidSpec", "create/update Deployment failed")
		return fmt.Errorf("create/update Deployment: %w", err)
	}
	return nil
}

func slimTemplateMatches(actual, desired corev1.PodTemplateSpec) bool {
	if !reflect.DeepEqual(actual.Labels, desired.Labels) ||
		!reflect.DeepEqual(actual.Annotations, desired.Annotations) ||
		len(actual.Spec.Containers) != len(desired.Spec.Containers) ||
		len(actual.Spec.Volumes) != len(desired.Spec.Volumes) ||
		len(actual.Spec.Tolerations) != len(desired.Spec.Tolerations) ||
		len(actual.Spec.ImagePullSecrets) != len(desired.Spec.ImagePullSecrets) {
		return false
	}
	for i := range desired.Spec.Containers {
		got, want := actual.Spec.Containers[i], desired.Spec.Containers[i]
		if len(got.Env) != len(want.Env) || len(got.EnvFrom) != len(want.EnvFrom) ||
			len(got.Ports) != len(want.Ports) || len(got.VolumeMounts) != len(want.VolumeMounts) ||
			!apiequality.Semantic.DeepEqual(got.Resources, want.Resources) {
			return false
		}
	}
	if desired.Spec.Affinity == nil && actual.Spec.Affinity != nil &&
		!reflect.DeepEqual(*actual.Spec.Affinity, corev1.Affinity{}) {
		return false
	}
	return apiequality.Semantic.DeepDerivative(desired.Spec, actual.Spec)
}

func slimInt32(value int32) *int32 { return &value }
func slimInt64(value int64) *int64 { return &value }
func slimBool(value bool) *bool    { return &value }

func (r *SlimReconciler) checkSlimDeploymentAvailable(ctx context.Context, ai *aiv1.AIService) error {
	platform, err := r.getAIPlatform(ctx, ai.Spec.AIPlatformRef)
	if err != nil {
		return fmt.Errorf("fetching AIPlatform for SLIM readiness: %w", err)
	}
	if platform.Spec.Profile != aiv1.AIPlatformProfileCPUOnly {
		return nil
	}
	name := ai.Name + "-slim-deployment"
	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ai.Namespace}, deployment); err != nil {
		return fmt.Errorf("observe SLIM Deployment %s: %w", name, err)
	}
	desired := ai.Spec.Replicas
	if deployment.Status.ObservedGeneration < deployment.Generation ||
		deployment.Status.UpdatedReplicas < desired ||
		deployment.Status.ReadyReplicas < desired ||
		deployment.Status.AvailableReplicas < desired {
		return fmt.Errorf("SLIM Deployment %s is not available for generation %d: %d/%d updated, %d/%d ready, %d/%d available",
			name, deployment.Generation, deployment.Status.UpdatedReplicas, desired,
			deployment.Status.ReadyReplicas, desired, deployment.Status.AvailableReplicas, desired)
	}
	return nil
}

func (r *SlimReconciler) agentRuntimeTarget(ctx context.Context, ai *aiv1.AIService) (string, string, error) {
	platform, err := r.getAIPlatform(ctx, ai.Spec.AIPlatformRef)
	if err != nil {
		return "", "", fmt.Errorf("fetching AIPlatform for AgentRuntime route: %w", err)
	}

	var runtimeFeature *aiv1.FeatureSpec
	for i := range platform.Spec.Features {
		feature := &platform.Spec.Features[i]
		if feature.Name != "agentruntime" {
			continue
		}
		if runtimeFeature != nil {
			return "", "", fmt.Errorf("AIPlatform %q has multiple AgentRuntime features; SLIM requires one selected LAS route", platform.Name)
		}
		runtimeFeature = feature
	}
	if runtimeFeature == nil {
		return "", "", fmt.Errorf("AIPlatform %q has no AgentRuntime feature for the SLIM caller route", platform.Name)
	}
	if runtimeFeature.LAS == nil || runtimeFeature.LAS.AssistantID == "" {
		return "", "", fmt.Errorf("AIPlatform %q AgentRuntime feature has no LAS assistantId for the SLIM caller route", platform.Name)
	}

	serviceBase := platform.Name + "-agentruntime"
	if runtimeFeature.Provider != "" {
		serviceBase += "-" + runtimeFeature.Provider
		serviceBase = boundedDNSName(serviceBase)
	}
	serviceName := boundedServiceName(serviceBase)
	clusterDomain := ai.Spec.ClusterDomain
	if clusterDomain == "" {
		clusterDomain = "cluster.local"
	}
	return fmt.Sprintf("http://%s.%s.svc.%s:%d", serviceName, platform.Namespace, clusterDomain, defaultAgentRuntimeHTTPPort), runtimeFeature.LAS.AssistantID, nil
}

func boundedDNSName(name string) string {
	if len(name) <= maxDNSLabelLength {
		return name
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return strings.TrimRight(name[:maxDNSLabelLength-9], "-") + "-" + fmt.Sprintf("%08x", h.Sum32())
}

func boundedServiceName(aiServiceName string) string {
	name := aiServiceName + "-svc"
	if len(name) <= maxDNSLabelLength {
		return name
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	baseLength := maxDNSLabelLength - len("svc") - 8 - 2
	return strings.TrimRight(aiServiceName[:baseLength], "-") + "-" + fmt.Sprintf("%08x", h.Sum32()) + "-svc"
}

// reconcileSlimService ensures the public-facing Service, which may be exposed via
// NodePort/LoadBalancer per ServiceTemplate. It intentionally excludes the metrics
// port so that enabling public exposure never publishes /metrics or consumes an
// extra NodePort; metrics are served by reconcileSlimMetricsService instead.
func (r *SlimReconciler) reconcileSlimService(ctx context.Context, ai *aiv1.AIService) error {
	serviceTemplate := ai.Spec.ServiceTemplate.DeepCopy()
	cleanServiceTemplate(serviceTemplate)

	ports := []corev1.ServicePort{
		{Name: "http", Port: 8080, TargetPort: intstr.FromInt(8080)},
	}
	if ai.Spec.MTLS.Enabled && ai.Spec.MTLS.Termination == "operator" {
		ports = append(ports, corev1.ServicePort{
			Name: "https", Port: 8443, TargetPort: intstr.FromInt(8443),
		})
	}

	svcType := corev1.ServiceTypeClusterIP
	switch serviceTemplate.Spec.Type {
	case corev1.ServiceTypeLoadBalancer:
		svcType = corev1.ServiceTypeLoadBalancer
	case corev1.ServiceTypeNodePort:
		svcType = corev1.ServiceTypeNodePort
		for i, port := range ports {
			for _, tplPort := range serviceTemplate.Spec.Ports {
				if port.Name == tplPort.Name && tplPort.NodePort != 0 {
					ports[i].NodePort = tplPort.NodePort
				}
			}
		}
	}

	labels := map[string]string{"app": ai.Name}
	for k, v := range ai.Labels {
		labels[k] = v
	}
	annotations := common.FilterPropagatedAnnotations(ai.Annotations)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ai.Name + "-slim-service",
			Namespace: ai.Namespace,
		},
	}
	if err := controllerutil.SetControllerReference(ai, svc, r.Scheme); err != nil {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "InvalidSpec", "ownerref on Service failed")
		return fmt.Errorf("ownerref on Service: %w", err)
	}
	// Type, Ports, and Selector are all set inside the mutate callback because
	// CreateOrUpdate reloads the live Service from the cluster before invoking it,
	// so any values set on svc beforehand are discarded on updates.
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.ObjectMeta.Labels = labels
		svc.ObjectMeta.Annotations = annotations
		svc.Spec.Selector = map[string]string{"app": ai.Name, "component": ai.Name}
		svc.Spec.Ports = ports
		svc.Spec.Type = svcType
		return nil
	}); err != nil {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "InvalidSpec", "create/update Service failed")
		return fmt.Errorf("create/update Service: %w", err)
	}
	return nil
}

// reconcileSlimMetricsService creates an internal ClusterIP-only Service carrying the
// metrics port, kept separate from the public Service so that NodePort/LoadBalancer
// exposure never publishes /metrics. It carries the "component" label so the
// ServiceMonitor's selector (app+component) matches it.
func (r *SlimReconciler) reconcileSlimMetricsService(ctx context.Context, ai *aiv1.AIService) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ai.Name + "-slim-metrics-service",
			Namespace: ai.Namespace,
		},
	}
	if err := controllerutil.SetControllerReference(ai, svc, r.Scheme); err != nil {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "InvalidSpec", "ownerref on metrics Service failed")
		return fmt.Errorf("ownerref on metrics Service: %w", err)
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.ObjectMeta.Labels = map[string]string{"app": ai.Name, "component": ai.Name}
		svc.Spec.Selector = map[string]string{"app": ai.Name, "component": ai.Name}
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: "metrics", Port: 8088, TargetPort: intstr.FromInt(8088)},
		}
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		return nil
	}); err != nil {
		r.Recorder.Event(ai, corev1.EventTypeWarning, "InvalidSpec", "create/update metrics Service failed")
		return fmt.Errorf("create/update metrics Service: %w", err)
	}
	return nil
}

func (r *SlimReconciler) reconcileServiceMonitor(ctx context.Context, ai *aiv1.AIService) error {
	if !ai.Spec.Metrics.Enabled {
		return nil
	}

	endpoints := []monitoringv1.Endpoint{
		{Port: "metrics", Path: ai.Spec.Metrics.Path, Scheme: "http"},
	}

	sm := &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: ai.Name + "-metrics", Namespace: ai.Namespace},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{"app": ai.Name, "component": ai.Name},
			},
			Endpoints: endpoints,
		},
	}
	if err := controllerutil.SetControllerReference(ai, sm, r.Scheme); err != nil {
		return err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sm, func() error {
		sm.Spec = monitoringv1.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{"app": ai.Name, "component": ai.Name},
			},
			Endpoints: endpoints,
		}
		return nil
	})
	return err
}

func (r *SlimReconciler) createOrUpdateConfigMap(
	ctx context.Context,
	name string,
	data map[string]string,
	ai *aiv1.AIService,
) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ai.Namespace,
		},
		Data: data,
	}
	if err := controllerutil.SetControllerReference(ai, cm, r.Scheme); err != nil {
		return err
	}

	found := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ai.Namespace}, found)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, cm)
	} else if err != nil {
		return err
	}

	if !reflect.DeepEqual(found.Data, data) {
		found.Data = data
		return r.Update(ctx, found)
	}
	return nil
}

func cleanServiceTemplate(template *corev1.Service) {
	if template == nil {
		return
	}
	template.ObjectMeta.CreationTimestamp = metav1.Time{}
	template.ObjectMeta.DeletionTimestamp = nil
	template.ObjectMeta.DeletionGracePeriodSeconds = nil
	template.ObjectMeta.UID = ""
	template.ObjectMeta.ResourceVersion = ""
	template.ObjectMeta.Generation = 0
	template.ObjectMeta.SelfLink = ""
	template.ObjectMeta.ManagedFields = nil
	template.Status = corev1.ServiceStatus{}
}
