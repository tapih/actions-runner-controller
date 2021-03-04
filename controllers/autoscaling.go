package controllers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/summerwind/actions-runner-controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultScaleUpThreshold   = 0.8
	defaultScaleDownThreshold = 0.3
	defaultScaleUpFactor      = 1.3
	defaultScaleDownFactor    = 0.7
)

func getValueAvailableAt(now time.Time, from, to *time.Time, reservedValue int) *int {
	if to != nil && now.After(*to) {
		return nil
	}

	if from != nil && now.Before(*from) {
		return nil
	}

	return &reservedValue
}

func (r *HorizontalRunnerAutoscalerReconciler) getDesiredReplicasFromCache(hra v1alpha1.HorizontalRunnerAutoscaler) *int {
	var entry *v1alpha1.CacheEntry

	for i := range hra.Status.CacheEntries {
		ent := hra.Status.CacheEntries[i]

		if ent.Key != v1alpha1.CacheEntryKeyDesiredReplicas {
			continue
		}

		if !time.Now().Before(ent.ExpirationTime.Time) {
			continue
		}

		entry = &ent

		break
	}

	if entry != nil {
		v := getValueAvailableAt(time.Now(), nil, &entry.ExpirationTime.Time, entry.Value)
		if v != nil {
			return v
		}
	}

	return nil
}

func (r *HorizontalRunnerAutoscalerReconciler) determineDesiredReplicas(rd v1alpha1.RunnerDeployment, hra v1alpha1.HorizontalRunnerAutoscaler) (*int, error) {
	if hra.Spec.MinReplicas == nil {
		return nil, fmt.Errorf("horizontalrunnerautoscaler %s/%s is missing minReplicas", hra.Namespace, hra.Name)
	} else if hra.Spec.MaxReplicas == nil {
		return nil, fmt.Errorf("horizontalrunnerautoscaler %s/%s is missing maxReplicas", hra.Namespace, hra.Name)
	}

	metrics := hra.Spec.Metrics
	if len(metrics) == 0 || metrics[0].Type == v1alpha1.AutoscalingMetricTypeTotalNumberOfQueuedAndInProgressWorkflowRuns {
		return r.calculateReplicasByQueuedAndInProgressWorkflowRuns(rd, hra)
	} else if metrics[0].Type == v1alpha1.AutoscalingMetricTypePercentageRunnersBusy {
		return r.calculateReplicasByPercentageRunnersBusy(rd, hra)
	} else {
		return nil, fmt.Errorf("validting autoscaling metrics: unsupported metric type %q", metrics[0].Type)
	}
}

