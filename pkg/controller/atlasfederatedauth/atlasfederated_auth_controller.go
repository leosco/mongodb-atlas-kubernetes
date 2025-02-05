package atlasfederatedauth

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/atlas-sdk/v20231115004/admin"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	mdbv1 "github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/api/v1"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/api/v1/status"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/controller/atlas"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/controller/customresource"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/controller/statushandler"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/controller/watch"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/controller/workflow"
)

// AtlasFederatedAuthReconciler reconciles an AtlasFederatedAuth object
type AtlasFederatedAuthReconciler struct {
	watch.ResourceWatcher
	Client                      client.Client
	Log                         *zap.SugaredLogger
	Scheme                      *runtime.Scheme
	GlobalPredicates            []predicate.Predicate
	EventRecorder               record.EventRecorder
	AtlasProvider               atlas.Provider
	ObjectDeletionProtection    bool
	SubObjectDeletionProtection bool
}

// +kubebuilder:rbac:groups=atlas.mongodb.com,resources=atlasfederatedauths,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=atlas.mongodb.com,resources=atlasfederatedauths/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=atlas.mongodb.com,namespace=default,resources=atlasfederatedauths,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=atlas.mongodb.com,namespace=default,resources=atlasfederatedauths/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *AtlasFederatedAuthReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.With("atlasfederatedauth", req.NamespacedName)

	fedauth := &mdbv1.AtlasFederatedAuth{}
	result := customresource.PrepareResource(ctx, r.Client, req, fedauth, log)
	if !result.IsOk() {
		return result.ReconcileResult(), nil
	}

	if customresource.ReconciliationShouldBeSkipped(fedauth) {
		log.Infow(fmt.Sprintf("-> Skipping AtlasFederatedAuth reconciliation as annotation %s=%s", customresource.ReconciliationPolicyAnnotation, customresource.ReconciliationPolicySkip), "spec", fedauth.Spec)
		if !fedauth.GetDeletionTimestamp().IsZero() {
			if err := customresource.ManageFinalizer(ctx, r.Client, fedauth, customresource.UnsetFinalizer); err != nil {
				result = workflow.Terminate(workflow.Internal, err.Error())
				log.Errorw("Failed to remove finalizer", "error", err)
				return result.ReconcileResult(), nil
			}
		}
		return workflow.OK().ReconcileResult(), nil
	}

	workflowCtx := customresource.MarkReconciliationStarted(r.Client, fedauth, log, ctx)
	log.Infow("-> Starting AtlasFederatedAuth reconciliation")

	defer statushandler.Update(workflowCtx, r.Client, r.EventRecorder, fedauth)

	resourceVersionIsValid := customresource.ValidateResourceVersion(workflowCtx, fedauth, r.Log)
	if !resourceVersionIsValid.IsOk() {
		r.Log.Debugf("federated auth validation result: %v", resourceVersionIsValid)
		return resourceVersionIsValid.ReconcileResult(), nil
	}

	if !r.AtlasProvider.IsResourceSupported(fedauth) {
		result := workflow.Terminate(workflow.AtlasGovUnsupported, "the AtlasFederatedAuth is not supported by Atlas for government").
			WithoutRetry()
		setCondition(workflowCtx, status.FederatedAuthReadyType, result)
		return result.ReconcileResult(), nil
	}

	atlasClient, orgID, err := r.AtlasProvider.SdkClient(workflowCtx.Context, fedauth.ConnectionSecretObjectKey(), log)
	if err != nil {
		result := workflow.Terminate(workflow.AtlasAPIAccessNotConfigured, err.Error())
		setCondition(workflowCtx, status.FederatedAuthReadyType, result)
		return result.ReconcileResult(), nil
	}
	workflowCtx.SdkClient = atlasClient
	workflowCtx.OrgID = orgID

	owner, err := customresource.IsOwner(fedauth, r.ObjectDeletionProtection, customresource.IsResourceManagedByOperator, managedByAtlas(ctx, atlasClient, orgID))
	if err != nil {
		result = workflow.Terminate(workflow.Internal, fmt.Sprintf("unable to resolve ownership for deletion protection: %s", err))
		workflowCtx.SetConditionFromResult(status.FederatedAuthReadyType, result)
		log.Error(result.GetMessage())

		return result.ReconcileResult(), nil
	}

	if !owner {
		result = workflow.Terminate(
			workflow.AtlasDeletionProtection,
			"unable to reconcile FederatedAuthConfig due to deletion protection being enabled. see https://dochub.mongodb.org/core/ako-deletion-protection for further information",
		)
		workflowCtx.SetConditionFromResult(status.FederatedAuthReadyType, result)
		log.Error(result.GetMessage())

		return result.ReconcileResult(), nil
	}

	result = r.ensureFederatedAuth(workflowCtx, fedauth)
	workflowCtx.SetConditionFromResult(status.FederatedAuthReadyType, result)
	workflowCtx.SetConditionFromResult(status.ReadyType, result)

	return result.ReconcileResult(), nil
}

func (r *AtlasFederatedAuthReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("AtlasFederatedAuth").
		For(&mdbv1.AtlasFederatedAuth{}, builder.WithPredicates(r.GlobalPredicates...)).
		Watches(&corev1.Secret{}, watch.NewSecretHandler(r.WatchedResources)).
		Complete(r)
}

func setCondition(ctx *workflow.Context, condition status.ConditionType, result workflow.Result) {
	ctx.SetConditionFromResult(condition, result)
	logIfWarning(ctx, result)
}

func logIfWarning(ctx *workflow.Context, result workflow.Result) {
	if result.IsWarning() {
		ctx.Log.Warnw(result.GetMessage())
	}
}

func managedByAtlas(ctx context.Context, atlasClient *admin.APIClient, orgID string) customresource.AtlasChecker {
	return func(resource mdbv1.AtlasCustomResource) (bool, error) {
		fedauth, ok := resource.(*mdbv1.AtlasFederatedAuth)
		if !ok {
			return false, errors.New("failed to match resource type as AtlasFederatedAuth")
		}

		atlasFedSettings, _, err := atlasClient.FederatedAuthenticationApi.GetFederationSettings(ctx, orgID).Execute()
		if err != nil {
			return false, err
		}

		atlasFedAuth, _, err := atlasClient.FederatedAuthenticationApi.
			GetConnectedOrgConfig(ctx, atlasFedSettings.GetId(), orgID).
			Execute()
		if err != nil {
			return false, err
		}

		projectlist, err := prepareProjectList(ctx, atlasClient)
		if err != nil {
			return false, err
		}

		convertedAuth, err := fedauth.Spec.ToAtlas(orgID, atlasFedAuth.GetIdentityProviderId(), projectlist)
		if err != nil {
			return false, err
		}

		return !federatedSettingsAreEqual(convertedAuth, atlasFedAuth), nil
	}
}
