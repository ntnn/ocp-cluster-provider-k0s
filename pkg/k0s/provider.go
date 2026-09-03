package k0s

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	"k8s.io/client-go/tools/clientcmd"
)

// DefaultVersion is the default k0s version.
// renovate: datasource=github-releases depName=k0sproject/k0s
const DefaultVersion = "v1.36.4+k0s.0"

// imageRepo is the k0s container image repository.
const imageRepo = "docker.io/k0sproject/k0s"

// Labels marking containers as managed by this provider.
const (
	// LabelApp marks a container as managed by this provider.
	LabelApp = "app"
	// LabelAppValue is the value of LabelApp.
	LabelAppValue = "cluster-provider-k0s"
	// LabelCluster carries the cluster name a container belongs to.
	LabelCluster = "cluster-provider-k0s/cluster"
)

const apiPort = "6443/tcp"

// Options configures the Provider.
type Options struct {
	// Version is the k0s version to run. Defaults to DefaultVersion.
	Version string
	// Network is an optional docker network created clusters join.
	Network string
	// Timeout bounds the wait for a created cluster to become ready.
	Timeout time.Duration
}

func (o *Options) validate() {
	if o.Version == "" {
		o.Version = DefaultVersion
	}
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Minute
	}
}

// Provider manages k0s clusters.
type Provider interface {
	// CreateCluster creates a new Kubernetes cluster with the given name.
	CreateCluster(ctx context.Context, name string) error

	// DeleteCluster deletes the Kubernetes cluster with the given name.
	DeleteCluster(ctx context.Context, name string) error

	// ClusterExists checks if a Kubernetes cluster with the given name exists.
	ClusterExists(ctx context.Context, name string) (bool, error)

	// KubeConfig retrieves the kubeconfig for the specified cluster name. The bool localhosts indicates whether the function returns a kubeconfig with the local host IP or the container IP.
	Kubeconfig(ctx context.Context, name string, internal bool) (string, error)
}

// k0sProvider manages k0s clusters through the docker SDK.
type k0sProvider struct {
	opts   Options
	docker *dockerclient.Client
}

var _ Provider = &k0sProvider{}

// New returns a k0s Provider backed by the docker daemon from the environment.
func New(opts Options) (Provider, error) {
	opts.validate()
	docker, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}
	return &k0sProvider{
		opts:   opts,
		docker: docker,
	}, nil
}

// image returns the k0s image reference for the configured version.
// Docker tags cannot contain '+' (as in v1.36.4+k0s.0), the published tags use '-'.
func (provider *k0sProvider) image() string {
	return imageRepo + ":" + strings.ReplaceAll(provider.opts.Version, "+", "-")
}

// containerName returns the container name for the given cluster name.
func containerName(name string) string {
	return "k0s-" + name
}

// CreateCluster implements Provider.
func (provider *k0sProvider) CreateCluster(ctx context.Context, name string) error {
	if err := provider.ensureImage(ctx); err != nil {
		return err
	}

	hostPort, err := freePort()
	if err != nil {
		return fmt.Errorf("finding free API server host port for cluster %q: %w", name, err)
	}

	cname := containerName(name)
	// Extra SANs cover both kubeconfig flavors: the container name on the
	// docker network and 127.0.0.1 behind the published host port.
	k0sConfig := fmt.Sprintf(`apiVersion: k0s.k0sproject.io/v1beta1
kind: ClusterConfig
spec:
  api:
    sans:
      - %s
      - 127.0.0.1
`, cname)

	config := &container.Config{
		Image:    provider.image(),
		Hostname: cname,
		Cmd:      []string{"k0s", "controller", "--config=/etc/k0s/config.yaml", "--enable-worker", "--no-taints"},
		Env:      []string{"K0S_CONFIG=" + k0sConfig},
		Labels: map[string]string{
			LabelApp:     LabelAppValue,
			LabelCluster: name,
		},
		ExposedPorts: nat.PortSet{apiPort: struct{}{}},
		Volumes: map[string]struct{}{
			"/var/lib/k0s": {},
		},
	}
	hostConfig := &container.HostConfig{
		Privileged: true,
		// k0s requires a private cgroup namespace to manage sub-cgroups.
		CgroupnsMode: container.CgroupnsModePrivate,
		RestartPolicy: container.RestartPolicy{
			Name: container.RestartPolicyUnlessStopped,
		},
		PortBindings: nat.PortMap{
			apiPort: []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: strconv.Itoa(hostPort)}},
		},
	}
	networkingConfig := &network.NetworkingConfig{}
	if provider.opts.Network != "" {
		networkingConfig.EndpointsConfig = map[string]*network.EndpointSettings{
			provider.opts.Network: {},
		}
	}

	created, err := provider.docker.ContainerCreate(ctx, config, hostConfig, networkingConfig, nil, cname)
	if err != nil {
		return fmt.Errorf("creating container for k0s cluster %q: %w", name, err)
	}
	if err := provider.docker.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("starting container for k0s cluster %q: %w", name, err)
	}
	if err := provider.waitReady(ctx, created.ID, name); err != nil {
		return err
	}
	return nil
}

