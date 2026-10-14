package agentruntime

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

func TestLASHelmActionClientLifecycle(t *testing.T) {
	config := &action.Configuration{
		Releases: storage.Init(driver.NewMemory()),
		KubeClient: &kubefake.FailingKubeClient{
			PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard},
		},
		Capabilities: chartutil.DefaultCapabilities,
		Log:          func(string, ...interface{}) {},
	}
	helm := &lasActionClient{config: config, namespace: "default"}
	ch := &chart.Chart{
		Metadata:  &chart.Metadata{Name: "fixture", Version: "0.1.0"},
		Templates: []*chart.File{{Name: "templates/configmap.yaml", Data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n")}},
	}
	ctx := context.Background()
	first, err := helm.Install(ctx, "las-fixture", ch, map[string]interface{}{"revision": "one"}, false, "service-uid")
	require.NoError(t, err)
	require.Equal(t, release.StatusDeployed, first.Info.Status)
	require.Equal(t, "service-uid", first.Labels[lasOwnerLabel])

	observed, err := helm.Status("las-fixture")
	require.NoError(t, err)
	require.Equal(t, 1, observed.Version)
	second, err := helm.Upgrade(ctx, "las-fixture", ch, map[string]interface{}{"revision": "two"}, "service-uid")
	require.NoError(t, err)
	require.Equal(t, 2, second.Version)
	require.Equal(t, release.StatusDeployed, second.Info.Status)

	history, err := helm.History("las-fixture")
	require.NoError(t, err)
	require.Len(t, history, 2)
	require.NoError(t, helm.Rollback("las-fixture", 1))
	rolledBack, err := helm.Status("las-fixture")
	require.NoError(t, err)
	require.Equal(t, 3, rolledBack.Version)
	require.Equal(t, "one", rolledBack.Config["revision"])
	require.Equal(t, "service-uid", rolledBack.Labels[lasOwnerLabel])

	require.NoError(t, helm.Uninstall("las-fixture"))
	_, err = helm.Status("las-fixture")
	require.ErrorIs(t, err, driver.ErrReleaseNotFound)
}
