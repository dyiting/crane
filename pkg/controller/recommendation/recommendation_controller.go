package recommendation

import (
	"context"

	"k8s.io/apimachinery/pkg/api/errors"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/scale"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	analysisv1alph1 "github.com/gocrane/api/analysis/v1alpha1"
	predictionapi "github.com/gocrane/api/prediction/v1alpha1"
	"github.com/gocrane/crane/pkg/prediction"
	"github.com/gocrane/crane/pkg/recommend"
)

// Controller is responsible for reconcile Recommendation
type Controller struct {
	client.Client
	Log         logr.Logger
	Scheme      *runtime.Scheme
	Recorder    record.EventRecorder
	RestMapper  meta.RESTMapper
	ScaleClient scale.ScalesGetter
	Predictors  map[predictionapi.AlgorithmType]prediction.Interface
}

func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	c.Log.Info("got", "Recommendation", req.NamespacedName)

	recommendation := &analysisv1alph1.Recommendation{}
	err := c.Client.Get(ctx, req.NamespacedName, recommendation)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, err
	}

	if recommendation.DeletionTimestamp != nil {
		// todo stop prediction
		return ctrl.Result{}, nil
	}

	newStatus := recommendation.Status.DeepCopy()

	recommender, err := recommend.NewRecommender(c.Client, c.RestMapper, c.ScaleClient, recommendation, c.Predictors, c.Log)
	if err != nil {
		c.Recorder.Event(recommendation, v1.EventTypeNormal, "FailedCreateRecommender", err.Error())
		c.Log.Error(err, "Failed to create recommender", "recommendation", klog.KObj(recommendation))
		setCondition(newStatus, "Ready", metav1.ConditionFalse, "FailedCreateRecommender", "Failed to create recommender")
		c.UpdateStatus(ctx, recommendation, newStatus)
		return ctrl.Result{}, err
	}

	proposed, err := recommender.Offer()
	if err != nil {
		c.Recorder.Event(recommendation, v1.EventTypeNormal, "FailedOfferRecommendation", err.Error())
		c.Log.Error(err, "Failed to offer recommend", "recommendation", klog.KObj(recommendation))
		setCondition(newStatus, "Ready", metav1.ConditionFalse, "FailedOfferRecommend", "Failed to offer recommend")
		c.UpdateStatus(ctx, recommendation, newStatus)
		return ctrl.Result{}, err
	}

	if proposed != nil {
		newStatus.ResourceRequest = proposed.ResourceRequest
		newStatus.EffectiveHPA = proposed.EffectiveHPA
	}

	setCondition(newStatus, "Ready", metav1.ConditionTrue, "RecommendationReady", "Recommendation is ready")
	c.UpdateStatus(ctx, recommendation, newStatus)

	return ctrl.Result{}, nil
}

func (c *Controller) UpdateStatus(ctx context.Context, recommendation *analysisv1alph1.Recommendation, newStatus *analysisv1alph1.RecommendationStatus) {
	if !equality.Semantic.DeepEqual(&recommendation.Status, newStatus) {
		c.Log.V(4).Info("Recommendation status should be updated", "currentStatus", &recommendation.Status, "newStatus", newStatus)

		recommendation.Status = *newStatus
		recommendation.Status.LastUpdateTime = metav1.Now()

		var ready = false
		for _, cond := range newStatus.Conditions {
			if cond.Reason == "RecommendationReady" && (newStatus.ResourceRequest != nil || newStatus.EffectiveHPA != nil) {
				ready = true
				break
			}
		}
		if ready {
			recommendation.Status.LastSuccessfulTime = &recommendation.Status.LastUpdateTime
		}

		err := c.Update(ctx, recommendation)
		if err != nil {
			c.Recorder.Event(recommendation, v1.EventTypeNormal, "FailedUpdateStatus", err.Error())
			c.Log.Error(err, "Failed to update status", "Recommendation", klog.KObj(recommendation))
			return
		}

		c.Log.Info("Update Recommendation status successful", "recommendation", klog.KObj(recommendation))
	}
}

func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&analysisv1alph1.Recommendation{}).
		Complete(c)
}

func setCondition(status *analysisv1alph1.RecommendationStatus, conditionType string, conditionStatus metav1.ConditionStatus, reason string, message string) {
	for i := range status.Conditions {
		if status.Conditions[i].Type == conditionType {
			status.Conditions[i].Status = conditionStatus
			status.Conditions[i].Reason = reason
			status.Conditions[i].Message = message
			status.Conditions[i].LastTransitionTime = metav1.Now()
			return
		}
	}
	status.Conditions = append(status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}
