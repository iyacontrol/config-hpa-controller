package confighpa

import (
	"fmt"
	"log"
	"math"
	"time"

	autoscaling "k8s.io/api/autoscaling/v2beta1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	v1coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	metricsclient "k8s.io/kubernetes/pkg/controller/podautoscaler/metrics"
)

const (
	// defaultTestingTolerance is default value for calculating when to
	// scale up/scale down.
	defaultTestingTolerance = 0.1
)

// ReplicaCalculator is responsible for calculation of the number of replicas
// It contains all the needed information
type ReplicaCalculator struct {
	metricsClient metricsclient.MetricsClient
	podsGetter    v1coreclient.PodsGetter
	tolerance     float64
}

// NewReplicaCalculator returns a ReplicaCalculator object reference
func NewReplicaCalculator(metricsClient metricsclient.MetricsClient, podsGetter v1coreclient.PodsGetter, tolerance float64) *ReplicaCalculator {
	return &ReplicaCalculator{
		metricsClient: metricsClient,
		podsGetter:    podsGetter,
		tolerance:     tolerance,
	}
}

// GetResourceReplicas calculates the desired replica count based on a target resource utilization percentage
// of the given resource for pods matching the given selector in the given namespace, and the current replica count
func (c *ReplicaCalculator) GetResourceReplicas(currentReplicas int32, targetUtilization int32, resource v1.ResourceName, namespace string, selector labels.Selector) (replicaCount int32, utilization int32, rawUtilization int64, timestamp time.Time, err error) {
	metrics, timestamp, err := c.metricsClient.GetResourceMetric(resource, namespace, selector)
	if err != nil {
		return 0, 0, 0, time.Time{}, fmt.Errorf("unable to get metrics for resource %s: %v", resource, err)
	}

	log.Printf("-> metrics: %v\n", metrics)
	podList, err := c.podsGetter.Pods(namespace).List(metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return 0, 0, 0, time.Time{}, fmt.Errorf("unable to get pods while calculating replica count: %v", err)
	}

	itemsLen := len(podList.Items)
	if itemsLen == 0 {
		return 0, 0, 0, time.Time{}, fmt.Errorf("no pods returned by selector while calculating replica count")
	}

	requests := make(map[string]int64, itemsLen)
	readyPodCount := 0
	unreadyPods := sets.NewString()
	missingPods := sets.NewString()

	for _, pod := range podList.Items {
		podSum := int64(0)
		for _, container := range pod.Spec.Containers {
			if containerRequest, ok := container.Resources.Requests[resource]; ok {
				podSum += containerRequest.MilliValue()
			} else {
				return 0, 0, 0, time.Time{}, fmt.Errorf("missing request for %s on container %s in pod %s/%s", resource, container.Name, namespace, pod.Name)
			}
		}

		requests[pod.Name] = podSum

		if pod.Status.Phase != v1.PodRunning || !podutil.IsPodReady(&pod) {
			// save this pod name for later, but pretend it doesn't exist for now
			if pod.Status.Phase != v1.PodFailed {
				// Failed pods should not be counted as unready pods as they will
				// not become running anymore.
				log.Printf("-> unready pod %s\n", pod.Name)
				unreadyPods.Insert(pod.Name)
			}
			delete(metrics, pod.Name)
			continue
		}

		if _, found := metrics[pod.Name]; !found {
			// save this pod name for later, but pretend it doesn't exist for now
			missingPods.Insert(pod.Name)
			continue
		}

		readyPodCount++
	}

	if len(metrics) == 0 {
		return 0, 0, 0, time.Time{}, fmt.Errorf("did not receive metrics for any ready pods")
	}

	usageRatio, utilization, rawUtilization, err := metricsclient.GetResourceUtilizationRatio(metrics, requests, targetUtilization)
	if err != nil {
		return 0, 0, 0, time.Time{}, err
	}

	rebalanceUnready := len(unreadyPods) > 0 && usageRatio > 1.0
	if !rebalanceUnready && len(missingPods) == 0 {
		if math.Abs(1.0-usageRatio) <= c.tolerance {
			// return the current replicas if the change would be too small
			log.Printf("GetResourceReplicas1 -> outReplicas:%v outUtilization:%v outRawValue:%v outTimestamp:%v\n", currentReplicas, utilization, rawUtilization, timestamp)
			return currentReplicas, utilization, rawUtilization, timestamp, nil
		}

		// if we don't have any unready or missing pods, we can calculate the new replica count now
		log.Printf("GetResourceReplicas2 -> outReplicas:%v outUtilization:%v outRawValue:%v outTimestamp:%v\n", int32(math.Ceil(usageRatio*float64(readyPodCount))), utilization, rawUtilization, timestamp)
		return int32(math.Ceil(usageRatio * float64(readyPodCount))), utilization, rawUtilization, timestamp, nil
	}

	if len(missingPods) > 0 {
		if usageRatio < 1.0 {
			// on a scale-down, treat missing pods as using 100% of the resource request
			for podName := range missingPods {
				metrics[podName] = requests[podName]
			}
		} else if usageRatio > 1.0 {
			// on a scale-up, treat missing pods as using 0% of the resource request
			for podName := range missingPods {
				metrics[podName] = 0
			}
		}
	}

	if rebalanceUnready {
		// on a scale-up, treat unready pods as using 0% of the resource request
		for podName := range unreadyPods {
			metrics[podName] = 0
		}
	}

	// re-run the utilization calculation with our new numbers
	newUsageRatio, _, _, err := metricsclient.GetResourceUtilizationRatio(metrics, requests, targetUtilization)
	if err != nil {
		return 0, utilization, rawUtilization, time.Time{}, err
	}

	if math.Abs(1.0-newUsageRatio) <= c.tolerance || (usageRatio < 1.0 && newUsageRatio > 1.0) || (usageRatio > 1.0 && newUsageRatio < 1.0) {
		// return the current replicas if the change would be too small,
		// or if the new usage ratio would cause a change in scale direction
		log.Printf("GetResourceReplicas3 -> outReplicas:%v outUtilization:%v outRawValue:%v outTimestamp:%v\n", currentReplicas, utilization, rawUtilization, timestamp)
		return currentReplicas, utilization, rawUtilization, timestamp, nil
	}

	// return the result, where the number of replicas considered is
	// however many replicas factored into our calculation
	log.Printf("GetResourceReplicas4 -> outReplicas:%v outUtilization:%v outRawValue:%v outTimestamp:%v\n", int32(math.Ceil(newUsageRatio*float64(len(metrics)))), utilization, rawUtilization, timestamp)
	return int32(math.Ceil(newUsageRatio * float64(len(metrics)))), utilization, rawUtilization, timestamp, nil
}

