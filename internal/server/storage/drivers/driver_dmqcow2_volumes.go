package drivers

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/lxc/incus/v7/internal/instancewriter"
	"github.com/lxc/incus/v7/internal/linux"
	"github.com/lxc/incus/v7/internal/server/backup"
	"github.com/lxc/incus/v7/internal/server/migration"
	"github.com/lxc/incus/v7/internal/server/operations"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/logger"
	"github.com/lxc/incus/v7/shared/revert"
	"github.com/lxc/incus/v7/shared/units"
	"github.com/lxc/incus/v7/shared/util"
)

// FillVolumeConfig applies default volume configuration.
func (d *dmQcow2) FillVolumeConfig(vol Volume) error {
	err := d.fillVolumeConfig(&vol, "block.filesystem", "block.mount_options", "block.create_options")
	if err != nil {
		return err
	}

	if vol.config["block.filesystem"] == "" {
		vol.config["block.filesystem"] = d.config["volume.block.filesystem"]
	}

	if vol.config["block.filesystem"] == "" {
		vol.config["block.filesystem"] = DefaultFilesystem
	}

	if vol.config["block.mount_options"] == "" {
		vol.config["block.mount_options"] = d.config["volume.block.mount_options"]
	}

	if vol.config["block.mount_options"] == "" {
		vol.config["block.mount_options"] = "nodiscard"
	}

	if vol.config["block.create_options"] == "" {
		vol.config["block.create_options"] = d.config["volume.block.create_options"]
	}

	return nil
}

// ValidateVolume checks the volume configuration.
func (d *dmQcow2) ValidateVolume(vol Volume, removeUnknownKeys bool) error {
	if vol.contentType != ContentTypeFS {
		return errors.New("dm-qcow2 only supports filesystem volumes")
	}

	err := d.validateVolume(vol, d.commonVolumeRules(), removeUnknownKeys)
	if err != nil {
		return err
	}

	for _, option := range strings.Split(vol.ExpandedConfig("block.mount_options"), ",") {
		if option == "discard" {
			return errors.New("The discard mount option is unsupported by dm-qcow2")
		}
	}

	return nil
}

// CreateVolume creates and optionally fills a volume.
func (d *dmQcow2) CreateVolume(vol Volume, filler *VolumeFiller, op *operations.Operation) error {
	if vol.contentType != ContentTypeFS {
		return ErrNotSupported
	}

	reverter := revert.New()
	defer reverter.Fail()

	err := vol.EnsureMountPath(true)
	if err != nil {
		return err
	}

	reverter.Add(func() { _ = os.RemoveAll(vol.MountPath()) })

	volumeDir := d.volumeDir(vol)
	err = os.MkdirAll(volumeDir, 0o700)
	if err != nil {
		return err
	}

	reverter.Add(func() { _ = os.RemoveAll(volumeDir) })

	sizeBytes, err := units.ParseByteSizeString(vol.ConfigSize())
	if err != nil {
		return err
	}

	sizeBytes, err = d.roundVolumeBlockSizeBytes(vol, sizeBytes)
	if err != nil {
		return err
	}

	meta := &dmQcow2Metadata{Active: dmQcow2NewImageName(), Snapshots: []dmQcow2Snapshot{}}
	err = dmQcow2CreateImage(d.imagePath(vol, meta.Active), sizeBytes)
	if err != nil {
		return err
	}

	err = d.saveMetadata(vol, meta)
	if err != nil {
		return err
	}

	created, err := d.createMapping(vol, false)
	if err != nil {
		return err
	}

	if created {
		reverter.Add(func() { _ = d.removeMapping(vol) })
	}

	_, err = makeFSType(d.mapperPath(vol), vol.ConfigBlockFilesystem(), &mkfsOptions{ExtraArgs: vol.ExpandedConfig("block.create_options")})
	if err != nil {
		return fmt.Errorf("Failed creating %q filesystem: %w", vol.ConfigBlockFilesystem(), err)
	}

	err = vol.MountTask(func(mountPath string, op *operations.Operation) error {
		err := genericRunFiller(d, vol, "", filler, true)
		if err != nil {
			return err
		}

		return vol.EnsureMountPath(true)
	}, op)
	if err != nil {
		return err
	}

	reverter.Success()
	return nil
}

