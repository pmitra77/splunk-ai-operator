package agentruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLASReconcileInstallsOwnedReleaseWithDesiredValues(t *testing.T) {
	r, ai, helm := healthyLASReconcileFixture(t, 1, true, corev1.ConditionTrue)
	helm.current = nil

	require.NoError(t, r.Reconcile(context.Background(), ai))

	require.Equal(t, 1, helm.installCalls)
	require.Zero(t, helm.upgradeCalls)
	require.Equal(t, lasReleaseName(ai.Name), helm.current.Name)
	require.Equal(t, string(ai.UID), helm.current.Labels[lasOwnerLabel])
	require.Equal(t, lasChartVersion, helm.current.Chart.Metadata.Version)

	images := helm.current.Config["images"].(map[string]interface{})
	apiImage := images["apiServerImage"].(map[string]interface{})
	require.Equal(t, "registry.example/fixture", apiImage["repository"])
	require.Equal(t, "candidate-1", apiImage["tag"])
	require.Equal(t, "fixture-auth", helm.current.Config["config"].(map[string]interface{})["existingSecretName"])
	require.Equal(t, "fixture-postgres", helm.current.Config["postgres"].(map[string]interface{})["external"].(map[string]interface{})["existingSecretName"])
	require.Equal(t, "fixture-redis", helm.current.Config["redis"].(map[string]interface{})["external"].(map[string]interface{})["existingSecretName"])

	ready := meta.FindStatusCondition(ai.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionTrue, ready.Status)
	require.Equal(t, "LASReady", ready.Reason)
}

func TestLASReconcileUnchangedConfigurationIsNoOp(t *testing.T) {
	r, ai, helm := healthyLASReconcileFixture(t, 1, true, corev1.ConditionTrue)
	initialVersion := helm.current.Version

	for range 2 {
		require.NoError(t, r.Reconcile(context.Background(), ai))
	}

	require.Zero(t, helm.installCalls)
	require.Zero(t, helm.upgradeCalls)
	require.Equal(t, initialVersion, helm.current.Version)
	revision, err := r.credentialRevision(context.Background(), ai)
	require.NoError(t, err)
	require.True(t, sameValues(helm.current.Config, lasValues(ai, revision)))
}

func TestLASReconcileAfterControllerRestartIsNoOp(t *testing.T) {
	r, ai, helm := healthyLASReconcileFixture(t, 1, true, corev1.ConditionTrue)
	ctx := context.Background()
	require.NoError(t, r.Reconcile(ctx, ai))
	initialVersion := helm.current.Version

	restarted := &LASReconciler{
		Client: r.Client, Scheme: r.Scheme, Recorder: r.Recorder,
		healthClient: r.healthClient, helmActionsFactory: r.helmActionsFactory,
	}
	require.NoError(t, restarted.Reconcile(ctx, ai))

	require.Zero(t, helm.installCalls)
	require.Zero(t, helm.upgradeCalls, "a controller restart with unchanged persisted state must not upgrade the release")
	require.Equal(t, initialVersion, helm.current.Version)
}

func TestLASReconcileDesiredConfigurationChangeUpgradesOnce(t *testing.T) {
	r, ai, helm := healthyLASReconcileFixture(t, 1, true, corev1.ConditionTrue)
	initialVersion := helm.current.Version
	ai.Spec.Feature.LAS.Image.Tag = "candidate-2"
	ai.Generation++
	// Keep the fake API server's stored AIService in sync with the desired
	// update before reconciling it, as the real controller receives the new
	// generation from the API server.
	require.NoError(t, r.Update(context.Background(), ai))

	require.NoError(t, r.Reconcile(context.Background(), ai))
	require.Equal(t, 1, helm.upgradeCalls)
	require.Equal(t, initialVersion+1, helm.current.Version)
	images := helm.current.Config["images"].(map[string]interface{})
	require.Equal(t, "candidate-2", images["apiServerImage"].(map[string]interface{})["tag"])

	require.NoError(t, r.Reconcile(context.Background(), ai))
	require.Equal(t, 1, helm.upgradeCalls, "repeating the applied desired configuration must not make another Helm revision")
	require.Equal(t, initialVersion+1, helm.current.Version)
}

func TestLASReconcileRefusesReleaseOwnedByAnotherAIService(t *testing.T) {
	r, ai, helm := healthyLASReconcileFixture(t, 1, true, corev1.ConditionTrue)
	initialVersion := helm.current.Version
	helm.current.Labels[lasOwnerLabel] = "another-aiservice-uid"

	err := r.Reconcile(context.Background(), ai)
	require.ErrorContains(t, err, "is not owned by this AIService")
	require.Zero(t, helm.installCalls)
	require.Zero(t, helm.upgradeCalls)
	require.Equal(t, initialVersion, helm.current.Version)
	ready := meta.FindStatusCondition(ai.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionFalse, ready.Status)
	require.Equal(t, "ReleaseOwnershipConflict", ready.Reason)
}

func TestLASReconcileInstallFailureReportsUnreadyStatus(t *testing.T) {
	r, ai, helm := healthyLASReconcileFixture(t, 1, true, corev1.ConditionTrue)
	helm.current = nil
	helm.installErr = errors.New("fixture install failed")

	err := r.Reconcile(context.Background(), ai)
	require.ErrorContains(t, err, "fixture install failed")
	require.Equal(t, 1, helm.installCalls)
	require.Zero(t, helm.upgradeCalls)
	require.Nil(t, helm.current)
	ready := meta.FindStatusCondition(ai.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionFalse, ready.Status)
	require.Equal(t, "HelmInstallFailed", ready.Reason)
}
