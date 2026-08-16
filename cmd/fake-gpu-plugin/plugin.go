package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	resourceName             = "nvidia.com/gpu"
	defaultDeviceCount       = 2
	defaultDevicePrefix      = "ember-gpu"
	defaultKubeletSocket     = "/var/lib/kubelet/device-plugins/kubelet.sock"
	defaultPluginSocketDir   = "/var/lib/kubelet/device-plugins"
	pluginSocketFilename     = "ember-fake-gpu.sock"
	registrationTimeout      = 5 * time.Second
	selfDialTimeout          = 3 * time.Second
	kubeletWatchPollInterval = 2 * time.Second
)

var devicePrefixPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?$`)

type config struct {
	DeviceCount     int
	DevicePrefix    string
	KubeletSocket   string
	PluginSocketDir string
}

type fakeGPUPlugin struct {
	pluginapi.UnimplementedDevicePluginServer

	cfg        config
	devices    []*pluginapi.Device
	deviceSet  map[string]struct{}
	socketPath string

	mu       sync.Mutex
	listener net.Listener
	server   *grpc.Server
	serveErr chan error
	stopCh   chan struct{}
	stopOnce sync.Once
}

func loadConfigFromEnv() (config, error) {
	cfg := config{
		DeviceCount:     defaultDeviceCount,
		DevicePrefix:    defaultDevicePrefix,
		KubeletSocket:   defaultKubeletSocket,
		PluginSocketDir: defaultPluginSocketDir,
	}

	if raw := strings.TrimSpace(os.Getenv("DEVICE_COUNT")); raw != "" {
		count, err := parsePositiveInt(raw)
		if err != nil {
			return config{}, fmt.Errorf("DEVICE_COUNT: %w", err)
		}
		cfg.DeviceCount = count
	}
	if raw := strings.TrimSpace(os.Getenv("DEVICE_PREFIX")); raw != "" {
		cfg.DevicePrefix = raw
	}
	if raw := strings.TrimSpace(os.Getenv("KUBELET_SOCKET")); raw != "" {
		cfg.KubeletSocket = raw
	}
	if raw := strings.TrimSpace(os.Getenv("PLUGIN_SOCKET_DIR")); raw != "" {
		cfg.PluginSocketDir = raw
	}

	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func parsePositiveInt(raw string) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("must be an integer: %w", err)
	}
	if value <= 0 {
		return 0, errors.New("must be greater than zero")
	}
	return value, nil
}

func (c config) validate() error {
	if c.DeviceCount <= 0 {
		return errors.New("DEVICE_COUNT must be greater than zero")
	}
	if err := validateDevicePrefix(c.DevicePrefix); err != nil {
		return fmt.Errorf("DEVICE_PREFIX: %w", err)
	}
	if err := validateSocketPath(c.KubeletSocket); err != nil {
		return fmt.Errorf("KUBELET_SOCKET: %w", err)
	}
	if err := validateSocketDir(c.PluginSocketDir); err != nil {
		return fmt.Errorf("PLUGIN_SOCKET_DIR: %w", err)
	}
	if _, err := buildInventory(c.DeviceCount, c.DevicePrefix); err != nil {
		return err
	}
	if _, err := pluginSocketPath(c.PluginSocketDir); err != nil {
		return err
	}
	return nil
}

func validateDevicePrefix(prefix string) error {
	if prefix == "" {
		return errors.New("must not be empty")
	}
	if strings.Contains(prefix, "/") || strings.Contains(prefix, string(filepath.Separator)) {
		return errors.New("must not contain path separators")
	}
	if !devicePrefixPattern.MatchString(prefix) {
		return errors.New("must match ^[A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?$")
	}
	return nil
}

func validateSocketDir(dir string) error {
	if dir == "" {
		return errors.New("must not be empty")
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("must be absolute: %q", dir)
	}
	clean := filepath.Clean(dir)
	if clean == string(filepath.Separator) {
		return errors.New("must not be root")
	}
	return nil
}

func validateSocketPath(path string) error {
	if path == "" {
		return errors.New("must not be empty")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("must be absolute: %q", path)
	}
	clean := filepath.Clean(path)
	if filepath.Ext(clean) != ".sock" {
		return fmt.Errorf("must end with .sock: %q", path)
	}
	if filepath.Base(clean) == ".sock" {
		return fmt.Errorf("must include a socket filename: %q", path)
	}
	return nil
}

func pluginSocketPath(dir string) (string, error) {
	if err := validateSocketDir(dir); err != nil {
		return "", err
	}
	cleanDir := filepath.Clean(dir)
	path := filepath.Join(cleanDir, pluginSocketFilename)
	if filepath.Dir(path) != cleanDir {
		return "", errors.New("refusing to derive socket path outside plugin socket dir")
	}
	if err := validateSocketPath(path); err != nil {
		return "", err
	}
	return path, nil
}

func buildInventory(count int, prefix string) ([]*pluginapi.Device, error) {
	if count <= 0 {
		return nil, errors.New("device count must be greater than zero")
	}
	if err := validateDevicePrefix(prefix); err != nil {
		return nil, err
	}
	devices := make([]*pluginapi.Device, 0, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%s-%d", prefix, i)
		if len(id) > 63 {
			return nil, fmt.Errorf("generated device id %q exceeds 63 characters", id)
		}
		devices = append(devices, &pluginapi.Device{ID: id, Health: pluginapi.Healthy})
	}
	return devices, nil
}

func newFakeGPUPlugin(cfg config) (*fakeGPUPlugin, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	devices, err := buildInventory(cfg.DeviceCount, cfg.DevicePrefix)
	if err != nil {
		return nil, err
	}
	socketPath, err := pluginSocketPath(cfg.PluginSocketDir)
	if err != nil {
		return nil, err
	}
	deviceSet := make(map[string]struct{}, len(devices))
	for _, device := range devices {
		deviceSet[device.ID] = struct{}{}
	}
	return &fakeGPUPlugin{
		cfg:        cfg,
		devices:    devices,
		deviceSet:  deviceSet,
		socketPath: socketPath,
		stopCh:     make(chan struct{}),
	}, nil
}

func (p *fakeGPUPlugin) Run(ctx context.Context) error {
	if err := p.startServer(); err != nil {
		_ = p.Stop()
		return err
	}
	defer p.Stop()

	if err := p.registerWithKubelet(ctx); err != nil {
		return err
	}
	go p.watchKubeletSocket(ctx)

	select {
	case <-ctx.Done():
		return nil
	case err := <-p.serveErr:
		if err == nil || errors.Is(err, grpc.ErrServerStopped) || isClosedNetworkError(err) {
			return nil
		}
		return err
	}
}

func (p *fakeGPUPlugin) startServer() error {
	if err := os.MkdirAll(p.cfg.PluginSocketDir, 0o755); err != nil {
		return fmt.Errorf("create plugin socket dir: %w", err)
	}
	if err := removeExactSocket(p.socketPath); err != nil {
		return err
	}

	listener, err := net.Listen("unix", p.socketPath)
	if err != nil {
		return fmt.Errorf("listen on plugin socket: %w", err)
	}
	server := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(server, p)

	p.mu.Lock()
	p.listener = listener
	p.server = server
	p.serveErr = make(chan error, 1)
	p.mu.Unlock()

	go func() {
		p.serveErr <- server.Serve(listener)
	}()
	if err := p.waitForSelfDial(); err != nil {
		server.Stop()
		_ = listener.Close()
		_ = removeExactSocket(p.socketPath)
		return err
	}
	return nil
}

func (p *fakeGPUPlugin) waitForSelfDial() error {
	deadline := time.Now().Add(selfDialTimeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		conn, err := dialUnixSocket(ctx, p.socketPath)
		cancel()
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("plugin gRPC server did not become ready in time")
}

func (p *fakeGPUPlugin) Stop() error {
	var stopErr error
	p.stopOnce.Do(func() {
		close(p.stopCh)
		p.mu.Lock()
		server := p.server
		listener := p.listener
		p.server = nil
		p.listener = nil
		p.mu.Unlock()

		if server != nil {
			server.Stop()
		}
		if listener != nil {
			_ = listener.Close()
		}
		if err := removeExactSocket(p.socketPath); err != nil {
			stopErr = err
		}
	})
	return stopErr
}

func removeExactSocket(path string) error {
	if err := validateSocketPath(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove plugin socket %q: %w", path, err)
	}
	return nil
}

func dialUnixSocket(ctx context.Context, socketPath string) (*grpc.ClientConn, error) {
	return grpc.DialContext(
		ctx,
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}),
	)
}

func (p *fakeGPUPlugin) registerWithKubelet(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, registrationTimeout)
	defer cancel()

	conn, err := dialUnixSocket(ctx, p.cfg.KubeletSocket)
	if err != nil {
		return fmt.Errorf("dial kubelet socket: %w", err)
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	_, err = client.Register(ctx, &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     filepath.Base(p.socketPath),
		ResourceName: resourceName,
		Options: &pluginapi.DevicePluginOptions{
			PreStartRequired:                false,
			GetPreferredAllocationAvailable: true,
		},
	})
	if err != nil {
		return fmt.Errorf("register with kubelet: %w", err)
	}
	return nil
}

func (p *fakeGPUPlugin) watchKubeletSocket(ctx context.Context) {
	ticker := time.NewTicker(kubeletWatchPollInterval)
	defer ticker.Stop()

	lastIdentity, _ := socketIdentity(p.cfg.KubeletSocket)
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case <-ticker.C:
			identity, ok := socketIdentity(p.cfg.KubeletSocket)
			if ok && identity != "" && identity != lastIdentity {
				registerCtx, cancel := context.WithTimeout(context.Background(), registrationTimeout)
				_ = p.registerWithKubelet(registerCtx)
				cancel()
			}
			if ok {
				lastIdentity = identity
			}
		}
	}
}

func socketIdentity(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if ok {
		return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), true
	}
	return info.ModTime().UTC().Format(time.RFC3339Nano), true
}

func isClosedNetworkError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "use of closed network connection")
}

func (p *fakeGPUPlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{
		PreStartRequired:                false,
		GetPreferredAllocationAvailable: true,
	}, nil
}

func (p *fakeGPUPlugin) ListAndWatch(_ *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: cloneDevices(p.devices)}); err != nil {
		return err
	}
	select {
	case <-p.stopCh:
		return nil
	case <-stream.Context().Done():
		return nil
	}
}

func (p *fakeGPUPlugin) GetPreferredAllocation(_ context.Context, req *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	if req == nil || len(req.ContainerRequests) == 0 {
		return nil, status.Error(codes.InvalidArgument, "container_requests must not be empty")
	}
	response := &pluginapi.PreferredAllocationResponse{ContainerResponses: make([]*pluginapi.ContainerPreferredAllocationResponse, 0, len(req.ContainerRequests))}
	for i, containerReq := range req.ContainerRequests {
		preferred, err := preferredAllocation(p.deviceSet, containerReq.AvailableDeviceIDs, containerReq.MustIncludeDeviceIDs, containerReq.AllocationSize)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "container_requests[%d]: %v", i, err)
		}
		response.ContainerResponses = append(response.ContainerResponses, &pluginapi.ContainerPreferredAllocationResponse{DeviceIDs: preferred})
	}
	return response, nil
}

func preferredAllocation(availableSet map[string]struct{}, availableIDs, mustIncludeIDs []string, allocationSize int32) ([]string, error) {
	if allocationSize < 0 {
		return nil, errors.New("allocation_size must be non-negative")
	}
	seenAvailable := make(map[string]struct{}, len(availableIDs))
	orderedAvailable := make([]string, 0, len(availableIDs))
	for _, id := range availableIDs {
		if _, ok := availableSet[id]; !ok {
			return nil, fmt.Errorf("unknown available device %q", id)
		}
		if _, dup := seenAvailable[id]; dup {
			return nil, fmt.Errorf("duplicate available device %q", id)
		}
		seenAvailable[id] = struct{}{}
		orderedAvailable = append(orderedAvailable, id)
	}

	result := make([]string, 0, allocationSize)
	selected := make(map[string]struct{}, len(mustIncludeIDs))
	for _, id := range mustIncludeIDs {
		if _, ok := availableSet[id]; !ok {
			return nil, fmt.Errorf("unknown required device %q", id)
		}
		if _, ok := seenAvailable[id]; !ok {
			return nil, fmt.Errorf("required device %q is not in available_deviceIDs", id)
		}
		if _, dup := selected[id]; dup {
			return nil, fmt.Errorf("duplicate required device %q", id)
		}
		selected[id] = struct{}{}
		result = append(result, id)
	}
	if int(allocationSize) < len(result) {
		return nil, errors.New("allocation_size is smaller than must_include_deviceIDs")
	}
	for _, id := range orderedAvailable {
		if len(result) == int(allocationSize) {
			break
		}
		if _, alreadySelected := selected[id]; alreadySelected {
			continue
		}
		selected[id] = struct{}{}
		result = append(result, id)
	}
	if len(result) != int(allocationSize) {
		return nil, errors.New("allocation_size exceeds available devices")
	}
	return result, nil
}

func (p *fakeGPUPlugin) Allocate(_ context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	if err := validateAllocateRequest(p.deviceSet, req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	response := &pluginapi.AllocateResponse{ContainerResponses: make([]*pluginapi.ContainerAllocateResponse, 0, len(req.ContainerRequests))}
	for range req.ContainerRequests {
		response.ContainerResponses = append(response.ContainerResponses, &pluginapi.ContainerAllocateResponse{})
	}
	return response, nil
}

func validateAllocateRequest(available map[string]struct{}, req *pluginapi.AllocateRequest) error {
	if req == nil || len(req.ContainerRequests) == 0 {
		return errors.New("container_requests must not be empty")
	}
	globalSeen := make(map[string]struct{})
	for i, containerReq := range req.ContainerRequests {
		if len(containerReq.DevicesIDs) == 0 {
			return fmt.Errorf("container_requests[%d] must include at least one device ID", i)
		}
		localSeen := make(map[string]struct{}, len(containerReq.DevicesIDs))
		for _, id := range containerReq.DevicesIDs {
			if _, ok := available[id]; !ok {
				return fmt.Errorf("container_requests[%d] contains unknown device %q", i, id)
			}
			if _, dup := localSeen[id]; dup {
				return fmt.Errorf("container_requests[%d] contains duplicate device %q", i, id)
			}
			if _, dup := globalSeen[id]; dup {
				return fmt.Errorf("device %q was requested more than once across containers", id)
			}
			localSeen[id] = struct{}{}
			globalSeen[id] = struct{}{}
		}
	}
	return nil
}

func (p *fakeGPUPlugin) PreStartContainer(_ context.Context, req *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request must not be nil")
	}
	if err := validateRequestedIDs(p.deviceSet, req.DevicesIDs, "devices_ids"); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pluginapi.PreStartContainerResponse{}, nil
}

func validateRequestedIDs(available map[string]struct{}, ids []string, field string) error {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := available[id]; !ok {
			return fmt.Errorf("%s contains unknown device %q", field, id)
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("%s contains duplicate device %q", field, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func cloneDevices(devices []*pluginapi.Device) []*pluginapi.Device {
	cloned := make([]*pluginapi.Device, 0, len(devices))
	for _, device := range devices {
		copy := *device
		cloned = append(cloned, &copy)
	}
	return cloned
}
