/*
Copyright 2023 The Nephio Authors.

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

package capi

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	// capictrl "github.com/vitu1234/transition-operator/reconcilers/capi"

	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	argov1alpha1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/nephio-project/nephio/controllers/pkg/resource"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/scale/scheme"
	"k8s.io/client-go/tools/clientcmd"
	capiv1beta1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	batchv1 "k8s.io/api/batch/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	kubeConfigSuffix = "-kubeconfig"
)

type Capi struct {
	client.Client
	Secret *corev1.Secret
	l      logr.Logger
}

func (r *Capi) GetClusterName() string {
	if r.Secret == nil {
		return ""
	}
	return strings.TrimSuffix(r.Secret.GetName(), kubeConfigSuffix)
}

func (r *Capi) GetClusterClient(ctx context.Context) (resource.APIPatchingApplicator, *rest.Config, bool, error) {
	if !r.isCapiClusterReady(ctx) {

		return resource.APIPatchingApplicator{}, nil, false, fmt.Errorf("workload cluster %q not Ready yet", r.GetClusterName())

	}
	return getCapiClusterClient(r.Secret)
}

func (r *Capi) isCapiClusterReady(ctx context.Context) bool {
	r.l = log.FromContext(ctx)
	name := r.GetClusterName()

	cl := resource.GetUnstructuredFromGVK(&schema.GroupVersionKind{Group: capiv1beta1.GroupVersion.Group, Version: capiv1beta1.GroupVersion.Version, Kind: reflect.TypeFor[capiv1beta1.Cluster]().Name()})
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Secret.GetNamespace(), Name: name}, cl); err != nil {
		r.l.Error(err, "cannot get cluster")
		return false
	}
	b, err := json.Marshal(cl)
	if err != nil {
		r.l.Error(err, "cannot marshal cluster")
		return false
	}
	cluster := &capiv1beta1.Cluster{}
	if err := json.Unmarshal(b, cluster); err != nil {
		r.l.Error(err, "cannot unmarshal cluster")
		return false
	}
	return isReady(cluster.GetConditions())
}

func isReady(cs capiv1beta1.Conditions) bool {
	for _, c := range cs {
		if c.Type == capiv1beta1.ReadyCondition {
			if c.Status == corev1.ConditionTrue {
				return true
			}
		}
	}
	return false
}

func getCapiClusterClient(secret *corev1.Secret) (resource.APIPatchingApplicator, *rest.Config, bool, error) {
	scheme := GetDefaultKubeScheme()
	utilruntime.Must(velerov1.AddToScheme(scheme))
	_ = argov1alpha1.AddToScheme(scheme)

	// build rest.Config from kubeconfig
	config, err := clientcmd.RESTConfigFromKubeConfig(secret.Data["value"])
	if err != nil {
		return resource.APIPatchingApplicator{}, nil, false, err
	}

	clClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return resource.APIPatchingApplicator{}, nil, false, err
	}

	return resource.NewAPIPatchingApplicator(clClient), config, true, nil
}

// GetCapiCluster returns a Capi instance for the given secret.
func GetCapiCluster(secret *corev1.Secret, cl client.Client) *Capi {
	if secret == nil {
		return nil
	}
	if !strings.HasSuffix(secret.GetName(), kubeConfigSuffix) {
		return nil
	}
	return &Capi{
		Client: cl,
		Secret: secret,
		l:      log.Log.WithName("capi"),
	}
}

// GetCapiClusterFromName returns a Capi instance for the given cluster name.
func GetCapiClusterFromName(ctx context.Context, name string, namespace string, cl client.Client) (*Capi, error) {
	if name == "" {
		return nil, nil
	}
	secret := &corev1.Secret{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name + kubeConfigSuffix}, secret); err != nil {
		return nil, err
	}
	return GetCapiCluster(secret, cl), nil
}

// GetCapiClusterFromSecret returns a Capi instance for the given secret.
func GetCapiClusterFromSecret(secret *corev1.Secret, cl client.Client) *Capi {
	if secret == nil {
		return nil
	}
	if !strings.HasSuffix(secret.GetName(), kubeConfigSuffix) {
		return nil
	}
	return &Capi{
		Client: cl,
		Secret: secret,
		l:      log.Log.WithName("capi"),
	}
}

// Register Velero resources
func GetSchemeWithVelero() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()

	// Add core types (like Pods, Secrets, etc.)
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}

	// Add Velero CRDs
	if err := velerov1.AddToScheme(scheme); err != nil {
		return nil, err
	}

	return scheme, nil
}

func GetDefaultKubeScheme() *runtime.Scheme {
	s := runtime.NewScheme()

	// Add all default types
	utilruntime.Must(corev1.AddToScheme(s))
	utilruntime.Must(appsv1.AddToScheme(s))
	utilruntime.Must(batchv1.AddToScheme(s))
	utilruntime.Must(rbacv1.AddToScheme(s))
	utilruntime.Must(autoscalingv1.AddToScheme(s))
	utilruntime.Must(networkingv1.AddToScheme(s))
	utilruntime.Must(policyv1.AddToScheme(s))
	utilruntime.Must(storagev1.AddToScheme(s))
	utilruntime.Must(apiextensionsv1.AddToScheme(s))

	// Optionally also add the client-go default scheme (aggregates many)
	utilruntime.Must(scheme.AddToScheme(s))

	return s
}

// NEW

// GetWorkloadClusterClient returns a client + rest config for a workload cluster.
func GetWorkloadClusterClient(
	ctx context.Context,
	c client.Client, // <— pass client explicitly instead of using r.Client
	clusterName string,
) (ctrl.Result, client.Client, *rest.Config, error) {
	log := logf.FromContext(ctx)

	clusterList := &capiv1beta1.ClusterList{}
	if err := c.List(ctx, clusterList); err != nil {
		return ctrl.Result{}, nil, nil, err
	}

	var capiCluster *Capi
	for _, workloadCluster := range clusterList.Items {
		if clusterName == workloadCluster.Name {
			var err error
			capiCluster, err = GetCapiClusterFromName(ctx, clusterName, "default", c)
			if err != nil {
				log.Error(err, "Failed to get CAPI cluster")
				return ctrl.Result{}, nil, nil, err
			}
			break
		}
	}

	if capiCluster == nil {
		return ctrl.Result{}, nil, nil, fmt.Errorf("no matching cluster found for %s", clusterName)
	}

	// fmt.Printf("DEBUG: Found CAPI cluster: %s\n", capiCluster.GetClusterName())

	clusterClient, restConfig, ready, err := capiCluster.GetClusterClient(ctx)
	if err != nil {
		log.Error(err, "Failed to get workload cluster client", "cluster", capiCluster.GetClusterName())
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil, nil, nil
	}
	if !ready {
		log.Info("Workload cluster not Ready yet, requeueing", "cluster", capiCluster.GetClusterName())
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil, nil, nil
	}

	return ctrl.Result{}, clusterClient, restConfig, nil
}