func (r *HorizontalRunnerAutoscalerReconciler) calculateReplicasByQueuedAndInProgressWorkflowRuns(rd v1alpha1.RunnerDeployment, hra v1alpha1.HorizontalRunnerAutoscaler) (*int, error) {

	var repos [][]string
	metrics := hra.Spec.Metrics
	repoID := rd.Spec.Template.Spec.Repository
	if repoID == "" {
		orgName := rd.Spec.Template.Spec.Organization
		if orgName == "" {
			return nil, fmt.Errorf("asserting runner deployment spec to detect bug: spec.template.organization should not be empty on this code path")
		}

		if len(metrics[0].RepositoryNames) == 0 {
			return nil, errors.New("validating autoscaling metrics: spec.autoscaling.metrics[].repositoryNames is required and must have one more more entries for organizational runner deployment")
		}

		for _, repoName := range metrics[0].RepositoryNames {
			repos = append(repos, []string{orgName, repoName})
		}
	} else {
		repo := strings.Split(repoID, "/")

		repos = append(repos, repo)
	}

	var total, inProgress, queued, completed, unknown int
	type callback func()
	listWorkflowJobs := func(user string, repoName string, runID int64, fallback_cb callback) {
		if runID == 0 {
			fallback_cb()
			return
		}
		jobs, _, err := r.GitHubClient.Actions.ListWorkflowJobs(context.TODO(), user, repoName, runID, nil)
		if err != nil {
			r.Log.Error(err, "Error listing workflow jobs")
			fallback_cb()
		} else if len(jobs.Jobs) == 0 {
			fallback_cb()
		} else {
			for _, job := range jobs.Jobs {
				switch job.GetStatus() {
				case "completed":
					// We add a case for `completed` so it is not counted in `unknown`.
					// And we do not increment the counter for completed because
					// that counter only refers to workflows. The reason for
					// this is because we do not get a list of jobs for
					// completed workflows in order to keep the number of API
					// calls to a minimum.
				case "in_progress":
					inProgress++
				case "queued":
					queued++
				default:
					unknown++
				}
			}
		}
	}

	for _, repo := range repos {
		user, repoName := repo[0], repo[1]
		workflowRuns, err := r.GitHubClient.ListRepositoryWorkflowRuns(context.TODO(), user, repoName)
		if err != nil {
			return nil, err
		}

		for _, run := range workflowRuns {
			total++

			// In May 2020, there are only 3 statuses.
			// Follow the below links for more details:
			// - https://developer.github.com/v3/actions/workflow-runs/#list-repository-workflow-runs
			// - https://developer.github.com/v3/checks/runs/#create-a-check-run
			switch run.GetStatus() {
			case "completed":
				completed++
			case "in_progress":
				listWorkflowJobs(user, repoName, run.GetID(), func() { inProgress++ })
			case "queued":
				listWorkflowJobs(user, repoName, run.GetID(), func() { queued++ })
			default:
				unknown++
			}
		}
	}

	minReplicas := *hra.Spec.MinReplicas
	maxReplicas := *hra.Spec.MaxReplicas
	necessaryReplicas := queued + inProgress

	var desiredReplicas int

	if necessaryReplicas < minReplicas {
		desiredReplicas = minReplicas
	} else if necessaryReplicas > maxReplicas {
		desiredReplicas = maxReplicas
	} else {
		desiredReplicas = necessaryReplicas
	}

	rd.Status.Replicas = &desiredReplicas
	replicas := desiredReplicas

	r.Log.V(1).Info(
		"Calculated desired replicas",
		"computed_replicas_desired", desiredReplicas,
		"spec_replicas_min", minReplicas,
		"spec_replicas_max", maxReplicas,
		"workflow_runs_completed", completed,
		"workflow_runs_in_progress", inProgress,
		"workflow_runs_queued", queued,
		"workflow_runs_unknown", unknown,
		"namespace", hra.Namespace,
		"runner_deployment", rd.Name,
		"horizontal_runner_autoscaler", hra.Name,
	)

	return &replicas, nil
}

