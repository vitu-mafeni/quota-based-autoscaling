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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// NamespaceQuotaSpec defines the desired state of NamespaceQuota
type NamespaceQuotaSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// foo is an example field of NamespaceQuota. Edit namespacequota_types.go to remove/update
	// +optional
	ClusterRef      ClusterRef      `json:"clusterRef,omitempty"`
	AppliedQuotaRef AppliedQuotaRef `json:"appliedQuotaRef,omitempty"`
	Behavior        Behavior        `json:"behavior,omitempty"`
}

type ClusterRef struct {
	Name          string `json:"name,omitempty"`
	RepositoryURL string `json:"repositoryUrl,omitempty"`
}

type AppliedQuotaRef struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
}

type Behavior struct {
	QuotaScaling QuotaScaling `json:"quotaScaling,omitempty"`
	NodeScaling  NodeScaling  `json:"nodeScaling,omitempty"`
}

type QuotaScaling struct {
	Enabled                bool              `json:"enabled"`
	MinQuota               map[string]string `json:"minQuota,omitempty"`               // resourceName -> quantity (string)
	MaxQuota               map[string]string `json:"maxQuota,omitempty"`               // resourceName -> quantity
	ScaleStep              map[string]string `json:"scaleStep,omitempty"`              // resourceName -> quantity
	TargetQuotaUtilization int               `json:"targetQuotaUtilization,omitempty"` // percent
	Mode                   string            `json:"mode,omitempty"`                   // either|both|cpu-only|memory-only (optional)
}

type ResourceQuota struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// NodeScaling config
type NodeScaling struct {
	Enabled                bool   `json:"enabled"`
	MinNodes               int    `json:"minNodes,omitempty"`
	MaxNodes               int    `json:"maxNodes,omitempty"`
	NodeFlavor             string `json:"nodeFlavor,omitempty"`
	ScaleUpCooldownSeconds int    `json:"scaleUpCooldownSeconds,omitempty"`
	ScaleUpThreshold       int    `json:"scaleUpThreshold,omitempty"`
}

// NamespaceQuotaStatus defines the observed state of NamespaceQuota.
type NamespaceQuotaStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the NamespaceQuota resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// NamespaceQuota is the Schema for the namespacequotas API
type NamespaceQuota struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of NamespaceQuota
	// +required
	Spec NamespaceQuotaSpec `json:"spec"`

	// status defines the observed state of NamespaceQuota
	// +optional
	Status NamespaceQuotaStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// NamespaceQuotaList contains a list of NamespaceQuota
type NamespaceQuotaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NamespaceQuota `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NamespaceQuota{}, &NamespaceQuotaList{})
}
