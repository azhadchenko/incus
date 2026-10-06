package drivers

import (
	"strings"
	"testing"
)

func TestDMQcow2EscapeName(t *testing.T) {
	t.Parallel()

	name := "default/container name%with/slash"
	escaped := dmQcow2EscapeName(name)
	if strings.Contains(escaped, "/") {
		t.Fatalf("Escaped name %q still contains a path separator", escaped)
	}

	unescaped, err := dmQcow2UnescapeName(escaped)
	if err != nil {
		t.Fatalf("Failed unescaping name: %v", err)
	}

	if unescaped != name {
		t.Fatalf("Expected %q, got %q", name, unescaped)
	}
}

func TestDMQcow2MapperName(t *testing.T) {
	t.Parallel()

	driver := &dmQcow2{}
	driver.name = "pool"

	vol := NewVolume(driver, "pool", VolumeTypeContainer, ContentTypeFS, "c1", nil, nil)
	snapVol, err := vol.NewSnapshot("snap0")
	if err != nil {
		t.Fatalf("Failed creating snapshot volume: %v", err)
	}

	volumeName := driver.mapperName(vol)
	if volumeName != driver.mapperName(vol) {
		t.Fatal("Mapper name is not stable")
	}

	if volumeName == driver.mapperName(snapVol) {
		t.Fatal("Volume and snapshot mapper names collide")
	}

	if len(volumeName) > 128 {
		t.Fatalf("Mapper name %q is too long", volumeName)
	}
}
