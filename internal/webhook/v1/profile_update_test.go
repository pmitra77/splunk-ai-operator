/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	"context"
	"strings"
	"testing"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// profileUpdatePlatform is valid under every profile, so only the profile
// change decides whether an update is admitted.
func profileUpdatePlatform(profile aiv1.AIPlatformProfile) *aiv1.AIPlatform {
	return &aiv1.AIPlatform{
		ObjectMeta: metav1.ObjectMeta{Name: "ai", Namespace: "ai-platform"},
		Spec: aiv1.AIPlatformSpec{
			Profile:       profile,
			ObjectStorage: aiv1.ObjectStorageSpec{Path: "s3://bucket/path", Region: "us-west-2"},
			Features: []aiv1.FeatureSpec{
				{Name: "slim"},
				{
					Name: "agentruntime", Provider: "mltk",
					LicenseSecretRef: "las-auth", PostgresSecretRef: "las-postgres", RedisSecretRef: "las-redis",
					LAS: &aiv1.LASFeatureSpec{
						Image:       aiv1.LASImageSpec{Repository: "registry.example/las", Tag: "v1"},
						AssistantID: "echo",
					},
				},
			},
		},
	}
}

func TestAIPlatformProfileUpdateRules(t *testing.T) {
	const (
		full        = aiv1.AIPlatformProfileFull
		cpuOnly     = aiv1.AIPlatformProfileCPUOnly
		toCPUOnly   = "switching an existing model-serving AIPlatform to cpuOnly is not supported"
		fromCPUOnly = "switching an existing cpuOnly AIPlatform to full is not supported"
	)
	for _, test := range []struct {
		name     string
		from, to aiv1.AIPlatformProfile
		wantErr  string
	}{
		{name: "omitted stays omitted", from: "", to: ""},
		{name: "omitted to explicit full is the same profile", from: "", to: full},
		{name: "explicit full to omitted is the same profile", from: full, to: ""},
		{name: "full stays full", from: full, to: full},
		{name: "cpuOnly stays cpuOnly", from: cpuOnly, to: cpuOnly},
		{name: "omitted to cpuOnly", from: "", to: cpuOnly, wantErr: toCPUOnly},
		{name: "full to cpuOnly", from: full, to: cpuOnly, wantErr: toCPUOnly},
		{name: "cpuOnly to full", from: cpuOnly, to: full, wantErr: fromCPUOnly},
		{name: "cpuOnly to omitted selects full", from: cpuOnly, to: "", wantErr: fromCPUOnly},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := (&AIPlatformCustomValidator{}).ValidateUpdate(
				context.Background(), profileUpdatePlatform(test.from), profileUpdatePlatform(test.to))
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("update %q -> %q rejected: %v", test.from, test.to, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("update %q -> %q was admitted; want %q", test.from, test.to, test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("update %q -> %q error = %v; want it to contain %q", test.from, test.to, err, test.wantErr)
			}
			if !strings.Contains(err.Error(), "spec.profile") {
				t.Fatalf("error does not name spec.profile: %v", err)
			}
		})
	}
}

// Only the profile is locked: other edits to a running cpuOnly platform stay allowed.
func TestAIPlatformCPUOnlyUpdateOtherFieldsStillAllowed(t *testing.T) {
	old := profileUpdatePlatform(aiv1.AIPlatformProfileCPUOnly)
	updated := profileUpdatePlatform(aiv1.AIPlatformProfileCPUOnly)
	updated.Spec.Features[1].LAS.Image.Tag = "v2"
	if _, err := (&AIPlatformCustomValidator{}).ValidateUpdate(context.Background(), old, updated); err != nil {
		t.Fatalf("changing the LAS image tag on a cpuOnly platform was rejected: %v", err)
	}
}
