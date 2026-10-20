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
	"testing"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestAIPlatformVectorDBStorageDefaultByProfile(t *testing.T) {
	for _, tt := range []struct {
		name     string
		profile  aiv1.AIPlatformProfile
		wantSize string
	}{
		{name: "cpu only", profile: aiv1.AIPlatformProfileCPUOnly},
		{name: "full", profile: aiv1.AIPlatformProfileFull, wantSize: "50Gi"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := &aiv1.AIPlatform{}
			obj.Spec.Profile = tt.profile
			if err := (&AIPlatformCustomDefaulter{}).Default(context.Background(), obj); err != nil {
				t.Fatalf("default AIPlatform: %v", err)
			}
			if got := obj.Spec.Storage.VectorDB.Size; got != tt.wantSize {
				t.Fatalf("vectorDB size = %q, want %q", got, tt.wantSize)
			}
			if tt.profile == aiv1.AIPlatformProfileCPUOnly {
				if errs := validateAIPlatformProfile(obj); len(errs) != 0 {
					t.Fatalf("cpuOnly profile rejected after defaulting: %v", errs)
				}
			}
		})
	}
}

func TestAIPlatformCRDDoesNotDefaultVectorDBStorage(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "..", "config", "crd", "bases", "ai.splunk.com_aiplatforms.yaml"),
		filepath.Join("..", "..", "..", "helm-chart", "splunk-ai-operator", "crds", "ai.splunk.com_aiplatforms.yaml"),
		filepath.Join("..", "..", "..", "tools", "cluster_setup", "artifacts.yaml"),
	} {
		t.Run(path, func(t *testing.T) {
			file, err := os.Open(path)
			if err != nil {
				t.Fatalf("open CRD manifest: %v", err)
			}
			defer file.Close()

			decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
			for {
				var crd apiextv1.CustomResourceDefinition
				if err := decoder.Decode(&crd); err == io.EOF {
					t.Fatal("AIPlatform CRD not found")
				} else if err != nil {
					t.Fatalf("decode manifest: %v", err)
				}
				if crd.Kind != "CustomResourceDefinition" || crd.Name != "aiplatforms.ai.splunk.com" {
					continue
				}
				for _, version := range crd.Spec.Versions {
					spec := version.Schema.OpenAPIV3Schema.Properties["spec"]
					storage := spec.Properties["storage"]
					vectorDB := storage.Properties["vectorDB"]
					size := vectorDB.Properties["size"]
					if size.Default != nil {
						t.Fatalf("CRD version %s defaults vectorDB.size, which makes cpuOnly admission fail", version.Name)
					}
				}
				return
			}
		})
	}
}
