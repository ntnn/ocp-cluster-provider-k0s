#!/usr/bin/env bash
#
# ocp on a k0s platform cluster with the k0s cluster provider installed.
# Requires docker, kubectl, task, go and crane.

log() { echo ">>> $@"; }
die() { echo "$@" >&2; exit 1; }
installed() { command -v "$1" &> /dev/null; }

root=$(cd "$(dirname "$0")/.." && pwd)

require_tools() {
    for tool in docker kubectl task go crane; do
        installed "$tool" || die "$tool is required"
    done
}

# renovate: datasource=github-releases depName=openmcp-project/openmcp-operator
OPENMCP_OPERATOR_VERSION=${OPENMCP_OPERATOR_VERSION:-v1.3.0}
OPENMCP_OPERATOR_IMAGE=${OPENMCP_OPERATOR_IMAGE:-ghcr.io/openmcp-project/images/openmcp-operator:${OPENMCP_OPERATOR_VERSION}}
OPENMCP_ENVIRONMENT=${OPENMCP_ENVIRONMENT:-debug}

# Defaults to the locally built image, see build_provider_image.
OPENMCP_CP_K0S_IMAGE=${OPENMCP_CP_K0S_IMAGE:-ghcr.io/openmcp-project/images/cluster-provider-k0s:$("$root/hack/common/get-version.sh")}

