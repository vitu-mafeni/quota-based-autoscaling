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

package controller

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	scalingv1 "github.com/vitumafeni/quota-based-scaling/api/v1"
)

// NamespaceQuotaReconciler reconciles a NamespaceQuota object
type NamespaceQuotaReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// RBAC
// +kubebuilder:rbac:groups=scaling.dcn.ssu.ac.kr,resources=namespacequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=scaling.dcn.ssu.ac.kr,resources=namespacequotas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=scaling.dcn.ssu.ac.kr,resources=namespacequotas/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch

func (r *NamespaceQuotaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("------------------ Starting reconciliation", "request", req.NamespacedName)

	var nsq scalingv1.NamespaceQuota
	if err := r.Get(ctx, req.NamespacedName, &nsq); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("NamespaceQuota resource not found. Ignoring since object must be deleted.")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling NamespaceQuota", "nsq", req.NamespacedName)

	// 1) Read the ResourceQuota referenced
	var rq corev1.ResourceQuota
	rqKey := types.NamespacedName{Namespace: nsq.Spec.AppliedQuotaRef.Namespace, Name: nsq.Spec.AppliedQuotaRef.Name}
	if err := r.Get(ctx, rqKey, &rq); err != nil {
		logger.Error(err, "failed to get ResourceQuota", "rq", rqKey)
		// Requeue after short delay
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Detect recent quota exceeded events (pod admission denied)
	quotaDenied, err := r.DetectQuotaExceeded(ctx, &rq)
	if err != nil {
		logger.Error(err, "failed to list events")
	}

	// 2) Compute usage vs hard
	usedCPU := GetUsedCPU(&rq)
	hardCPU := GetHardCPU(&rq)
	usedMem := GetUsedMemory(&rq)
	hardMem := GetHardMemory(&rq)

	utilizationCPU := 0
	utilizationMem := 0

	if hardCPU > 0 {
		utilizationCPU = int((usedCPU * 100) / hardCPU)
	}

	if hardMem > 0 {
		utilizationMem = int((usedMem * 100) / hardMem)
	}

	logger.Info("ResourceQuota utilization", "namespace", rq.Namespace, "name", rq.Name,
		"usedCPU(m)", usedCPU, "hardCPU(m)", hardCPU, "utilCPU(%)", utilizationCPU,
		"usedMem(bytes)", usedMem, "hardMem(bytes)", hardMem, "utilMem(%)", utilizationMem,
		"quotaDenied", quotaDenied)

	// Determine target
	target := int64(nsq.Spec.Behavior.QuotaScaling.TargetQuotaUtilization)

	// Primary path: quota prevented pod creation -> try quota patch or node scale immediately
	if quotaDenied {
		logger.Info("Detected quota denial event — treating as immediate shortage")

		// attempt to compute required step sizes from CR; if absent, fallback to sensible defaults
		stepCPU, errCPU := parseScaledQuantity(nsq.Spec.Behavior.QuotaScaling.ScaleStep, corev1.ResourceCPU.String())
		if errCPU != nil {
			// fallback default: 500m
			logger.Info("CPU scaleStep missing or invalid; using default 500m")
			stepCPU = 500
		}

		stepMem, errMem := parseScaledQuantity(nsq.Spec.Behavior.QuotaScaling.ScaleStep, corev1.ResourceMemory.String())
		if errMem != nil {
			logger.Info("Memory scaleStep missing or invalid; using default 512Mi")
			stepMem = 512 * 1024 * 1024
		}

		stepGPU, _ := parseScaledQuantity(nsq.Spec.Behavior.QuotaScaling.ScaleStep, "nvidia.com/gpu")

		// Free cluster capacity
		freeCPU, freeMem, freeGPU, err := computeClusterFree(ctx, r.Client)
		if err != nil {
			logger.Error(err, "failed to compute cluster free resources")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// safety margin (20%)
		safetyCPU := stepCPU + (stepCPU / 5)
		safetyMem := stepMem + (stepMem / 5)
		safetyGPU := stepGPU + (stepGPU / 5)

		if freeCPU >= safetyCPU || freeMem >= safetyMem || freeGPU >= safetyGPU {
			logger.Info("Cluster has free resources, attempting quota patch (quotaDenied path)")

			if err := r.patchQuota(ctx, &rq, stepCPU, stepMem, stepGPU, &nsq); err != nil {
				logger.Error(err, "failed to patch quota")
				return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
			}

			logger.Info("Successfully patched ResourceQuota")
			return ctrl.Result{}, nil
		}

		// Otherwise → cluster scale-up
		if nsq.Spec.Behavior.NodeScaling.Enabled {
			logger.Info("Not enough free capacity - triggering node scale-up (quotaDenied path)")

			if err := triggerScaleUpForCluster(nsq.Spec); err != nil {
				logger.Error(err, "failed to trigger scale-up")
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}

			logger.Info("Scale-up request sent")
			return ctrl.Result{}, nil
		}
	}

	// Secondary path: utilization-based proactive scaling
	if int64(utilizationCPU) >= target || int64(utilizationMem) >= target {
		logger.Info("Quota utilization above target — proactive scaling", "CPU (%)", utilizationCPU, "MEM (%)", utilizationMem)

		// ScaleStep parsing from map[string]string
		stepCPU, err := parseScaledQuantity(nsq.Spec.Behavior.QuotaScaling.ScaleStep, corev1.ResourceLimitsCPU.String())
		if err != nil {
			logger.Error(err, "missing CPU scaleStep quantity")
			return ctrl.Result{}, nil
		}

		stepMem, err := parseScaledQuantity(nsq.Spec.Behavior.QuotaScaling.ScaleStep, corev1.ResourceLimitsMemory.String())
		if err != nil {
			logger.Error(err, "missing Memory scaleStep quantity")
			return ctrl.Result{}, nil
		}

		// GPU optional
		stepGPU, _ := parseScaledQuantity(nsq.Spec.Behavior.QuotaScaling.ScaleStep, "nvidia.com/gpu")

		// Free cluster capacity
		freeCPU, freeMem, freeGPU, err := computeClusterFree(ctx, r.Client)
		if err != nil {
			logger.Error(err, "failed to compute cluster free resources")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// safety margin (20%)
		safetyCPU := stepCPU + (stepCPU / 5)
		safetyMem := stepMem + (stepMem / 5)
		safetyGPU := stepGPU + (stepGPU / 5)

		if freeCPU >= safetyCPU || freeMem >= safetyMem || freeGPU >= safetyGPU {
			logger.Info("Cluster has free resources, attempting quota patch (utilization path)")

			if err := r.patchQuota(ctx, &rq, stepCPU, stepMem, stepGPU, &nsq); err != nil {
				logger.Error(err, "failed to patch quota")
				return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
			}

			logger.Info("Successfully patched ResourceQuota")
			return ctrl.Result{}, nil
		}

		// Otherwise → cluster scale-up
		if nsq.Spec.Behavior.NodeScaling.Enabled {
			logger.Info("Not enough free capacity → triggering node scale-up (utilization path)")

			if err := triggerScaleUpForCluster(nsq.Spec); err != nil {
				logger.Error(err, "failed to trigger scale-up")
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}

			logger.Info("Scale-up request sent")
			return ctrl.Result{}, nil
		}
	}

	// Nothing to do
	return ctrl.Result{}, nil
}

func (r *NamespaceQuotaReconciler) patchQuota(
	ctx context.Context,
	rq *corev1.ResourceQuota,
	stepCPU, stepMem, stepGPU int64,
	nsq *scalingv1.NamespaceQuota,
) error {
	// Read current hard values
	curCPU := rq.Spec.Hard[corev1.ResourceLimitsCPU]
	curMem := rq.Spec.Hard[corev1.ResourceLimitsMemory]
	curGPU := rq.Spec.Hard["nvidia.com/gpu"]

	// desired = current + step
	newCPU := curCPU.DeepCopy()
	newCPU.Add(*resource.NewMilliQuantity(stepCPU, resource.DecimalSI))

	newMem := curMem.DeepCopy()
	newMem.Add(*resource.NewQuantity(stepMem, resource.BinarySI))

	if stepGPU > 0 {
		newGPU := curGPU.DeepCopy()
		newGPU.Add(*resource.NewQuantity(stepGPU, resource.DecimalSI))
		rq.Spec.Hard["nvidia.com/gpu"] = newGPU
	}

	rq.Spec.Hard[corev1.ResourceLimitsCPU] = newCPU
	rq.Spec.Hard[corev1.ResourceLimitsMemory] = newMem

	// Check against maxQuota
	maxCPUQty, ok := nsq.Spec.Behavior.QuotaScaling.MaxQuota[corev1.ResourceLimitsCPU.String()]
	if ok && maxCPUQty != "" {
		maxCPU := parseQuantityMilli(maxCPUQty)
		if newCPU.MilliValue() > maxCPU {
			return fmt.Errorf("new CPU quota %dm exceeds maxQuota %dm",
				newCPU.MilliValue(), maxCPU)
		}
	}

	maxMemQty, ok := nsq.Spec.Behavior.QuotaScaling.MaxQuota[corev1.ResourceLimitsMemory.String()]
	if ok && maxMemQty != "" {
		maxMem := parseQuantityBytes(maxMemQty)
		if newMem.Value() > maxMem {
			return fmt.Errorf("new Memory quota %d bytes exceeds maxQuota %d bytes",
				newMem.Value(), maxMem)
		}
	}

	maxGPUQty, ok := nsq.Spec.Behavior.QuotaScaling.MaxQuota["nvidia.com/gpu"]
	if ok && maxGPUQty != "" && stepGPU > 0 {
		maxGPU := parseQuantityMilli(maxGPUQty)
		newGPU := rq.Spec.Hard["nvidia.com/gpu"]
		if newGPU.MilliValue() > maxGPU {
			return fmt.Errorf("new GPU quota %dm exceeds maxQuota %dm",
				newGPU.MilliValue(), maxGPU)
		}
	}

	// print the new whole complete quotas resource for logging
	cpu := rq.Spec.Hard[corev1.ResourceLimitsCPU]
	mem := rq.Spec.Hard[corev1.ResourceLimitsMemory]
	gpu := rq.Spec.Hard[corev1.ResourceName("nvidia.com/gpu")]

	postRQ := &corev1.ResourceQuota{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ResourceQuota",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      rq.Name,
			Namespace: rq.Namespace,
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceLimitsCPU:    rq.Spec.Hard[corev1.ResourceLimitsCPU],
				corev1.ResourceLimitsMemory: rq.Spec.Hard[corev1.ResourceLimitsMemory],
			},
		},
	}

	if curGPU, ok := rq.Spec.Hard["nvidia.com/gpu"]; ok {
		postRQ.Spec.Hard["nvidia.com/gpu"] = curGPU
	}

	postRQ.ObjectMeta.CreationTimestamp = metav1.Time{}

	fmt.Printf(
		"Patching ResourceQuota %s/%s: CPU=%s, Memory=%s, GPU=%s\n",
		rq.Namespace, rq.Name,
		cpu.String(), mem.String(), gpu.String(),
	)

	// print the whole patched ResourceQuota for logging
	// yamlBytes, _ := yaml.Marshal(postRQ)
	// fmt.Println("Sending minimal ResourceQuota YAML:\n" + string(yamlBytes))

	// send POST request with the yaml of the patched resourcequota to some endpoint URL
	endpointURL := nsq.Spec.ClusterRef.EndpointServer
	if endpointURL == "" {
		return fmt.Errorf("clusterRef.endpointServer is empty")
	}

	// Send the patched ResourceQuota YAML
	if err := postYAML(postRQ, endpointURL); err != nil {
		return err
	}

	fmt.Printf("Successfully POSTed patched ResourceQuota YAML to %s\n", endpointURL)

	// Apply patch
	return r.Update(ctx, rq)
}

func postYAML(obj interface{}, url string) error {
	// Convert object to YAML
	data, err := yaml.Marshal(obj)
	if err != nil {
		return fmt.Errorf("failed to marshal object to YAML: %w", err)
	}

	// Prepare request
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(data))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-yaml")

	// Send request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to POST yaml: %w", err)
	}
	defer resp.Body.Close()

	// Ensure success status
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("endpoint returned non-2xx status: %s", resp.Status)
	}

	return nil
}