// GetRawResourceReplicas calculates the desired replica count based on a target resource utilization (as a raw milli-value)
// for pods matching the given selector in the given namespace, and the current replica count
func (c *ReplicaCalculator) GetRawResourceReplicas(currentReplicas int32, targetUtilization int64, resource v1.ResourceName, namespace string, selector labels.Selector) (replicaCount int32, utilization int64, timestamp time.Time, err error) {
	metrics, timestamp, err := c.metricsClient.GetResourceMetric(resource, namespace, selector)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("unable to get metrics for resource %s: %v", resource, err)
	}

	replicaCount, utilization, err = c.calcPlainMetricReplicas(metrics, currentReplicas, targetUtilization, namespace, selector)
	return replicaCount, utilization, timestamp, err
}

// GetMetricReplicas calculates the desired replica count based on a target metric utilization
// (as a milli-value) for pods matching the given selector in the given namespace, and the
// current replica count
func (c *ReplicaCalculator) GetMetricReplicas(currentReplicas int32, targetUtilization int64, metricName string, namespace string, selector labels.Selector) (replicaCount int32, utilization int64, timestamp time.Time, err error) {
	metrics, timestamp, err := c.metricsClient.GetRawMetric(metricName, namespace, selector)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("unable to get metric %s: %v", metricName, err)
	}

	replicaCount, utilization, err = c.calcPlainMetricReplicas(metrics, currentReplicas, targetUtilization, namespace, selector)
	return replicaCount, utilization, timestamp, err
}

// calcPlainMetricReplicas calculates the desired replicas for plain (i.e. non-utilization percentage) metrics.
func (c *ReplicaCalculator) calcPlainMetricReplicas(metrics metricsclient.PodMetricsInfo, currentReplicas int32, targetUtilization int64, namespace string, selector labels.Selector) (replicaCount int32, utilization int64, err error) {
	podList, err := c.podsGetter.Pods(namespace).List(metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return 0, 0, fmt.Errorf("unable to get pods while calculating replica count: %v", err)
	}

	if len(podList.Items) == 0 {
		return 0, 0, fmt.Errorf("no pods returned by selector while calculating replica count")
	}

	readyPodCount := 0
	unreadyPods := sets.NewString()
	missingPods := sets.NewString()

	for _, pod := range podList.Items {
		if pod.Status.Phase != v1.PodRunning || !podutil.IsPodReady(&pod) {
			// save this pod name for later, but pretend it doesn't exist for now
			unreadyPods.Insert(pod.Name)
			delete(metrics, pod.Name)
			continue
		}

		if _, found := metrics[pod.Name]; !found {
			// save this pod name for later, but pretend it doesn't exist for now
			missingPods.Insert(pod.Name)
			continue
		}

		readyPodCount++
	}

	if len(metrics) == 0 {
		return 0, 0, fmt.Errorf("did not receive metrics for any ready pods")
	}

	usageRatio, utilization := metricsclient.GetMetricUtilizationRatio(metrics, targetUtilization)

	rebalanceUnready := len(unreadyPods) > 0 && usageRatio > 1.0

	if !rebalanceUnready && len(missingPods) == 0 {
		if math.Abs(1.0-usageRatio) <= c.tolerance {
			// return the current replicas if the change would be too small
			return currentReplicas, utilization, nil
		}

		// if we don't have any unready or missing pods, we can calculate the new replica count now
		return int32(math.Ceil(usageRatio * float64(readyPodCount))), utilization, nil
	}

	if len(missingPods) > 0 {
		if usageRatio < 1.0 {
			// on a scale-down, treat missing pods as using 100% of the resource request
			for podName := range missingPods {
				metrics[podName] = targetUtilization
			}
		} else {
			// on a scale-up, treat missing pods as using 0% of the resource request
			for podName := range missingPods {
				metrics[podName] = 0
			}
		}
	}

	if rebalanceUnready {
		// on a scale-up, treat unready pods as using 0% of the resource request
		for podName := range unreadyPods {
			metrics[podName] = 0
		}
	}

	// re-run the utilization calculation with our new numbers
	newUsageRatio, _ := metricsclient.GetMetricUtilizationRatio(metrics, targetUtilization)

	if math.Abs(1.0-newUsageRatio) <= c.tolerance || (usageRatio < 1.0 && newUsageRatio > 1.0) || (usageRatio > 1.0 && newUsageRatio < 1.0) {
		// return the current replicas if the change would be too small,
		// or if the new usage ratio would cause a change in scale direction
		return currentReplicas, utilization, nil
	}

	// return the result, where the number of replicas considered is
	// however many replicas factored into our calculation
	return int32(math.Ceil(newUsageRatio * float64(len(metrics)))), utilization, nil
}