func (r *HorizontalRunnerAutoscalerReconciler) calculateReplicasByPercentageRunnersBusy(rd v1alpha1.RunnerDeployment, hra v1alpha1.HorizontalRunnerAutoscaler) (*int, error) {
	ctx := context.Background()
	minReplicas := *hra.Spec.MinReplicas
	maxReplicas := *hra.Spec.MaxReplicas
	metrics := hra.Spec.Metrics[0]
	scaleUpThreshold := defaultScaleUpThreshold
	scaleDownThreshold := defaultScaleDownThreshold
	scaleUpFactor := defaultScaleUpFactor
	scaleDownFactor := defaultScaleDownFactor

	if metrics.ScaleUpThreshold != "" {
		sut, err := strconv.ParseFloat(metrics.ScaleUpThreshold, 64)
		if err != nil {
			return nil, errors.New("validating autoscaling metrics: spec.autoscaling.metrics[].scaleUpThreshold cannot be parsed into a float64")
		}
		scaleUpThreshold = sut
	}
	if metrics.ScaleDownThreshold != "" {
		sdt, err := strconv.ParseFloat(metrics.ScaleDownThreshold, 64)
		if err != nil {
			return nil, errors.New("validating autoscaling metrics: spec.autoscaling.metrics[].scaleDownThreshold cannot be parsed into a float64")
		}

		scaleDownThreshold = sdt
	}

	scaleUpAdjustment := metrics.ScaleUpAdjustment
	if scaleUpAdjustment != 0 {
		if metrics.ScaleUpAdjustment < 0 {
			return nil, errors.New("validating autoscaling metrics: spec.autoscaling.metrics[].scaleUpAdjustment cannot be lower than 0")
		}

		if metrics.ScaleUpFactor != "" {
			return nil, errors.New("validating autoscaling metrics: spec.autoscaling.metrics[]: scaleUpAdjustment and scaleUpFactor cannot be specified together")
		}
	} else if metrics.ScaleUpFactor != "" {
		suf, err := strconv.ParseFloat(metrics.ScaleUpFactor, 64)
		if err != nil {
			return nil, errors.New("validating autoscaling metrics: spec.autoscaling.metrics[].scaleUpFactor cannot be parsed into a float64")
		}
		scaleUpFactor = suf
	}

	scaleDownAdjustment := metrics.ScaleDownAdjustment
	if scaleDownAdjustment != 0 {
		if metrics.ScaleDownAdjustment < 0 {
			return nil, errors.New("validating autoscaling metrics: spec.autoscaling.metrics[].scaleDownAdjustment cannot be lower than 0")
		}

		if metrics.ScaleDownFactor != "" {
			return nil, errors.New("validating autoscaling metrics: spec.autoscaling.metrics[]: scaleDownAdjustment and scaleDownFactor cannot be specified together")
		}
	} else if metrics.ScaleDownFactor != "" {
		sdf, err := strconv.ParseFloat(metrics.ScaleDownFactor, 64)
		if err != nil {
			return nil, errors.New("validating autoscaling metrics: spec.autoscaling.metrics[].scaleDownFactor cannot be parsed into a float64")
		}
		scaleDownFactor = sdf
	}

	var allRunnerReplicaSets v1alpha1.RunnerReplicaSetList
	if err := r.List(ctx, &allRunnerReplicaSets, client.InNamespace(rd.Namespace)); err != nil {
		return nil, err
	}

	var rs *v1alpha1.RunnerReplicaSet
	for _, s := range allRunnerReplicaSets.Items {
		if metav1.IsControlledBy(&s, &rd) {
			if rs != nil {
				return nil, fmt.Errorf("the number of RunnerReplicaSet existing should be 1")
			}
			rs = &s
		}
	}

	// return the list of runners in namespace. Horizontal Runner Autoscaler should only be responsible for scaling resources in its own ns.
	var runnerList v1alpha1.RunnerList
	if err := r.List(ctx, &runnerList, client.InNamespace(rs.Namespace), client.MatchingFields{runnerSetOwnerKey: rs.Name}); err != nil {
		return nil, err
	}
	runnerMap := make(map[string]struct{})
	for _, items := range runnerList.Items {
		runnerMap[items.Name] = struct{}{}
	}

	var (
		enterprise   = rd.Spec.Template.Spec.Enterprise
		organization = rd.Spec.Template.Spec.Organization
		repository   = rd.Spec.Template.Spec.Repository
	)

	// ListRunners will return all runners managed by GitHub - not restricted to ns
	runners, err := r.GitHubClient.ListRunners(
		ctx,
		enterprise,
		organization,
		repository)
	if err != nil {
		return nil, err
	}
	numRunners := len(runnerList.Items)
	numRunnersBusy := 0
	for _, runner := range runners {
		if _, ok := runnerMap[*runner.Name]; ok && runner.GetBusy() {
			numRunnersBusy++
		}
	}

	var desiredReplicas int
	fractionBusy := float64(numRunnersBusy) / float64(numRunners)
	if fractionBusy >= scaleUpThreshold {
		if scaleUpAdjustment > 0 {
			desiredReplicas = numRunners + scaleUpAdjustment
		} else {
			desiredReplicas = int(math.Ceil(float64(numRunners) * scaleUpFactor))
		}
	} else if fractionBusy < scaleDownThreshold {
		if scaleDownAdjustment > 0 {
			desiredReplicas = numRunners - scaleDownAdjustment
		} else {
			desiredReplicas = int(float64(numRunners) * scaleDownFactor)
		}
	} else {
		desiredReplicas = *rd.Spec.Replicas
	}

	if desiredReplicas < minReplicas {
		desiredReplicas = minReplicas
	} else if desiredReplicas > maxReplicas {
		desiredReplicas = maxReplicas
	}

	r.Log.V(1).Info(
		"Calculated desired replicas",
		"computed_replicas_desired", desiredReplicas,
		"spec_replicas_min", minReplicas,
		"spec_replicas_max", maxReplicas,
		"current_replicas", rd.Spec.Replicas,
		"num_runners", numRunners,
		"num_runners_busy", numRunnersBusy,
		"namespace", hra.Namespace,
		"runner_deployment", rd.Name,
		"horizontal_runner_autoscaler", hra.Name,
		"enterprise", enterprise,
		"organization", organization,
		"repository", repository,
	)

	rd.Status.Replicas = &desiredReplicas
	replicas := desiredReplicas

	return &replicas, nil
}