// CreateVolumeFromCopy copies a volume.
func (d *dmQcow2) CreateVolumeFromCopy(vol Volume, srcVol Volume, copySnapshots bool, allowInconsistent bool, op *operations.Operation) error {
	var srcSnapshots []Volume
	var err error
	if copySnapshots && !srcVol.IsSnapshot() {
		srcSnapshots, err = srcVol.Snapshots(op)
		if err != nil {
			return err
		}
	}

	return genericVFSCopyVolume(d, nil, vol, srcVol, srcSnapshots, false, allowInconsistent, op)
}

// RefreshVolume refreshes a volume from another volume.
func (d *dmQcow2) RefreshVolume(vol Volume, srcVol Volume, srcSnapshots []Volume, allowInconsistent bool, op *operations.Operation) error {
	return genericVFSCopyVolume(d, nil, vol, srcVol, srcSnapshots, true, allowInconsistent, op)
}

// CreateVolumeFromMigration creates a volume from a migration stream.
func (d *dmQcow2) CreateVolumeFromMigration(vol Volume, conn io.ReadWriteCloser, args migration.VolumeTargetArgs, filler *VolumeFiller, op *operations.Operation) error {
	return genericVFSCreateVolumeFromMigration(d, nil, vol, conn, args, filler, op)
}

// MigrateVolume sends a volume over a migration stream.
func (d *dmQcow2) MigrateVolume(vol Volume, conn io.ReadWriteCloser, args *migration.VolumeSourceArgs, op *operations.Operation) error {
	return genericVFSMigrateVolume(d, d.state, vol, conn, args, op)
}

// BackupVolume writes a non-optimized volume backup.
func (d *dmQcow2) BackupVolume(vol Volume, writer instancewriter.InstanceWriter, basePrefix string, optimized bool, snapshots []string, op *operations.Operation) error {
	return genericVFSBackupVolume(d, vol, writer, basePrefix, snapshots, op)
}

// CreateVolumeFromBackup restores a non-optimized volume backup.
func (d *dmQcow2) CreateVolumeFromBackup(vol Volume, srcBackup backup.Info, srcData io.ReadSeeker, basePrefix string, op *operations.Operation) (VolumePostHook, revert.Hook, error) {
	return genericVFSBackupUnpack(d, d.state.OS, vol, srcBackup.Snapshots, srcData, basePrefix, op)
}

// HasVolume indicates whether a volume exists.
func (d *dmQcow2) HasVolume(vol Volume) (bool, error) {
	topPath, err := d.topPath(vol)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}

		return false, err
	}

	return util.PathExists(topPath), nil
}

