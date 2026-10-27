package slim

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
)

const (
	gateNS       = "ai-platform"
	gatePlatform = "launchpad"
	gateLAS      = "launchpad-agentruntime-mltk"
	gateSlim     = "launchpad-slim"
)

var (
	gateSlimFeature = aiv1.FeatureSpec{Name: "slim"}
	gateLASFeature  = aiv1.FeatureSpec{Name: "agentruntime", Provider: "mltk", LAS: &aiv1.LASFeatureSpec{AssistantID: "echo"}}
	gateSAIA        = aiv1.FeatureSpec{Name: "saia"}
)

type gateEnv struct {
	t        *testing.T
	scheme   *runtime.Scheme
	client   client.Client
	r        *SlimReconciler
	recorder *record.FakeRecorder
	platform *aiv1.AIPlatform
}

type gateOpts struct {
	profile  aiv1.AIPlatformProfile
	features []aiv1.FeatureSpec
	withRay  bool
	objects  []client.Object
	funcs    *interceptor.Funcs
}

func newGateEnv(t *testing.T, o gateOpts) *gateEnv {
	t.Helper()
	t.Setenv("RELATED_IMAGE_SLIM_API", "slim:test")

	scheme := buildTestScheme(t)
	if o.withRay {
		require.NoError(t, rayv1.AddToScheme(scheme))
	}
	platform := &aiv1.AIPlatform{
		ObjectMeta: metav1.ObjectMeta{Name: gatePlatform, Namespace: gateNS, UID: types.UID("platform-uid")},
		Spec:       aiv1.AIPlatformSpec{Profile: o.profile, Features: o.features},
	}
	if o.profile != aiv1.AIPlatformProfileCPUOnly {
		platform.Status.RayServiceName = gatePlatform + "-head-svc"
	}
	slimSvc := &aiv1.AIService{
		ObjectMeta: metav1.ObjectMeta{Name: gateSlim, Namespace: gateNS, UID: types.UID("slim-uid")},
		Spec: aiv1.AIServiceSpec{
			Feature:       aiv1.FeatureSpec{Name: "slim"},
			AIPlatformRef: corev1.ObjectReference{Name: gatePlatform, Namespace: gateNS},
		},
	}
	objs := append([]client.Object{platform, slimSvc}, o.objects...)
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&aiv1.AIService{}, &aiv1.AIPlatform{}, &appsv1.Deployment{})
	if o.withRay {
		b = b.WithStatusSubresource(&rayv1.RayService{})
	}
	if o.funcs != nil {
		b = b.WithInterceptorFuncs(*o.funcs)
	}
	c := b.Build()
	rec := record.NewFakeRecorder(50)
	return &gateEnv{t: t, scheme: scheme, client: c, recorder: rec, platform: platform,
		r: &SlimReconciler{Client: c, Scheme: scheme, Recorder: rec}}
}

// reconcile runs the real SLIM reconcile. A CPU-only platform also waits for
// its Deployment, so the Deployment is made available after the first pass to
// let the reconcile reach the dependency gates.
func (e *gateEnv) reconcile() error {
	e.t.Helper()
	ctx := context.Background()
	for pass := 0; pass < 2; pass++ {
		ai := e.slim()
		err := e.r.Reconcile(ctx, ai)
		dep := &appsv1.Deployment{}
		key := types.NamespacedName{Name: gateSlim + "-slim-deployment", Namespace: gateNS}
		if getErr := e.client.Get(ctx, key, dep); getErr != nil || (dep.Status.AvailableReplicas >= 1 && dep.Status.ReadyReplicas >= 1) {
			return err
		}
		dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
		require.NoError(e.t, e.client.Status().Update(ctx, dep))
	}
	return e.r.Reconcile(ctx, e.slim())
}

func (e *gateEnv) slim() *aiv1.AIService {
	e.t.Helper()
	ai := &aiv1.AIService{}
	require.NoError(e.t, e.client.Get(context.Background(), types.NamespacedName{Name: gateSlim, Namespace: gateNS}, ai))
	return ai
}

func (e *gateEnv) cond(condType string) *metav1.Condition {
	e.t.Helper()
	return meta.FindStatusCondition(e.slim().Status.Conditions, condType)
}

func (e *gateEnv) deploymentExists() bool {
	err := e.client.Get(context.Background(), types.NamespacedName{Name: gateSlim + "-slim-deployment", Namespace: gateNS}, &appsv1.Deployment{})
	return err == nil
}

