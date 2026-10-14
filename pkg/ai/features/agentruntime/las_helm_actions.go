package agentruntime

import (
	"context"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
)

const lasHelmActionTimeout = 5 * time.Minute

// This small boundary lets lifecycle tests exercise reconciliation without a
// Kubernetes-backed Helm storage driver or a live cluster.
type lasHelmActions interface {
	Status(string) (*release.Release, error)
	History(string) ([]*release.Release, error)
	Install(context.Context, string, *chart.Chart, map[string]interface{}, bool, string) (*release.Release, error)
	Upgrade(context.Context, string, *chart.Chart, map[string]interface{}, string) (*release.Release, error)
	Rollback(string, int) error
	Uninstall(string) error
}

type lasActionClient struct {
	config    *action.Configuration
	namespace string
}

func (h *lasActionClient) Status(name string) (*release.Release, error) {
	return action.NewStatus(h.config).Run(name)
}

func (h *lasActionClient) History(name string) ([]*release.Release, error) {
	return action.NewHistory(h.config).Run(name)
}

func (h *lasActionClient) Install(ctx context.Context, name string, ch *chart.Chart, values map[string]interface{}, replace bool, ownerUID string) (*release.Release, error) {
	install := action.NewInstall(h.config)
	install.ReleaseName = name
	install.Namespace = h.namespace
	install.Replace = replace
	install.Labels = map[string]string{lasOwnerLabel: ownerUID}
	install.Wait = true
	install.Atomic = true
	install.Timeout = lasHelmActionTimeout
	return install.RunWithContext(ctx, ch, values)
}

func (h *lasActionClient) Upgrade(ctx context.Context, name string, ch *chart.Chart, values map[string]interface{}, ownerUID string) (*release.Release, error) {
	upgrade := action.NewUpgrade(h.config)
	upgrade.Namespace = h.namespace
	upgrade.Labels = map[string]string{lasOwnerLabel: ownerUID}
	upgrade.Wait = true
	upgrade.Atomic = true
	upgrade.Timeout = lasHelmActionTimeout
	return upgrade.RunWithContext(ctx, name, ch, values)
}

func (h *lasActionClient) Rollback(name string, version int) error {
	rollback := action.NewRollback(h.config)
	rollback.Version = version
	rollback.Wait = true
	rollback.Timeout = lasHelmActionTimeout
	return rollback.Run(name)
}

func (h *lasActionClient) Uninstall(name string) error {
	uninstall := action.NewUninstall(h.config)
	uninstall.Wait = true
	uninstall.Timeout = lasHelmActionTimeout
	_, err := uninstall.Run(name)
	return err
}
