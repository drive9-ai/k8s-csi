package driver

import (
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestBuildRemoteRoot(t *testing.T) {
	got, err := buildRemoteRoot("/k8s/workspaces", "pvc-123_test")
	if err != nil {
		t.Fatalf("buildRemoteRoot error = %v", err)
	}
	if got == "" || got[0] != '/' {
		t.Fatalf("remote root = %q, want absolute path", got)
	}
	if got == "/k8s/workspaces" {
		t.Fatalf("remote root did not include volume suffix")
	}
}

func TestBuildRemoteRootRejectsRootPrefix(t *testing.T) {
	if _, err := buildRemoteRoot("/", "pvc"); err == nil {
		t.Fatal("expected root prefix to be rejected")
	}
}

func TestEncodeDrive9FSPathEscapesSegments(t *testing.T) {
	got, err := encodeDrive9FSPath("/team a/vol#1")
	if err != nil {
		t.Fatalf("encodeDrive9FSPath error = %v", err)
	}
	want := "/v1/fs/team%20a/vol%231"
	if got != want {
		t.Fatalf("encoded path = %q, want %q", got, want)
	}
}

func TestVolumeIDIsShortAndStable(t *testing.T) {
	remoteRoot := "/k8s/pvc/demo"
	first := volumeIDForRemoteRoot(remoteRoot)
	second := volumeIDForRemoteRoot(remoteRoot)
	if first != second {
		t.Fatalf("volume id is not stable: %q != %q", first, second)
	}
	if len(first) > 64 {
		t.Fatalf("volume id len = %d, want <= 64", len(first))
	}
}

func TestIndexPathUsesMetadataRoot(t *testing.T) {
	remoteRoot := "/k8s/pvc/demo"
	volumeID := volumeIDForRemoteRoot(remoteRoot)
	got := indexPath(volumeID)
	if got != metadataRoot+"/"+volumeID+".json" {
		t.Fatalf("index path = %q", got)
	}
}

func TestNormalizeRemotePathRejectsTraversal(t *testing.T) {
	if _, err := normalizeRemotePath("/safe/../other"); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}

func TestValidateVolumeCapabilitiesRejectsRWX(t *testing.T) {
	err := validateVolumeCapabilities([]*csi.VolumeCapability{
		{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		},
	})
	if err == nil {
		t.Fatal("expected RWX capability to be rejected")
	}
}

func TestValidateVolumeCapabilitiesAllowsSingleNodeWriter(t *testing.T) {
	err := validateVolumeCapabilities([]*csi.VolumeCapability{
		{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	})
	if err != nil {
		t.Fatalf("validateVolumeCapabilities error = %v", err)
	}
}