func (e *gateEnv) setLAS(mutate func(*aiv1.AIService)) {
	e.t.Helper()
	las := &aiv1.AIService{}
	require.NoError(e.t, e.client.Get(context.Background(), types.NamespacedName{Name: gateLAS, Namespace: gateNS}, las))
	mutate(las)
	require.NoError(e.t, e.client.Status().Update(context.Background(), las))
}

func (e *gateEnv) setRay(mutate func(*rayv1.RayService)) {
	e.t.Helper()
	rs := &rayv1.RayService{}
	require.NoError(e.t, e.client.Get(context.Background(), types.NamespacedName{Name: gatePlatform, Namespace: gateNS}, rs))
	mutate(rs)
	require.NoError(e.t, e.client.Status().Update(context.Background(), rs))
}

// events drains the recorder and returns only the dependency-readiness Events.
func (e *gateEnv) events() []string {
	var out []string
	for {
		select {
		case ev := <-e.recorder.Events:
			if strings.Contains(ev, reasonDependencyNotReady) || strings.Contains(ev, "DependenciesReady") {
				out = append(out, ev)
			}
		default:
			return out
		}
	}
}

func lasChild(ready metav1.ConditionStatus, reason string) *aiv1.AIService {
	return &aiv1.AIService{
		ObjectMeta: metav1.ObjectMeta{
			Name: gateLAS, Namespace: gateNS, Generation: 3,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "ai.splunk.com/v1", Kind: "AIPlatform", Name: gatePlatform,
				UID: types.UID("platform-uid"), Controller: boolPtr(true),
			}},
		},
		Spec: aiv1.AIServiceSpec{Feature: gateLASFeature},
		Status: aiv1.AIServiceStatus{
			ObservedGeneration: 3,
			Conditions: []metav1.Condition{{
				Type: "Ready", Status: ready, Reason: reason, ObservedGeneration: 3,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

func rayService(ready bool) *rayv1.RayService {
	rs := &rayv1.RayService{
		ObjectMeta: metav1.ObjectMeta{Name: gatePlatform, Namespace: gateNS, Generation: 2},
		Status:     rayv1.RayServiceStatuses{ObservedGeneration: 2},
	}
	if ready {
		rs.Status.Conditions = []metav1.Condition{{Type: string(rayv1.RayServiceReady), Status: metav1.ConditionTrue, Reason: "Running", LastTransitionTime: metav1.Now()}}
	}
	return rs
}

func boolPtr(b bool) *bool { return &b }

func cpuOnlyOpts(objs ...client.Object) gateOpts {
	return gateOpts{profile: aiv1.AIPlatformProfileCPUOnly, features: []aiv1.FeatureSpec{gateSlimFeature, gateLASFeature}, objects: objs}
}

func fullOpts(features []aiv1.FeatureSpec, objs ...client.Object) gateOpts {
	return gateOpts{profile: aiv1.AIPlatformProfileFull, features: features, withRay: true, objects: objs}
}

func requireBlocked(t *testing.T, e *gateEnv, wantInMessage ...string) {
	t.Helper()
	ready := e.cond("Ready")
	require.NotNil(t, ready, "SLIM must publish Ready=False, not drop the condition")
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, reasonDependencyNotReady, ready.Reason)
	for _, want := range wantInMessage {
		assert.Contains(t, ready.Message, want)
	}
}

// --- CPU-only -----------------------------------------------------------

func TestSlimCPUOnly_ReadyWhenAgentRuntimeReadyAndNoModelServingExists(t *testing.T) {
	// The scheme deliberately has no RayService type: any attempt to read one
	// would fail the reconcile.
	e := newGateEnv(t, cpuOnlyOpts(lasChild(metav1.ConditionTrue, "Ready")))
	require.NoError(t, e.reconcile())

	ready := e.cond("Ready")
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, metav1.ConditionTrue, e.cond("AgentRuntimeReady").Status)
	assert.Nil(t, e.cond("ModelServingReady"), "cpuOnly has no model serving condition to wait for")
}

func TestSlimCPUOnly_AgentRuntimeLossMakesSlimUnreadyThenRecovers(t *testing.T) {
	e := newGateEnv(t, cpuOnlyOpts(lasChild(metav1.ConditionTrue, "Ready")))
	require.NoError(t, e.reconcile())
	require.Equal(t, metav1.ConditionTrue, e.cond("Ready").Status)
	becameReady := e.cond("Ready").LastTransitionTime
	e.events()

	e.setLAS(func(las *aiv1.AIService) {
		las.Status.Conditions[0].Status = metav1.ConditionFalse
		las.Status.Conditions[0].Reason = "APIUnavailable"
	})
	require.NoError(t, e.reconcile(), "an unready dependency is reported, not returned as an error that backs off")
	requireBlocked(t, e, "AgentRuntime", gateLAS, "APIUnavailable")
	assert.Equal(t, metav1.ConditionFalse, e.cond("AgentRuntimeReady").Status)
	assert.Equal(t, reasonDependencyNotReady, e.cond("AgentRuntimeReady").Reason)
	require.Len(t, e.events(), 1)

	e.setLAS(func(las *aiv1.AIService) {
		las.Status.Conditions[0].Status = metav1.ConditionTrue
		las.Status.Conditions[0].Reason = "Ready"
	})
	require.NoError(t, e.reconcile())
	ready := e.cond("Ready")
	assert.Equal(t, metav1.ConditionTrue, ready.Status, "recovery needs no manual restart")
	assert.Equal(t, "AllReconciled", ready.Reason)
	assert.Equal(t, metav1.ConditionTrue, e.cond("AgentRuntimeReady").Status)
	assert.False(t, ready.LastTransitionTime.Before(&becameReady))
	evs := e.events()
	require.Len(t, evs, 1)
	assert.Contains(t, evs[0], corev1.EventTypeNormal)
}

func TestSlimCPUOnly_AgentRuntimeStatesThatBlockReadiness(t *testing.T) {
	cases := []struct {
		name    string
		las     func() *aiv1.AIService
		want    string
		noChild bool
	}{
		{name: "no AgentRuntime AIService yet", noChild: true, want: "no AgentRuntime AIService exists"},
		{name: "Ready condition not reported", want: "has not reported Ready", las: func() *aiv1.AIService {
			l := lasChild(metav1.ConditionTrue, "Ready")
			l.Status.Conditions = nil
			return l
		}},
		{name: "Ready is Unknown", want: "Ready is Unknown", las: func() *aiv1.AIService { return lasChild(metav1.ConditionUnknown, "Progressing") }},
		{name: "spec generation not yet reconciled", want: "has not been reconciled for generation 4", las: func() *aiv1.AIService {
			l := lasChild(metav1.ConditionTrue, "Ready")
			l.Generation = 4
			return l
		}},
		{name: "stale Ready from an older generation", want: "Ready was reported for generation 2, not 3", las: func() *aiv1.AIService {
			l := lasChild(metav1.ConditionTrue, "Ready")
			l.Status.Conditions[0].ObservedGeneration = 2
			return l
		}},
		{name: "being deleted", want: "is being deleted", las: func() *aiv1.AIService {
			l := lasChild(metav1.ConditionTrue, "Ready")
			now := metav1.Now()
			l.DeletionTimestamp = &now
			l.Finalizers = []string{"ai.splunk.com/aiservice-protect"}
			return l
		}},
		{name: "AgentRuntime child without a las block is still the dependency", want: "has not reported Ready", las: func() *aiv1.AIService {
			l := lasChild(metav1.ConditionTrue, "Ready")
			l.Spec.Feature.LAS = nil
			l.Status.Conditions = nil
			return l
		}},
		{name: "owned by a different platform", want: "no AgentRuntime AIService exists", las: func() *aiv1.AIService {
			l := lasChild(metav1.ConditionTrue, "Ready")
			l.OwnerReferences[0].UID = types.UID("someone-else")
			return l
		}},
		{name: "not controlled by anything", want: "no AgentRuntime AIService exists", las: func() *aiv1.AIService {
			l := lasChild(metav1.ConditionTrue, "Ready")
			l.OwnerReferences = nil
			return l
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []client.Object
			if !tc.noChild {
				objs = append(objs, tc.las())
			}
			e := newGateEnv(t, cpuOnlyOpts(objs...))
			require.NoError(t, e.reconcile())
			requireBlocked(t, e, "AgentRuntime: ", tc.want)
		})
	}
}

func TestSlimCPUOnly_EveryAgentRuntimeAIServiceMustBeReady(t *testing.T) {
	second := lasChild(metav1.ConditionFalse, "Starting")
	second.Name = "launchpad-agentruntime-other"
	e := newGateEnv(t, cpuOnlyOpts(lasChild(metav1.ConditionTrue, "Ready"), second))
	require.NoError(t, e.reconcile())
	requireBlocked(t, e, "launchpad-agentruntime-other", "Starting")
}

func TestSlimCPUOnly_WorkloadIsDeployedWhileAgentRuntimeIsNotReady(t *testing.T) {
	e := newGateEnv(t, cpuOnlyOpts(lasChild(metav1.ConditionFalse, "Starting")))
	require.NoError(t, e.reconcile())
	requireBlocked(t, e, "AgentRuntime")
	assert.True(t, e.deploymentExists(), "the workload is created ahead of the dependency so Ready only waits on the dependency")
}

// --- Full -----------------------------------------------------------

var fullWithSAIA = []aiv1.FeatureSpec{gateSAIA, gateSlimFeature, gateLASFeature}

func TestSlimFull_ReadyOnlyWhenBothDependenciesAreReady(t *testing.T) {
	e := newGateEnv(t, fullOpts(fullWithSAIA, lasChild(metav1.ConditionTrue, "Ready"), rayService(true)))
	require.NoError(t, e.reconcile())
	assert.Equal(t, metav1.ConditionTrue, e.cond("Ready").Status)
	assert.Equal(t, metav1.ConditionTrue, e.cond("AgentRuntimeReady").Status)
	assert.Equal(t, metav1.ConditionTrue, e.cond("ModelServingReady").Status)
}

func TestSlimFull_UnreadyAgentRuntimeBlocksEvenWhenModelServingIsReady(t *testing.T) {
	e := newGateEnv(t, fullOpts(fullWithSAIA, lasChild(metav1.ConditionFalse, "APIUnavailable"), rayService(true)))
	require.NoError(t, e.reconcile())
	requireBlocked(t, e, "AgentRuntime", "APIUnavailable")
	assert.NotContains(t, e.cond("Ready").Message, "ModelServing")
	assert.Equal(t, metav1.ConditionTrue, e.cond("ModelServingReady").Status)
}

func TestSlimFull_UnreadyModelServingBlocksEvenWhenAgentRuntimeIsReady(t *testing.T) {
	e := newGateEnv(t, fullOpts(fullWithSAIA, lasChild(metav1.ConditionTrue, "Ready"), rayService(false)))
	require.NoError(t, e.reconcile(), "an unready dependency is not a reconcile error")
	requireBlocked(t, e, "ModelServing", "RayService "+gatePlatform+" is not ready")
	assert.NotContains(t, e.cond("Ready").Message, "AgentRuntime")
	assert.Equal(t, metav1.ConditionTrue, e.cond("AgentRuntimeReady").Status)
	assert.False(t, e.deploymentExists(), "with SAIA the endpoint URL comes from Ray, so the workload waits for it as before")
}

func TestSlimFull_NamesEveryBlockingDependency(t *testing.T) {
	e := newGateEnv(t, fullOpts(fullWithSAIA, lasChild(metav1.ConditionFalse, "APIUnavailable"), rayService(false)))
	require.NoError(t, e.reconcile())
	requireBlocked(t, e, "AgentRuntime: ", "; ModelServing: ")
}

func TestSlimFull_RecoversWhenModelServingReturns(t *testing.T) {
	e := newGateEnv(t, fullOpts(fullWithSAIA, lasChild(metav1.ConditionTrue, "Ready"), rayService(false)))
	require.NoError(t, e.reconcile())
	requireBlocked(t, e, "ModelServing")

	e.setRay(func(rs *rayv1.RayService) {
		rs.Status.Conditions = []metav1.Condition{{Type: string(rayv1.RayServiceReady), Status: metav1.ConditionTrue, Reason: "Running", LastTransitionTime: metav1.Now()}}
	})
	require.NoError(t, e.reconcile())
	assert.Equal(t, metav1.ConditionTrue, e.cond("Ready").Status)
	assert.True(t, e.deploymentExists())
}

func TestSlimFull_WithoutSAIAAlsoHoldsFirstCreationUntilModelServingIsReady(t *testing.T) {
	e := newGateEnv(t, fullOpts([]aiv1.FeatureSpec{gateSlimFeature, gateLASFeature}, lasChild(metav1.ConditionTrue, "Ready"), rayService(false)))
	require.NoError(t, e.reconcileWithURL())
	requireBlocked(t, e, "ModelServing")
	assert.False(t, e.deploymentExists(), "the hold follows the profile, not the SAIA feature")

	e.setRay(func(rs *rayv1.RayService) {
		rs.Status.Conditions = []metav1.Condition{{Type: string(rayv1.RayServiceReady), Status: metav1.ConditionTrue, Reason: "Running", LastTransitionTime: metav1.Now()}}
	})
	require.NoError(t, e.reconcile())
	assert.True(t, e.deploymentExists())
	assert.Equal(t, metav1.ConditionTrue, e.cond("Ready").Status)
}

func TestSlimFull_WithoutSAIAExistingWorkloadKeepsReconcilingThroughAnOutage(t *testing.T) {
	e := newGateEnv(t, fullOpts([]aiv1.FeatureSpec{gateSlimFeature, gateLASFeature}, lasChild(metav1.ConditionTrue, "Ready"), rayService(true)))
	require.NoError(t, e.reconcileWithURL())
	require.True(t, e.deploymentExists())

	e.setRay(func(rs *rayv1.RayService) { rs.Status.Conditions = nil })
	require.NoError(t, e.reconcile())
	requireBlocked(t, e, "ModelServing")
	assert.NotNil(t, e.cond("ValidateReady"))
}

// reconcileWithURL gives the SLIM service the explicit model URL that a
// platform without SAIA would configure on its child.
func (e *gateEnv) reconcileWithURL() error {
	ai := e.slim()
	ai.Spec.AIPlatformUrl = "http://models.example.svc:8000"
	require.NoError(e.t, e.client.Update(context.Background(), ai))
	return e.reconcile()
}

func TestSlimFull_OmittedProfileIsTreatedAsFull(t *testing.T) {
	o := fullOpts(fullWithSAIA, lasChild(metav1.ConditionTrue, "Ready"), rayService(false))
	o.profile = ""
	e := newGateEnv(t, o)
	require.NoError(t, e.reconcile())
	requireBlocked(t, e, "ModelServing")
}

func TestModelServingState(t *testing.T) {
	running := rayService(false)
	running.Status.ServiceStatus = rayv1.Running

	lagging := rayService(true)
	lagging.Status.ObservedGeneration = 1

	deleting := rayService(true)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"ray.io/finalizer"}

	cases := []struct {
		name      string
		ray       *rayv1.RayService
		rayName   string
		wantReady bool
		want      string
	}{
		{name: "Ready condition", ray: rayService(true), rayName: "head", wantReady: true},
		{name: "Running service status", ray: running, rayName: "head", wantReady: true},
		{name: "not ready", ray: rayService(false), rayName: "head", want: "is not ready"},
		{name: "missing RayService", rayName: "head", want: "does not exist"},
		{name: "status generation behind spec is not a block", ray: lagging, rayName: "head", wantReady: true},
		{name: "deleting", ray: deleting, rayName: "head", want: "is being deleted"},
		{name: "service name not published", ray: rayService(true), want: "not yet published"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []client.Object
			if tc.ray != nil {
				objs = append(objs, tc.ray)
			}
			e := newGateEnv(t, fullOpts(fullWithSAIA, objs...))
			e.platform.Status.RayServiceName = tc.rayName
			state, err := e.r.modelServingState(context.Background(), e.platform)
			require.NoError(t, err)
			assert.Equal(t, tc.wantReady, state.ready)
			assert.Contains(t, state.message, tc.want)
		})
	}
}

