package agentruntime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	helmtime "helm.sh/helm/v3/pkg/time"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeLASHelmActions struct {
	current        *release.Release
	history        []*release.Release
	upgradeErr     error
	upgradeCalls   int
	installCalls   int
	rollbackTo     int
	uninstallCalls int
}

type lasRoundTripper func(*http.Request) (*http.Response, error)

func (roundTripper lasRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTripper(request)
}

func lasHealthClient(statusCode int) *http.Client {
	return &http.Client{Transport: lasRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: statusCode, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header), Request: request}, nil
	})}
}

func (h *fakeLASHelmActions) Status(string) (*release.Release, error) {
	if h.current == nil {
		return nil, driver.ErrReleaseNotFound
	}
	return h.current, nil
}

func (h *fakeLASHelmActions) History(string) ([]*release.Release, error) {
	return h.history, nil
}

func (h *fakeLASHelmActions) Install(_ context.Context, name string, ch *chart.Chart, values map[string]interface{}, _ bool, ownerUID string) (*release.Release, error) {
	h.installCalls++
	h.current = testLASRelease(name, ownerUID, release.StatusDeployed, 1, time.Now())
	h.current.Chart, h.current.Config = ch, values
	return h.current, nil
}

func (h *fakeLASHelmActions) Upgrade(_ context.Context, name string, ch *chart.Chart, values map[string]interface{}, ownerUID string) (*release.Release, error) {
	h.upgradeCalls++
	if h.upgradeErr != nil {
		return nil, h.upgradeErr
	}
	h.current = testLASRelease(name, ownerUID, release.StatusDeployed, h.current.Version+1, time.Now())
	h.current.Chart, h.current.Config = ch, values
	return h.current, nil
}

func (h *fakeLASHelmActions) Rollback(name string, version int) error {
	h.rollbackTo = version
	for _, prior := range h.history {
		if prior.Version == version {
			h.current = testLASRelease(name, prior.Labels[lasOwnerLabel], release.StatusDeployed, h.current.Version+1, time.Now())
			h.current.Chart, h.current.Config = prior.Chart, prior.Config
			return nil
		}
	}
	return errors.New("revision not found")
}

func (h *fakeLASHelmActions) Uninstall(string) error {
	h.uninstallCalls++
	h.current = nil
	return nil
}

func testLASRelease(name, owner string, status release.Status, version int, started time.Time) *release.Release {
	result := &release.Release{
		Name: name, Version: version, Labels: map[string]string{lasOwnerLabel: owner},
		Info:   &release.Info{Status: status, FirstDeployed: helmtime.Time{Time: started}, LastDeployed: helmtime.Time{Time: started}},
		Chart:  &chart.Chart{Metadata: &chart.Metadata{Version: lasChartVersion}},
		Config: map[string]interface{}{"image": "previous"},
	}
	if status == release.StatusUninstalling {
		result.Info.Deleted = helmtime.Time{Time: started}
	}
	return result
}

func testLASReconciler(t *testing.T, helm *fakeLASHelmActions, includeSecrets bool, extra ...client.Object) (*LASReconciler, *aiv1.AIService) {
	t.Helper()
	ai := &aiv1.AIService{
		ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "default", UID: types.UID("service-uid"), Generation: 1},
		Spec: aiv1.AIServiceSpec{Feature: aiv1.FeatureSpec{
			Name: "agentruntime", LicenseSecretRef: "fixture-auth", PostgresSecretRef: "fixture-postgres", RedisSecretRef: "fixture-redis",
			LAS: &aiv1.LASFeatureSpec{Image: aiv1.LASImageSpec{Repository: "registry.example/fixture", Tag: "candidate-1"}, AssistantID: "fixture"},
		}},
	}
	objects := []client.Object{ai}
	if includeSecrets {
		objects = append(objects,
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fixture-auth", Namespace: "default", UID: types.UID("auth-uid")}, Data: map[string][]byte{"langgraph_cloud_license_key": []byte("fixture-license")}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fixture-postgres", Namespace: "default", UID: types.UID("postgres-uid")}, Data: map[string][]byte{"postgres_connection_url": []byte("postgresql://fixture")}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fixture-redis", Namespace: "default", UID: types.UID("redis-uid")}, Data: map[string][]byte{"redis_connection_url": []byte("redis://fixture")}},
		)
	}
	objects = append(objects, extra...)
	scheme := buildAgentRuntimeTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&aiv1.AIService{}, &appsv1.Deployment{}).WithObjects(objects...).Build()
	r := &LASReconciler{Client: c, Scheme: scheme, healthClient: lasHealthClient(http.StatusOK), helmActionsFactory: func(context.Context, string) (lasHelmActions, error) { return helm, nil }}
	return r, ai
}

func TestLASPendingActionObservationAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         release.Status
		age            time.Duration
		wantReason     string
		wantRollbackTo int
		wantUninstall  int
	}{
		{name: "recent upgrade is observed", status: release.StatusPendingUpgrade, age: time.Minute, wantReason: "HelmActionPending"},
		{name: "stale upgrade restores owned revision", status: release.StatusPendingUpgrade, age: 10 * time.Minute, wantReason: "DesiredRevisionNotApplied", wantRollbackTo: 1},
		{name: "stale install is uninstalled", status: release.StatusPendingInstall, age: 10 * time.Minute, wantReason: "DesiredRevisionNotApplied", wantUninstall: 1},
		{name: "stale uninstall is completed", status: release.StatusUninstalling, age: 10 * time.Minute, wantReason: "DesiredRevisionNotApplied", wantUninstall: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helm := &fakeLASHelmActions{current: testLASRelease("las-fixture", "service-uid", tc.status, 2, time.Now().Add(-tc.age))}
			helm.history = []*release.Release{testLASRelease("las-fixture", "service-uid", release.StatusDeployed, 1, time.Now().Add(-time.Hour)), helm.current}
			r, ai := testLASReconciler(t, helm, true)
			require.NoError(t, r.Reconcile(context.Background(), ai))
			condition := meta.FindStatusCondition(ai.Status.Conditions, "Ready")
			require.NotNil(t, condition)
			require.Equal(t, tc.wantReason, condition.Reason)
			require.Equal(t, tc.wantRollbackTo, helm.rollbackTo)
			require.Equal(t, tc.wantUninstall, helm.uninstallCalls)
			require.Zero(t, helm.upgradeCalls, "pending action must not trigger another upgrade")
			require.Zero(t, helm.installCalls, "pending action must not trigger another install")
		})
	}
}

func TestLASPendingRecoveryRefusesForeignHistory(t *testing.T) {
	helm := &fakeLASHelmActions{current: testLASRelease("las-fixture", "service-uid", release.StatusPendingRollback, 2, time.Now().Add(-10*time.Minute))}
	helm.history = []*release.Release{testLASRelease("las-fixture", "foreign-uid", release.StatusDeployed, 1, time.Now().Add(-time.Hour)), helm.current}
	r, ai := testLASReconciler(t, helm, true)
	require.ErrorContains(t, r.Reconcile(context.Background(), ai), "another owner")
	require.Zero(t, helm.rollbackTo)
	require.Equal(t, "ReleaseOwnershipConflict", meta.FindStatusCondition(ai.Status.Conditions, "Ready").Reason)
}

func TestLASPendingRecoveryRequiresSafeHistoryAndTimestamp(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configure  func(*fakeLASHelmActions)
		wantReason string
	}{
		{name: "no deployed history", configure: func(h *fakeLASHelmActions) { h.history = []*release.Release{h.current} }, wantReason: "HelmRecoveryUnsafe"},
		{name: "missing timestamp", configure: func(h *fakeLASHelmActions) {
			h.current.Info.FirstDeployed = helmtime.Time{}
			h.current.Info.LastDeployed = helmtime.Time{}
		}, wantReason: "HelmPendingStateInvalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helm := &fakeLASHelmActions{current: testLASRelease("las-fixture", "service-uid", release.StatusPendingUpgrade, 2, time.Now().Add(-10*time.Minute))}
			tc.configure(helm)
			r, ai := testLASReconciler(t, helm, true)
			require.Error(t, r.Reconcile(context.Background(), ai))
			require.Equal(t, tc.wantReason, meta.FindStatusCondition(ai.Status.Conditions, "Ready").Reason)
			require.Zero(t, helm.upgradeCalls)
			require.Zero(t, helm.rollbackTo)
		})
	}
}