// computeClusterFree is a helper that sums allocatable and subtracts requested
// computeClusterFree returns free CPU (milli), free Mem (bytes), free GPU (count)
// It excludes control-plane / unschedulable nodes by:
//   - skipping nodes with Spec.Unschedulable == true
//   - skipping nodes that have taints typically used for control-plane/master (NoSchedule)
func computeClusterFree(ctx context.Context, c client.Client) (int64, int64, int64, error) {
	var nodeList corev1.NodeList
	if err := c.List(ctx, &nodeList); err != nil {
		return 0, 0, 0, err
	}

	// helper to detect control-plane/master taints
	isControlTaint := func(t corev1.Taint) bool {
		// common control-plane taint keys; adapt if your cluster uses different keys
		switch t.Key {
		case "node-role.kubernetes.io/control-plane",
			"node-role.kubernetes.io/master",
			"node.kubernetes.io/master":
			return true
		default:
			return false
		}
	}

	workerNodes := make(map[string]struct{})
	var allocCPU, allocMem, allocGPU int64

	for _, n := range nodeList.Items {
		// skip explicitly unschedulable nodes
		if n.Spec.Unschedulable {
			continue
		}

		// skip if any NoSchedule control-plane/master taint present
		skip := false
		for _, t := range n.Spec.Taints {
			if isControlTaint(t) && (t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectPreferNoSchedule) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		// This node is considered a worker for our calculations
		workerNodes[n.Name] = struct{}{}

		if cpu := n.Status.Allocatable[corev1.ResourceCPU]; cpu.Value() > 0 {
			allocCPU += cpu.MilliValue()
		}
		if mem := n.Status.Allocatable[corev1.ResourceMemory]; mem.Value() > 0 {
			allocMem += mem.Value()
		}
		if gpu, exists := n.Status.Allocatable["nvidia.com/gpu"]; exists {
			allocGPU += gpu.Value()
		}
	}

	// List pods cluster-wide and sum only those scheduled on worker nodes
	var podList corev1.PodList
	if err := c.List(ctx, &podList); err != nil {
		return 0, 0, 0, err
	}

	var usedCPU, usedMem, usedGPU int64
	for _, p := range podList.Items {
		// skip terminated pods
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}

		// skip pods not yet scheduled (they don't consume node allocatable)
		if p.Spec.NodeName == "" {
			continue
		}

		// only count pods that are scheduled onto considered worker nodes
		if _, ok := workerNodes[p.Spec.NodeName]; !ok {
			continue
		}

		// sum container requests
		for _, ctr := range p.Spec.Containers {
			if cpu, exists := ctr.Resources.Requests[corev1.ResourceCPU]; exists {
				usedCPU += cpu.MilliValue()
			}
			if mem, exists := ctr.Resources.Requests[corev1.ResourceMemory]; exists {
				usedMem += mem.Value()
			}
			if gpu, exists := ctr.Resources.Requests["nvidia.com/gpu"]; exists {
				usedGPU += gpu.Value()
			}
		}
		// include init containers too (they contribute to requests at runtime)
		for _, ict := range p.Spec.InitContainers {
			if cpu, exists := ict.Resources.Requests[corev1.ResourceCPU]; exists {
				usedCPU += cpu.MilliValue()
			}
			if mem, exists := ict.Resources.Requests[corev1.ResourceMemory]; exists {
				usedMem += mem.Value()
			}
			if gpu, exists := ict.Resources.Requests["nvidia.com/gpu"]; exists {
				usedGPU += gpu.Value()
			}
		}
	}

	freeCPU := allocCPU - usedCPU
	freeMem := allocMem - usedMem
	freeGPU := allocGPU - usedGPU

	// Defensive: don't return negative free values
	if freeCPU < 0 {
		freeCPU = 0
	}
	if freeMem < 0 {
		freeMem = 0
	}
	if freeGPU < 0 {
		freeGPU = 0
	}

	// debug log: which nodes we considered (optional)
	// build a small slice to print
	var nodes []string
	for n := range workerNodes {
		nodes = append(nodes, n)
	}
	fmt.Printf("Counted worker nodes: %v\n", nodes)
	fmt.Printf("Cluster allocatable (workers only): CPU=%dm, Mem=%d bytes, GPU=%d\n", allocCPU, allocMem, allocGPU)
	fmt.Printf("Cluster requested (workers only):   CPU=%dm, Mem=%d bytes, GPU=%d\n", usedCPU, usedMem, usedGPU)

	return freeCPU, freeMem, freeGPU, nil
}