func TestModelServingState_RayAPINotInstalledIsNotReadyNotAnError(t *testing.T) {
	// A full platform on a cluster without the KubeRay CRDs: the API server
	// answers with a "no match for kind" error.
	funcs := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*rayv1.RayService); ok {
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "ray.io", Kind: "RayService"}}
		}
		return c.Get(ctx, key, obj, opts...)
	}}
	o := fullOpts(fullWithSAIA, lasChild(metav1.ConditionTrue, "Ready"))
	o.funcs = &funcs
	e := newGateEnv(t, o)

	state, err := e.r.modelServingState(context.Background(), e.platform)
	require.NoError(t, err)
	assert.False(t, state.ready)
	assert.Contains(t, state.message, "not installed")

	require.NoError(t, e.reconcile())
	requireBlocked(t, e, "ModelServing", "not installed")
}

func TestModelServingState_LookupFailureIsAnError(t *testing.T) {
	funcs := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*rayv1.RayService); ok {
			return errors.New("connection refused")
		}
		return c.Get(ctx, key, obj, opts...)
	}}
	o := fullOpts(fullWithSAIA)
	o.funcs = &funcs
	e := newGateEnv(t, o)

	_, err := e.r.modelServingState(context.Background(), e.platform)
	require.ErrorContains(t, err, "connection refused")
}

