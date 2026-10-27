package slim

import (
	"context"
	"fmt"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
)

const (
	dependencyAgentRuntime = "AgentRuntime"
	dependencyModelServing = "ModelServing"

	reasonDependencyNotReady = "DependencyNotReady"
)

// dependencyState is the readiness of one runtime dependency of SLIM. Messages
// are built only from resource names, generations and condition reasons so they
// are stable between reconciles and never carry credentials.
type dependencyState struct {
	dependency string
	ready      bool
	message    string
}

// dependencyPendingError reports a dependency SLIM cannot be configured
// without. It holds the workload back; it is not a reconcile failure.
type dependencyPendingError struct{ state dependencyState }

func (e *dependencyPendingError) Error() string {
	return fmt.Sprintf("%s is not ready: %s", e.state.dependency, e.state.message)
}

// dependencyStates returns the dependencies that gate SLIM readiness for the
// platform's profile, in a fixed order. AgentRuntime applies whenever the
// platform selects it. Model serving applies to the full profile only; a
// cpuOnly platform has none and never waits for it.
func (r *SlimReconciler) dependencyStates(ctx context.Context, ai *aiv1.AIService) ([]dependencyState, error) {
	platform, err := r.getAIPlatform(ctx, ai.Spec.AIPlatformRef)
	if err != nil {
		return nil, fmt.Errorf("fetching AIPlatform for SLIM readiness: %w", err)
	}

	var states []dependencyState
	if platformHasFeature(platform, "agentruntime") {
		state, err := r.agentRuntimeState(ctx, platform)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	if platform.Spec.Profile != aiv1.AIPlatformProfileCPUOnly {
		state, err := r.modelServingState(ctx, platform)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}

// withHeldDependency makes sure a dependency that held the workload back is
// reported as not ready even if it became ready since it was checked.
func withHeldDependency(states []dependencyState, held *dependencyPendingError) []dependencyState {
	for i := range states {
		if states[i].dependency == held.state.dependency {
			states[i] = held.state
			return states
		}
	}
	return append(states, held.state)
}

// agentRuntimeState is ready only when every LAS AIService owned by the
// platform is current and Ready. It applies the same generation rules as the
// platform's own aggregation of the LAS child.
func (r *SlimReconciler) agentRuntimeState(ctx context.Context, platform *aiv1.AIPlatform) (dependencyState, error) {
	state := dependencyState{dependency: dependencyAgentRuntime}

	var children aiv1.AIServiceList
	if err := r.List(ctx, &children, client.InNamespace(platform.Namespace)); err != nil {
		return state, fmt.Errorf("listing AgentRuntime AIServices: %w", err)
	}

	found := false
	for i := range children.Items {
		child := &children.Items[i]
		if child.Spec.Feature.Name != "agentruntime" || !metav1.IsControlledBy(child, platform) {
			continue
		}
		found = true
		if message := lasNotReadyMessage(child); message != "" {
			state.message = message
			return state, nil
		}
	}
	if !found {
		state.message = "no AgentRuntime AIService exists for this AIPlatform yet"
		return state, nil
	}
	state.ready = true
	return state, nil
}

func lasNotReadyMessage(child *aiv1.AIService) string {
	if child.DeletionTimestamp != nil {
		return fmt.Sprintf("AIService %s is being deleted", child.Name)
	}
	ready := meta.FindStatusCondition(child.Status.Conditions, "Ready")
	switch {
	case child.Status.ObservedGeneration < child.Generation:
		return fmt.Sprintf("AIService %s has not been reconciled for generation %d", child.Name, child.Generation)
	case ready == nil:
		return fmt.Sprintf("AIService %s has not reported Ready for generation %d", child.Name, child.Generation)
	case ready.Status != metav1.ConditionTrue:
		return fmt.Sprintf("AIService %s Ready is %s (reason %s)", child.Name, ready.Status, ready.Reason)
	case ready.ObservedGeneration < child.Generation:
		return fmt.Sprintf("AIService %s Ready was reported for generation %d, not %d", child.Name, ready.ObservedGeneration, child.Generation)
	}
	return ""
}

// modelServingState reads the RayService directly. The AIPlatform's
// RayServiceStatusReady condition only records that the platform's status stage
// ran, not that Ray is serving, so it is not used here. The readiness test
// matches the platform's RayServiceReady condition: the KubeRay Ready condition
// or a Running service status. RayService status.observedGeneration is not
// checked: KubeRay stores status only when something else in it changed, so it
// can stay behind the spec generation indefinitely.
func (r *SlimReconciler) modelServingState(ctx context.Context, platform *aiv1.AIPlatform) (dependencyState, error) {
	state := dependencyState{dependency: dependencyModelServing}

	rayService := &rayv1.RayService{}
	err := r.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.Name}, rayService)
	switch {
	case meta.IsNoMatchError(err):
		state.message = "the RayService API is not installed"
		return state, nil
	case errors.IsNotFound(err):
		state.message = fmt.Sprintf("RayService %s does not exist", platform.Name)
		return state, nil
	case err != nil:
		return state, fmt.Errorf("fetching RayService %s: %w", platform.Name, err)
	}

	switch {
	case rayService.DeletionTimestamp != nil:
		state.message = fmt.Sprintf("RayService %s is being deleted", rayService.Name)
	case !rayServiceServing(rayService):
		state.message = fmt.Sprintf("RayService %s is not ready", rayService.Name)
	case platform.Status.RayServiceName == "":
		state.message = "the Ray service name is not yet published in the AIPlatform status"
	default:
		state.ready = true
	}
	return state, nil
}

func rayServiceServing(rayService *rayv1.RayService) bool {
	if meta.IsStatusConditionTrue(rayService.Status.Conditions, string(rayv1.RayServiceReady)) {
		return true
	}
	return rayService.Status.ServiceStatus == rayv1.Running
}