// DeleteVolume removes a volume.
func (d *dmQcow2) DeleteVolume(vol Volume, op *operations.Operation) error {
	snapshots, err := d.VolumeSnapshots(vol, op)
	if err != nil {
		return err
	}

	if len(snapshots) > 0 {
		return errors.New("Cannot remove a volume that has snapshots")
	}

	if linux.IsMountPoint(vol.MountPath()) || d.mappingExists(vol) {
		return ErrInUse
	}

	err = os.RemoveAll(d.volumeDir(vol))
	if err != nil {
		return err
	}

	err = os.RemoveAll(vol.MountPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}

// GetVolumeUsage returns volume usage.
func (d *dmQcow2) GetVolumeUsage(vol Volume) (int64, error) {
	if linux.IsMountPoint(vol.MountPath()) {
		var stat unix.Statfs_t
		err := unix.Statfs(vol.MountPath(), &stat)
		if err != nil {
			return -1, err
		}

		return int64(stat.Blocks-stat.Bfree) * int64(stat.Bsize), nil
	}

	return -1, ErrNotSupported
}

// SetVolumeQuota grows a volume.
func (d *dmQcow2) SetVolumeQuota(vol Volume, size string, allowUnsafeResize bool, op *operations.Operation) error {
	if size == "" || size == "0" {
		return nil
	}

	sizeBytes, err := units.ParseByteSizeString(size)
	if err != nil {
		return err
	}

	sizeBytes, err = d.roundVolumeBlockSizeBytes(vol, sizeBytes)
	if err != nil {
		return err
	}

	activePath, err := d.topPath(vol)
	if err != nil {
		return err
	}

	info, err := Qcow2Info(activePath)
	if err != nil {
		return err
	}

	oldSize := int64(info.VirtualSize)
	if sizeBytes < oldSize {
		return fmt.Errorf("dm-qcow2 volumes cannot be shrunk: %w", ErrCannotBeShrunk)
	}

	if sizeBytes == oldSize {
		return nil
	}

	mapped := d.mappingExists(vol)
	needsResume := false
	if mapped {
		err = d.suspendMapping(vol)
		if err != nil {
			return err
		}

		needsResume = true
		defer func() {
			if needsResume {
				logger.WarnOnError(func() error { return d.resumeMapping(vol) }, "Failed resuming dm-qcow2 volume after resize")
			}
		}()
	}

	err = Qcow2Resize(activePath, sizeBytes)
	if err != nil {
		return err
	}

	if mapped {
		err = d.reloadMapping(vol)
		if err != nil {
			return err
		}

		err = d.resumeMapping(vol)
		if err != nil {
			return err
		}

		needsResume = false
	}

	err = growFileSystem(vol.ConfigBlockFilesystem(), d.mapperPath(vol), vol)
	if err != nil {
		return err
	}

	return nil
}

// UpdateVolume applies volume configuration changes.
func (d *dmQcow2) UpdateVolume(vol Volume, changedConfig map[string]string) error {
	size, changed := changedConfig["size"]
	if changed {
		err := d.SetVolumeQuota(vol, size, false, nil)
		if err != nil {
			return err
		}
	}

	for _, key := range []string{"block.filesystem", "block.create_options"} {
		_, changed := changedConfig[key]
		if changed {
			return fmt.Errorf("%s cannot be changed", key)
		}
	}

	return d.updateVolume(vol, changedConfig)
}

// GetVolumeDiskPath returns the active mapper path.
func (d *dmQcow2) GetVolumeDiskPath(vol Volume) (string, error) {
	if !d.mappingExists(vol) {
		return "", os.ErrNotExist
	}

	return d.mapperPath(vol), nil
}

// ListVolumes returns all volumes in the pool.
func (d *dmQcow2) ListVolumes() ([]Volume, error) {
	volumes := []Volume{}
	for _, volType := range d.Info().VolumeTypes {
		typeDir := filepath.Join(GetPoolMountPath(d.name), dmQcow2DataDir, string(volType))
		entries, err := os.ReadDir(typeDir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}

			return nil, err
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			name, err := dmQcow2UnescapeName(entry.Name())
			if err != nil {
				continue
			}

			volumes = append(volumes, NewVolume(d, d.name, volType, ContentTypeFS, name, map[string]string{}, d.config))
		}
	}

	return volumes, nil
}

// ActivateTask runs a task with an exclusively mapped volume.
func (d *dmQcow2) ActivateTask(vol Volume, task func(devPath string, op *operations.Operation) error, op *operations.Operation) error {
	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	created, err := d.createMapping(vol, vol.IsSnapshot())
	if err != nil {
		return err
	}

	if !created {
		return errors.New("Volume is already active, can't run exclusive activation task")
	}

	taskErr := task(d.mapperPath(vol), op)
	cleanupErr := d.removeMapping(vol)
	return errors.Join(taskErr, cleanupErr)
}

