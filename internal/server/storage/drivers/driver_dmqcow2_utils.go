package drivers

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v4"
	"golang.org/x/sys/unix"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/subprocess"
	"github.com/lxc/incus/v7/shared/util"
)

const (
	dmQcow2DataDir      = ".dm-qcow2"
	dmQcow2MetadataFile = "volume.yaml"

	dmQcow2CreateOptions = "cluster_size=1M,extended_l2=on"
)

// dmQcow2Snapshot maps a snapshot name to its QCOW2 image.
type dmQcow2Snapshot struct {
	Name  string `yaml:"name"`
	Image string `yaml:"image"`
}

// dmQcow2Metadata records the QCOW2 images making up a volume.
type dmQcow2Metadata struct {
	Active    string            `yaml:"active"`
	Snapshots []dmQcow2Snapshot `yaml:"snapshots"`
}

func (m *dmQcow2Metadata) snapshotIndex(name string) int {
	return slices.IndexFunc(m.Snapshots, func(snapshot dmQcow2Snapshot) bool { return snapshot.Name == name })
}

func (m *dmQcow2Metadata) images() []string {
	images := []string{m.Active}
	for _, snapshot := range m.Snapshots {
		images = append(images, snapshot.Image)
	}

	return images
}

func dmQcow2NewImageName() string {
	return uuid.New().String() + ".qcow2"
}

func dmQcow2EscapeName(name string) string {
	return url.PathEscape(name)
}

func dmQcow2UnescapeName(name string) (string, error) {
	return url.PathUnescape(name)
}

func (d *dmQcow2) volumeDir(vol Volume) string {
	name := vol.name
	if vol.IsSnapshot() {
		name, _, _ = api.GetParentAndSnapshotName(name)
	}

	return filepath.Join(GetPoolMountPath(d.name), dmQcow2DataDir, string(vol.volType), dmQcow2EscapeName(name))
}

func (d *dmQcow2) imagePath(vol Volume, image string) string {
	return filepath.Join(d.volumeDir(vol), image)
}

func (d *dmQcow2) metadataPath(vol Volume) string {
	return filepath.Join(d.volumeDir(vol), dmQcow2MetadataFile)
}

func (d *dmQcow2) loadMetadata(vol Volume) (*dmQcow2Metadata, error) {
	path := d.metadataPath(vol)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	meta := &dmQcow2Metadata{}
	err = yaml.Load(data, meta)
	if err != nil {
		return nil, fmt.Errorf("Failed parsing %q: %w", path, err)
	}

	for _, image := range meta.images() {
		if filepath.Base(image) != image || !strings.HasSuffix(image, ".qcow2") {
			return nil, fmt.Errorf("Invalid QCOW2 image name %q in %q", image, path)
		}
	}

	return meta, nil
}

func (d *dmQcow2) saveMetadata(vol Volume, meta *dmQcow2Metadata) error {
	data, err := yaml.Dump(meta, yaml.WithV2Defaults())
	if err != nil {
		return err
	}

	path := d.metadataPath(vol)
	tmpPath := path + tmpVolSuffix
	file, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}

	err = errors.Join(err, file.Close())
	if err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("Failed writing %q: %w", tmpPath, err)
	}

	return os.Rename(tmpPath, path)
}

func (d *dmQcow2) topPath(vol Volume) (string, error) {
	meta, err := d.loadMetadata(vol)
	if err != nil {
		return "", err
	}

	if !vol.IsSnapshot() {
		return d.imagePath(vol, meta.Active), nil
	}

	_, snapName, _ := api.GetParentAndSnapshotName(vol.name)
	idx := meta.snapshotIndex(snapName)
	if idx < 0 {
		return "", fmt.Errorf("Snapshot %q not found: %w", snapName, fs.ErrNotExist)
	}

	return d.imagePath(vol, meta.Snapshots[idx].Image), nil
}

func (d *dmQcow2) mapperName(vol Volume) string {
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%s", d.name, vol.volType, vol.contentType, vol.name)
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("incus-dmqcow2-%x", sum[:12])
}

func (d *dmQcow2) mapperPath(vol Volume) string {
	return filepath.Join("/dev/mapper", d.mapperName(vol))
}

