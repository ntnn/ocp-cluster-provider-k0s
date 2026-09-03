package cluster

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	// reusing metallb from the kind provider, which will have cosmetic
	// side-effects with kind labels etcpp but less pain than copying.
	"github.com/openmcp-project/cluster-provider-kind/pkg/metallb"

	"github.com/openmcp-project/cluster-provider-k0s/pkg/k0s"
)

const metalLBRequeue = 10 * time.Second

// ensureSubnet assigns the cluster a free slice of the docker network for its metallb address pool.
// Persisted as an annotation before the cluster is created so a restart cannot lose the allocation.
func (r *reconciler) ensureSubnet(ctx context.Context) error {
	if _, ok := r.cluster.Annotations[k0s.AnnotationAssignedSubnet]; ok {
		return nil
	}

	subnet, err := r.opts.Provider.NextAvailableLBNetwork(ctx, r.opts.PlatformCluster.Client())
	if err != nil {
		return fmt.Errorf("assigning subnet: %w", err)
	}

	metav1.SetMetaDataAnnotation(&r.cluster.ObjectMeta, k0s.AnnotationAssignedSubnet, subnet.String())
	if err := r.opts.PlatformCluster.Client().Update(ctx, r.cluster); err != nil {
		return fmt.Errorf("persisting assigned subnet: %w", err)
	}
	return nil
}

// ensureMetalLB installs metallb into the target cluster and configures its address pool from the assigned subnet.
// Returns false while the metallb pods are still coming up.
func (r *reconciler) ensureMetalLB(ctx context.Context) (bool, error) {
	subnet, err := k0s.SubnetFromCluster(r.cluster)
	if err != nil {
		return false, err
	}
	if subnet == nil {
		return false, fmt.Errorf("cluster has no assigned subnet")
	}

	targetClient, err := r.targetClient(ctx)
	if err != nil {
		return false, err
	}

	if err := metallb.Install(ctx, targetClient); err != nil {
		r.setConditionMetalLBReady(false, "InstallFailed", err.Error())
		return false, fmt.Errorf("installing metallb: %w", err)
	}

	ready, err := metallb.IsReady(ctx, targetClient)
	if err != nil {
		r.setConditionMetalLBReady(false, "ReadinessCheckFailed", err.Error())
		return false, fmt.Errorf("checking metallb readiness: %w", err)
	}

	if !ready {
		r.setConditionMetalLBReady(false, "PodsNotReady", "metallb pods are not ready yet")
		return false, nil
	}

	if err := metallb.ConfigureSubnet(ctx, targetClient, *subnet); err != nil {
		r.setConditionMetalLBReady(false, "SubnetConfigurationFailed", err.Error())
		return false, fmt.Errorf("configuring metallb subnet: %w", err)
	}

	r.setConditionMetalLBReady(true, "AllPodsReady", "")
	return true, nil
}

// targetClient returns a client for the cluster's internal endpoint.
func (r *reconciler) targetClient(ctx context.Context) (client.Client, error) {
	restConfig, err := r.restConfig(ctx, K0sName(r.cluster), true)
	if err != nil {
		return nil, err
	}
	targetClient, err := client.New(restConfig, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("creating client for k0s cluster %q: %w", K0sName(r.cluster), err)
	}
	return targetClient, nil
}
