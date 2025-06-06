package reconciler

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rancher/fleet/internal/cmd/controller/status"
	"github.com/rancher/fleet/internal/cmd/controller/summary"
	"github.com/rancher/fleet/internal/resourcestatus"
	fleet "github.com/rancher/fleet/pkg/apis/fleet.cattle.io/v1alpha1"
	"github.com/rancher/fleet/pkg/durations"
	"github.com/rancher/fleet/pkg/sharding"
	"github.com/rancher/wrangler/v3/pkg/genericcondition"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// enqueueDelay is used as an artificial delay for enqueuing GitRepo status reconciliation requests
// This allows aggregating multiple consecutive Bundle update events, reducing the number of GitRepo status changes at the cost of introducing a delay in the notification
const enqueueDelay = 3 * time.Second

type StatusReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Workers int
	ShardID string
}

func (r *StatusReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleet.GitRepo{}).
		WatchesRawSource(wrapSourceWithForcedDelay(enqueueDelay, source.TypedKind(
			// Fan out from bundle to gitrepo
			mgr.GetCache(),
			&fleet.Bundle{},
			handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, a *fleet.Bundle) []ctrl.Request {
				repo := a.GetLabels()[fleet.RepoLabel]
				if repo != "" {
					return []ctrl.Request{{
						NamespacedName: types.NamespacedName{
							Namespace: a.GetNamespace(),
							Name:      repo,
						},
					}}
				}

				return []ctrl.Request{}
			}),
			sharding.TypedFilterByShardID[*fleet.Bundle](r.ShardID), // WatchesRawSources ignores event filters, we need to use a predicate
			status.BundleStatusChangedPredicate(),
		))).
		WithEventFilter(sharding.FilterByShardID(r.ShardID)).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.Workers}).
		Named("GitRepoStatus").
		Complete(r)
}

// Reconcile reads the stat of the GitRepo and BundleDeployments and
// computes status fields for the GitRepo. This status is used to
// display information to the user.
func (r *StatusReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("gitops-status")

	gitrepo := &fleet.GitRepo{}
	if err := r.Get(ctx, req.NamespacedName, gitrepo); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	} else if errors.IsNotFound(err) {
		logger.V(1).Info("Gitrepo deleted, cleaning up poll jobs")
		return ctrl.Result{}, nil
	}

	orig := gitrepo.DeepCopy()

	// Restrictions / Overrides, gitrepo reconciler is responsible for setting error in status
	if err := AuthorizeAndAssignDefaults(ctx, r.Client, gitrepo); err != nil {
		// the gitjob_controller will handle the error
		return ctrl.Result{}, nil
	}

	if !gitrepo.DeletionTimestamp.IsZero() {
		// the gitjob_controller will handle deletion
		return ctrl.Result{}, nil
	}

	if gitrepo.Spec.Repo == "" {
		return ctrl.Result{}, nil
	}

	logger = logger.WithValues("generation", gitrepo.Generation, "commit", gitrepo.Status.Commit).WithValues("conditions", gitrepo.Status.Conditions)
	ctx = log.IntoContext(ctx, logger)

	logger.V(1).Info("Reconciling GitRepo status")

	bdList := &fleet.BundleDeploymentList{}
	err := r.List(ctx, bdList, client.MatchingLabels{
		fleet.RepoLabel:            gitrepo.Name,
		fleet.BundleNamespaceLabel: gitrepo.Namespace,
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	err = setStatus(bdList, gitrepo)
	if err != nil {
		return ctrl.Result{}, err
	}

	if gitrepo.Status.GitJobStatus != "Current" {
		gitrepo.Status.Display.State = "GitUpdating"
	}

	// We're explicitly setting the ready status from a bundle here, but only if it isn't ready.
	//
	// - If the bundle has no deployments, there is no status to be copied from the setStatus
	// function, so that we won't overwrite anything.
	//
	// - If the bundle has rendering issues and there are deployments of which there is at least one
	// in a failed state, the status of the bundle deployments would be overwritten by the bundle
	// status.
	//
	// - If the bundle has no rendering issues but there are deployments in a failed state, the code
	// will overwrite the gitrepo's ready status condition with the ready status condition coming
	// from the bundle. Because both have the same content, we can unconditionally set the status
	// from the bundle.
	//
	// So we're basically just making sure the status from the bundle is being set on the gitrepo,
	// even if there are no bundle deployments, which is the case for issues with rendering the
	// manifests, for instance. In that case no bundle deployments are created, but an error is set
	// in a ready status condition on the bundle.
	err = r.setReadyStatusFromBundle(ctx, gitrepo)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.updateStatus(ctx, orig, gitrepo); err != nil {
		logger.Error(err, "Reconcile failed update to git repo status", "status", gitrepo.Status)
		return ctrl.Result{RequeueAfter: durations.GitRepoStatusDelay}, nil
	}

	return ctrl.Result{}, nil
}

func (r *StatusReconciler) updateStatus(ctx context.Context, orig *fleet.GitRepo, obj *fleet.GitRepo) error {
	statusPatch := client.MergeFrom(orig)
	if patchData, err := statusPatch.Data(obj); err == nil && string(patchData) == "{}" {
		// skip update if patch is empty
		return nil
	}
	return r.Client.Status().Patch(ctx, obj, statusPatch)
}

func setStatus(list *fleet.BundleDeploymentList, gitrepo *fleet.GitRepo) error {
	// sort bundledeployments so lists in status are always in the same order
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].UID < list.Items[j].UID
	})

	err := status.SetFields(list, &gitrepo.Status.StatusBase)
	if err != nil {
		return err
	}

	resourcestatus.SetResources(list.Items, &gitrepo.Status.StatusBase)

	summary.SetReadyConditions(&gitrepo.Status, "Bundle", gitrepo.Status.Summary)

	gitrepo.Status.Display.ReadyBundleDeployments = fmt.Sprintf("%d/%d",
		gitrepo.Status.Summary.Ready,
		gitrepo.Status.Summary.DesiredReady)

	return nil
}