func (d *dmQcow2) mappingExists(vol Volume) bool {
	return util.PathExists(d.mapperPath(vol))
}

func (d *dmQcow2) imageChain(topPath string) ([]string, int64, error) {
	volumeDir := filepath.Dir(topPath)
	canonicalVolumeDir, err := filepath.EvalSymlinks(volumeDir)
	if err != nil {
		return nil, 0, fmt.Errorf("Failed resolving volume directory %q: %w", volumeDir, err)
	}

	currentPath := filepath.Clean(topPath)
	seen := map[string]struct{}{}
	chain := []string{}
	var virtualSize int64

	for range 100 {
		fileInfo, err := os.Lstat(currentPath)
		if err != nil {
			return nil, 0, err
		}

		if fileInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() {
			return nil, 0, fmt.Errorf("QCOW2 layer %q must be a regular non-symlink file", currentPath)
		}

		canonicalPath, err := filepath.EvalSymlinks(currentPath)
		if err != nil {
			return nil, 0, fmt.Errorf("Failed resolving QCOW2 layer %q: %w", currentPath, err)
		}

		rel, err := filepath.Rel(canonicalVolumeDir, canonicalPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, 0, fmt.Errorf("QCOW2 backing file %q escapes volume directory %q", currentPath, canonicalVolumeDir)
		}

		_, ok := seen[currentPath]
		if ok {
			return nil, 0, fmt.Errorf("QCOW2 backing chain contains a loop at %q", currentPath)
		}

		seen[currentPath] = struct{}{}
		info, err := Qcow2Info(currentPath)
		if err != nil {
			return nil, 0, fmt.Errorf("Failed reading QCOW2 metadata for %q: %w", currentPath, err)
		}

		if virtualSize == 0 {
			virtualSize = int64(info.VirtualSize)
		}

		chain = append(chain, currentPath)
		if info.BackingFilename == "" {
			slices.Reverse(chain)
			return chain, virtualSize, nil
		}

		currentPath = info.BackingFilename
		if !filepath.IsAbs(currentPath) {
			currentPath = filepath.Join(filepath.Dir(chain[len(chain)-1]), currentPath)
		}

		currentPath = filepath.Clean(currentPath)
	}

	return nil, 0, errors.New("QCOW2 backing chain exceeds 100 layers")
}

func dmQcow2OpenImages(chain []string, readOnly bool) ([]*os.File, error) {
	files := make([]*os.File, 0, len(chain))
	for i, path := range chain {
		flags := unix.O_RDONLY | unix.O_DIRECT | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if !readOnly && i == len(chain)-1 {
			flags = unix.O_RDWR | unix.O_DIRECT | unix.O_CLOEXEC | unix.O_NOFOLLOW
		}

		fd, err := unix.Open(path, flags, 0)
		if err != nil {
			for _, file := range files {
				_ = file.Close()
			}

			return nil, fmt.Errorf("Failed opening QCOW2 layer %q with O_DIRECT: %w", path, err)
		}

		files = append(files, os.NewFile(uintptr(fd), path))
	}

	return files, nil
}

func (d *dmQcow2) loadMapping(vol Volume, reload bool, readOnly bool) error {
	topPath, err := d.topPath(vol)
	if err != nil {
		return err
	}

	chain, virtualSize, err := d.imageChain(topPath)
	if err != nil {
		return err
	}

	if virtualSize <= 0 || virtualSize%512 != 0 {
		return fmt.Errorf("Invalid QCOW2 virtual size %d", virtualSize)
	}

	files, err := dmQcow2OpenImages(chain, readOnly)
	if err != nil {
		return err
	}

	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()

	fdArgs := make([]string, len(files))
	for i := range files {
		fdArgs[i] = fmt.Sprintf("%d", 3+i)
	}

	table := fmt.Sprintf("0 %d qcow2 %s", virtualSize/512, strings.Join(fdArgs, " "))
	action := "create"
	if reload {
		action = "reload"
	}

	args := []string{action, d.mapperName(vol)}
	if readOnly && !reload {
		args = append(args, "--readonly")
	}

	args = append(args, "--table", table)
	_, err = subprocess.RunCommandInheritFds(context.TODO(), files, "dmsetup", args...)
	if err != nil {
		return fmt.Errorf("Failed to %s dm-qcow2 mapping %q: %w", action, d.mapperName(vol), err)
	}

	if !reload {
		deadline := time.Now().Add(2 * time.Second)
		for !d.mappingExists(vol) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}

		if !d.mappingExists(vol) {
			_ = d.removeMapping(vol)
			return fmt.Errorf("Device path %q did not appear", d.mapperPath(vol))
		}
	}

	return nil
}