// MountVolume maps and mounts a volume.
func (d *dmQcow2) MountVolume(vol Volume, op *operations.Operation) error {
	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	reverter := revert.New()
	defer reverter.Fail()

	created, err := d.createMapping(vol, false)
	if err != nil {
		return err
	}

	if created {
		reverter.Add(func() { _ = d.removeMapping(vol) })
	}

	if !linux.IsMountPoint(vol.MountPath()) {
		err = vol.EnsureMountPath(false)
		if err != nil {
			return err
		}

		flags, options := linux.ResolveMountOptions(strings.Split(vol.ConfigBlockMountOptions(), ","))
		err = TryMount(d.mapperPath(vol), vol.MountPath(), vol.ConfigBlockFilesystem(), flags, options)
		if err != nil {
			return fmt.Errorf("Failed mounting dm-qcow2 volume: %w", err)
		}

		reverter.Add(func() { _ = TryUnmount(vol.MountPath(), 0) })
	}

	vol.MountRefCountIncrement()
	reverter.Success()
	return nil
}

// UnmountVolume unmounts and optionally unmaps a volume.
func (d *dmQcow2) UnmountVolume(vol Volume, keepBlockDev bool, op *operations.Operation) (bool, error) {
	unlock, err := vol.MountLock()
	if err != nil {
		return false, err
	}

	defer unlock()

	refCount := vol.MountRefCountDecrement()
	if refCount > 0 {
		return false, ErrInUse
	}

	ourUnmount := false
	if linux.IsMountPoint(vol.MountPath()) {
		err = TryUnmount(vol.MountPath(), 0)
		if err != nil {
			return false, err
		}

		ourUnmount = true
	}

	if !keepBlockDev {
		err = d.removeMapping(vol)
		if err != nil {
			return ourUnmount, err
		}
	}

	return ourUnmount, nil
}

// RenameVolume renames a volume.
func (d *dmQcow2) RenameVolume(vol Volume, newVolName string, op *operations.Operation) error {
	snapshotMapped, err := d.snapshotMappingExists(vol, op)
	if err != nil {
		return err
	}

	if d.mappingExists(vol) || snapshotMapped {
		return ErrInUse
	}

	oldDataPath := d.volumeDir(vol)
	newVol := NewVolume(d, d.name, vol.volType, vol.contentType, newVolName, vol.config, vol.poolConfig)
	newDataPath := d.volumeDir(newVol)

	err = os.Rename(oldDataPath, newDataPath)
	if err != nil {
		return err
	}

	err = genericVFSRenameVolume(d, vol, newVolName, op)
	if err != nil {
		_ = os.Rename(newDataPath, oldDataPath)
		return err
	}

	return nil
}

func (d *dmQcow2) snapshotMappingExists(vol Volume, op *operations.Operation) (bool, error) {
	snapshotNames, err := d.VolumeSnapshots(vol, op)
	if err != nil {
		return false, err
	}

	for _, snapshotName := range snapshotNames {
		snapVol, err := vol.NewSnapshot(snapshotName)
		if err != nil {
			return false, err
		}

		if d.mappingExists(snapVol) {
			return true, nil
		}
	}

	return false, nil
}

// CreateVolumeSnapshot freezes the active layer as a snapshot and stacks a new active overlay on top of it.
func (d *dmQcow2) CreateVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	if !snapVol.IsSnapshot() {
		return errors.New("Volume must be a snapshot")
	}

	parentName, snapName, _ := api.GetParentAndSnapshotName(snapVol.name)
	parentVol := NewVolume(d, d.name, snapVol.volType, snapVol.contentType, parentName, snapVol.config, snapVol.poolConfig)

	meta, err := d.loadMetadata(parentVol)
	if err != nil {
		return err
	}

	if meta.snapshotIndex(snapName) >= 0 {
		return fmt.Errorf("Snapshot %q already exists", snapName)
	}

	reverter := revert.New()
	defer reverter.Fail()

	err = snapVol.EnsureMountPath(false)
	if err != nil {
		return err
	}

	reverter.Add(func() {
		_ = os.RemoveAll(snapVol.MountPath())
		_ = deleteParentSnapshotDirIfEmpty(d.name, snapVol.volType, parentName)
	})

	live := d.mappingExists(parentVol)
	if live {
		err = d.suspendMapping(parentVol)
		if err != nil {
			return err
		}

		reverter.Add(func() {
			logger.WarnOnError(func() error { return d.resumeMapping(parentVol) }, "Failed resuming dm-qcow2 volume after snapshot failure")
		})
	}

	overlayPath := d.imagePath(parentVol, dmQcow2NewImageName())
	err = dmQcow2CreateOverlay(overlayPath, d.imagePath(parentVol, meta.Active))
	if err != nil {
		return err
	}

	reverter.Add(func() { _ = os.Remove(overlayPath) })

	newMeta := &dmQcow2Metadata{
		Active:    filepath.Base(overlayPath),
		Snapshots: append(slices.Clone(meta.Snapshots), dmQcow2Snapshot{Name: snapName, Image: meta.Active}),
	}

	err = d.saveMetadata(parentVol, newMeta)
	if err != nil {
		return err
	}

	reverter.Add(func() { _ = d.saveMetadata(parentVol, meta) })

	if !live {
		reverter.Success()
		return nil
	}

	err = d.reloadMapping(parentVol)
	if err != nil {
		return err
	}

	reverter.Success()

	err = d.resumeMapping(parentVol)
	if err != nil {
		retryErr := d.resumeMapping(parentVol)
		if retryErr != nil {
			return errors.Join(err, retryErr)
		}
	}

	return nil
}