// GetObjectMetricReplicas calculates the desired replica count based on a target metric utilization (as a milli-value)
// for the given object in the given namespace, and the current replica count.
func (c *ReplicaCalculator) GetObjectMetricReplicas(currentReplicas int32, targetUtilization int64, metricName string, namespace string, objectRef *autoscaling.CrossVersionObjectReference) (replicaCount int32, utilization int64, timestamp time.Time, err error) {
	utilization, timestamp, err = c.metricsClient.GetObjectMetric(metricName, namespace, objectRef)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("unable to get metric %s: %v on %s %s/%s", metricName, objectRef.Kind, namespace, objectRef.Name, err)
	}

	usageRatio := float64(utilization) / float64(targetUtilization)
	if math.Abs(1.0-usageRatio) <= c.tolerance {
		// return the current replicas if the change would be too small
		return currentReplicas, utilization, timestamp, nil
	}
	replicaCount = int32(math.Ceil(usageRatio * float64(currentReplicas)))

	return replicaCount, utilization, timestamp, nil
}

// GetExternalMetricReplicas calculates the desired replica count based on a
// target metric value (as a milli-value) for the external metric in the given
// namespace, and the current replica count.
func (c *ReplicaCalculator) GetExternalMetricReplicas(currentReplicas int32, targetUtilization int64, metricName, namespace string, selector *metav1.LabelSelector) (replicaCount int32, utilization int64, timestamp time.Time, err error) {
	labelSelector, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return 0, 0, time.Time{}, err
	}
	metrics, timestamp, err := c.metricsClient.GetExternalMetric(metricName, namespace, labelSelector)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("unable to get external metric %s/%s/%+v: %s", namespace, metricName, selector, err)
	}
	utilization = 0
	for _, val := range metrics {
		utilization = utilization + val
	}

	usageRatio := float64(utilization) / float64(targetUtilization)
	if math.Abs(1.0-usageRatio) <= c.tolerance {
		// return the current replicas if the change would be too small
		return currentReplicas, utilization, timestamp, nil
	}

	return int32(math.Ceil(usageRatio * float64(currentReplicas))), utilization, timestamp, nil
}

// GetExternalPerPodMetricReplicas calculates the desired replica count based on a
// target metric value per pod (as a milli-value) for the external metric in the
// given namespace, and the current replica count.
func (c *ReplicaCalculator) GetExternalPerPodMetricReplicas(currentReplicas int32, targetUtilizationPerPod int64, metricName, namespace string, selector *metav1.LabelSelector) (replicaCount int32, utilization int64, timestamp time.Time, err error) {
	labelSelector, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return 0, 0, time.Time{}, err
	}
	metrics, timestamp, err := c.metricsClient.GetExternalMetric(metricName, namespace, labelSelector)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("unable to get external metric %s/%s/%+v: %s", namespace, metricName, selector, err)
	}
	utilization = 0
	for _, val := range metrics {
		utilization = utilization + val
	}

	replicaCount = currentReplicas
	usageRatio := float64(utilization) / (float64(targetUtilizationPerPod) * float64(replicaCount))
	if math.Abs(1.0-usageRatio) > c.tolerance {
		// update number of replicas if the change is large enough
		replicaCount = int32(math.Ceil(float64(utilization) / float64(targetUtilizationPerPod)))
	}
	utilization = int64(math.Ceil(float64(utilization) / float64(currentReplicas)))
	return replicaCount, utilization, timestamp, nil
}