// DeleteCluster implements Provider.
func (provider *k0sProvider) DeleteCluster(ctx context.Context, name string) error {
	c, err := provider.findContainer(ctx, name)
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	err = provider.docker.ContainerRemove(ctx, c.ID, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
	if err != nil {
		return fmt.Errorf("removing container of k0s cluster %q: %w", name, err)
	}
	return nil
}

// ClusterExists implements Provider.
func (provider *k0sProvider) ClusterExists(ctx context.Context, name string) (bool, error) {
	c, err := provider.findContainer(ctx, name)
	if err != nil {
		return false, err
	}
	return c != nil, nil
}

// Kubeconfig implements Provider.
func (provider *k0sProvider) Kubeconfig(ctx context.Context, name string, internal bool) (string, error) {
	c, err := provider.findContainer(ctx, name)
	if err != nil {
		return "", err
	}
	if c == nil {
		return "", fmt.Errorf("k0s cluster %q not found", name)
	}

	stdout, err := provider.exec(ctx, c.ID, []string{"k0s", "kubeconfig", "admin"})
	if err != nil {
		return "", fmt.Errorf("getting admin kubeconfig of k0s cluster %q: %w", name, err)
	}
	kubeconfig, err := clientcmd.Load([]byte(stdout))
	if err != nil {
		return "", fmt.Errorf("parsing admin kubeconfig of k0s cluster %q: %w", name, err)
	}

	server := fmt.Sprintf("https://%s:6443", containerName(name))
	if !internal {
		hostPort, err := provider.publishedPort(ctx, c.ID)
		if err != nil {
			return "", fmt.Errorf("determining published API server port of k0s cluster %q: %w", name, err)
		}
		server = "https://127.0.0.1:" + hostPort
	}
	for _, cluster := range kubeconfig.Clusters {
		cluster.Server = server
	}

	raw, err := clientcmd.Write(*kubeconfig)
	if err != nil {
		return "", fmt.Errorf("serializing kubeconfig of k0s cluster %q: %w", name, err)
	}
	return string(raw), nil
}

// findContainer returns the container backing the named cluster, nil when it
// does not exist.
func (provider *k0sProvider) findContainer(ctx context.Context, name string) (*container.Summary, error) {
	containers, err := provider.docker.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", LabelApp+"="+LabelAppValue),
			filters.Arg("label", LabelCluster+"="+name),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("listing containers for k0s cluster %q: %w", name, err)
	}
	if len(containers) == 0 {
		return nil, nil
	}
	return &containers[0], nil
}

// ensureImage pulls the k0s image if it is not present locally.
func (provider *k0sProvider) ensureImage(ctx context.Context) error {
	ref := provider.image()
	if _, err := provider.docker.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	reader, err := provider.docker.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pulling image %q: %w", ref, err)
	}
	defer func() { _ = reader.Close() }()
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return fmt.Errorf("pulling image %q: %w", ref, err)
	}
	return nil
}

// waitReady polls the API server through k0s' embedded kubectl until it
// reports ready or the configured timeout expires.
func (provider *k0sProvider) waitReady(ctx context.Context, containerID, name string) error {
	deadline := time.Now().Add(provider.opts.Timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := provider.exec(ctx, containerID, []string{"k0s", "kubectl", "get", "--raw=/readyz"}); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for k0s cluster %q: %w", name, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("k0s cluster %q did not become ready within %s: %w", name, provider.opts.Timeout, lastErr)
}

// exec runs a command in the container and returns its stdout, failing on
// non-zero exit codes.
func (provider *k0sProvider) exec(ctx context.Context, containerID string, cmd []string) (string, error) {
	execCreate, err := provider.docker.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", fmt.Errorf("creating exec: %w", err)
	}
	attach, err := provider.docker.ContainerExecAttach(ctx, execCreate.ID, container.ExecStartOptions{})
	if err != nil {
		return "", fmt.Errorf("attaching exec: %w", err)
	}
	defer attach.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attach.Reader); err != nil {
		return "", fmt.Errorf("reading exec output: %w", err)
	}
	inspect, err := provider.docker.ContainerExecInspect(ctx, execCreate.ID)
	if err != nil {
		return "", fmt.Errorf("inspecting exec: %w", err)
	}
	if inspect.ExitCode != 0 {
		return "", fmt.Errorf("command %v failed with exit code %d: %s", cmd, inspect.ExitCode, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.String(), nil
}

// publishedPort returns the host port the API server is published on.
func (provider *k0sProvider) publishedPort(ctx context.Context, containerID string) (string, error) {
	inspect, err := provider.docker.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspecting container: %w", err)
	}
	bindings := inspect.NetworkSettings.Ports[nat.Port(apiPort)]
	if len(bindings) == 0 {
		return "", fmt.Errorf("no host port published for %s", apiPort)
	}
	return bindings[0].HostPort, nil
}

// freePort asks the kernel for a free TCP port.
// Probed from the process' network namespace, so not guaranteed to be free on the host.
// If the host port isn't free the container fails and is retried.
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("probing free port: %w", err)
	}
	defer func() { _ = listener.Close() }()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected listener address type %T", listener.Addr())
	}
	return addr.Port, nil
}