// Helpers to parse CR strings
func parseQuantityMilli(s string) int64 {
	if s == "" {
		return 0
	}
	q := resource.MustParse(s)
	return q.MilliValue()
}
func parseQuantityBytes(s string) int64 {
	if s == "" {
		return 0
	}
	q := resource.MustParse(s)
	return q.Value()
}

func parseScaledQuantity(m map[string]string, key string) (int64, error) {
	val, ok := m[key]
	if !ok || val == "" {
		return 0, fmt.Errorf("scaleStep missing key %s", key)
	}

	q, err := resource.ParseQuantity(val)
	if err != nil {
		return 0, fmt.Errorf("invalid quantity for %s: %v", key, err)
	}

	switch key {
	case "cpu":
		return q.MilliValue(), nil
	case "memory":
		return q.Value(), nil
	default:
		// Future extensible: GPU, ephemeral-storage, and others
		// Use Value() by default, but log a warning
		return q.Value(), nil
	}
}

// Reads used CPU from ResourceQuota.Status.Used
func GetUsedCPU(rq *corev1.ResourceQuota) int64 {
	// check limits.cpu then requests.cpu
	if qty, ok := rq.Status.Used[corev1.ResourceName("limits.cpu")]; ok {
		return qty.MilliValue()
	}
	if qty, ok := rq.Status.Used[corev1.ResourceName("requests.cpu")]; ok {
		return qty.MilliValue()
	}
	return 0
}