func TestWithHeldDependency_NeverReportsAHeldDependencyAsReady(t *testing.T) {
	held := &dependencyPendingError{state: dependencyState{dependency: dependencyModelServing, message: "RayService x is not ready"}}

	got := withHeldDependency([]dependencyState{{dependency: dependencyAgentRuntime, ready: true}, {dependency: dependencyModelServing, ready: true}}, held)
	require.Len(t, got, 2)
	assert.True(t, got[0].ready)
	assert.False(t, got[1].ready, "the dependency that held the workload back stays unready for this pass")
	assert.Equal(t, "RayService x is not ready", got[1].message)

	got = withHeldDependency(nil, held)
	require.Len(t, got, 1)
	assert.False(t, got[0].ready)
}

// --- cross-cutting ---------------------------------------------------------

func TestSlimDependencyMessagesNeverCarryDependencyFreeText(t *testing.T) {
	las := lasChild(metav1.ConditionFalse, "PostgresUnavailable")
	las.Status.Conditions[0].Message = "dial postgres://admin:hunter2@db.internal:5432 failed"
	e := newGateEnv(t, cpuOnlyOpts(las))
	require.NoError(t, e.reconcile())

	requireBlocked(t, e, "PostgresUnavailable")
	for _, c := range e.slim().Status.Conditions {
		assert.NotContains(t, c.Message, "hunter2", "condition %s", c.Type)
		assert.NotContains(t, c.Message, "postgres://", "condition %s", c.Type)
	}
	for _, ev := range e.events() {
		assert.NotContains(t, ev, "hunter2")
	}
}

