// +build windows

package hcsoci

// Contains functions relating to a LCOW container, as opposed to a utility VM

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Microsoft/hcsshim/internal/log"
	hcsschema "github.com/Microsoft/hcsshim/internal/schema2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
)

const lcowMountPathPrefix = "/mounts/m%d"
const lcowNvidiaMountPath = "/nvidia"

func getNvidiaGpuVhdPath(coi *createOptionsInternal) (string, error) {
	path := filepath.Join(filepath.Dir(os.Args[0]), "gpu.vhd")
	if _, err := os.Stat(path); err != nil {
		return "", errors.Wrapf(err, "failed to find nvidia gpu support vhd %s", path)
	}
	return path, nil
}

func allocateLinuxResources(ctx context.Context, coi *createOptionsInternal, resources *Resources) error {
	if coi.Spec.Root == nil {
		coi.Spec.Root = &specs.Root{}
	}
	if coi.Spec.Windows != nil && len(coi.Spec.Windows.LayerFolders) > 0 {
		log.G(ctx).Debug("hcsshim::allocateLinuxResources mounting storage")
		rootPath, err := MountContainerLayers(ctx, coi.Spec.Windows.LayerFolders, resources.containerRootInUVM, coi.HostingSystem)
		if err != nil {
			return fmt.Errorf("failed to mount container storage: %s", err)
		}
		if coi.HostingSystem == nil {
			coi.Spec.Root.Path = rootPath // Argon v1 or v2
		} else {
			coi.Spec.Root.Path = rootPath // v2 Xenon LCOW
		}
		resources.layers = coi.Spec.Windows.LayerFolders
	} else if coi.Spec.Root.Path != "" {
		// This is the "Plan 9" root filesystem.
		// TODO: We need a test for this. Ask @jstarks how you can even lay this out on Windows.
		hostPath := coi.Spec.Root.Path
		uvmPathForContainersFileSystem := path.Join(resources.containerRootInUVM, rootfsPath)
		share, err := coi.HostingSystem.AddPlan9(ctx, hostPath, uvmPathForContainersFileSystem, coi.Spec.Root.Readonly, false, nil)
		if err != nil {
			return fmt.Errorf("adding plan9 root: %s", err)
		}
		coi.Spec.Root.Path = uvmPathForContainersFileSystem
		resources.plan9Mounts = append(resources.plan9Mounts, share)
	} else {
		return errors.New("must provide either Windows.LayerFolders or Root.Path")
	}

	for i, mount := range coi.Spec.Mounts {
		switch mount.Type {
		case "bind":
		case "physical-disk":
		case "virtual-disk":
		case "automanage-virtual-disk":
		default:
			// Unknown mount type
			continue
		}
		if mount.Destination == "" || mount.Source == "" {
			return fmt.Errorf("invalid OCI spec - a mount must have both source and a destination: %+v", mount)
		}

		if coi.HostingSystem != nil {
			hostPath := mount.Source
			uvmPathForShare := path.Join(resources.containerRootInUVM, fmt.Sprintf(lcowMountPathPrefix, i))
			uvmPathForFile := uvmPathForShare

			readOnly := false
			for _, o := range mount.Options {
				if strings.ToLower(o) == "ro" {
					readOnly = true
					break
				}
			}
			l := log.G(ctx).WithField("mount", fmt.Sprintf("%+v", mount))
			if mount.Type == "physical-disk" {
				l.Debug("hcsshim::allocateLinuxResources Hot-adding SCSI physical disk for OCI mount")
				_, _, err := coi.HostingSystem.AddSCSIPhysicalDisk(ctx, hostPath, uvmPathForShare, readOnly)
				if err != nil {
					return fmt.Errorf("adding SCSI physical disk mount %+v: %s", mount, err)
				}
				resources.scsiMounts = append(resources.scsiMounts, scsiMount{path: hostPath})
				coi.Spec.Mounts[i].Type = "none"
			} else if mount.Type == "virtual-disk" || mount.Type == "automanage-virtual-disk" {
				l.Debug("hcsshim::allocateLinuxResources Hot-adding SCSI virtual disk for OCI mount")
				_, _, err := coi.HostingSystem.AddSCSI(ctx, hostPath, uvmPathForShare, readOnly)
				if err != nil {
					return fmt.Errorf("adding SCSI virtual disk mount %+v: %s", mount, err)
				}
				resources.scsiMounts = append(resources.scsiMounts, scsiMount{path: hostPath, autoManage: mount.Type == "automanage-virtual-disk"})
				coi.Spec.Mounts[i].Type = "none"
			} else if strings.HasPrefix(mount.Source, "sandbox://") {
				// Mounts that map to a path in UVM are specified with 'sandbox://' prefix.
				// example: sandbox:///a/dirInUvm destination:/b/dirInContainer
				uvmPathForFile = mount.Source
			} else {
				st, err := os.Stat(hostPath)
				if err != nil {
					return fmt.Errorf("could not open bind mount target: %s", err)
				}
				restrictAccess := false
				var allowedNames []string
				if !st.IsDir() {
					// Map the containing directory in, but restrict the share to a single
					// file.
					var fileName string
					hostPath, fileName = filepath.Split(hostPath)
					allowedNames = append(allowedNames, fileName)
					restrictAccess = true
					uvmPathForFile = path.Join(uvmPathForShare, fileName)
				}
				l.Debug("hcsshim::allocateLinuxResources Hot-adding Plan9 for OCI mount")
				share, err := coi.HostingSystem.AddPlan9(ctx, hostPath, uvmPathForShare, readOnly, restrictAccess, allowedNames)
				if err != nil {
					return fmt.Errorf("adding plan9 mount %+v: %s", mount, err)
				}
				resources.plan9Mounts = append(resources.plan9Mounts, share)
			}
			coi.Spec.Mounts[i].Source = uvmPathForFile
		}
	}

	if coi.HostingSystem != nil && coi.Spec.Windows != nil {
		addGpuVhd := false
		for i, d := range coi.Spec.Windows.Devices {
			switch d.IDType {
			case "gpu":
				addGpuVhd = true
				index := coi.HostingSystem.GpuCounter() - 1
				name := fmt.Sprintf("gpu:%v", index)
				v := hcsschema.VirtualPciDevice{
					Functions: []hcsschema.VirtualPciFunction{
						{
							DeviceInstancePath: d.ID,
						},
					},
				}
				if err := coi.HostingSystem.AssignDevice(ctx, name, v); err != nil {
					return err
				}
				resources.vpciDevices = append(resources.vpciDevices, name)

				// update device ID so the gcs knows which nvidia devices to map into the container
				coi.Spec.Windows.Devices[i].ID = fmt.Sprintf("%v", index)
			}
		}

		if addGpuVhd {
			nvidiaSupportVhdPath, err := getNvidiaGpuVhdPath(coi)
			if err != nil {
				return errors.Wrapf(err, "failed to add nvidia vhd to %v", coi.HostingSystem.ID())
			}
			_, _, err = coi.HostingSystem.AddSCSI(ctx, nvidiaSupportVhdPath, lcowNvidiaMountPath, true)
			if err != nil {
				return errors.Wrapf(err, "failed to add scsi device %s in the UVM %s at %s", nvidiaSupportVhdPath, coi.HostingSystem.ID(), lcowNvidiaMountPath)
			}
			resources.scsiMounts = append(resources.scsiMounts, scsiMount{path: lcowNvidiaMountPath})
		}
	}

	return nil
}