// Reads hard CPU from ResourceQuota.Spec.Hard
func GetHardCPU(rq *corev1.ResourceQuota) int64 {
	if qty, ok := rq.Spec.Hard[corev1.ResourceName("limits.cpu")]; ok {
		return qty.MilliValue()
	}
	if qty, ok := rq.Spec.Hard[corev1.ResourceName("requests.cpu")]; ok {
		return qty.MilliValue()
	}
	return 0
}

func GetUsedMemory(rq *corev1.ResourceQuota) int64 {
	// prefer limits.memory then requests.memory
	if qty, ok := rq.Status.Used[corev1.ResourceName("limits.memory")]; ok {
		return qty.Value()
	}
	if qty, ok := rq.Status.Used[corev1.ResourceName("requests.memory")]; ok {
		return qty.Value()
	}
	return 0
}

// Reads hard memory from ResourceQuota.Spec.Hard
func GetHardMemory(rq *corev1.ResourceQuota) int64 {
	if qty, ok := rq.Spec.Hard[corev1.ResourceName("limits.memory")]; ok {
		return qty.Value()
	}
	if qty, ok := rq.Spec.Hard[corev1.ResourceName("requests.memory")]; ok {
		return qty.Value()
	}
	return 0
}

// Detect whether a recent event indicates quota exceeded for this ResourceQuota
// DetectQuotaExceeded attempts multiple signals to determine whether
// quota exhaustion is blocking pod creation in the namespace.
func (r *NamespaceQuotaReconciler) DetectQuotaExceeded(ctx context.Context, rq *corev1.ResourceQuota) (bool, error) {
	// Check ResourceQuota status usage
	hardCPU := rq.Status.Hard.Cpu().MilliValue()
	usedCPU := rq.Status.Used.Cpu().MilliValue()

	hardMem := rq.Status.Hard.Memory().Value()
	usedMem := rq.Status.Used.Memory().Value()

	// Guard against zero values — no quota set
	if hardCPU > 0 && usedCPU >= hardCPU {
		return true, nil
	}
	if hardMem > 0 && usedMem >= hardMem {
		return true, nil
	}

	// Check Deployment/ReplicaSet failure conditions in namespace
	var depList appsv1.DeploymentList
	if err := r.List(ctx, &depList, client.InNamespace(rq.Namespace)); err == nil {
		for _, dep := range depList.Items {
			for _, cond := range dep.Status.Conditions {
				if cond.Type == appsv1.DeploymentReplicaFailure &&
					strings.Contains(strings.ToLower(cond.Message), "quota") {
					return true, nil
				}
			}
		}
	}

	var rsList appsv1.ReplicaSetList
	if err := r.List(ctx, &rsList, client.InNamespace(rq.Namespace)); err == nil {
		for _, rs := range rsList.Items {
			for _, cond := range rs.Status.Conditions {
				if cond.Type == appsv1.ReplicaSetReplicaFailure &&
					strings.Contains(strings.ToLower(cond.Message), "quota") {
					return true, nil
				}
			}
		}
	}

	// Fallback → Check for recent FailedCreate events
	var evList corev1.EventList
	if err := r.List(ctx, &evList, client.InNamespace(rq.Namespace)); err == nil {
		now := time.Now()
		for _, ev := range evList.Items {
			if ev.Type != corev1.EventTypeWarning {
				continue
			}
			msg := strings.ToLower(ev.Message)
			if ev.Reason != "FailedCreate" && !strings.Contains(msg, "exceeded quota") {
				continue
			}
			if now.Sub(ev.LastTimestamp.Time) <= 2*time.Minute {
				return true, nil
			}
		}
	}

	// Nothing detected — quota OK
	return false, nil
}