func TestSlimReadyConditionIsStableAcrossRepeatedReconciles(t *testing.T) {
	// The platform wakes on any change to a child's Ready status, reason,
	// message or observed generation, so a polling reconcile must not alter them.
	e := newGateEnv(t, cpuOnlyOpts(lasChild(metav1.ConditionFalse, "Starting")))
	require.NoError(t, e.reconcile())
	first := *e.cond("Ready")
	require.Len(t, e.events(), 1)

	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, e.reconcile())
	second := *e.cond("Ready")

	assert.Equal(t, first.Status, second.Status)
	assert.Equal(t, first.Reason, second.Reason)
	assert.Equal(t, first.Message, second.Message)
	assert.True(t, first.LastTransitionTime.Equal(&second.LastTransitionTime), "LastTransitionTime only moves when the status changes")
	assert.Empty(t, e.events(), "an unchanged block does not re-emit an Event")
}

func TestSlimEmitsAnEventWhenTheBlockingDependencyChanges(t *testing.T) {
	e := newGateEnv(t, fullOpts(fullWithSAIA, lasChild(metav1.ConditionFalse, "Starting"), rayService(true)))
	require.NoError(t, e.reconcile())
	require.Len(t, e.events(), 1)

	e.setLAS(func(las *aiv1.AIService) { las.Status.Conditions[0].Status = metav1.ConditionTrue })
	e.setRay(func(rs *rayv1.RayService) { rs.Status.Conditions = nil })
	require.NoError(t, e.reconcile())
	requireBlocked(t, e, "ModelServing")
	evs := e.events()
	require.Len(t, evs, 1)
	assert.Contains(t, evs[0], "ModelServing")
}

