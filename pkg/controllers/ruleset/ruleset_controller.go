/*
Copyright 2023 The KusionStack Authors.

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

package ruleset

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	appsv1alpha1 "kusionstack.io/kafed/apis/apps/v1alpha1"
	"kusionstack.io/kafed/pkg/controllers/ruleset/processor"
	"kusionstack.io/kafed/pkg/controllers/ruleset/register"
	rulesetutils "kusionstack.io/kafed/pkg/controllers/ruleset/utils"
	"kusionstack.io/kafed/pkg/utils"
)

const (
	controllerName          = "ruleset-controller"
	resourceName            = "RuleSet"
	maxConcurrentReconciles = 10
	cleanUpFinalizer        = "ruleset.kafed.kusionstack.io/need-clean-up"

	rulesetTerminatingLabel = "ruleset.kafed.kusionstack.io/terminating"
)

var (
	podEventQueues []workqueue.DelayingInterface
)

// NewReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &RuleSetReconciler{
		Client:   mgr.GetClient(),
		Policy:   register.DefaultPolicy(),
		Logger:   logf.Log.WithName(controllerName).WithValues("kind", resourceName),
		recorder: mgr.GetEventRecorderFor(controllerName),
	}
}

func addToMgr(mgr manager.Manager, r reconcile.Reconciler) (controller.Controller, error) {
	// Create a new controller
	c, err := controller.New(controllerName, mgr, controller.Options{
		MaxConcurrentReconciles: maxConcurrentReconciles,
		Reconciler:              r,
	})
	if err != nil {
		return nil, err
	}

	// Watch for changes to RuleSet
	err = c.Watch(&source.Kind{Type: &appsv1alpha1.RuleSet{}}, &RulesetEventHandler{})
	if err != nil {
		return c, err
	}

	err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &EventHandler{client: mgr.GetClient()})
	if err != nil {
		return c, err
	}

	return c, nil
}

// RuleSetReconciler reconciles a RuleSet object
type RuleSetReconciler struct {
	client.Client
	register.Policy
	scheme   *runtime.Scheme
	recorder record.EventRecorder
	logr.Logger
}

//+kubebuilder:rbac:groups=apps.kusionstack.io,resources=rulesets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps.kusionstack.io,resources=rulesets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps.kusionstack.io,resources=rulesets/finalizers,verbs=update

func (r *RuleSetReconciler) Reconcile(ctx context.Context, request reconcile.Request) (result reconcile.Result, reconcileErr error) {

	result = reconcile.Result{}
	ruleSet := &appsv1alpha1.RuleSet{}
	if err := r.Get(context.TODO(), request.NamespacedName, ruleSet); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if !rulesetutils.RulesetVersionExpectation.SatisfiedExpectations(utils.ObjectKey(ruleSet), ruleSet.ResourceVersion) {
		r.Info(fmt.Sprintf("expected ruleset %s update, resource version %s, retry later", utils.ObjectKey(ruleSet), ruleSet.ResourceVersion))
		fmt.Printf("expected ruleset %s update, resource version %s, retry later", utils.ObjectKey(ruleSet), ruleSet.ResourceVersion)
		return reconcile.Result{}, nil
	}

	selector, _ := metav1.LabelSelectorAsSelector(&ruleSet.Spec.Selector)
	selectedPods := &corev1.PodList{}
	if err := r.List(context.TODO(), selectedPods, &client.ListOptions{Namespace: ruleSet.Namespace, LabelSelector: selector}); err != nil {
		r.Error(err, fmt.Sprintf("fail to list pods by ruleset %s", request.NamespacedName.String()))
		return reconcile.Result{}, err
	}

	// Delete
	if ruleSet.DeletionTimestamp != nil {
		if _, find := ruleSet.Labels[rulesetTerminatingLabel]; find || !r.hasRunningPod(selectedPods) {
			if err := r.cleanUpPod(ruleSet); err != nil {
				return reconcile.Result{}, err
			}
			return reconcile.Result{}, utils.RemoveFinalizer(ctx, r.Client, ruleSet, cleanUpFinalizer)
		}
		msg := fmt.Sprintf("can not delete ruleset: there are some pods waiting for process by ruleset %s/%s. Please terminate pods first or label ruleset kafed.kusionstack.io/terminating=true to force delete it", ruleSet.Namespace, ruleSet.Name)
		result.RequeueAfter = 5 * time.Second
		r.recorder.Event(ruleSet, corev1.EventTypeWarning, "BlockProtection", msg)
	} else if err := utils.AddFinalizer(ctx, r.Client, ruleSet, cleanUpFinalizer); err != nil {
		return result, fmt.Errorf("fail to add finalizer on RuleSet %s: %s", request, err)
	}

	selectedPodNames := sets.String{}
	for _, pod := range selectedPods.Items {
		if !rulesetutils.PodVersionExpectation.SatisfiedExpectations(utils.ObjectKey(&pod), pod.ResourceVersion) {
			r.Info(fmt.Sprintf("expected pod %s update, resource version %s, retry later", utils.ObjectKey(&pod), pod.ResourceVersion))
			return reconcile.Result{}, nil
		}
		selectedPodNames.Insert(pod.Name)
	}
	targetPods := map[string]*corev1.Pod{}
	for i, pod := range selectedPods.Items {
		targetPods[pod.Name] = &selectedPods.Items[i]
	}

	// remove unselected pods
	for _, name := range ruleSet.Status.Targets {
		if selectedPodNames.Has(name) {
			continue
		}

		if err := r.updateRuleSetOnPod(ruleSet.Name,
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ruleSet.Namespace}},
			rulesetutils.MoveRulesetAnno); err != nil {
			r.Info(fmt.Sprintf("fail to remove ruleset on pod %s, %v", name, err))
			return result, err
		}
	}

	// own selected pods
	for name, pod := range targetPods {
		if err := r.updateRuleSetOnPod(ruleSet.Name, pod, rulesetutils.AddRuleSetAnno); err != nil {
			r.Info(fmt.Sprintf("fail to add ruleset on pod %s, %v", name, err))
			return result, err
		}
	}

	// process rules
	shouldRetry, interval, details, ruleStates := r.process(ruleSet, targetPods)

	res := reconcile.Result{
		Requeue: shouldRetry,
	}
	if interval != nil {
		res.RequeueAfter = *interval
	}

	// TODO: Sync WebhookStates in Details

	detailList := make([]*appsv1alpha1.Detail, 0, len(details))
	keys := make([]string, 0, len(details))
	for key := range details {
		keys = append(keys, key)
	}
	// ensure the order of the slice. (ensure DeepEqual)
	sort.Strings(keys)
	for _, key := range keys {
		detailList = append(detailList, details[key])
	}
	// update ruleset status
	tm := metav1.NewTime(time.Now())
	newStatus := &appsv1alpha1.RuleSetStatus{
		Targets:            selectedPodNames.List(),
		ObservedGeneration: ruleSet.Generation,
		Details:            detailList,
		RuleStates:         ruleStates,
		UpdateTime:         &tm,
	}

	if !equalStatus(newStatus, &ruleSet.Status) {
		rulesetutils.RulesetVersionExpectation.ExpectUpdate(utils.ObjectKey(ruleSet), ruleSet.ResourceVersion)
		ruleSet.Status = *newStatus
		if err := r.Status().Update(ctx, ruleSet); err != nil {
			rulesetutils.RulesetVersionExpectation.DeleteExpectations(utils.ObjectKey(ruleSet))
			r.Error(err, fmt.Sprintf("fail to update ruleset %s status", utils.ObjectKey(ruleSet)))
			return reconcile.Result{}, err
		}
	}

	oldDetails := map[string]*appsv1alpha1.Detail{}

	for i, de := range ruleSet.Status.Details {
		oldDetails[de.Name] = ruleSet.Status.Details[i]
	}
	addQueues(ruleSet, details, oldDetails)
	return res, nil
}

func (r *RuleSetReconciler) process(rs *appsv1alpha1.RuleSet, pods map[string]*corev1.Pod) (shouldRetry bool, interval *time.Duration, details map[string]*appsv1alpha1.Detail, ruleStates []*appsv1alpha1.RuleState) {
	stages := r.GetStages()
	wg := sync.WaitGroup{}
	wg.Add(len(stages))
	mu := sync.RWMutex{}
	details = map[string]*appsv1alpha1.Detail{}
	for _, stage := range stages {
		currentStage := stage
		go func() {
			defer wg.Done()
			res := processor.NewRuleProcessor(r.Client, currentStage, rs, r.Logger).Process(pods)
			mu.Lock()
			defer mu.Unlock()
			if res.Interval != nil {
				if interval == nil || *interval > *res.Interval {
					interval = res.Interval
				}
			}
			if res.RuleStates != nil {
				ruleStates = append(ruleStates, res.RuleStates...)
			}
			if res.Retry {
				shouldRetry = true
			}
			updateDetail(details, res, currentStage)
		}()
	}
	wg.Wait()
	return shouldRetry, interval, details, ruleStates
}

func addQueues(rs *appsv1alpha1.RuleSet, details map[string]*appsv1alpha1.Detail, oldDetails map[string]*appsv1alpha1.Detail) {
	if len(podEventQueues) == 0 {
		return
	}
	for name, old := range oldDetails {
		detail, ok := details[name]
		if !ok {
			addToEveryQueue(rs.Namespace, name)
			continue
		}
		if !equality.Semantic.DeepEqual(old, detail) {
			addToEveryQueue(rs.Namespace, name)
		}
	}
	for name := range details {
		_, ok := oldDetails[name]
		if !ok {
			addToEveryQueue(rs.Namespace, name)
			continue
		}
	}
}

func addToEveryQueue(namespace, name string) {
	for _, q := range podEventQueues {
		if q.ShuttingDown() {
			continue
		}
		q.Add(types.NamespacedName{
			Name:      name,
			Namespace: namespace,
		})
	}
}

func (r *RuleSetReconciler) cleanUpPod(ruleSet *appsv1alpha1.RuleSet) error {
	for _, name := range ruleSet.Status.Targets {
		pod := &corev1.Pod{}
		if err := r.Get(context.TODO(), types.NamespacedName{Namespace: ruleSet.Namespace, Name: name}, pod); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}
		if err := r.updateRuleSetOnPod(ruleSet.Name, pod, rulesetutils.MoveRulesetAnno); err != nil {
			return fmt.Errorf("fail to remove RuleSet %s on pod %s: %v", utils.ObjectKey(ruleSet), name, err)
		}
	}
	return nil
}

func (r *RuleSetReconciler) updateRuleSetOnPod(ruleSet string, pod *corev1.Pod, fn func(pod *corev1.Pod, name string) bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(context.TODO(), types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, pod); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if fn(pod, ruleSet) {
			err := r.Update(context.TODO(), pod)
			if errors.IsNotFound(err) {
				return nil
			}
		}
		return nil
	})
}

func (r *RuleSetReconciler) hasRunningPod(pods *corev1.PodList) bool {
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp == nil {
			return true
		}
	}
	return false
}

func updateDetail(details map[string]*appsv1alpha1.Detail, passRules *processor.ProcessResult, stage string) {
	for po, rules := range passRules.PassRules {
		var rejectInfo *appsv1alpha1.RejectInfo
		if rej, ok := passRules.Rejected[po]; ok {
			rejectInfo = &appsv1alpha1.RejectInfo{
				RuleName: rej.RuleName,
				Reason:   rej.Reason,
			}
		}
		detail, ok := details[po]
		if !ok {
			detail = &appsv1alpha1.Detail{
				Name:  po,
				Stage: stage,
			}
		}
		detail.PassedRules = append(detail.PassedRules, rules.List()...)
		if rejectInfo != nil {
			detail.RejectInfo = append(detail.RejectInfo, *rejectInfo)
		}
		detail.Passed = detail.RejectInfo == nil || len(detail.RejectInfo) == 0
		details[po] = detail
	}
}

func equalStatus(updated *appsv1alpha1.RuleSetStatus, current *appsv1alpha1.RuleSetStatus) bool {
	return equality.Semantic.DeepEqual(updated.Targets, current.Targets) &&
		equality.Semantic.DeepEqual(updated.Details, current.Details) &&
		equality.Semantic.DeepEqual(updated.RuleStates, current.RuleStates) &&
		updated.ObservedGeneration == current.ObservedGeneration
}