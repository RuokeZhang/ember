package main

import (
	"testing"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func TestBuildInventory(t *testing.T) {
	devices, err := buildInventory(3, "ember-gpu")
	if err != nil {
		t.Fatalf("buildInventory: %v", err)
	}
	if len(devices) != 3 {
		t.Fatalf("device count = %d, want 3", len(devices))
	}
	for i, want := range []string{"ember-gpu-0", "ember-gpu-1", "ember-gpu-2"} {
		if devices[i].ID != want {
			t.Fatalf("device[%d].ID = %q, want %q", i, devices[i].ID, want)
		}
		if devices[i].Health != pluginapi.Healthy {
			t.Fatalf("device[%d].Health = %q, want %q", i, devices[i].Health, pluginapi.Healthy)
		}
	}
}

func TestValidateAllocateRequest(t *testing.T) {
	inventory := map[string]struct{}{
		"ember-gpu-0": {},
		"ember-gpu-1": {},
	}

	tests := []struct {
		name    string
		req     *pluginapi.AllocateRequest
		wantErr bool
	}{
		{
			name: "valid single container",
			req: &pluginapi.AllocateRequest{ContainerRequests: []*pluginapi.ContainerAllocateRequest{{
				DevicesIDs: []string{"ember-gpu-0", "ember-gpu-1"},
			}}},
		},
		{
			name: "unknown device",
			req: &pluginapi.AllocateRequest{ContainerRequests: []*pluginapi.ContainerAllocateRequest{{
				DevicesIDs: []string{"ember-gpu-2"},
			}}},
			wantErr: true,
		},
		{
			name: "duplicate in one container",
			req: &pluginapi.AllocateRequest{ContainerRequests: []*pluginapi.ContainerAllocateRequest{{
				DevicesIDs: []string{"ember-gpu-0", "ember-gpu-0"},
			}}},
			wantErr: true,
		},
		{
			name: "duplicate across containers",
			req: &pluginapi.AllocateRequest{ContainerRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIDs: []string{"ember-gpu-0"}},
				{DevicesIDs: []string{"ember-gpu-0"}},
			}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAllocateRequest(inventory, tt.req)
			if tt.wantErr && err == nil {
				t.Fatalf("validateAllocateRequest() error = nil, want non-nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateAllocateRequest() error = %v, want nil", err)
			}
		})
	}
}

func TestPreferredAllocation(t *testing.T) {
	inventory := map[string]struct{}{
		"ember-gpu-0": {},
		"ember-gpu-1": {},
		"ember-gpu-2": {},
	}
	got, err := preferredAllocation(inventory, []string{"ember-gpu-0", "ember-gpu-1", "ember-gpu-2"}, []string{"ember-gpu-1"}, 2)
	if err != nil {
		t.Fatalf("preferredAllocation: %v", err)
	}
	if len(got) != 2 || got[0] != "ember-gpu-1" || got[1] != "ember-gpu-0" {
		t.Fatalf("preferred allocation = %#v, want [ember-gpu-1 ember-gpu-0]", got)
	}
}

func TestConfigValidationRejectsUnsafeSockets(t *testing.T) {
	tests := []struct {
		name string
		cfg  config
	}{
		{
			name: "relative plugin socket dir",
			cfg: config{
				DeviceCount:     2,
				DevicePrefix:    "ember-gpu",
				KubeletSocket:   "/var/lib/kubelet/device-plugins/kubelet.sock",
				PluginSocketDir: "relative/path",
			},
		},
		{
			name: "relative kubelet socket",
			cfg: config{
				DeviceCount:     2,
				DevicePrefix:    "ember-gpu",
				KubeletSocket:   "relative/kubelet.sock",
				PluginSocketDir: "/var/lib/kubelet/device-plugins",
			},
		},
		{
			name: "non socket kubelet path",
			cfg: config{
				DeviceCount:     2,
				DevicePrefix:    "ember-gpu",
				KubeletSocket:   "/var/lib/kubelet/device-plugins/kubelet.txt",
				PluginSocketDir: "/var/lib/kubelet/device-plugins",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.validate(); err == nil {
				t.Fatalf("validate() error = nil, want non-nil")
			}
		})
	}
}

func TestPluginSocketPath(t *testing.T) {
	path, err := pluginSocketPath("/var/lib/kubelet/device-plugins")
	if err != nil {
		t.Fatalf("pluginSocketPath: %v", err)
	}
	const want = "/var/lib/kubelet/device-plugins/ember-fake-gpu.sock"
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}