func TestSlimDependencyLookupFailureKeepsTheLastKnownReadiness(t *testing.T) {
	failLookups := false
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*aiv1.AIServiceList); ok && failLookups {
			return errors.New("apiserver timeout for 10.0.0.7")
		}
		return c.List(ctx, list, opts...)
	}}
	o := cpuOnlyOpts(lasChild(metav1.ConditionTrue, "Ready"))
	o.funcs = &funcs
	e := newGateEnv(t, o)
	require.NoError(t, e.reconcile())
	before := *e.cond("Ready")
	require.Equal(t, metav1.ConditionTrue, before.Status)
	e.events()

	failLookups = true
	require.Error(t, e.reconcile(), "a lookup failure is a real error and is retried")

	after := e.cond("Ready")
	require.NotNil(t, after)
	assert.Equal(t, before.Status, after.Status, "a transient lookup error must not flip a Ready SLIM")
	assert.Equal(t, before.Reason, after.Reason)
	assert.Equal(t, before.Message, after.Message, "an unchanged Ready message does not wake the platform")
	assert.NotNil(t, e.cond("AgentRuntimeReady"))
	assert.Empty(t, e.events())

	failLookups = false
	require.NoError(t, e.reconcile())
	assert.Equal(t, metav1.ConditionTrue, e.cond("Ready").Status)
	assert.Empty(t, e.events(), "recovering from a lookup error is not a dependency recovery")
}

func TestSlimDependencyLookupFailureOnFirstReconcileDoesNotInventReady(t *testing.T) {
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*aiv1.AIServiceList); ok {
			return errors.New("apiserver timeout")
		}
		return c.List(ctx, list, opts...)
	}}
	o := cpuOnlyOpts()
	o.funcs = &funcs
	e := newGateEnv(t, o)
	require.Error(t, e.reconcile())
	assert.Nil(t, e.cond("Ready"))
}