// MountVolumeSnapshot maps and mounts a snapshot read-only.
func (d *dmQcow2) MountVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	unlock, err := snapVol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	reverter := revert.New()
	defer reverter.Fail()

	created, err := d.createMapping(snapVol, true)
	if err != nil {
		return err
	}

	if created {
		reverter.Add(func() { _ = d.removeMapping(snapVol) })
	}

	if !linux.IsMountPoint(snapVol.MountPath()) {
		err = snapVol.EnsureMountPath(false)
		if err != nil {
			return err
		}

		flags, options := linux.ResolveMountOptions(strings.Split(snapVol.ConfigBlockMountOptions(), ","))
		err = TryMount(d.mapperPath(snapVol), snapVol.MountPath(), snapVol.ConfigBlockFilesystem(), flags|unix.MS_RDONLY, options)
		if err != nil {
			return err
		}

		reverter.Add(func() { _ = TryUnmount(snapVol.MountPath(), 0) })
	}

	snapVol.MountRefCountIncrement()
	reverter.Success()
	return nil
}

// UnmountVolumeSnapshot unmounts and unmaps a snapshot.
func (d *dmQcow2) UnmountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	unlock, err := snapVol.MountLock()
	if err != nil {
		return false, err
	}

	defer unlock()

	refCount := snapVol.MountRefCountDecrement()
	if refCount > 0 {
		return false, ErrInUse
	}

	ourUnmount := false
	if linux.IsMountPoint(snapVol.MountPath()) {
		err = TryUnmount(snapVol.MountPath(), 0)
		if err != nil {
			return false, err
		}

		ourUnmount = true
	}

	err = d.removeMapping(snapVol)
	if err != nil {
		return ourUnmount, err
	}

	return ourUnmount, nil
}

// VolumeSnapshots lists a volume's snapshots.
func (d *dmQcow2) VolumeSnapshots(vol Volume, op *operations.Operation) ([]string, error) {
	meta, err := d.loadMetadata(vol)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []string{}, nil
		}

		return nil, err
	}

	snapshots := make([]string, 0, len(meta.Snapshots))
	for _, snapshot := range meta.Snapshots {
		snapshots = append(snapshots, snapshot.Name)
	}

	return snapshots, nil
}