// triggerScaleUpForCluster is a stub that would call cloud provider APIs
func triggerScaleUpForCluster(spec scalingv1.NamespaceQuotaSpec) error {
	// TODO: implement provider-specific scale-up (ASG API, GKE node pool resize, OpenStack heat)
	// For now, just log and return nil
	fmt.Println("[stub] triggerScaleUpForCluster called for clusterRef:", spec.ClusterRef.Name)
	return nil
}

// map Deployment changes to NamespaceQuota CRs that reference a ResourceQuota
func (r *NamespaceQuotaReconciler) watchDeploymentToNamespaceQuotas(obj client.Object) []reconcile.Request {
	dep := obj.(*appsv1.Deployment)
	var out []reconcile.Request

	// find NamespaceQuota CRs in the deployment namespace
	var list scalingv1.NamespaceQuotaList
	if err := r.List(context.Background(), &list, client.InNamespace(dep.Namespace)); err != nil {
		return nil
	}
	for _, nsq := range list.Items {
		// if the NSQ references a ResourceQuota in this namespace, enqueue it
		if nsq.Spec.AppliedQuotaRef.Namespace == dep.Namespace {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: nsq.Namespace, Name: nsq.Name}})
		}
	}
	return out
}

// map ReplicaSet changes to NamespaceQuota CRs that reference a ResourceQuota
func (r *NamespaceQuotaReconciler) watchReplicaSetToNamespaceQuotas(obj client.Object) []reconcile.Request {
	rs := obj.(*appsv1.ReplicaSet)
	var out []reconcile.Request

	var list scalingv1.NamespaceQuotaList
	if err := r.List(context.Background(), &list, client.InNamespace(rs.Namespace)); err != nil {
		return nil
	}
	for _, nsq := range list.Items {
		if nsq.Spec.AppliedQuotaRef.Namespace == rs.Namespace {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: nsq.Namespace, Name: nsq.Name}})
		}
	}
	return out
}

