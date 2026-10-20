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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAIServiceVectorDBURLAdmissionByPlatformProfile(t *testing.T) {
	for _, tt := range []struct {
		name      string
		profile   aiv1.AIPlatformProfile
		feature   string
		vectorURL string
		wantError bool
	}{
		{name: "cpu only SLIM", profile: aiv1.AIPlatformProfileCPUOnly, feature: "slim"},
		{name: "cpu only Agent Runtime", profile: aiv1.AIPlatformProfileCPUOnly, feature: "agentruntime"},
		{name: "full SLIM requires vector DB", profile: aiv1.AIPlatformProfileFull, feature: "slim", wantError: true},
		{name: "full Agent Runtime requires vector DB", profile: aiv1.AIPlatformProfileFull, feature: "agentruntime", wantError: true},
		{name: "full SLIM with vector DB", profile: aiv1.AIPlatformProfileFull, feature: "slim", vectorURL: "weaviate.ai.svc.cluster.local"},
		{name: "cpu only rejects missing vector DB for SAIA", profile: aiv1.AIPlatformProfileCPUOnly, feature: "saia", wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := aiv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			platform := &aiv1.AIPlatform{ObjectMeta: metav1.ObjectMeta{Name: "platform", Namespace: "ai"}}
			platform.Spec.Profile = tt.profile
			validator := &AIServiceCustomValidator{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform).Build()}
			service := &aiv1.AIService{
				ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "ai"},
				Spec: aiv1.AIServiceSpec{
					AIPlatformRef: corev1.ObjectReference{Name: platform.Name},
					Feature:       aiv1.FeatureSpec{Name: tt.feature},
					TaskVolume:    aiv1.ObjectStorageSpec{Path: "s3://fixture/tasks", Region: "us-west-2"},
					VectorDbUrl:   tt.vectorURL,
				},
			}
			if tt.feature == "agentruntime" {
				service.Spec.Feature.Provider = "mltk"
				service.Spec.Feature.LAS = fixtureLASForWebhookTest()
				service.Spec.Feature.LicenseSecretRef = "las-auth"
				service.Spec.Feature.PostgresSecretRef = "las-postgres"
				service.Spec.Feature.RedisSecretRef = "las-redis"
			}
			if err := (&AIServiceCustomDefaulter{}).Default(context.Background(), service); err != nil {
				t.Fatal(err)
			}
			_, err := validator.ValidateCreate(context.Background(), service)
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "spec.vectorDbUrl") {
					t.Fatalf("expected vectorDbUrl rejection, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected AIService admission rejection: %v", err)
			}
		})
	}
}

func TestAIServiceCRDAllowsMissingVectorDBURL(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "..", "config", "crd", "bases", "ai.splunk.com_aiservices.yaml"),
		filepath.Join("..", "..", "..", "helm-chart", "splunk-ai-operator", "crds", "ai.splunk.com_aiservices.yaml"),
		filepath.Join("..", "..", "..", "tools", "cluster_setup", "artifacts.yaml"),
	} {
		t.Run(path, func(t *testing.T) {
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
			for {
				var crd apiextv1.CustomResourceDefinition
				if err := decoder.Decode(&crd); err == io.EOF {
					t.Fatal("AIService CRD not found")
				} else if err != nil {
					t.Fatal(err)
				}
				if crd.Kind != "CustomResourceDefinition" || crd.Name != "aiservices.ai.splunk.com" {
					continue
				}
				for _, version := range crd.Spec.Versions {
					spec := version.Schema.OpenAPIV3Schema.Properties["spec"]
					for _, required := range spec.Required {
						if required == "vectorDbUrl" {
							t.Fatalf("CRD version %s unconditionally requires vectorDbUrl", version.Name)
						}
					}
				}
				return
			}
		})
	}
}