// setReadyStatusFromBundle fetches all bundles from a given gitrepo, checks the ready status conditions
// from the bundles and applies one on the gitrepo if it isn't ready. The purpose is to make
// rendering issues visible in the gitrepo status. Those issues need to be made explicitly visible
// since the other statuses are calculated from bundle deployments, which do not exist when
// rendering manifests fail. Should an issue be on the bundle, it will be copied to the gitrepo.
func (r StatusReconciler) setReadyStatusFromBundle(ctx context.Context, gitrepo *fleet.GitRepo) error {
	bList := &fleet.BundleList{}
	err := r.List(ctx, bList, client.MatchingLabels{
		fleet.RepoLabel: gitrepo.Name,
	}, client.InNamespace(gitrepo.Namespace))
	if err != nil {
		return err
	}

	// Make sure the bundles are always iterated in the same order
	// The code below will pick the first element matching the condition, so successive executions should produce the same result.
	sort.Slice(bList.Items, func(i, j int) bool {
		return bList.Items[i].UID < bList.Items[j].UID
	})

	found := false
	// Find a ready status condition in a bundle which is not ready.
	var condition genericcondition.GenericCondition
bundles:
	for _, bundle := range bList.Items {
		if bundle.Status.Conditions == nil {
			continue
		}

		for _, c := range bundle.Status.Conditions {
			if c.Type == string(fleet.Ready) && c.Status == v1.ConditionFalse {
				condition = c
				found = true
				break bundles
			}
		}
	}

	// No ready condition found in any bundle, nothing to do here.
	if !found {
		return nil
	}

	found = false
	newConditions := make([]genericcondition.GenericCondition, 0, len(gitrepo.Status.Conditions))
	for _, c := range gitrepo.Status.Conditions {
		if c.Type == string(fleet.Ready) {
			// Replace the ready condition with the one from the bundle
			newConditions = append(newConditions, condition)
			found = true
			continue
		}
		newConditions = append(newConditions, c)
	}
	if !found {
		// Add the ready condition from the bundle to the gitrepo.
		newConditions = append(newConditions, condition)
	}
	gitrepo.Status.Conditions = newConditions

	return nil
}

type forcedDelayingSource[R comparable] struct {
	source.TypedSource[R]
	delay time.Duration
}

func wrapSourceWithForcedDelay[R comparable](delay time.Duration, delegate source.TypedSource[R]) source.TypedSource[R] {
	return &forcedDelayingSource[R]{TypedSource: delegate, delay: delay}
}

func (s *forcedDelayingSource[R]) Start(ctx context.Context, delegate workqueue.TypedRateLimitingInterface[R]) error {
	return s.TypedSource.Start(ctx, &forcedDelayingQueue[R]{delegate, s.delay})
}

type forcedDelayingQueue[R comparable] struct {
	workqueue.TypedRateLimitingInterface[R]
	delay time.Duration
}

func (f *forcedDelayingQueue[R]) Add(obj R) {
	f.AddAfter(obj, f.delay)
}