# Must match pkg/k0s DefaultVersion; docker tags use '-' instead of '+'.
# renovate: datasource=github-releases depName=k0sproject/k0s
K0S_VERSION=${K0S_VERSION:-v1.36.4+k0s.0}
K0S_IMAGE=${K0S_IMAGE:-docker.io/k0sproject/k0s:${K0S_VERSION//+/-}}

# Service providers and platform services, versions matching ocpctl's
# environment defaults (pkg/config/environment-defaults.yaml).
# renovate: datasource=docker registryUrl=https://ghcr.io depName=openmcp-project/images/service-provider-crossplane
SP_CROSSPLANE_IMAGE=${SP_CROSSPLANE_IMAGE:-ghcr.io/openmcp-project/images/service-provider-crossplane:v1.0.2}
# renovate: datasource=docker registryUrl=https://ghcr.io depName=openmcp-project/images/service-provider-flux
SP_FLUX_IMAGE=${SP_FLUX_IMAGE:-ghcr.io/openmcp-project/images/service-provider-flux:v1.1.0}
# renovate: datasource=docker registryUrl=https://ghcr.io depName=open-component-model/images/service-provider-ocm
SP_OCM_IMAGE=${SP_OCM_IMAGE:-ghcr.io/open-component-model/images/service-provider-ocm:v0.3.0}
# renovate: datasource=docker registryUrl=https://ghcr.io depName=openmcp-project/images/service-provider-kro
SP_KRO_IMAGE=${SP_KRO_IMAGE:-ghcr.io/openmcp-project/images/service-provider-kro:v1.1.0}
# renovate: datasource=docker registryUrl=https://ghcr.io depName=openmcp-project/images/platform-service-gateway
PS_GATEWAY_IMAGE=${PS_GATEWAY_IMAGE:-ghcr.io/openmcp-project/images/platform-service-gateway:v0.0.14}
# renovate: datasource=docker registryUrl=https://ghcr.io depName=openmcp-project/images/platform-service-project-workspace
# v2.4.0 enforces FIPS 140-3 and fails to verify ECDSA-signed CAs, stay below until fixed upstream.
PS_PROJECT_WORKSPACE_IMAGE=${PS_PROJECT_WORKSPACE_IMAGE:-ghcr.io/openmcp-project/images/platform-service-project-workspace:v2.3.0}
FLUX2_INSTALL_URL=${FLUX2_INSTALL_URL:-https://github.com/fluxcd/flux2/releases/latest/download/install.yaml}
ENVOY_PROXY_IMAGE=${ENVOY_PROXY_IMAGE:-ghcr.io/openmcp-project/components/github.com/openmcp-project/openmcp/images/envoy-proxy:distroless-v1.36.2}
ENVOY_GATEWAY_IMAGE=${ENVOY_GATEWAY_IMAGE:-ghcr.io/openmcp-project/components/github.com/openmcp-project/openmcp/images/envoy-gateway:v1.5.4}
ENVOY_RATELIMIT_IMAGE=${ENVOY_RATELIMIT_IMAGE:-ghcr.io/openmcp-project/components/github.com/openmcp-project/openmcp/images/envoy-ratelimit:99d85510}
ENVOY_GATEWAY_CHART_URL=${ENVOY_GATEWAY_CHART_URL:-oci://ghcr.io/openmcp-project/components/github.com/openmcp-project/openmcp/charts/envoy-gateway}
ENVOY_GATEWAY_CHART_TAG=${ENVOY_GATEWAY_CHART_TAG:-1.5.4}

platform_cluster=platform
platform_container="k0s-${platform_cluster}"
# Must match pkg/k0s DefaultNetwork; all clusters join it so pods on the
# platform cluster can reach the created API servers by container name.
platform_network="k0s"

webhook_host=pwo-webhooks.platform.openmcp-system.openmcp.cluster.local

# k0s_exec runs a command in a cluster container.
k0s_exec() {
    container=$1
    shift
    docker exec -i "$container" "$@"
}

# write_kubeconfig writes a host-usable kubeconfig (127.0.0.1 + published
# port) for the given cluster container to the given file.
write_kubeconfig() {
    container=$1
    outfile=$2
    port=$(docker port "$container" 6443/tcp | head -1 | awk -F: '{print $NF}') || die "failed to determine API port of $container"
    k0s_exec "$container" k0s kubeconfig admin | sed "s|server: https://.*|server: https://127.0.0.1:${port}|" > "$outfile" \
        || die "failed to write kubeconfig for $container"
}

create_platform_cluster() {
    docker network inspect "$platform_network" &> /dev/null \
        || docker network create "$platform_network" > /dev/null \
        || die "failed to create docker network $platform_network"

    if docker inspect "$platform_container" &> /dev/null; then
        log "platform cluster exists"
    else
        log "creating platform k0s cluster"
        # Same mechanics as pkg/k0s, including the provider labels so the
        # platform Cluster resource adopts this container. The host docker
        # socket is mounted so the provider pod can create sibling clusters.
        k0s_config="apiVersion: k0s.k0sproject.io/v1beta1
kind: ClusterConfig
spec:
  api:
    sans:
      - ${platform_container}
      - 127.0.0.1"
        docker run -d --name "$platform_container" --hostname "$platform_container" \
            --privileged --cgroupns=private --restart unless-stopped \
            --network "$platform_network" \
            --label "app=cluster-provider-k0s" \
            --label "cluster-provider-k0s/cluster=${platform_cluster}" \
            --volume /var/lib/k0s \
            --volume /var/run/docker.sock:/var/run/host-docker.sock \
            --publish 6443 \
            --env K0S_CONFIG="$k0s_config" \
            "$K0S_IMAGE" k0s controller --enable-worker --no-taints > /dev/null \
            || die "failed to create platform cluster"
    fi

    log "waiting for platform API server"
    for _ in $(seq 1 150); do
        k0s_exec "$platform_container" k0s kubectl get --raw=/readyz &> /dev/null && break
        sleep 2
    done
    k0s_exec "$platform_container" k0s kubectl get --raw=/readyz > /dev/null || die "platform API server did not become ready"

    kubeconfig=$(mktemp -t k0s-platform-kubeconfig-XXXXXX) || die "failed to create temp file"
    write_kubeconfig "$platform_container" "$kubeconfig"
    export KUBECONFIG=$kubeconfig
}

build_provider_image() {
    test "${SKIP_BUILD:-false}" = "true" && return
    log "building provider image"
    (cd "$root" && task build:img:build-test) || die "failed to build provider image"
}

# ctr_import streams a single-platform image tarball into the platform
# cluster's containerd through k0s' embedded ctr.
ctr_import() {
    platform=$1
    tarball=$2
    k0s_exec "$platform_container" \
        k0s ctr --namespace k8s.io images import --platform "$platform" --snapshotter=overlayfs - < "$tarball"
}

native_platform() {
    echo "linux/$(go env GOARCH)"
}

# import_registry_image pulls via crane into a single-platform tarball,
# bypassing the docker daemon: `docker save` with the containerd image store
# drops shared layer blobs of multi-platform images (moby#49473, kind#3795).
# Falls back to amd64 for images without a native-arch variant.
import_registry_image() {
    image=$1
    tarball=$(mktemp -t k0s-images-XXXXXX.tar) || die "failed to create temp file"
    platform=$(native_platform)
    if ! crane pull --platform "$platform" --format tarball "$image" "$tarball" 2> /dev/null; then
        platform=linux/amd64
        crane pull --platform "$platform" --format tarball "$image" "$tarball" \
            || { rm -f "$tarball"; die "failed to pull $image"; }
    fi
    ctr_import "$platform" "$tarball"
    status=$?
    rm -f "$tarball"
    test $status -eq 0 || die "failed to import $image"
}

# import_local_image streams a locally built image from the docker daemon;
# safe because a local single-arch build carries no manifest list.
import_local_image() {
    image=$1
    docker save "$image" | ctr_import "$(native_platform)" /dev/stdin || die "failed to import $image"
}

import_images() {
    log "importing images into platform cluster"
    import_registry_image "$OPENMCP_OPERATOR_IMAGE"
    import_local_image "$OPENMCP_CP_K0S_IMAGE"
}

deploy_openmcp_operator() {
    log "deploying openmcp-operator"
    kubectl apply -f - << EOF || die "failed to apply openmcp-operator resources"
apiVersion: v1
kind: Namespace
metadata:
  name: openmcp-system
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: openmcp-operator
  namespace: openmcp-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: openmcp-operator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
- kind: ServiceAccount
  name: openmcp-operator
  namespace: openmcp-system
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: openmcp-operator
  namespace: openmcp-system
data:
  config: |
    managedControlPlane:
      mcpClusterPurpose: mcp
      exposedEndpoints:
      - name: apiserver-external
      - name: apiserver-internal
    scheduler:
      scope: Cluster
      purposeMappings:
        mcp:
          template:
            spec:
              profile: k0s
              tenancy: Exclusive
        platform:
          template:
            spec:
              profile: k0s
              tenancy: Shared
        onboarding:
          template:
            spec:
              profile: k0s
              tenancy: Shared
        workload:
          template:
            spec:
              profile: k0s
              tenancy: Shared
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: openmcp-operator
  namespace: openmcp-system
spec:
  replicas: 1
  selector:
    matchLabels:
      app: openmcp-operator
  template:
    metadata:
      labels:
        app: openmcp-operator
    spec:
      serviceAccountName: openmcp-operator
      initContainers:
      - image: ${OPENMCP_OPERATOR_IMAGE}
        name: openmcp-operator-init
        args:
        - init
        - --environment
        - ${OPENMCP_ENVIRONMENT}
        - --config
        - /etc/openmcp-operator/config
        env:
        - name: POD_NAME
          valueFrom: {fieldRef: {fieldPath: metadata.name}}
        - name: POD_NAMESPACE
          valueFrom: {fieldRef: {fieldPath: metadata.namespace}}
        - name: POD_IP
          valueFrom: {fieldRef: {fieldPath: status.podIP}}
        - name: POD_SERVICE_ACCOUNT_NAME
          valueFrom: {fieldRef: {fieldPath: spec.serviceAccountName}}
        volumeMounts:
        - name: config
          mountPath: /etc/openmcp-operator
          readOnly: true
      containers:
      - image: ${OPENMCP_OPERATOR_IMAGE}
        name: openmcp-operator
        args:
        - run
        - --environment
        - ${OPENMCP_ENVIRONMENT}
        - --config
        - /etc/openmcp-operator/config
        env:
        - name: POD_NAME
          valueFrom: {fieldRef: {fieldPath: metadata.name}}
        - name: POD_NAMESPACE
          valueFrom: {fieldRef: {fieldPath: metadata.namespace}}
        - name: POD_IP
          valueFrom: {fieldRef: {fieldPath: status.podIP}}
        - name: POD_SERVICE_ACCOUNT_NAME
          valueFrom: {fieldRef: {fieldPath: spec.serviceAccountName}}
        volumeMounts:
        - name: config
          mountPath: /etc/openmcp-operator
          readOnly: true
      volumes:
      - name: config
        configMap:
          name: openmcp-operator
EOF
}

install_cluster_provider() {
    log "installing k0s cluster provider"
    kubectl wait --for=create customresourcedefinitions.apiextensions.k8s.io/clusterproviders.openmcp.cloud --timeout=60s \
        || die "clusterproviders CRD did not appear"
    kubectl apply -f - << EOF || die "failed to apply ClusterProvider"
apiVersion: openmcp.cloud/v1alpha1
kind: ClusterProvider
metadata:
  name: k0s
spec:
  image: ${OPENMCP_CP_K0S_IMAGE}
  env:
  - name: K0S_VERSION
    value: "${K0S_VERSION}"
  - name: K0S_NETWORK
    value: "${platform_network}"
  extraVolumes:
  - name: docker-socket
    hostPath:
      path: /var/run/host-docker.sock
      type: Socket
  extraVolumeMounts:
  - name: docker-socket
    mountPath: /var/run/docker.sock
EOF
}

restart_provider() {
    kubectl get deployment cp-k0s -n openmcp-system &> /dev/null || return 0
    kubectl rollout restart deployment/cp-k0s -n openmcp-system || die "failed to restart provider deployment"
    kubectl rollout status deployment/cp-k0s -n openmcp-system --timeout=120s || die "provider deployment did not become ready"
}

create_provider_config() {
    log "creating ProviderConfig"
    kubectl wait --for=create customresourcedefinitions.apiextensions.k8s.io/providerconfigs.k0s.cluster.open-control-plane.io --timeout=120s \
        || die "providerconfigs CRD did not appear, check the cluster-provider-k0s-init job"
    kubectl apply -f - << EOF || die "failed to apply ProviderConfig"
apiVersion: k0s.cluster.open-control-plane.io/v1alpha1
kind: ProviderConfig
metadata:
  name: k0s
spec: {}
EOF
}

create_platform_cluster_resource() {
    log "registering platform cluster"
    kubectl apply -f - << EOF || die "failed to apply platform Cluster"
apiVersion: clusters.openmcp.cloud/v1alpha1
kind: Cluster
metadata:
  name: platform
  namespace: openmcp-system
  annotations:
    # Adopt the already existing platform cluster instead of creating one.
    k0s.cluster.open-control-plane.io/name: ${platform_cluster}
spec:
  kubernetes: {}
  profile: k0s
  purposes:
  - platform
  tenancy: Shared
EOF
}

install_service_providers() {
    log "installing service providers"
    kubectl apply -f - << EOF || die "failed to apply service providers"
apiVersion: openmcp.cloud/v1alpha1
kind: ServiceProvider
metadata:
  name: crossplane
spec:
  image: ${SP_CROSSPLANE_IMAGE}
---
apiVersion: openmcp.cloud/v1alpha1
kind: ServiceProvider
metadata:
  name: flux
spec:
  image: ${SP_FLUX_IMAGE}
---
apiVersion: openmcp.cloud/v1alpha1
kind: ServiceProvider
metadata:
  name: ocm
spec:
  image: ${SP_OCM_IMAGE}
---
apiVersion: openmcp.cloud/v1alpha1
kind: ServiceProvider
metadata:
  name: kro
spec:
  image: ${SP_KRO_IMAGE}
---
apiVersion: openmcp.cloud/v1alpha1
kind: PlatformService
metadata:
  name: gateway
spec:
  image: ${PS_GATEWAY_IMAGE}
---
apiVersion: openmcp.cloud/v1alpha1
kind: PlatformService
metadata:
  name: project-workspace
spec:
  image: ${PS_PROJECT_WORKSPACE_IMAGE}
EOF
}

install_flux() {
    log "installing flux2 on the platform cluster"
    kubectl apply -f "$FLUX2_INSTALL_URL" > /dev/null || die "failed to install flux2"
}

configure_flux_provider() {
    log "configuring flux service provider"
    kubectl wait --for=create customresourcedefinitions.apiextensions.k8s.io/providerconfigs.flux.services.open-control-plane.io --timeout=120s \
        || die "flux providerconfigs CRD did not appear, check the sp-flux-init job"
    kubectl apply -f - << EOF || die "failed to apply flux ProviderConfig"
apiVersion: flux.services.open-control-plane.io/v1alpha1
kind: ProviderConfig
metadata:
  name: flux
spec:
  versions:
    - version: "2.8.3"
      chartVersion: "2.18.2"
      chartUrl: "oci://ghcr.io/fluxcd-community/charts/flux2"
EOF
}

configure_gateway() {
    log "configuring gateway platform service"
    kubectl wait --for=create customresourcedefinitions.apiextensions.k8s.io/gatewayserviceconfigs.gateway.openmcp.cloud --timeout=120s \
        || die "gatewayserviceconfigs CRD did not appear, check the ps-gateway-init job"
    kubectl apply -f - << EOF || die "failed to apply GatewayServiceConfig"
apiVersion: gateway.openmcp.cloud/v1alpha1
kind: GatewayServiceConfig
metadata:
  name: gateway
spec:
  envoyGateway:
    images:
      proxy: "${ENVOY_PROXY_IMAGE}"
      gateway: "${ENVOY_GATEWAY_IMAGE}"
      rateLimit: "${ENVOY_RATELIMIT_IMAGE}"
    chart:
      url: "${ENVOY_GATEWAY_CHART_URL}"
      tag: "${ENVOY_GATEWAY_CHART_TAG}"
  clusters:
    - selector:
        matchPurpose: platform
    - selector:
        matchPurpose: workload
  dns:
    baseDomain: openmcp.cluster.local
EOF
}

# configure_project_workspace enables the Project/Workspace onboarding APIs.
# The init job creates the CRD but then requires this resource to exist and
# retries until it does.
configure_project_workspace() {
    log "configuring project-workspace platform service"
    kubectl wait --for=create customresourcedefinitions.apiextensions.k8s.io/projectworkspaceconfigs.core.openmcp.cloud --timeout=120s \
        || die "projectworkspaceconfigs CRD did not appear, check the ps-project-workspace-init job"
    kubectl apply -f - << EOF || die "failed to apply ProjectWorkspaceConfig"
apiVersion: core.openmcp.cloud/v1alpha1
kind: ProjectWorkspaceConfig
metadata:
  name: project-workspace
spec: {}
EOF
}

wait_for_onboarding_cluster() {
    log "waiting for onboarding cluster"
    kubectl wait --for=create -n openmcp-system cluster/onboarding --timeout=120s \
        || die "onboarding Cluster resource did not appear"
    kubectl wait --for='jsonpath={.status.phase}=Ready' -n openmcp-system cluster/onboarding --timeout=600s \
        || die "onboarding cluster did not become ready"
}

configure_webhook_dns() {
    log "configuring webhook dns"
    envoy_ip=""
    for _ in $(seq 1 60); do
        envoy_ip=$(kubectl get svc -n envoy-gateway-system -o jsonpath='{.items[?(@.spec.type=="LoadBalancer")].status.loadBalancer.ingress[0].ip}' 2> /dev/null | awk '{print $1}')
        test -n "$envoy_ip" && break
        sleep 5
    done
    test -n "$envoy_ip" || die "envoy loadbalancer got no IP; check metallb on the platform cluster"

    for container in $(docker ps --filter "label=app=cluster-provider-k0s" --format '{{.Names}}'); do
        docker exec "$container" sh -c "grep -q '$webhook_host' /etc/hosts || echo '$envoy_ip $webhook_host' >> /etc/hosts" \
            || die "failed to patch /etc/hosts of $container"
    done
}

export_kubeconfigs() {
    write_kubeconfig "$platform_container" platform.kubeconfig
    log "platform cluster kubeconfig at $(pwd)/platform.kubeconfig"
    onboarding="$(kubectl --kubeconfig platform.kubeconfig -n openmcp-system get cluster onboarding -o jsonpath='{.status.providerStatus.k0sClusterName}')"
    write_kubeconfig "k0s-${onboarding}" onboarding.kubeconfig
    log "onboarding cluster kubeconfig at $(pwd)/onboarding.kubeconfig"
}

deploy() {
    create_platform_cluster
    build_provider_image
    import_images
    deploy_openmcp_operator
    install_cluster_provider
    restart_provider
    create_provider_config
    create_platform_cluster_resource
    install_service_providers
    install_flux
    configure_flux_provider
    configure_gateway
    configure_project_workspace
    wait_for_onboarding_cluster
    configure_webhook_dns
    export_kubeconfigs
    log "done - see README.md for how to request clusters"
}

reset() {
    if [ "${1:-}" != "--force" ]; then
        read -p "Delete ALL k0s clusters? (yes/no): " confirmation
        test "$confirmation" = "yes" || die "aborted"
    fi
    containers=$(docker ps -a --filter "label=app=cluster-provider-k0s" --format '{{.Names}}')
    test -n "$containers" && { docker rm -fv $containers || die "failed to delete clusters"; }
    docker network rm "$platform_network" 2> /dev/null
    return 0
}

usage() {
    cat << EOF
Usage: $(basename "$0") <command>

Commands:
    deploy           Deploy the openMCP environment with the k0s provider
    kubeconfigs      Write platform and onboarding kubeconfigs to the cwd
    reset [--force]  Delete all k0s clusters
EOF
}

test $# -eq 0 && { usage; exit 0; }

subcmd="$1"
shift

case "$subcmd" in
    (deploy) require_tools; deploy;;
    (kubeconfigs) require_tools; export_kubeconfigs;;
    (reset) require_tools; reset "$@";;
    (help|-h|--help) usage;;
    (*) die "Unknown subcommand: $subcmd";;
esac
