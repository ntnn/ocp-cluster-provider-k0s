package cluster

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types on the Cluster resource.
const (
	// ConditionK0sReady reports whether the backing k0s cluster exists and
	// came up.
	ConditionK0sReady = "K0sReady"
	// ConditionMetalLBReady reports whether metallb is installed and ready in the backing k0s cluster.
	ConditionMetalLBReady = "MetalLBReady"
	// ConditionReady is the aggregate readiness of the Cluster.
	ConditionReady = "Ready"
)

func (r *reconciler) setConditionReady(status bool, reason, message string) {
	r.setCondition(ConditionReady, status, reason, message)
}

func (r *reconciler) setConditionK0sReady(status bool, reason, message string) {
	r.setCondition(ConditionK0sReady, status, reason, message)
	// A failed prerequisite immediately falsifies the aggregate.
	if !status {
		r.setConditionReady(false, "K0sNotReady", "k0s cluster is not ready")
	}
}

func (r *reconciler) setConditionMetalLBReady(status bool, reason, message string) {
	r.setCondition(ConditionMetalLBReady, status, reason, message)
	// A failed prerequisite immediately falsifies the aggregate.
	if !status {
		r.setConditionReady(false, "MetalLBNotReady", "metallb is not ready")
	}
}

func (r *reconciler) setCondition(conditionType string, status bool, reason, message string) {
	metaStatus := metav1.ConditionTrue
	if !status {
		metaStatus = metav1.ConditionFalse
	}
	meta.SetStatusCondition(
		&r.cluster.Status.Conditions,
		metav1.Condition{
			Type:               conditionType,
			ObservedGeneration: r.cluster.Generation,
			Status:             metaStatus,
			Reason:             reason,
			Message:            message,
		},
	)
}