func (d *dmQcow2) createMapping(vol Volume, readOnly bool) (bool, error) {
	if d.mappingExists(vol) {
		return false, nil
	}

	err := d.loadMapping(vol, false, readOnly)
	return err == nil, err
}

func (d *dmQcow2) removeMapping(vol Volume) error {
	if !d.mappingExists(vol) {
		return nil
	}

	_, err := subprocess.RunCommand("dmsetup", "remove", d.mapperName(vol))
	if err != nil {
		return fmt.Errorf("Failed removing dm-qcow2 mapping %q: %w", d.mapperName(vol), err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for d.mappingExists(vol) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	if d.mappingExists(vol) {
		return fmt.Errorf("Device path %q did not disappear", d.mapperPath(vol))
	}

	return nil
}

func (d *dmQcow2) suspendMapping(vol Volume) error {
	_, err := subprocess.RunCommand("dmsetup", "suspend", d.mapperName(vol))
	if err != nil {
		return fmt.Errorf("Failed suspending dm-qcow2 mapping %q: %w", d.mapperName(vol), err)
	}

	return nil
}

func (d *dmQcow2) resumeMapping(vol Volume) error {
	_, err := subprocess.RunCommand("dmsetup", "resume", d.mapperName(vol))
	if err != nil {
		return fmt.Errorf("Failed resuming dm-qcow2 mapping %q: %w", d.mapperName(vol), err)
	}

	return nil
}

func (d *dmQcow2) reloadMapping(vol Volume) error {
	return d.loadMapping(vol, true, vol.IsSnapshot())
}

func dmQcow2RunQemuImg(dir string, args ...string) error {
	cmd := exec.Command("qemu-img", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img %s failed: %s: %w", args[0], strings.TrimSpace(string(output)), err)
	}

	return nil
}

func dmQcow2CreateImage(path string, size int64) error {
	return dmQcow2RunQemuImg(filepath.Dir(path), "create", "-f", "qcow2", "-o", dmQcow2CreateOptions, filepath.Base(path), fmt.Sprintf("%db", size))
}

func dmQcow2CreateOverlay(path string, backingPath string) error {
	return dmQcow2RunQemuImg(filepath.Dir(path), "create", "-f", "qcow2", "-o", dmQcow2CreateOptions, "-F", "qcow2", "-b", filepath.Base(backingPath), filepath.Base(path))
}

func dmQcow2Rebase(path string, backingPath string) error {
	args := []string{"rebase"}
	if backingPath != "" {
		args = append(args, "-f", "qcow2", "-F", "qcow2", "-b", filepath.Base(backingPath))
	} else {
		args = append(args, "-f", "qcow2", "-b", "")
	}

	args = append(args, filepath.Base(path))
	return dmQcow2RunQemuImg(filepath.Dir(path), args...)
}

func dmQcow2BackingPath(path string) (string, error) {
	info, err := Qcow2Info(path)
	if err != nil {
		return "", err
	}

	if info.BackingFilename == "" {
		return "", nil
	}

	backingPath := info.BackingFilename
	if !filepath.IsAbs(backingPath) {
		backingPath = filepath.Join(filepath.Dir(path), backingPath)
	}

	return filepath.Clean(backingPath), nil
}

func dmQcow2ChildPaths(volumeDir string, images []string, parentPath string) ([]string, error) {
	parentPath = filepath.Clean(parentPath)
	children := []string{}
	for _, image := range images {
		path := filepath.Join(volumeDir, image)
		if path == parentPath {
			continue
		}

		backingPath, err := dmQcow2BackingPath(path)
		if err != nil {
			return nil, err
		}

		if backingPath == parentPath {
			children = append(children, path)
		}
	}

	return children, nil
}
