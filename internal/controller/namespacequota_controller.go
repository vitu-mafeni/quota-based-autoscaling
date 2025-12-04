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
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	scalingv1 "github.com/vitumafeni/quota-based-scaling/api/v1"
)

// NamespaceQuotaReconciler reconciles a NamespaceQuota object
type NamespaceQuotaReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=scaling.dcn.ssu.ac.kr,resources=namespacequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=scaling.dcn.ssu.ac.kr,resources=namespacequotas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=scaling.dcn.ssu.ac.kr,resources=namespacequotas/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the NamespaceQuota object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *NamespaceQuotaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var nsq scalingv1.NamespaceQuota
	if err := r.Get(ctx, req.NamespacedName, &nsq); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 1) Read the ResourceQuota referenced
	var rq corev1.ResourceQuota
	rqKey := types.NamespacedName{Namespace: nsq.Spec.AppliedQuotaRef.Namespace, Name: nsq.Spec.AppliedQuotaRef.Name}
	if err := r.Get(ctx, rqKey, &rq); err != nil {
		logger.Error(err, "failed to get ResourceQuota", "rq", rqKey)
		// Requeue after short delay
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// 2) Compute usage vs hard
	usedCPU := GetUsedCPU(&rq)
	hardCPU := GetHardCPU(&rq)
	usedMem := GetUsedMemory(&rq)
	hardMem := GetHardMemory(&rq)

	utilizationCPU := int((usedCPU * 100) / hardCPU)
	utilizationMem := int((usedMem * 100) / hardMem)

	logger.Info("ResourceQuota utilization", "namespace", rq.Namespace, "name", rq.Name,
		"usedCPU(m)", usedCPU, "hardCPU(m)", hardCPU, "utilCPU(%)", utilizationCPU,
		"usedMem(bytes)", usedMem, "hardMem(bytes)", hardMem, "utilMem(%)", utilizationMem)

	// 3) If utilization exceeds targetQuotaUtilization → attempt quota patch or node scale
	// 3) If utilization exceeds targetQuotaUtilization → attempt quota patch or node scale
	target := int64(nsq.Spec.Behavior.QuotaScaling.TargetQuotaUtilization)

	if int64(utilizationCPU) >= target || int64(utilizationMem) >= target {
		logger.Info("Quota utilization above target", "CPU (%)", utilizationCPU, "MEM (%)", utilizationMem)

		// ScaleStep parsing from map[string]string
		stepCPU, err := parseScaledQuantity(nsq.Spec.Behavior.QuotaScaling.ScaleStep, corev1.ResourceCPU.String())
		if err != nil {
			logger.Error(err, "missing CPU scaleStep quantity")
			return ctrl.Result{}, nil
		}

		stepMem, err := parseScaledQuantity(nsq.Spec.Behavior.QuotaScaling.ScaleStep, corev1.ResourceMemory.String())
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
			logger.Info("Cluster has free resources, attempting quota patch")

			if err := r.patchQuota(ctx, &rq, stepCPU, stepMem, stepGPU, &nsq); err != nil {
				logger.Error(err, "failed to patch quota")
				return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
			}

			logger.Info("Successfully patched ResourceQuota")
			return ctrl.Result{}, nil
		}

		// Otherwise → cluster scale-up
		if nsq.Spec.Behavior.NodeScaling.Enabled {
			logger.Info("Not enough free capacity → triggering node scale-up")

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

func (r *NamespaceQuotaReconciler) patchQuota(ctx context.Context, rq *corev1.ResourceQuota, stepCPU, stepMem, stepGPU int64, nsq *scalingv1.NamespaceQuota) error {
	// Read current hard values
	// current hard values
	curCPU := rq.Spec.Hard[corev1.ResourceLimitsCPU]
	curMem := rq.Spec.Hard[corev1.ResourceLimitsMemory]
	curGPU := rq.Spec.Hard["nvidia.com/gpu"]
	// desired = current + step
	newCPU := curCPU.DeepCopy()
	newCPU.Add(*resource.NewMilliQuantity(stepCPU, resource.DecimalSI))

	newMem := curMem.DeepCopy()
	newMem.Add(*resource.NewMilliQuantity(stepMem, resource.BinarySI))

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
			return fmt.Errorf("new CPU quota %dm exceeds maxQuota %dm", newCPU.MilliValue(), maxCPU)
		}
	}

	maxMemQty, ok := nsq.Spec.Behavior.QuotaScaling.MaxQuota[corev1.ResourceLimitsMemory.String()]
	if ok && maxMemQty != "" {
		maxMem := parseQuantityBytes(maxMemQty)
		if newMem.Value() > maxMem {
			return fmt.Errorf("new Memory quota %d bytes exceeds maxQuota %d bytes", newMem.Value(), maxMem)
		}
	}
	maxGPUQty, ok := nsq.Spec.Behavior.QuotaScaling.MaxQuota["nvidia.com/gpu"]
	if ok && maxGPUQty != "" && stepGPU > 0 {
		maxGPU := parseQuantityMilli(maxGPUQty)
		newGPU := rq.Spec.Hard["nvidia.com/gpu"]
		if newGPU.MilliValue() > maxGPU {
			return fmt.Errorf("new GPU quota %dm exceeds maxQuota %dm", newGPU.MilliValue(), maxGPU)
		}
	}

	// print the new whole complete quotas resource for logging
	cpu := rq.Spec.Hard[corev1.ResourceLimitsCPU]
	mem := rq.Spec.Hard[corev1.ResourceLimitsMemory]
	gpu := rq.Spec.Hard[corev1.ResourceName("nvidia.com/gpu")]

	fmt.Printf(
		"Patching ResourceQuota %s/%s: CPU=%s, Memory=%s, GPU=%s\n",
		rq.Namespace, rq.Name,
		cpu.String(),
		mem.String(),
		gpu.String(),
	)

	// Apply patch
	return r.Update(ctx, rq)
}

