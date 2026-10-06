package drivers

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	deviceConfig "github.com/lxc/incus/v7/internal/server/device/config"
	"github.com/lxc/incus/v7/internal/server/operations"
	internalUtil "github.com/lxc/incus/v7/internal/util"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/subprocess"
	"github.com/lxc/incus/v7/shared/util"
	"github.com/lxc/incus/v7/shared/validate"
)

var dmQcow2Version string

type dmQcow2 struct {
	common
}

func (d *dmQcow2) load() error {
	for _, tool := range []string{"dmsetup", "qemu-img"} {
		_, err := exec.LookPath(tool)
		if err != nil {
			return fmt.Errorf("Required tool %q is missing", tool)
		}
	}

	_, _ = subprocess.RunCommand("modprobe", "dm-qcow2", "kernel_sets_dirty_bit=1")

	output, err := subprocess.RunCommand("dmsetup", "targets")
	if err != nil {
		return fmt.Errorf("Failed listing device-mapper targets: %w", err)
	}

	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "qcow2" {
			dirtyBitMode, err := os.ReadFile("/sys/module/dm_qcow2/parameters/kernel_sets_dirty_bit")
			if err != nil {
				return fmt.Errorf("Failed reading dm-qcow2 dirty bit mode: %w", err)
			}

			dirtyBitModeValue := strings.TrimSpace(string(dirtyBitMode))
			if dirtyBitModeValue != "Y" && dirtyBitModeValue != "1" {
				return errors.New("dm-qcow2 must be loaded with kernel_sets_dirty_bit=1")
			}

			dmQcow2Version = strings.Trim(fields[1], "v")
			return nil
		}
	}

	return errors.New("Device-mapper target \"qcow2\" is unavailable; load the dm-qcow2 module built for this kernel")
}

// Info returns information about the driver.
func (d *dmQcow2) Info() Info {
	return Info{
		Name:                         "dm-qcow2",
		Version:                      dmQcow2Version,
		VolumeTypes:                  []VolumeType{VolumeTypeContainer, VolumeTypeImage},
		DefaultVMBlockFilesystemSize: deviceConfig.DefaultVMBlockFilesystemSize,
		OptimizedImages:              false,
		PreservesInodes:              false,
		BlockBacking:                 true,
		RunningCopyFreeze:            true,
		DirectIO:                     true,
		IOUring:                      true,
		MountedRoot:                  true,
	}
}

// FillConfig applies default pool configuration.
func (d *dmQcow2) FillConfig() error {
	if d.config["source"] == "" {
		d.config["source"] = GetPoolMountPath(d.name)
	}

	return nil
}

// Create validates the backing directory.
func (d *dmQcow2) Create() error {
	err := d.FillConfig()
	if err != nil {
		return err
	}

	sourcePath := filepath.Clean(d.config["source"])
	if !filepath.IsAbs(sourcePath) {
		return errors.New("Source must be an absolute directory path")
	}

	if !util.PathExists(sourcePath) {
		return fmt.Errorf("Source path %q doesn't exist", sourcePath)
	}

	canonicalSourcePath, err := filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return fmt.Errorf("Failed resolving source path %q: %w", sourcePath, err)
	}

	canonicalVarPath, err := filepath.EvalSymlinks(internalUtil.VarPath())
	if err != nil {
		return fmt.Errorf("Failed resolving Incus data path: %w", err)
	}

	varPathPrefix := strings.TrimRight(canonicalVarPath, "/") + "/"
	poolPath := filepath.Clean(GetPoolMountPath(d.name))
	if (canonicalSourcePath == canonicalVarPath || strings.HasPrefix(canonicalSourcePath, varPathPrefix)) && sourcePath != poolPath {
		return fmt.Errorf("Source path %q is within the Incus directory", sourcePath)
	}

	isEmpty, err := internalUtil.PathIsEmpty(sourcePath)
	if err != nil {
		return err
	}

	if !isEmpty {
		return fmt.Errorf("Source path %q isn't empty", sourcePath)
	}

	return nil
}

// Delete removes the storage pool.
func (d *dmQcow2) Delete(op *operations.Operation) error {
	err := wipeDirectory(GetPoolMountPath(d.name))
	if err != nil {
		return err
	}

	_, err = d.Unmount()
	return err
}

func (d *dmQcow2) commonVolumeRules() map[string]func(string) error {
	return map[string]func(string) error{
		"block.filesystem":     validate.Optional(validate.IsOneOf(blockBackedAllowedFilesystems...)),
		"block.mount_options":  validate.IsAny,
		"block.create_options": validate.IsAny,
	}
}

// Validate checks the pool configuration.
func (d *dmQcow2) Validate(config map[string]string) error {
	return d.validatePool(config, nil, d.commonVolumeRules())
}

// Update applies pool configuration changes.
func (d *dmQcow2) Update(changedConfig map[string]string) error {
	_, changed := changedConfig["source"]
	if changed {
		return errors.New("Source cannot be changed")
	}

	return nil
}

// Mount mounts the storage pool.
func (d *dmQcow2) Mount() (bool, error) {
	poolPath := GetPoolMountPath(d.name)
	sourcePath := d.config["source"]
	if sourcePath == poolPath {
		return false, nil
	}

	if sameMount(sourcePath, poolPath) {
		return false, nil
	}

	err := TryMount(sourcePath, poolPath, "none", unix.MS_BIND, "")
	if err != nil {
		return false, err
	}

	return true, nil
}

// Unmount unmounts the storage pool.
func (d *dmQcow2) Unmount() (bool, error) {
	if d.config["source"] == GetPoolMountPath(d.name) {
		return false, nil
	}

	return forceUnmount(GetPoolMountPath(d.name))
}

// GetResources returns pool usage information.
func (d *dmQcow2) GetResources() (*api.ResourcesStoragePool, error) {
	return genericVFSGetResources(d)
}