// DeleteVolumeSnapshot repairs the chain and removes an offline snapshot.
func (d *dmQcow2) DeleteVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	parentName, snapName, _ := api.GetParentAndSnapshotName(snapVol.name)
	parentVol := NewVolume(d, d.name, snapVol.volType, snapVol.contentType, parentName, snapVol.config, snapVol.poolConfig)
	snapshotMapped, err := d.snapshotMappingExists(parentVol, op)
	if err != nil {
		return err
	}

	if d.mappingExists(parentVol) || snapshotMapped || linux.IsMountPoint(snapVol.MountPath()) {
		return ErrInUse
	}

	meta, err := d.loadMetadata(parentVol)
	if err != nil {
		return err
	}

	idx := meta.snapshotIndex(snapName)
	if idx >= 0 {
		snapshotPath := d.imagePath(parentVol, meta.Snapshots[idx].Image)
		backingPath, err := dmQcow2BackingPath(snapshotPath)
		if err != nil {
			return err
		}

		children, err := dmQcow2ChildPaths(d.volumeDir(parentVol), meta.images(), snapshotPath)
		if err != nil {
			return err
		}

		for _, childPath := range children {
			err = dmQcow2Rebase(childPath, backingPath)
			if err != nil {
				return fmt.Errorf("Failed repairing QCOW2 chain through %q: %w", snapshotPath, err)
			}
		}

		newMeta := &dmQcow2Metadata{
			Active:    meta.Active,
			Snapshots: slices.Delete(slices.Clone(meta.Snapshots), idx, idx+1),
		}

		err = d.saveMetadata(parentVol, newMeta)
		if err != nil {
			return err
		}

		err = os.Remove(snapshotPath)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}

	err = os.RemoveAll(snapVol.MountPath())
	if err != nil {
		return err
	}

	return deleteParentSnapshotDirIfEmpty(d.name, snapVol.volType, parentName)
}

// RenameVolumeSnapshot renames a snapshot in the volume metadata.
func (d *dmQcow2) RenameVolumeSnapshot(snapVol Volume, newSnapshotName string, op *operations.Operation) error {
	if d.mappingExists(snapVol) {
		return ErrInUse
	}

	parentName, oldSnapshotName, _ := api.GetParentAndSnapshotName(snapVol.name)
	parentVol := NewVolume(d, d.name, snapVol.volType, snapVol.contentType, parentName, snapVol.config, snapVol.poolConfig)
	newSnapVol, err := parentVol.NewSnapshot(newSnapshotName)
	if err != nil {
		return err
	}

	meta, err := d.loadMetadata(parentVol)
	if err != nil {
		return err
	}

	idx := meta.snapshotIndex(oldSnapshotName)
	if idx < 0 {
		return fmt.Errorf("Snapshot %q not found", oldSnapshotName)
	}

	if meta.snapshotIndex(newSnapshotName) >= 0 {
		return fmt.Errorf("Snapshot %q already exists", newSnapshotName)
	}

	reverter := revert.New()
	defer reverter.Fail()

	err = genericVFSRenameVolumeSnapshot(d, snapVol, newSnapshotName, op)
	if err != nil {
		return err
	}

	reverter.Add(func() {
		logger.WarnOnError(func() error { return genericVFSRenameVolumeSnapshot(d, newSnapVol, oldSnapshotName, op) }, "Failed reverting snapshot mount path rename")
	})

	newMeta := &dmQcow2Metadata{Active: meta.Active, Snapshots: slices.Clone(meta.Snapshots)}
	newMeta.Snapshots[idx].Name = newSnapshotName
	err = d.saveMetadata(parentVol, newMeta)
	if err != nil {
		return err
	}

	reverter.Success()
	return nil
}

// RestoreVolume replaces the active overlay with a new one based on a snapshot.
func (d *dmQcow2) RestoreVolume(vol Volume, snapshotName string, op *operations.Operation) error {
	if d.mappingExists(vol) || linux.IsMountPoint(vol.MountPath()) {
		return ErrInUse
	}

	meta, err := d.loadMetadata(vol)
	if err != nil {
		return err
	}

	idx := meta.snapshotIndex(snapshotName)
	if idx < 0 {
		return errors.New("Snapshot not found")
	}

	overlayPath := d.imagePath(vol, dmQcow2NewImageName())
	err = dmQcow2CreateOverlay(overlayPath, d.imagePath(vol, meta.Snapshots[idx].Image))
	if err != nil {
		return err
	}

	err = d.saveMetadata(vol, &dmQcow2Metadata{Active: filepath.Base(overlayPath), Snapshots: meta.Snapshots})
	if err != nil {
		_ = os.Remove(overlayPath)
		return err
	}

	oldActivePath := d.imagePath(vol, meta.Active)
	logger.WarnOnError(func() error { return os.Remove(oldActivePath) }, "Failed removing previous dm-qcow2 active layer")

	return nil
}