func TestLASFailedDesiredUpgradeStaysUnreadyAndRetriesAfterSecretCorrection(t *testing.T) {
	helm := &fakeLASHelmActions{current: testLASRelease("las-fixture", "service-uid", release.StatusDeployed, 1, time.Now().Add(-time.Hour)), upgradeErr: errors.New("fixture upgrade failed")}
	r, ai := testLASReconciler(t, helm, false)
	ctx := context.Background()
	require.ErrorContains(t, r.Reconcile(ctx, ai), "fixture-auth")
	require.Zero(t, helm.upgradeCalls)
	for _, secret := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: "fixture-auth", Namespace: "default"}, Data: map[string][]byte{"langgraph_cloud_license_key": []byte("fixture-license")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "fixture-postgres", Namespace: "default"}, Data: map[string][]byte{"postgres_connection_url": []byte("postgresql://fixture")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "fixture-redis", Namespace: "default"}, Data: map[string][]byte{"redis_connection_url": []byte("redis://fixture")}},
	} {
		require.NoError(t, r.Create(ctx, secret))
	}
	require.ErrorContains(t, r.Reconcile(ctx, ai), "fixture upgrade failed")
	require.Equal(t, 1, helm.upgradeCalls)
	condition := meta.FindStatusCondition(ai.Status.Conditions, "Ready")
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, "HelmUpgradeFailed", condition.Reason)
	require.False(t, lasDesiredReleaseApplied(helm.current, lasValues(ai)), "old release cannot count as desired generation")
}

func TestLASFinalizeAbsentReleasePreservesSecretsAndScopesServiceDeletion(t *testing.T) {
	ctx := context.Background()
	owned := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "fixture-svc", Namespace: "default", OwnerReferences: []metav1.OwnerReference{{APIVersion: aiv1.GroupVersion.String(), Kind: "AIService", Name: "fixture", UID: types.UID("service-uid"), Controller: ptrBool(true)}}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fixture-auth", Namespace: "default"}, Data: map[string][]byte{"langgraph_cloud_license_key": []byte("fixture-license")}}
	helm := &fakeLASHelmActions{}
	r, ai := testLASReconciler(t, helm, false, owned, secret)
	require.NoError(t, r.Finalize(ctx, ai))
	require.NoError(t, r.Finalize(ctx, ai), "deletion must be idempotent")
	require.Error(t, r.Get(ctx, client.ObjectKeyFromObject(owned), &corev1.Service{}))
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{}))
	require.Zero(t, helm.uninstallCalls)

	foreign := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "fixture-svc", Namespace: "default"}}
	require.NoError(t, r.Create(ctx, foreign))
	require.NoError(t, r.Finalize(ctx, ai))
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.Service{}), "foreign Service must be preserved")
}

func TestLASFinalizeRefusesForeignUninstalledRelease(t *testing.T) {
	helm := &fakeLASHelmActions{current: testLASRelease("las-fixture", "foreign-uid", release.StatusUninstalled, 2, time.Now())}
	r, ai := testLASReconciler(t, helm, false)
	require.ErrorContains(t, r.Finalize(context.Background(), ai), "owner UID does not match")
	require.Zero(t, helm.uninstallCalls)
}

func TestLASFinalizeWaitsForActivePendingAction(t *testing.T) {
	helm := &fakeLASHelmActions{current: testLASRelease("las-fixture", "service-uid", release.StatusPendingUpgrade, 2, time.Now())}
	r, ai := testLASReconciler(t, helm, false)
	require.ErrorContains(t, r.Finalize(context.Background(), ai), "waiting for pending")
	require.Zero(t, helm.uninstallCalls)
	helm.current.Info.LastDeployed = helmtime.Time{Time: time.Now().Add(-10 * time.Minute)}
	require.NoError(t, r.Finalize(context.Background(), ai))
	require.Equal(t, 1, helm.uninstallCalls)
}

func ptrBool(value bool) *bool { return &value }