// computeClusterFree is a helper that sums allocatable and subtracts requested
func computeClusterFree(ctx context.Context, c client.Client) (int64, int64, int64, error) {
	var nodeList corev1.NodeList
	if err := c.List(ctx, &nodeList); err != nil {
		return 0, 0, 0, err
	}

	var allocCPU, allocMem, allocGPU int64
	for _, n := range nodeList.Items {
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

	var podList corev1.PodList
	if err := c.List(ctx, &podList); err != nil {
		return 0, 0, 0, err
	}

	var usedCPU, usedMem, usedGPU int64
	for _, p := range podList.Items {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}

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
	}

	freeCPU := allocCPU - usedCPU
	freeMem := allocMem - usedMem
	freeGPU := allocGPU - usedGPU

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
	return q.MilliValue(), nil // CPU → milli-core, Memory → milli-bytes (convert later if needed)
}

// Reads used CPU from ResourceQuota.Status.Used
func GetUsedCPU(rq *corev1.ResourceQuota) int64 {
	// Change to "limits.cpu" if that is your quota key
	key := corev1.ResourceCPU

	qty, ok := rq.Status.Used[key]
	if !ok {
		return 0
	}
	return qty.MilliValue()
}

// Reads hard CPU from ResourceQuota.Spec.Hard
func GetHardCPU(rq *corev1.ResourceQuota) int64 {
	// Change to "limits.cpu" if needed
	key := corev1.ResourceCPU

	qty, ok := rq.Spec.Hard[key]
	if !ok {
		return 0
	}
	return qty.MilliValue()
}

func GetUsedMemory(rq *corev1.ResourceQuota) int64 {
	// Change to "limits.memory" if needed
	key := corev1.ResourceMemory

	qty, ok := rq.Status.Used[key]
	if !ok {
		return 0
	}
	return qty.Value() // bytes
}

// Reads hard memory from ResourceQuota.Spec.Hard
func GetHardMemory(rq *corev1.ResourceQuota) int64 {
	key := corev1.ResourceMemory

	qty, ok := rq.Spec.Hard[key]
	if !ok {
		return 0
	}
	return qty.Value() // bytes
}

// triggerScaleUpForCluster is a stub that would call cloud provider APIs
func triggerScaleUpForCluster(spec scalingv1.NamespaceQuotaSpec) error {
	// TODO: implement provider-specific scale-up (ASG API, GKE node pool resize, OpenStack heat)
	// For now, just log and return nil
	fmt.Println("[stub] triggerScaleUpForCluster called for clusterRef:", spec.ClusterRef.Name)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NamespaceQuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&scalingv1.NamespaceQuota{}).
		Named("namespacequota").
		Complete(r)
}
