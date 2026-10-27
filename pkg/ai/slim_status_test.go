package ai_platform

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	aiApi "github.com/splunk/splunk-ai-operator/api/v1"
)

// The platform follows SLIM's own Ready, which waits for the dependencies of
// the platform's profile, so both profiles must report a blocked SLIM and name
// what blocks it.
func TestCheckAIServiceStatus_FollowsSlimReadyInBothProfiles(t *testing.T) {
	const blocker = "blocked by ModelServing: RayService platform is not ready"

	cases := []struct {
		name     string
		slim     func() aiApi.AIServiceStatus
		generate int64
		wantErr  []string
	}{
		{name: "Ready", slim: func() aiApi.AIServiceStatus {
			return aiApi.AIServiceStatus{ObservedGeneration: 2, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllReconciled"}}}
		}},
		{name: "blocked by a dependency", slim: func() aiApi.AIServiceStatus {
			return aiApi.AIServiceStatus{ObservedGeneration: 2, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "DependencyNotReady", Message: blocker}}}
		}, wantErr: []string{"SLIM is not ready", "DependencyNotReady", blocker}},
		{name: "no Ready condition yet", slim: func() aiApi.AIServiceStatus {
			return aiApi.AIServiceStatus{ObservedGeneration: 2}
		}, wantErr: []string{"SLIM is not ready for generation 2"}},
		{name: "Ready from an older generation", slim: func() aiApi.AIServiceStatus {
			return aiApi.AIServiceStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllReconciled"}}}
		}, wantErr: []string{"SLIM is not ready for generation 2"}},
	}

	for _, profile := range []aiApi.AIPlatformProfile{aiApi.AIPlatformProfileCPUOnly, aiApi.AIPlatformProfileFull, ""} {
		for _, tc := range cases {
			t.Run(string(profile)+"/"+tc.name, func(t *testing.T) {
				scheme := buildTestScheme(t)
				platform := &aiApi.AIPlatform{
					ObjectMeta: metav1.ObjectMeta{Name: "platform", Namespace: "default", UID: types.UID("platform-uid")},
					Spec:       aiApi.AIPlatformSpec{Profile: profile},
				}
				slim := &aiApi.AIService{
					ObjectMeta: metav1.ObjectMeta{Name: "platform-slim", Namespace: "default", Generation: 2},
					Spec:       aiApi.AIServiceSpec{Feature: aiApi.FeatureSpec{Name: "slim"}},
					Status:     tc.slim(),
				}
				require.NoError(t, controllerutil.SetControllerReference(platform, slim, scheme))
				c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform, slim).
					WithIndex(&aiApi.AIService{}, ownerKey, func(obj client.Object) []string {
						if owner := metav1.GetControllerOfNoCopy(obj); owner != nil {
							return []string{owner.Name}
						}
						return nil
					}).Build()

				err := (&AIPlatformReconciler{Client: c, Scheme: scheme}).CheckAIServiceStatus(context.Background(), platform)
				if len(tc.wantErr) == 0 {
					require.NoError(t, err)
					return
				}
				require.Error(t, err)
				for _, want := range tc.wantErr {
					require.ErrorContains(t, err, want)
				}
			})
		}
	}
}