func TestSlimEventsAreOnlyEmittedAfterTheStatusIsStored(t *testing.T) {
	failWrites := true
	funcs := interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if _, ok := obj.(*aiv1.AIService); ok && failWrites {
			return errors.New("conflict")
		}
		return c.SubResource(sub).Update(ctx, obj, opts...)
	}}
	o := cpuOnlyOpts(lasChild(metav1.ConditionFalse, "Starting"))
	o.funcs = &funcs
	e := newGateEnv(t, o)

	require.NoError(t, e.reconcile())
	require.NoError(t, e.reconcile())
	assert.Empty(t, e.events(), "no Event while the status that justifies it cannot be written")

	failWrites = false
	require.NoError(t, e.reconcile())
	require.Len(t, e.events(), 1)
	require.NoError(t, e.reconcile())
	assert.Empty(t, e.events())
}

func TestSlimWithoutAnyDependencyIsReady(t *testing.T) {
	// CPU-only with a configured model URL and no AgentRuntime has nothing to wait for.
	o := gateOpts{profile: aiv1.AIPlatformProfileCPUOnly, features: []aiv1.FeatureSpec{gateSlimFeature}}
	e := newGateEnv(t, o)
	ai := e.slim()
	ai.Spec.AIPlatformUrl = "http://byo-llm.example.svc:8000"
	require.NoError(t, e.client.Update(context.Background(), ai))
	require.NoError(t, e.reconcile())
	assert.Equal(t, metav1.ConditionTrue, e.cond("Ready").Status)
}

func TestSlimStageFailureStillReturnsTheErrorAndNoReady(t *testing.T) {
	e := newGateEnv(t, cpuOnlyOpts(lasChild(metav1.ConditionTrue, "Ready")))
	t.Setenv("RELATED_IMAGE_SLIM_API", "")
	require.Error(t, e.r.Reconcile(context.Background(), e.slim()))
	assert.Nil(t, e.cond("Ready"), "a failed stage does not publish Ready")
	assert.Equal(t, "Error", e.cond("ValidateReady").Reason)
}

func TestSlimFull_RunningWorkloadKeepsReconcilingThroughAModelServingOutage(t *testing.T) {
	e := newGateEnv(t, fullOpts(fullWithSAIA, lasChild(metav1.ConditionTrue, "Ready"), rayService(true)))
	require.NoError(t, e.reconcile())
	require.True(t, e.deploymentExists())
	require.Equal(t, metav1.ConditionTrue, e.cond("Ready").Status)

	e.setRay(func(rs *rayv1.RayService) { rs.Status.Conditions = nil })
	require.NoError(t, e.reconcile())

	requireBlocked(t, e, "ModelServing")
	validate := e.cond("ValidateReady")
	require.NotNil(t, validate, "the workload stages keep running, so their conditions are not dropped")
	assert.Equal(t, metav1.ConditionTrue, validate.Status)
	assert.NotNil(t, e.cond("SlimDeploymentReady"))
}

func TestSlimFull_WorkloadIsHeldWhenItsURLCannotBeDerived(t *testing.T) {
	// The Deployment exists, but the Ray service name is not published, so the
	// model endpoint is unknown: the workload cannot be reconciled.
	e := newGateEnv(t, fullOpts(fullWithSAIA, lasChild(metav1.ConditionTrue, "Ready"), rayService(true)))
	require.NoError(t, e.reconcile())
	require.True(t, e.deploymentExists())

	e.platform.Status.RayServiceName = ""
	require.NoError(t, e.client.Status().Update(context.Background(), e.platform))
	// A real API server ignores spec writes made through the status endpoint;
	// the fake client does not, so clear the URL it kept from the first pass.
	ai := e.slim()
	ai.Spec.AIPlatformUrl = ""
	require.NoError(t, e.client.Update(context.Background(), ai))
	e.setRay(func(rs *rayv1.RayService) { rs.Status.Conditions = nil })
	require.NoError(t, e.reconcile())
	requireBlocked(t, e, "ModelServing")
	assert.Nil(t, e.cond("ValidateReady"))
}

func TestSlimFull_WithoutAgentRuntimeOnlyModelServingGates(t *testing.T) {
	e := newGateEnv(t, fullOpts(fullWithSAIA[:2], rayService(true)))
	require.NoError(t, e.reconcile())
	assert.Equal(t, metav1.ConditionTrue, e.cond("Ready").Status)
	assert.Nil(t, e.cond("AgentRuntimeReady"))
	assert.NotNil(t, e.cond("ModelServingReady"))
}