// SetupWithManager sets up the controller with the Manager.
func (r *NamespaceQuotaReconciler) watchResourceQuotaToNamespaceQuotas(ctx context.Context, obj client.Object) []reconcile.Request {
	// Map ResourceQuota changes to NamespaceQuota CRs in the same namespace that reference it
	rq := obj.(*corev1.ResourceQuota)
	var out []reconcile.Request
	var list scalingv1.NamespaceQuotaList
	if err := r.List(context.Background(), &list, client.InNamespace(rq.Namespace)); err != nil {
		return nil
	}
	for _, nsq := range list.Items {
		if nsq.Spec.AppliedQuotaRef.Name == rq.Name && nsq.Spec.AppliedQuotaRef.Namespace == rq.Namespace {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: nsq.Namespace, Name: nsq.Name}})
		}
	}
	return out
}

func (r *NamespaceQuotaReconciler) watchEventToNamespaceQuotas(obj client.Object) []reconcile.Request {
	ev := obj.(*corev1.Event)
	if ev.Type != corev1.EventTypeWarning {
		return nil
	}
	if ev.Reason != "FailedCreate" && !strings.Contains(strings.ToLower(ev.Message), "exceeded quota") {
		return nil
	}

	// find NamespaceQuota CRs in the event namespace and enqueue them (cheap if few)
	var out []reconcile.Request
	var list scalingv1.NamespaceQuotaList
	if err := r.List(context.Background(), &list, client.InNamespace(ev.InvolvedObject.Namespace)); err != nil {
		return nil
	}
	for _, nsq := range list.Items {
		// only enqueue if the ResourceQuota name appears in the event message or if the NSQ references a quota in this namespace
		if strings.Contains(ev.Message, nsq.Spec.AppliedQuotaRef.Name) || nsq.Spec.AppliedQuotaRef.Namespace == ev.InvolvedObject.Namespace {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: nsq.Namespace, Name: nsq.Name}})
		}
	}
	return out
}
func (r *NamespaceQuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(
			&scalingv1.NamespaceQuota{}).
		Watches(&v1.ResourceQuota{},
			handler.EnqueueRequestsFromMapFunc(
				r.watchResourceQuotaToNamespaceQuotas)).
		Named("namespacequota").Complete(r)
}
