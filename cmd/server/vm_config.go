package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func writeVMConfig(configPath string, cfg config, tapDevice, rootfsPath string, subnet int, vsockPath string, guestCID uint32) error {
	type bootSource struct {
		KernelImagePath string `json:"kernel_image_path"`
		BootArgs        string `json:"boot_args"`
	}
	type drive struct {
		DriveID      string `json:"drive_id"`
		PathOnHost   string `json:"path_on_host"`
		IsRootDevice bool   `json:"is_root_device"`
		IsReadOnly   bool   `json:"is_read_only"`
	}
	type netIf struct {
		IfaceID     string `json:"iface_id"`
		GuestMAC    string `json:"guest_mac"`
		HostDevName string `json:"host_dev_name"`
	}
	type machineConfig struct {
		VCPUCount  int `json:"vcpu_count"`
		MemSizeMiB int `json:"mem_size_mib"`
	}
	type vsockConfig struct {
		GuestCID uint32 `json:"guest_cid"`
		UDSPath  string `json:"uds_path"`
	}

	guestMAC := fmt.Sprintf("06:00:AC:10:%02X:%02X", (subnet>>8)&0xFF, subnet&0xFF)

	cfgObj := map[string]any{
		"boot-source": bootSource{
			KernelImagePath: cfg.KernelPath,
			BootArgs:        cfg.BootArgs,
		},
		"drives": []drive{
			{
				DriveID:      "rootfs",
				PathOnHost:   rootfsPath,
				IsRootDevice: true,
				IsReadOnly:   false,
			},
		},
		"network-interfaces": []netIf{
			{
				IfaceID:     "eth0",
				GuestMAC:    guestMAC,
				HostDevName: tapDevice,
			},
		},
		"machine-config": machineConfig{
			VCPUCount:  cfg.DefaultVCPU,
			MemSizeMiB: cfg.DefaultMemMiB,
		},
		"vsock": vsockConfig{
			GuestCID: guestCID,
			UDSPath:  vsockPath,
		},
	}

	raw, err := json.MarshalIndent(cfgObj, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(configPath, raw, 0o644)
}
