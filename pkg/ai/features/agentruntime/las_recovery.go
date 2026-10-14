package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"time"

	aiv1 "github.com/splunk/splunk-ai-operator/api/v1"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A Helm action can outlive a controller reconcile. Leave a minute beyond the
// configured action timeout before treating a pending revision as interrupted.
const lasPendingRecoveryAfter = lasHelmActionTimeout + time.Minute

func lasPendingActionStartedAt(current *release.Release) (time.Time, bool) {
	if current == nil || current.Info == nil {
		return time.Time{}, false
	}
	if current.Info.Status == release.StatusUninstalling && !current.Info.Deleted.IsZero() {
		return current.Info.Deleted.Time, true
	}
	if !current.Info.LastDeployed.IsZero() {
		return current.Info.LastDeployed.Time, true
	}
	if !current.Info.FirstDeployed.IsZero() {
		return current.Info.FirstDeployed.Time, true
	}
	return time.Time{}, false
}

func lasPendingActionExpired(current *release.Release, now time.Time) bool {
	started, ok := lasPendingActionStartedAt(current)
	return ok && now.Sub(started) >= lasPendingRecoveryAfter
}

func (r *LASReconciler) recoverPendingHelmAction(ctx context.Context, ai *aiv1.AIService, helm lasHelmActions, current *release.Release) error {
	name := current.Name
	if _, ok := lasPendingActionStartedAt(current); !ok {
		return r.fail(ctx, ai, "HelmPendingStateInvalid", fmt.Errorf("Helm release %s revision %d has no action timestamp; recovery requires inspection", name, current.Version))
	}
	if !lasPendingActionExpired(current, time.Now()) {
		return r.setCondition(ctx, ai, metav1.ConditionUnknown, "HelmActionPending", fmt.Sprintf("Waiting for Helm release %s revision %d (%s)", name, current.Version, current.Info.Status))
	}

	// Recheck before recovery. A completed action or a new revision must not be
	// overwritten by a decision made from an older status observation.
	latest, err := helm.Status(name)
	if err != nil {
		return r.fail(ctx, ai, "HelmStatusFailed", err)
	}
	if latest == nil || latest.Version != current.Version || latest.Info == nil || latest.Info.Status != current.Info.Status {
		return r.setCondition(ctx, ai, metav1.ConditionUnknown, "HelmActionChanged", fmt.Sprintf("Helm release %s changed during recovery; observing the new state", name))
	}
	if latest.Labels[lasOwnerLabel] != string(ai.UID) {
		return r.fail(ctx, ai, "ReleaseOwnershipConflict", fmt.Errorf("Helm release %q is not owned by this AIService", name))
	}

	switch latest.Info.Status {
	case release.StatusPendingInstall, release.StatusUninstalling:
		// An interrupted install has no successful revision to restore. Helm
		// uninstall removes only chart resources; credential Secrets are external.
		if err := helm.Uninstall(name); err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
			return r.fail(ctx, ai, "HelmRecoveryFailed", fmt.Errorf("finish interrupted LAS uninstall %s: %w", name, err))
		}
		remaining, err := helm.Status(name)
		if err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
			return r.fail(ctx, ai, "HelmRecoveryFailed", fmt.Errorf("check LAS release %s after uninstall: %w", name, err))
		}
		if err == nil && (remaining == nil || remaining.Info == nil || remaining.Info.Status != release.StatusUninstalled) {
			return r.fail(ctx, ai, "HelmRecoveryFailed", fmt.Errorf("LAS release %s remains active after recovery uninstall", name))
		}
	case release.StatusPendingUpgrade, release.StatusPendingRollback:
		history, err := helm.History(name)
		if err != nil {
			return r.fail(ctx, ai, "HelmRecoveryFailed", fmt.Errorf("inspect LAS release %s history: %w", name, err))
		}
		var target *release.Release
		for _, previous := range history {
			if previous == nil || previous.Info == nil || previous.Version >= latest.Version || previous.Info.Status != release.StatusDeployed {
				continue
			}
			if previous.Labels[lasOwnerLabel] != string(ai.UID) {
				return r.fail(ctx, ai, "ReleaseOwnershipConflict", fmt.Errorf("refusing to recover Helm release %s from a revision with another owner", name))
			}
			if target == nil || previous.Version > target.Version {
				target = previous
			}
		}
		if target == nil {
			return r.fail(ctx, ai, "HelmRecoveryUnsafe", fmt.Errorf("Helm release %s has no UID-owned deployed revision to restore", name))
		}
		if err := helm.Rollback(name, target.Version); err != nil {
			return r.fail(ctx, ai, "HelmRecoveryFailed", fmt.Errorf("restore LAS release %s revision %d: %w", name, target.Version, err))
		}
		restored, err := helm.Status(name)
		if err != nil || restored == nil || restored.Info == nil || restored.Info.Status != release.StatusDeployed ||
			restored.Labels[lasOwnerLabel] != string(ai.UID) || !sameValues(restored.Config, target.Config) ||
			restored.Chart == nil || restored.Chart.Metadata == nil || target.Chart == nil || target.Chart.Metadata == nil ||
			restored.Chart.Metadata.Version != target.Chart.Metadata.Version {
			return r.fail(ctx, ai, "HelmRecoveryFailed", fmt.Errorf("LAS release %s did not confirm a UID-owned deployed rollback", name))
		}
	default:
		return r.fail(ctx, ai, "HelmRecoveryUnsafe", fmt.Errorf("Helm release %s changed to unsupported state %s", name, latest.Info.Status))
	}

	r.event(ai, corev1.EventTypeWarning, "LASHelmActionRecovered", fmt.Sprintf("Recovered interrupted Helm release %s revision %d; desired configuration will be retried", name, latest.Version))
	return r.setCondition(ctx, ai, metav1.ConditionFalse, "DesiredRevisionNotApplied", fmt.Sprintf("Recovered interrupted Helm release %s revision %d; desired configuration is not yet confirmed", name, latest.Version))
}

func (r *LASReconciler) removeStableService(ctx context.Context, ai *aiv1.AIService) error {
	name := agentRuntimeServiceName(ai.Name)
	service := &corev1.Service{}
	key := client.ObjectKey{Namespace: ai.Namespace, Name: name}
	if err := r.Get(ctx, key, service); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(service, ai) {
		// A same-name Service owned by another resource is outside our cleanup.
		return nil
	}
	if service.DeletionTimestamp == nil {
		if err := r.Delete(ctx, service); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if err := r.Get(ctx, key, &corev1.Service{}); err == nil {
		return fmt.Errorf("waiting for AIService-owned Service %s to be removed", name)
	} else {
		return client.IgnoreNotFound(err)
	}
}