func TestSlimCPUOnly_NeverWaitsForModelServingEvenIfSAIAIsListed(t *testing.T) {
	// The platform rejects this spec, but SLIM reads the spec on its own and
	// must still honour "cpuOnly never waits for model serving". The scheme has
	// no RayService type, so any attempt to read one fails the reconcile.
	o := cpuOnlyOpts(lasChild(metav1.ConditionTrue, "Ready"))
	o.features = []aiv1.FeatureSpec{gateSAIA, gateSlimFeature, gateLASFeature}
	e := newGateEnv(t, o)
	require.NoError(t, e.reconcile())
	assert.Equal(t, metav1.ConditionTrue, e.cond("Ready").Status)
	assert.Nil(t, e.cond("ModelServingReady"))
}

// CPU-only is the point of the profile: SLIM reaches Ready with no Ray, no
// model endpoint and no Weaviate, whatever state leftover Ray objects are in.
func TestSlimCPUOnly_ReadyWithNoModelServingAnywhere(t *testing.T) {
	cases := []struct {
		name    string
		withRay bool
		objs    func() []client.Object
	}{
		{name: "KubeRay not installed (RayService type unknown to the client)", objs: func() []client.Object {
			return []client.Object{lasChild(metav1.ConditionTrue, "Ready")}
		}},
		{name: "KubeRay installed, no RayService", withRay: true, objs: func() []client.Object {
			return []client.Object{lasChild(metav1.ConditionTrue, "Ready")}
		}},
		{name: "a failing RayService with the platform's name exists", withRay: true, objs: func() []client.Object {
			return []client.Object{lasChild(metav1.ConditionTrue, "Ready"), rayService(false)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := cpuOnlyOpts(tc.objs()...)
			o.withRay = tc.withRay
			e := newGateEnv(t, o)
			require.Empty(t, e.platform.Status.RayServiceName, "a cpuOnly platform never publishes a Ray service")
			require.Empty(t, e.slim().Spec.AIPlatformUrl, "and its SLIM child has no model endpoint")

			require.NoError(t, e.reconcile())
			assert.Equal(t, metav1.ConditionTrue, e.cond("Ready").Status)
			assert.Equal(t, "AllReconciled", e.cond("Ready").Reason)
			assert.Nil(t, e.cond("ModelServingReady"))
			assert.NotContains(t, e.cond("Ready").Message, "ModelServing")
			assert.Empty(t, e.slim().Spec.AIPlatformUrl, "SLIM does not synthesise a Ray URL for cpuOnly")
		})
	}
}

func TestSlimCPUOnly_WorkloadRoutesOnlyToAgentRuntime(t *testing.T) {
	e := newGateEnv(t, cpuOnlyOpts(lasChild(metav1.ConditionTrue, "Ready")))
	require.NoError(t, e.reconcile())

	dep := &appsv1.Deployment{}
	require.NoError(t, e.client.Get(context.Background(), types.NamespacedName{Name: gateSlim + "-slim-deployment", Namespace: gateNS}, dep))
	env := map[string]string{}
	for _, v := range dep.Spec.Template.Spec.Containers[0].Env {
		env[v.Name] = v.Value
	}
	assert.Contains(t, env, "AGENT_SERVER_URL")
	assert.Contains(t, env, "LAS_GRAPH_ID")
	assert.NotContains(t, env, "PLATFORM_URL", "no model-serving endpoint is configured for cpuOnly")
}

func slimEnv(t *testing.T, e *gateEnv) map[string]string {
	t.Helper()
	dep := &appsv1.Deployment{}
	require.NoError(t, e.client.Get(context.Background(), types.NamespacedName{Name: gateSlim + "-slim-deployment", Namespace: gateNS}, dep))
	env := map[string]string{}
	for _, v := range dep.Spec.Template.Spec.Containers[0].Env {
		env[v.Name] = v.Value
	}
	return env
}

// Only a platform with the SAIA feature has SLIM derive its model endpoint from
// the Ray service; this is unchanged behaviour and must not spread to others.
func TestSlimFull_ModelEndpointIsDerivedFromRayOnlyForSAIA(t *testing.T) {
	withSAIA := newGateEnv(t, fullOpts(fullWithSAIA, lasChild(metav1.ConditionTrue, "Ready"), rayService(true)))
	require.NoError(t, withSAIA.reconcile())
	assert.Equal(t, "http://launchpad-head-svc.ai-platform.svc.cluster.local:8000", slimEnv(t, withSAIA)["PLATFORM_URL"])

	withoutSAIA := newGateEnv(t, fullOpts([]aiv1.FeatureSpec{gateSlimFeature, gateLASFeature}, lasChild(metav1.ConditionTrue, "Ready"), rayService(true)))
	require.NoError(t, withoutSAIA.reconcile())
	assert.NotContains(t, slimEnv(t, withoutSAIA), "PLATFORM_URL")
}
