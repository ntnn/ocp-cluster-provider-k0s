package cluster

import (
	"fmt"

	clustersv1alpha1 "github.com/openmcp-project/openmcp-operator/api/clusters/v1alpha1"

	"github.com/openmcp-project/cluster-provider-k0s/api/v1alpha1"
)

// AnnotationName overrides the name of the backing k0s cluster.
var AnnotationName = v1alpha1.GroupVersion.Group + "/name"

// K0sName returns the name of the k0s cluster backing the given Cluster.
// The AnnotationName annotation overrides the derived name.
func K0sName(cluster *clustersv1alpha1.Cluster) string {
	if name, ok := cluster.Annotations[AnnotationName]; ok {
		return name
	}
	return fmt.Sprintf("%s-%s", cluster.Name, string(cluster.UID)[:8])
}
