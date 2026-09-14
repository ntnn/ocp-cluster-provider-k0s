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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterOptionsSpec defines the desired state of ClusterOptions.
type ClusterOptionsSpec struct {
	// Aliases are the fully-qualified hostnames to assign to the cluster's
	// container on the docker network.
	// +optional
	Aliases []string `json:"aliases,omitempty"`
}

// ClusterOptions carries per-cluster options for a single managed cluster.
// It is cluster-scoped and named after the requesting ControlPlane.
// The k0s provider reads it when creating the backing container.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:metadata:labels="openmcp.cloud/cluster=platform"
type ClusterOptions struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// Spec defines the desired state.
	// +required
	Spec ClusterOptionsSpec `json:"spec"`
}

// ClusterOptionsList contains a list of ClusterOptions.
//
// +kubebuilder:object:root=true
type ClusterOptionsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterOptions `json:"items"`
}
