package driver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
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

func TestBuildRemoteRootRejectsMetadataOverlap(t *testing.T) {
	if _, err := buildRemoteRoot("/k8s/.drive9-csi/volumes", "pvc"); err == nil {
		t.Fatal("expected metadata prefix overlap to be rejected")
	}
}

func TestValidateSafeDeleteRootRejectsRootAndMetadata(t *testing.T) {
	for _, path := range []string{"/", "/k8s/.drive9-csi", "/k8s/.drive9-csi/volumes/x"} {
		if err := validateSafeDeleteRoot(path); err == nil {
			t.Fatalf("expected %q to be unsafe", path)
		}
	}
}

func TestValidateVolumeRootRejectsOutsideAllowedRoot(t *testing.T) {
	for _, path := range []string{"/tmp/pvc", "/drive9/pvc", "/k8s/.drive9-csi/volumes/demo"} {
		if err := validateVolumeRoot(path); err == nil {
			t.Fatalf("expected %q to be unsafe", path)
		}
	}
}

func TestValidateMountRootAllowsWorkspaceRootButRejectsMetadataPath(t *testing.T) {
	for _, remotePath := range []string{"/", "/projects/team-a", "/k8s"} {
		if err := validateMountRoot(remotePath); err != nil {
			t.Fatalf("validateMountRoot(%q) error = %v", remotePath, err)
		}
	}
	for _, remotePath := range []string{"/k8s/.drive9-csi", "/k8s/.drive9-csi/volumes/demo"} {
		if err := validateMountRoot(remotePath); err == nil {
			t.Fatalf("expected metadata path %q to be rejected", remotePath)
		}
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

func TestWorkspaceRootVolumeIDIncludesVolumeName(t *testing.T) {
	first := volumeIDForWorkspaceRoot("pvc-a", "/")
	second := volumeIDForWorkspaceRoot("pvc-a", "/")
	other := volumeIDForWorkspaceRoot("pvc-b", "/")
	if first != second {
		t.Fatalf("workspace root volume id is not stable: %q != %q", first, second)
	}
	if first == other {
		t.Fatalf("workspace root volume id must include volume name: %q", first)
	}
	if !isWorkspaceRootVolumeID(first) {
		t.Fatalf("workspace root volume id was not recognized: %q", first)
	}
	if len(first) > 64 {
		t.Fatalf("workspace root volume id len = %d, want <= 64", len(first))
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

func TestValidateVolumeCapabilitiesRejectsNilAndBlock(t *testing.T) {
	if err := validateVolumeCapabilities([]*csi.VolumeCapability{nil}); err == nil {
		t.Fatal("expected nil capability to be rejected")
	}
	if err := validateVolumeCapabilities([]*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}}); err == nil {
		t.Fatal("expected block capability to be rejected")
	}
}

func TestValidateVolumeCapabilitiesRejectsUnsupportedMountOptions(t *testing.T) {
	tests := []struct {
		name  string
		mount *csi.VolumeCapability_MountVolume
	}{
		{name: "fsType", mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		{name: "mountFlags", mount: &csi.VolumeCapability_MountVolume{MountFlags: []string{"noexec"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateVolumeCapabilities([]*csi.VolumeCapability{{
				AccessType: &csi.VolumeCapability_Mount{Mount: tt.mount},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			}})
			if err == nil {
				t.Fatal("expected unsupported mount option to be rejected")
			}
		})
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

func TestCreateVolumeRejectsUnsupportedRequestFields(t *testing.T) {
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}}
	tests := []struct {
		name   string
		mutate func(*csi.CreateVolumeRequest)
	}{
		{
			name: "content source",
			mutate: func(req *csi.CreateVolumeRequest) {
				req.VolumeContentSource = &csi.VolumeContentSource{
					Type: &csi.VolumeContentSource_Volume{
						Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: "source-volume"},
					},
				}
			},
		},
		{
			name: "negative capacity",
			mutate: func(req *csi.CreateVolumeRequest) {
				req.CapacityRange = &csi.CapacityRange{RequiredBytes: -1}
			},
		},
		{
			name: "required exceeds limit",
			mutate: func(req *csi.CreateVolumeRequest) {
				req.CapacityRange = &csi.CapacityRange{RequiredBytes: 2, LimitBytes: 1}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &csi.CreateVolumeRequest{
				Name:               "pvc-demo",
				VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
			}
			tt.mutate(req)
			_, err := d.CreateVolume(context.Background(), req)
			if status.Code(err).String() != "InvalidArgument" {
				t.Fatalf("CreateVolume status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
			}
		})
	}
}

func TestNodeStageVolumeRejectsUnsupportedCapabilities(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir(), DriverName: "csi.drive9.ai"}}
	tests := []struct {
		name string
		cap  *csi.VolumeCapability
	}{
		{name: "nil", cap: nil},
		{name: "block", cap: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		}},
		{name: "rwx", cap: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
				VolumeId:          "vol-" + tt.name,
				StagingTargetPath: t.TempDir(),
				VolumeContext:     map[string]string{"remoteRoot": "/k8s/pvc/demo"},
				Secrets:           map[string]string{"server": "http://127.0.0.1", "apiKey": "test-key"},
				VolumeCapability:  tt.cap,
			})
			if status.Code(err).String() != "InvalidArgument" {
				t.Fatalf("NodeStageVolume status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
			}
		})
	}
}

func TestNodeStageVolumeRejectsRemoteRootOutsideAllowedRoot(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir(), DriverName: "csi.drive9.ai"}}
	_, err := d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-outside",
		StagingTargetPath: t.TempDir(),
		VolumeContext:     map[string]string{"remoteRoot": "/outside/pvc"},
		Secrets:           map[string]string{"server": "http://127.0.0.1", "apiKey": "test-key"},
		VolumeCapability:  singleNodeMountCapability(),
	})
	if status.Code(err).String() != "InvalidArgument" {
		t.Fatalf("NodeStageVolume status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

func TestNodeStageVolumeRejectsMismatchedVolumeID(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir(), DriverName: "csi.drive9.ai"}}
	_, err := d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "drive9-00000000000000000000000000000000",
		StagingTargetPath: t.TempDir(),
		VolumeContext:     map[string]string{"remoteRoot": "/k8s/pvc/demo"},
		Secrets:           map[string]string{"server": "http://127.0.0.1", "apiKey": "test-key"},
		VolumeCapability:  singleNodeMountCapability(),
	})
	if status.Code(err).String() != "FailedPrecondition" {
		t.Fatalf("NodeStageVolume status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
}

func TestNodeStageVolumeRejectsWorkspaceRootWithoutVolumeName(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir(), DriverName: "csi.drive9.ai"}}
	_, err := d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeIDForWorkspaceRoot("pvc-root", "/"),
		StagingTargetPath: t.TempDir(),
		VolumeContext:     map[string]string{"remoteRoot": "/"},
		Secrets:           map[string]string{"server": "http://127.0.0.1", "apiKey": "test-key"},
		VolumeCapability:  singleNodeMountCapability(),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("NodeStageVolume status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

func TestNodeStageVolumeRejectsWorkspaceRootMismatchedVolumeID(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir(), DriverName: "csi.drive9.ai"}}
	_, err := d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeIDForWorkspaceRoot("pvc-root", "/"),
		StagingTargetPath: t.TempDir(),
		VolumeContext: map[string]string{
			"remoteRoot": "/",
			"volumeName": "other-pvc",
		},
		Secrets:          map[string]string{"server": "http://127.0.0.1", "apiKey": "test-key"},
		VolumeCapability: singleNodeMountCapability(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("NodeStageVolume status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
}

func TestNodeLocalPathsMustBeAbsolute(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir(), DriverName: "csi.drive9.ai"}}
	remoteRoot := "/k8s/pvc/demo"
	volumeID := volumeIDForRemoteRoot(remoteRoot)

	stageReq := &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: "relative-stage",
		VolumeContext:     map[string]string{"remoteRoot": remoteRoot},
		Secrets:           map[string]string{"server": "http://127.0.0.1", "apiKey": "test-key"},
		VolumeCapability:  singleNodeMountCapability(),
	}
	if _, err := d.NodeStageVolume(context.Background(), stageReq); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("NodeStageVolume relative target status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
	}

	if _, err := d.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: "relative-stage",
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("NodeUnstageVolume relative target status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
	}

	if _, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: t.TempDir(),
		TargetPath:        "relative-target",
		VolumeCapability:  singleNodeMountCapability(),
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("NodePublishVolume relative target status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
	}

	if _, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: "relative-target",
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("NodeUnpublishVolume relative target status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

func TestNodeStageVolumeRequiresMatchingRemoteMarker(t *testing.T) {
	fake := newFakeDrive9(t)
	defer fake.close()

	k8s := k8sfake.NewSimpleClientset(fake.k8sSecret("drive9-secret", "default"))
	d := &Driver{cfg: Config{StateDir: t.TempDir(), DriverName: "csi.drive9.ai"}, k8s: k8s}
	remoteRoot := "/k8s/pvc/demo"
	volumeID := volumeIDForRemoteRoot(remoteRoot)
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: t.TempDir(),
		VolumeContext: map[string]string{
			"remoteRoot":        remoteRoot,
			attrSecretName:      "drive9-secret",
			attrSecretNamespace: "default",
		},
		VolumeCapability: singleNodeMountCapability(),
	}

	_, err := d.NodeStageVolume(context.Background(), req)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("NodeStageVolume missing marker status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
	}

	fake.putJSON(markerPath(remoteRoot), volumeMarker{
		Version:    1,
		Driver:     "csi.drive9.ai",
		VolumeID:   "other-volume",
		RemoteRoot: remoteRoot,
	})
	_, err = d.NodeStageVolume(context.Background(), req)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("NodeStageVolume mismatched marker status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
}

func TestValidateRemoteVolumeMarkerAllowsMatchingMarker(t *testing.T) {
	fake := newFakeDrive9(t)
	defer fake.close()

	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}}
	remoteRoot := "/k8s/pvc/demo"
	volumeID := volumeIDForRemoteRoot(remoteRoot)
	fake.putJSON(markerPath(remoteRoot), volumeMarker{
		Version:    1,
		Driver:     "csi.drive9.ai",
		VolumeID:   volumeID,
		RemoteRoot: remoteRoot,
	})

	client := newDrive9Client(drive9Credentials{Server: fake.server.URL, APIKey: "test-key"})
	if err := d.validateRemoteVolumeMarker(context.Background(), client, volumeID, remoteRoot); err != nil {
		t.Fatalf("validateRemoteVolumeMarker error = %v", err)
	}
}

func TestNodeUnstageVolumeIgnoresMismatchedStateWhenTargetAlreadyUnmounted(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir(), DriverName: "csi.drive9.ai"}}
	volumeID := "vol"
	if err := d.writeMountState(mountState{
		VolumeID:      volumeID,
		RemoteRoot:    "/k8s/pvc/demo",
		StagingTarget: filepath.Join(t.TempDir(), "other-stage"),
	}); err != nil {
		t.Fatalf("writeMountState error = %v", err)
	}
	_, err := d.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: filepath.Join(t.TempDir(), "requested-stage"),
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume error = %v", err)
	}
	if _, statErr := os.Stat(d.mountStatePath(volumeID)); statErr != nil {
		t.Fatalf("mismatched stage state should remain for the matching target cleanup, stat err = %v", statErr)
	}
}

func TestNodeUnpublishVolumeRequiresVolumeID(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir(), DriverName: "csi.drive9.ai"}}
	_, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		TargetPath: t.TempDir(),
	})
	if status.Code(err).String() != "InvalidArgument" {
		t.Fatalf("NodeUnpublishVolume status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

func TestNodeUnpublishVolumeMissingStateIsIdempotentWhenTargetAlreadyUnmounted(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir(), DriverName: "csi.drive9.ai"}}
	_, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "vol",
		TargetPath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NodeUnpublishVolume error = %v", err)
	}
}

func TestNodeUnpublishVolumeIgnoresMismatchedStateWhenTargetAlreadyUnmounted(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir(), DriverName: "csi.drive9.ai"}}
	target := t.TempDir()
	if err := d.writePublishState(publishState{
		VolumeID:      "other-volume",
		StagingTarget: "/stage",
		Target:        target,
	}); err != nil {
		t.Fatalf("writePublishState error = %v", err)
	}
	_, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "requested-volume",
		TargetPath: target,
	})
	if err != nil {
		t.Fatalf("NodeUnpublishVolume error = %v", err)
	}
	if _, statErr := os.Stat(d.publishStatePath(target)); statErr != nil {
		t.Fatalf("mismatched publish state should remain for the matching volume cleanup, stat err = %v", statErr)
	}
}

func TestNodeUnpublishVolumeRemovesMatchingStateWhenTargetAlreadyUnmounted(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir(), DriverName: "csi.drive9.ai"}}
	target := t.TempDir()
	if err := d.writePublishState(publishState{
		VolumeID:      "vol",
		StagingTarget: "/stage",
		Target:        target,
	}); err != nil {
		t.Fatalf("writePublishState error = %v", err)
	}
	_, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "vol",
		TargetPath: target,
	})
	if err != nil {
		t.Fatalf("NodeUnpublishVolume error = %v", err)
	}
	if _, statErr := os.Stat(d.publishStatePath(target)); !os.IsNotExist(statErr) {
		t.Fatalf("publish state should be removed, stat err = %v", statErr)
	}
}

func TestStageAndPublishStateStatus(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir(), DriverName: "csi.drive9.ai"}}
	stageTarget := filepath.Join(t.TempDir(), "stage")
	publishTarget := filepath.Join(t.TempDir(), "publish")

	status, err := d.stageStateStatus("vol", stageTarget)
	if err != nil {
		t.Fatalf("stageStateStatus missing error = %v", err)
	}
	if status != stateMissing {
		t.Fatalf("stageStateStatus missing = %v, want %v", status, stateMissing)
	}
	status, err = d.publishStateStatus("vol", publishTarget)
	if err != nil {
		t.Fatalf("publishStateStatus missing error = %v", err)
	}
	if status != stateMissing {
		t.Fatalf("publishStateStatus missing = %v, want %v", status, stateMissing)
	}

	if err := d.writeMountState(mountState{VolumeID: "vol", StagingTarget: stageTarget}); err != nil {
		t.Fatalf("writeMountState error = %v", err)
	}
	if err := d.writePublishState(publishState{VolumeID: "vol", Target: publishTarget}); err != nil {
		t.Fatalf("writePublishState error = %v", err)
	}
	status, err = d.stageStateStatus("vol", stageTarget)
	if err != nil {
		t.Fatalf("stageStateStatus matching error = %v", err)
	}
	if status != stateMatching {
		t.Fatalf("stageStateStatus matching = %v, want %v", status, stateMatching)
	}
	status, err = d.publishStateStatus("vol", publishTarget)
	if err != nil {
		t.Fatalf("publishStateStatus matching error = %v", err)
	}
	if status != stateMatching {
		t.Fatalf("publishStateStatus matching = %v, want %v", status, stateMatching)
	}

	status, err = d.stageStateStatus("vol", filepath.Join(t.TempDir(), "other-stage"))
	if err != nil {
		t.Fatalf("stageStateStatus mismatched error = %v", err)
	}
	if status != stateMismatched {
		t.Fatalf("stageStateStatus mismatched = %v, want %v", status, stateMismatched)
	}
	status, err = d.publishStateStatus("other-vol", publishTarget)
	if err != nil {
		t.Fatalf("publishStateStatus mismatched error = %v", err)
	}
	if status != stateMismatched {
		t.Fatalf("publishStateStatus mismatched = %v, want %v", status, stateMismatched)
	}
}

func TestListenCSIEndpointRefusesToRemoveNonSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "csi.sock")
	if err := os.WriteFile(socketPath, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	listener, cleanup, err := listenCSIEndpoint("unix://" + socketPath)
	if err == nil {
		if cleanup != nil {
			cleanup()
		}
		if listener != nil {
			_ = listener.Close()
		}
		t.Fatal("expected listenCSIEndpoint to reject existing non-socket path")
	}
	body, readErr := os.ReadFile(socketPath)
	if readErr != nil {
		t.Fatalf("existing file was removed or unreadable: %v", readErr)
	}
	if string(body) != "keep" {
		t.Fatalf("existing file body = %q, want keep", string(body))
	}
}

func TestCredentialsFromSecretsValidatesServerURL(t *testing.T) {
	for _, server := range []string{"drive9.example.com", "ftp://drive9.example.com", "https://", "https://drive9.example.com?token=x"} {
		t.Run(server, func(t *testing.T) {
			_, err := credentialsFromSecrets(map[string]string{
				"server": server,
				"apiKey": "test-key",
			})
			if status.Code(err).String() != "InvalidArgument" {
				t.Fatalf("credentialsFromSecrets status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
			}
		})
	}

	creds, err := credentialsFromSecrets(map[string]string{
		"server": "https://drive9.example.com/api/",
		"apiKey": " test-key ",
	})
	if err != nil {
		t.Fatalf("credentialsFromSecrets valid URL error = %v", err)
	}
	if creds.Server != "https://drive9.example.com/api" || creds.APIKey != "test-key" {
		t.Fatalf("credentials = %+v", creds)
	}
}

func TestGRPCHTTPErrorMapsClientAndRetryableStatuses(t *testing.T) {
	tests := []struct {
		statusCode int
		want       codes.Code
	}{
		{statusCode: http.StatusBadRequest, want: codes.InvalidArgument},
		{statusCode: http.StatusTooManyRequests, want: codes.Unavailable},
		{statusCode: http.StatusBadGateway, want: codes.Unavailable},
		{statusCode: http.StatusServiceUnavailable, want: codes.Unavailable},
		{statusCode: http.StatusInternalServerError, want: codes.Unavailable},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.statusCode), func(t *testing.T) {
			resp := &http.Response{
				StatusCode: tt.statusCode,
				Status:     http.StatusText(tt.statusCode),
				Body:       io.NopCloser(strings.NewReader("drive9 error")),
			}
			err := grpcHTTPError(resp, "test op")
			if status.Code(err) != tt.want {
				t.Fatalf("grpcHTTPError status = %s, want %s (err=%v)", status.Code(err), tt.want, err)
			}
		})
	}
}

func TestCreateVolumeDefaultsToWorkspaceRootWithoutMarkers(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-root", "default", "drive9-secret"),
		fd.k8sPVC("pvc-other", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-root",
		Parameters:         pvcParams("pvc-root", "default", nil),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	volumeID := createResp.GetVolume().GetVolumeId()
	ctx := createResp.GetVolume().GetVolumeContext()
	if !isWorkspaceRootVolumeID(volumeID) {
		t.Fatalf("CreateVolume volumeID = %q, want workspace root volume id", volumeID)
	}
	if ctx["remoteRoot"] != "/" {
		t.Fatalf("CreateVolume remoteRoot = %q, want workspace root", ctx["remoteRoot"])
	}
	if ctx["volumeName"] != "pvc-root" {
		t.Fatalf("CreateVolume volumeName = %q, want pvc-root", ctx["volumeName"])
	}
	if ctx["drive9VolumeMode"] != "workspace-root" {
		t.Fatalf("CreateVolume drive9VolumeMode = %q, want workspace-root", ctx["drive9VolumeMode"])
	}
	// Verify secret ref is fixated in volume attributes.
	if ctx[attrSecretName] != "drive9-secret" {
		t.Fatalf("CreateVolume secretName = %q, want drive9-secret", ctx[attrSecretName])
	}
	if ctx[attrSecretNamespace] != "default" {
		t.Fatalf("CreateVolume secretNamespace = %q, want default", ctx[attrSecretNamespace])
	}
	assertVolumeContextMountTTLs(t, ctx, mountTTLs{AttrTTL: "30s", EntryTTL: "30s", DirTTL: "30s"})
	assertVolumeContextMountPerf(t, ctx, false)
	assertVolumeContextMountTuningAbsent(t, ctx)
	for _, remotePath := range []string{markerPath("/"), indexPath(volumeID), nameIndexPath("pvc-root")} {
		if fd.exists(remotePath) {
			t.Fatalf("workspace root mode should not write CSI metadata path %s", remotePath)
		}
	}

	second, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-root",
		Parameters:         pvcParams("pvc-root", "default", nil),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("idempotent CreateVolume error = %v", err)
	}
	if second.GetVolume().GetVolumeId() != volumeID {
		t.Fatalf("idempotent CreateVolume volumeID = %q, want %q", second.GetVolume().GetVolumeId(), volumeID)
	}

	other, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-other",
		Parameters:         pvcParams("pvc-other", "default", nil),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("second PVC CreateVolume error = %v", err)
	}
	if other.GetVolume().GetVolumeId() == volumeID {
		t.Fatalf("different PVCs must not share a volumeID: %q", volumeID)
	}

	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	}); err != nil {
		t.Fatalf("DeleteVolume root mode error = %v", err)
	}
	if !fd.exists("/") {
		t.Fatal("DeleteVolume root mode must not delete the Drive9 workspace root")
	}
}

func TestCreateVolumeStoresConfiguredMountTTLs(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-ttl", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-ttl",
		Parameters: pvcParams("pvc-ttl", "default", map[string]string{
			paramAttrTTL:  "1000ms",
			paramEntryTTL: "1m",
			paramDirTTL:   "2m30s",
		}),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	assertVolumeContextMountTTLs(t, createResp.GetVolume().GetVolumeContext(), mountTTLs{
		AttrTTL:  "1s",
		EntryTTL: "1m0s",
		DirTTL:   "2m30s",
	})
}

func TestCreateVolumeStoresConfiguredMountPerf(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-perf", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-perf",
		Parameters: pvcParams("pvc-perf", "default", map[string]string{
			paramPerfEnabled: "true",
		}),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	assertVolumeContextMountPerf(t, createResp.GetVolume().GetVolumeContext(), true)
}

func TestCreateVolumeStoresConfiguredMountTuning(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-tuning", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-tuning",
		Parameters: pvcParams("pvc-tuning", "default", map[string]string{
			paramReaddirPrefetch:             "true",
			paramReaddirPrefetchMaxFiles:     "64",
			paramReaddirPrefetchMaxFileBytes: "50000",
			paramReaddirPrefetchMaxBytes:     "4194304",
			paramWritebackBatchWindow:        "20ms",
		}),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	assertVolumeContextMountTuning(t, createResp.GetVolume().GetVolumeContext(), mountTuning{
		ReaddirPrefetchGiven:        true,
		ReaddirPrefetch:             true,
		ReaddirPrefetchMaxFiles:     "64",
		ReaddirPrefetchMaxFileBytes: "50000",
		ReaddirPrefetchMaxBytes:     "4194304",
		WritebackBatchWindow:        "20ms",
	})
}

func TestCreateVolumeStoresMutableMountParameters(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-vac", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-vac",
		Parameters: pvcParams("pvc-vac", "default", nil),
		MutableParameters: map[string]string{
			paramProfile:                     " coding-agent ",
			paramAttrTTL:                     "1000ms",
			paramEntryTTL:                    "1m",
			paramDirTTL:                      "2m30s",
			paramPerfEnabled:                 "true",
			paramReaddirPrefetch:             "true",
			paramReaddirPrefetchMaxFiles:     "64",
			paramReaddirPrefetchMaxFileBytes: "50000",
			paramReaddirPrefetchMaxBytes:     "4194304",
			paramWritebackBatchWindow:        "20ms",
		},
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	ctx := createResp.GetVolume().GetVolumeContext()
	if got := ctx[paramProfile]; got != "coding-agent" {
		t.Fatalf("volume context %s = %q, want coding-agent", paramProfile, got)
	}
	assertVolumeContextMountTTLs(t, ctx, mountTTLs{
		AttrTTL:  "1s",
		EntryTTL: "1m0s",
		DirTTL:   "2m30s",
	})
	assertVolumeContextMountPerf(t, ctx, true)
	assertVolumeContextMountTuning(t, ctx, mountTuning{
		ReaddirPrefetchGiven:        true,
		ReaddirPrefetch:             true,
		ReaddirPrefetchMaxFiles:     "64",
		ReaddirPrefetchMaxFileBytes: "50000",
		ReaddirPrefetchMaxBytes:     "4194304",
		WritebackBatchWindow:        "20ms",
	})
}

func TestCreateVolumeMutableParametersOverrideStorageClassParameters(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-vac-override", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-vac-override",
		Parameters: pvcParams("pvc-vac-override", "default", map[string]string{
			paramProfile:     "legacy-profile",
			paramAttrTTL:     "30s",
			paramPerfEnabled: "false",
		}),
		MutableParameters: map[string]string{
			paramProfile:     "coding-agent",
			paramAttrTTL:     "5s",
			paramPerfEnabled: "true",
		},
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	ctx := createResp.GetVolume().GetVolumeContext()
	if got := ctx[paramProfile]; got != "coding-agent" {
		t.Fatalf("volume context %s = %q, want coding-agent", paramProfile, got)
	}
	assertVolumeContextMountTTLs(t, ctx, mountTTLs{
		AttrTTL:  "5s",
		EntryTTL: "30s",
		DirTTL:   "30s",
	})
	assertVolumeContextMountPerf(t, ctx, true)
}

func TestCreateVolumeRejectsUnsupportedMutableParameters(t *testing.T) {
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}}

	tests := []map[string]string{
		{"remoteRootPrefix": "/k8s/pvc"},
		{"remoteRoot": "/team/workspace"},
		{"csi.storage.k8s.io/pvc/name": "pvc-demo"},
		{"apiKey": "secret"},
		{"server": "https://api.drive9.ai"},
		{attrSecretName: "drive9-secret"},
		{attrSecretNamespace: "default"},
	}
	for _, mutable := range tests {
		_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
			Name:               "pvc-vac-invalid",
			MutableParameters:  mutable,
			VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("CreateVolume mutable=%v status = %s, want InvalidArgument (err=%v)", mutable, status.Code(err), err)
		}
	}
}

func TestCreateVolumeUsesStorageClassRemoteRootPrefixWithMutableMountParameters(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-vac-managed", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-vac-managed",
		Parameters: pvcParams("pvc-vac-managed", "default", map[string]string{
			"remoteRootPrefix": "/k8s/pvc",
		}),
		MutableParameters: map[string]string{
			paramProfile: "coding-agent",
		},
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	ctx := createResp.GetVolume().GetVolumeContext()
	if got := ctx[paramProfile]; got != "coding-agent" {
		t.Fatalf("volume context %s = %q, want coding-agent", paramProfile, got)
	}
	if got := ctx["remoteRoot"]; !strings.HasPrefix(got, "/k8s/pvc/pvc-vac-managed-") {
		t.Fatalf("remoteRoot = %q, want generated managed path under /k8s/pvc", got)
	}
}

func TestCreateVolumeRejectsInvalidMountTTLs(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-ttl", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	tests := []map[string]string{
		{paramAttrTTL: ""},
		{paramEntryTTL: "abc"},
		{paramDirTTL: "0s"},
		{paramAttrTTL: "-1s"},
	}
	for _, extra := range tests {
		_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
			Name:               "pvc-ttl",
			Parameters:         pvcParams("pvc-ttl", "default", extra),
			VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("CreateVolume(%v) status = %s, want InvalidArgument (err=%v)", extra, status.Code(err), err)
		}
	}
}

func TestCreateVolumeRejectsInvalidMountPerf(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-perf", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	for _, value := range []string{"", "yes", "TRUE"} {
		_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
			Name: "pvc-perf",
			Parameters: pvcParams("pvc-perf", "default", map[string]string{
				paramPerfEnabled: value,
			}),
			VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("CreateVolume(%q) status = %s, want InvalidArgument (err=%v)", value, status.Code(err), err)
		}
	}
}

func TestCreateVolumeRejectsInvalidMountTuning(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-tuning", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	tests := []map[string]string{
		{paramReaddirPrefetch: "yes"},
		{paramReaddirPrefetchMaxFiles: "0"},
		{paramReaddirPrefetchMaxFileBytes: "-1"},
		{paramReaddirPrefetchMaxBytes: "no"},
		{paramWritebackBatchWindow: "0s"},
	}
	for _, extra := range tests {
		_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
			Name:               "pvc-tuning",
			Parameters:         pvcParams("pvc-tuning", "default", extra),
			VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("CreateVolume(%v) status = %s, want InvalidArgument (err=%v)", extra, status.Code(err), err)
		}
	}
}

func TestCreateVolumeUsesRemoteRootFromAnnotation(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()
	fd.mkdir("/team/workspace")

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVCWithRemoteRoot("pvc-team", "default", "drive9-secret", "/team/workspace"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-team",
		Parameters:         pvcParams("pvc-team", "default", nil),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	if got := createResp.GetVolume().GetVolumeContext()["remoteRoot"]; got != "/team/workspace" {
		t.Fatalf("CreateVolume remoteRoot = %q, want /team/workspace", got)
	}
}

func TestCreateVolumeRejectsMissingWorkspaceRoot(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVCWithRemoteRoot("pvc-missing", "default", "drive9-secret", "/missing/workspace"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-missing",
		Parameters:         pvcParams("pvc-missing", "default", nil),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CreateVolume status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
}

func TestCreateVolumeRejectsRemoteRootAndPrefixTogether(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVCWithRemoteRoot("pvc-conflict", "default", "drive9-secret", "/custom/root"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-conflict",
		Parameters:         pvcParams("pvc-conflict", "default", map[string]string{"remoteRootPrefix": "/k8s/pvc"}),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateVolume status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

func TestCreateDeleteManagedDirectoryVolumeWritesIndexAndMarker(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-demo", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-demo",
		Parameters:         pvcParams("pvc-demo", "default", map[string]string{"remoteRootPrefix": "/k8s/pvc"}),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	volumeID := createResp.GetVolume().GetVolumeId()
	remoteRoot := createResp.GetVolume().GetVolumeContext()["remoteRoot"]
	if volumeID == "" || remoteRoot == "" {
		t.Fatalf("CreateVolume returned volumeID=%q remoteRoot=%q", volumeID, remoteRoot)
	}
	if !fd.exists(markerPath(remoteRoot)) {
		t.Fatalf("missing root marker %s", markerPath(remoteRoot))
	}
	if !fd.exists(indexPath(volumeID)) {
		t.Fatalf("missing index marker %s", indexPath(volumeID))
	}
	if !fd.exists(nameIndexPath("pvc-demo")) {
		t.Fatalf("missing name index marker %s", nameIndexPath("pvc-demo"))
	}
	assertVolumeContextMountPerf(t, createResp.GetVolume().GetVolumeContext(), false)
	assertVolumeContextMountTuningAbsent(t, createResp.GetVolume().GetVolumeContext())

	// Write a user file into the managed directory to verify data is preserved.
	fd.putFile(remoteRoot+"/user-data.txt", []byte("keep me"))

	// Simulate external-provisioner creating PV with volumeAttributes.
	createPVForVolume(t, k8s, createResp, "csi.drive9.ai")

	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	}); err != nil {
		t.Fatalf("DeleteVolume error = %v", err)
	}
	// DeleteVolume detaches CSI ownership but preserves Drive9 workspace data.
	if !fd.exists(remoteRoot) {
		t.Fatalf("remote root must be preserved after delete: %s", remoteRoot)
	}
	if !fd.existsFile(remoteRoot + "/user-data.txt") {
		t.Fatal("user data must be preserved after delete")
	}
	if fd.exists(markerPath(remoteRoot)) {
		t.Fatalf("root marker must be removed after delete: %s", markerPath(remoteRoot))
	}
	if fd.exists(indexPath(volumeID)) {
		t.Fatalf("index must be removed after delete: %s", indexPath(volumeID))
	}
	if fd.exists(nameIndexPath("pvc-demo")) {
		t.Fatalf("name index must be removed after delete: %s", nameIndexPath("pvc-demo"))
	}
}

func TestCreateVolumeIsIdempotentByName(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-same-name", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	first, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-same-name",
		Parameters:         pvcParams("pvc-same-name", "default", map[string]string{"remoteRootPrefix": "/k8s/pvc"}),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("first CreateVolume error = %v", err)
	}
	second, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-same-name",
		Parameters:         pvcParams("pvc-same-name", "default", map[string]string{"remoteRootPrefix": "/k8s/pvc"}),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("idempotent CreateVolume error = %v", err)
	}
	if second.GetVolume().GetVolumeId() != first.GetVolume().GetVolumeId() {
		t.Fatalf("idempotent CreateVolume volumeID = %q, want %q", second.GetVolume().GetVolumeId(), first.GetVolume().GetVolumeId())
	}

	conflictingRoot, err := buildRemoteRoot("/k8s/other", "pvc-same-name")
	if err != nil {
		t.Fatalf("build conflicting remote root: %v", err)
	}
	_, err = d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-same-name",
		Parameters:         pvcParams("pvc-same-name", "default", map[string]string{"remoteRootPrefix": "/k8s/other"}),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("conflicting CreateVolume status = %s, want AlreadyExists (err=%v)", status.Code(err), err)
	}
	if fd.exists(conflictingRoot) {
		t.Fatalf("conflicting CreateVolume created remote root: %s", conflictingRoot)
	}
}

func TestCreateVolumeRecoversFromNameIndexOnly(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	name := "pvc-partial"
	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC(name, "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	remoteRoot, err := buildRemoteRoot("/k8s/pvc", name)
	if err != nil {
		t.Fatalf("build remote root: %v", err)
	}
	volumeID := volumeIDForRemoteRoot(remoteRoot)
	marker := volumeMarker{
		Version:    1,
		Driver:     "csi.drive9.ai",
		VolumeID:   volumeID,
		Name:       name,
		RemoteRoot: remoteRoot,
	}
	fd.putJSON(nameIndexPath(name), marker)

	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               name,
		Parameters:         pvcParams(name, "default", map[string]string{"remoteRootPrefix": "/k8s/pvc"}),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume recovery error = %v", err)
	}
	if resp.GetVolume().GetVolumeId() != volumeID {
		t.Fatalf("recovered volumeID = %q, want %q", resp.GetVolume().GetVolumeId(), volumeID)
	}
	for _, remotePath := range []string{remoteRoot, markerPath(remoteRoot), indexPath(volumeID), nameIndexPath(name)} {
		if !fd.exists(remotePath) {
			t.Fatalf("CreateVolume recovery did not create %s", remotePath)
		}
	}
}

func TestCreateVolumeDefaultRemoteRootIsWorkspaceRoot(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-default", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-default",
		Parameters:         pvcParams("pvc-default", "default", nil),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume with default remote root error = %v", err)
	}
	remoteRoot := createResp.GetVolume().GetVolumeContext()["remoteRoot"]
	if remoteRoot != defaultRemoteRoot {
		t.Fatalf("default remote root = %q, want %q", remoteRoot, defaultRemoteRoot)
	}
}

func TestCreateVolumeCreatesRemoteParents(t *testing.T) {
	fd := newFakeDrive9(t)
	fd.requireMkdirParents()
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-parent-demo", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-parent-demo",
		Parameters:         pvcParams("pvc-parent-demo", "default", map[string]string{"remoteRootPrefix": "/k8s/pvc"}),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	remoteRoot := createResp.GetVolume().GetVolumeContext()["remoteRoot"]
	for _, remotePath := range []string{"/k8s", "/k8s/pvc", "/k8s/.drive9-csi", metadataRoot, nameIndexRoot, remoteRoot} {
		if !fd.exists(remotePath) {
			t.Fatalf("expected parent path to exist: %s", remotePath)
		}
	}
}

func TestDeleteVolumeRejectsTamperedRootIndex(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	volumeID := volumeIDForRemoteRoot("/")
	k8s := k8sfake.NewSimpleClientset(
		fd.k8sSecret("drive9-secret", "default"),
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pv-tampered"},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:       "csi.drive9.ai",
						VolumeHandle: volumeID,
						VolumeAttributes: map[string]string{
							attrSecretName:      "drive9-secret",
							attrSecretNamespace: "default",
						},
					},
				},
			},
		},
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}
	fd.putJSON(indexPath(volumeID), volumeMarker{
		Version:    1,
		Driver:     "csi.drive9.ai",
		VolumeID:   volumeID,
		Name:       "tampered",
		RemoteRoot: "/",
	})
	_, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	if err == nil {
		t.Fatal("expected DeleteVolume to reject tampered root index")
	}
	if status.Code(err).String() != "FailedPrecondition" {
		t.Fatalf("DeleteVolume status = %s, want FailedPrecondition", status.Code(err))
	}
}

func TestDeleteVolumeRejectsTamperedRootMarkerName(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-delete-safe", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-delete-safe",
		Parameters:         pvcParams("pvc-delete-safe", "default", map[string]string{"remoteRootPrefix": "/k8s/pvc"}),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	volumeID := createResp.GetVolume().GetVolumeId()
	remoteRoot := createResp.GetVolume().GetVolumeContext()["remoteRoot"]
	fd.putJSON(markerPath(remoteRoot), volumeMarker{
		Version:    1,
		Driver:     "csi.drive9.ai",
		VolumeID:   volumeID,
		Name:       "other-name",
		RemoteRoot: remoteRoot,
	})

	// Simulate external-provisioner creating PV with volumeAttributes.
	createPVForVolume(t, k8s, createResp, "csi.drive9.ai")

	_, err = d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("DeleteVolume status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if !fd.exists(remoteRoot) {
		t.Fatalf("DeleteVolume removed remote root despite tampered marker: %s", remoteRoot)
	}
}

func TestDeleteVolumePreservesDataAndAllowsRecreate(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-lifecycle", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-lifecycle",
		Parameters:         pvcParams("pvc-lifecycle", "default", map[string]string{"remoteRootPrefix": "/k8s/pvc"}),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	volumeID := createResp.GetVolume().GetVolumeId()
	remoteRoot := createResp.GetVolume().GetVolumeContext()["remoteRoot"]

	// Simulate user writing data.
	fd.putFile(remoteRoot+"/important.txt", []byte("do not delete"))

	// Simulate external-provisioner creating PV with volumeAttributes.
	createPVForVolume(t, k8s, createResp, "csi.drive9.ai")

	// Delete the volume — should only remove CSI metadata.
	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	}); err != nil {
		t.Fatalf("DeleteVolume error = %v", err)
	}
	if !fd.existsFile(remoteRoot + "/important.txt") {
		t.Fatal("user data must survive DeleteVolume")
	}
	if fd.exists(indexPath(volumeID)) {
		t.Fatal("index must be removed after DeleteVolume")
	}

	// Recreate the same-name PVC — should succeed and restore CSI ownership.
	recreateResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-lifecycle",
		Parameters:         pvcParams("pvc-lifecycle", "default", map[string]string{"remoteRootPrefix": "/k8s/pvc"}),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("recreate CreateVolume error = %v", err)
	}
	if recreateResp.GetVolume().GetVolumeId() != volumeID {
		t.Fatalf("recreated volumeID = %q, want %q", recreateResp.GetVolume().GetVolumeId(), volumeID)
	}
	if !fd.exists(markerPath(remoteRoot)) {
		t.Fatal("marker must be restored after recreate")
	}
	if !fd.existsFile(remoteRoot + "/important.txt") {
		t.Fatal("user data must still exist after recreate")
	}
}

func TestDeleteVolumeRetryAfterPartialFailure(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-partial", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-partial",
		Parameters:         pvcParams("pvc-partial", "default", map[string]string{"remoteRootPrefix": "/k8s/pvc"}),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	volumeID := createResp.GetVolume().GetVolumeId()
	remoteRoot := createResp.GetVolume().GetVolumeContext()["remoteRoot"]

	// Simulate external-provisioner creating PV with volumeAttributes.
	createPVForVolume(t, k8s, createResp, "csi.drive9.ai")

	// Simulate partial previous delete: marker removed, index/name-index still present.
	fd.removeFile(markerPath(remoteRoot))
	if fd.exists(markerPath(remoteRoot)) {
		t.Fatal("precondition: marker should be gone")
	}
	if !fd.exists(indexPath(volumeID)) {
		t.Fatal("precondition: index should still exist")
	}
	if !fd.exists(nameIndexPath("pvc-partial")) {
		t.Fatal("precondition: name-index should still exist")
	}
	if !fd.exists(remoteRoot) {
		t.Fatal("precondition: remoteRoot should still exist")
	}

	// Retry DeleteVolume — must succeed, not get stuck.
	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	}); err != nil {
		t.Fatalf("retry DeleteVolume error = %v (stuck on missing marker)", err)
	}
	if fd.exists(indexPath(volumeID)) {
		t.Fatal("index must be removed after retry")
	}
	if fd.exists(nameIndexPath("pvc-partial")) {
		t.Fatal("name-index must be removed after retry")
	}
	if !fd.exists(remoteRoot) {
		t.Fatal("remoteRoot must be preserved")
	}
}

func TestDeleteVolumeTransientNameIndexFailureRetry(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-transient", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-transient",
		Parameters:         pvcParams("pvc-transient", "default", map[string]string{"remoteRootPrefix": "/k8s/pvc"}),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	volumeID := createResp.GetVolume().GetVolumeId()
	remoteRoot := createResp.GetVolume().GetVolumeContext()["remoteRoot"]

	fd.putFile(remoteRoot+"/data.txt", []byte("keep"))

	// Simulate external-provisioner creating PV with volumeAttributes.
	createPVForVolume(t, k8s, createResp, "csi.drive9.ai")

	// Inject a one-shot transient failure on name-index DELETE.
	fd.failDeleteOnce(nameIndexPath("pvc-transient"), 1)

	// First DeleteVolume attempt — should fail on name-index transient error.
	_, err = d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	if err == nil {
		t.Fatal("expected DeleteVolume to fail on transient name-index error")
	}
	// With correct ordering (name-index first), index should still exist.
	if !fd.exists(indexPath(volumeID)) {
		t.Fatal("index must still exist after name-index transient failure — " +
			"if index is gone, deletion order is wrong (index deleted before name-index)")
	}

	// Retry — transient error cleared, should succeed and clean everything.
	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	}); err != nil {
		t.Fatalf("retry DeleteVolume error = %v", err)
	}
	if fd.exists(indexPath(volumeID)) {
		t.Fatal("index must be removed after retry")
	}
	if fd.exists(nameIndexPath("pvc-transient")) {
		t.Fatal("name-index must be removed after retry")
	}
	if !fd.existsFile(remoteRoot + "/data.txt") {
		t.Fatal("user data must be preserved")
	}
	if !fd.exists(remoteRoot) {
		t.Fatal("remoteRoot must be preserved")
	}
}

func TestStageAndPublishStateMatching(t *testing.T) {
	if !mountStateMatches(mountState{
		VolumeID:      "vol",
		RemoteRoot:    "/k8s/pvc/demo",
		StagingTarget: "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/demo/globalmount",
	}, "vol", "/k8s/pvc/demo", "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/demo/globalmount") {
		t.Fatal("expected matching mount state")
	}
	if mountStateMatches(mountState{
		VolumeID:      "other",
		RemoteRoot:    "/k8s/pvc/demo",
		StagingTarget: "/stage",
	}, "vol", "/k8s/pvc/demo", "/stage") {
		t.Fatal("expected volume mismatch to fail")
	}
	if !publishStateMatches(publishState{
		VolumeID:      "vol",
		StagingTarget: "/stage",
		Target:        "/target",
		Readonly:      true,
		AccessMode:    csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER.String(),
	}, "vol", "/stage", "/target", true, csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER.String()) {
		t.Fatal("expected matching publish state")
	}
	if publishStateMatches(publishState{
		VolumeID:      "vol",
		StagingTarget: "/stage",
		Target:        "/target",
		Readonly:      false,
		AccessMode:    csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER.String(),
	}, "vol", "/stage", "/target", true, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER.String()) {
		t.Fatal("expected readonly mismatch to fail")
	}
	if publishStateMatches(publishState{
		VolumeID:      "vol",
		StagingTarget: "/stage",
		Target:        "/target",
		Readonly:      true,
		AccessMode:    csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER.String(),
	}, "vol", "/stage", "/target", true, csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER.String()) {
		t.Fatal("expected access mode mismatch to fail")
	}
}

func TestPublishSubtreeTargetEnsuresAnchorThenBindsWorkspace(t *testing.T) {
	state := publishState{
		VolumeID:     "vol",
		Target:       "/target",
		Layout:       publishLayoutSubtree,
		WorkspaceDir: defaultWorkspaceDir,
		Readonly:     true,
	}
	var calls []string
	err := publishSubtreeTargetWithOps("/stage", state,
		func(target string) error {
			calls = append(calls, "ensure:"+target)
			return nil
		},
		func(source string, target string, readonly bool) error {
			calls = append(calls, "bind:"+source+":"+target)
			if source != "/stage" || target != "/target/workspace" || !readonly {
				t.Fatalf("bind args = (%q, %q, %v)", source, target, readonly)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("publishSubtreeTargetWithOps error = %v", err)
	}
	want := []string{"ensure:/target", "bind:/stage:/target/workspace"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestCleanupSubtreePublishTargetUnmountsChildBeforeAnchor(t *testing.T) {
	state := publishState{
		Target:       "/target",
		Layout:       publishLayoutSubtree,
		WorkspaceDir: defaultWorkspaceDir,
	}
	var unmounted []string
	err := cleanupPublishTargetWithOps(state,
		func(target string) (bool, error) {
			return true, nil
		},
		func(target string) error {
			unmounted = append(unmounted, target)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("cleanupPublishTargetWithOps error = %v", err)
	}
	want := []string{"/target/workspace", "/target"}
	if strings.Join(unmounted, "|") != strings.Join(want, "|") {
		t.Fatalf("unmounted = %v, want %v", unmounted, want)
	}
}

func singleNodeMountCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

type fakeDrive9 struct {
	t                  *testing.T
	server             *httptest.Server
	mu                 sync.Mutex
	dirs               map[string]bool
	files              map[string][]byte
	strictMkdirParents bool
	// deleteFailOnce maps normalized paths to remaining failure count.
	// When a DELETE hits a path in this map with count > 0, the fake
	// returns HTTP 500 and decrements the count.  Once count reaches 0
	// the entry is removed and subsequent DELETEs succeed normally.
	deleteFailOnce map[string]int
}

func newFakeDrive9(t *testing.T) *fakeDrive9 {
	t.Helper()
	f := &fakeDrive9{
		t:     t,
		dirs:  map[string]bool{"/": true},
		files: map[string][]byte{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeDrive9) requireMkdirParents() {
	f.strictMkdirParents = true
}

// failDeleteOnce causes the next n DELETE requests for remotePath to
// return HTTP 500, simulating a transient Drive9 error.
func (f *fakeDrive9) failDeleteOnce(remotePath string, n int) {
	remotePath = normalizeForTest(f.t, remotePath)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteFailOnce == nil {
		f.deleteFailOnce = map[string]int{}
	}
	f.deleteFailOnce[remotePath] = n
}

func (f *fakeDrive9) close() {
	f.server.Close()
}

func (f *fakeDrive9) secrets() map[string]string {
	return map[string]string{
		"server": f.server.URL,
		"apiKey": "test-key",
	}
}

// k8sSecret returns a Kubernetes Secret object containing Drive9 credentials.
func (f *fakeDrive9) k8sSecret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"server": []byte(f.server.URL),
			"apiKey": []byte("test-key"),
		},
	}
}

// k8sPVC returns a PVC with drive9.ai/secret-name annotation.
func (f *fakeDrive9) k8sPVC(pvcName, namespace, secretName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: namespace,
			Annotations: map[string]string{
				annotationSecretName: secretName,
			},
		},
	}
}

// k8sPVCWithRemoteRoot returns a PVC with both secret-name and remote-root annotations.
func (f *fakeDrive9) k8sPVCWithRemoteRoot(pvcName, namespace, secretName, remoteRoot string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: namespace,
			Annotations: map[string]string{
				annotationSecretName: secretName,
				annotationRemoteRoot: remoteRoot,
			},
		},
	}
}

// createPVForVolume simulates what the external-provisioner does after
// CreateVolume: it creates a PV with CSI volumeAttributes. Tests call
// this after CreateVolume so resolveSecretRefFromPV can find the PV.
func createPVForVolume(t *testing.T, k8s *k8sfake.Clientset, resp *csi.CreateVolumeResponse, driverName string) {
	t.Helper()
	volCtx := resp.GetVolume().GetVolumeContext()
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pv-" + resp.GetVolume().GetVolumeId()[:20],
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:           driverName,
					VolumeHandle:     resp.GetVolume().GetVolumeId(),
					VolumeAttributes: volCtx,
				},
			},
		},
	}
	_, err := k8s.CoreV1().PersistentVolumes().Create(context.Background(), pv, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create PV for test: %v", err)
	}
}

// pvcParams returns CreateVolume parameters with PVC metadata for the given PVC.
func pvcParams(pvcName, namespace string, extra map[string]string) map[string]string {
	params := map[string]string{
		paramPVCName:      pvcName,
		paramPVCNamespace: namespace,
	}
	for k, v := range extra {
		params[k] = v
	}
	return params
}

func assertVolumeContextMountTTLs(t *testing.T, ctx map[string]string, want mountTTLs) {
	t.Helper()
	if got := ctx[paramAttrTTL]; got != want.AttrTTL {
		t.Fatalf("volume context %s = %q, want %q", paramAttrTTL, got, want.AttrTTL)
	}
	if got := ctx[paramEntryTTL]; got != want.EntryTTL {
		t.Fatalf("volume context %s = %q, want %q", paramEntryTTL, got, want.EntryTTL)
	}
	if got := ctx[paramDirTTL]; got != want.DirTTL {
		t.Fatalf("volume context %s = %q, want %q", paramDirTTL, got, want.DirTTL)
	}
}

func assertVolumeContextMountPerf(t *testing.T, ctx map[string]string, want bool) {
	t.Helper()
	wantValue := "false"
	if want {
		wantValue = "true"
	}
	if got := ctx[paramPerfEnabled]; got != wantValue {
		t.Fatalf("volume context %s = %q, want %q", paramPerfEnabled, got, wantValue)
	}
}

func assertVolumeContextMountTuning(t *testing.T, ctx map[string]string, want mountTuning) {
	t.Helper()
	if got := ctx[paramReaddirPrefetch]; got != boolString(want.ReaddirPrefetch) {
		t.Fatalf("volume context %s = %q, want %q", paramReaddirPrefetch, got, boolString(want.ReaddirPrefetch))
	}
	if got := ctx[paramReaddirPrefetchMaxFiles]; got != want.ReaddirPrefetchMaxFiles {
		t.Fatalf("volume context %s = %q, want %q", paramReaddirPrefetchMaxFiles, got, want.ReaddirPrefetchMaxFiles)
	}
	if got := ctx[paramReaddirPrefetchMaxFileBytes]; got != want.ReaddirPrefetchMaxFileBytes {
		t.Fatalf("volume context %s = %q, want %q", paramReaddirPrefetchMaxFileBytes, got, want.ReaddirPrefetchMaxFileBytes)
	}
	if got := ctx[paramReaddirPrefetchMaxBytes]; got != want.ReaddirPrefetchMaxBytes {
		t.Fatalf("volume context %s = %q, want %q", paramReaddirPrefetchMaxBytes, got, want.ReaddirPrefetchMaxBytes)
	}
	if got := ctx[paramWritebackBatchWindow]; got != want.WritebackBatchWindow {
		t.Fatalf("volume context %s = %q, want %q", paramWritebackBatchWindow, got, want.WritebackBatchWindow)
	}
}

func assertVolumeContextMountTuningAbsent(t *testing.T, ctx map[string]string) {
	t.Helper()
	for _, key := range []string{
		paramReaddirPrefetch,
		paramReaddirPrefetchMaxFiles,
		paramReaddirPrefetchMaxFileBytes,
		paramReaddirPrefetchMaxBytes,
		paramWritebackBatchWindow,
	} {
		if _, ok := ctx[key]; ok {
			t.Fatalf("volume context must not contain default mount tuning key %s", key)
		}
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func (f *fakeDrive9) exists(remotePath string) bool {
	remotePath = normalizeForTest(f.t, remotePath)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dirs[remotePath] || f.files[remotePath] != nil
}

func (f *fakeDrive9) mkdir(remotePath string) {
	remotePath = normalizeForTest(f.t, remotePath)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[remotePath] = true
}

func (f *fakeDrive9) putFile(remotePath string, data []byte) {
	remotePath = normalizeForTest(f.t, remotePath)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[remotePath] = data
}

func (f *fakeDrive9) removeFile(remotePath string) {
	remotePath = normalizeForTest(f.t, remotePath)
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, remotePath)
}

func (f *fakeDrive9) existsFile(remotePath string) bool {
	remotePath = normalizeForTest(f.t, remotePath)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[remotePath] != nil
}

func (f *fakeDrive9) putJSON(remotePath string, marker volumeMarker) {
	body, err := jsonMarshal(marker)
	if err != nil {
		f.t.Fatalf("marshal marker: %v", err)
	}
	remotePath = normalizeForTest(f.t, remotePath)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[remotePath] = body
}

func (f *fakeDrive9) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-key" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	remotePath := strings.TrimPrefix(r.URL.Path, "/v1/fs")
	if remotePath == "" {
		remotePath = "/"
	}
	var err error
	remotePath, err = url.PathUnescape(remotePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	remotePath = normalizeForTest(f.t, remotePath)

	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodHead:
		if f.dirs[remotePath] || f.files[remotePath] != nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	case r.Method == http.MethodPost && r.URL.Query().Has("mkdir"):
		if f.strictMkdirParents && remotePath != "/" && !f.dirs[path.Dir(remotePath)] {
			http.Error(w, "missing parent directory", http.StatusNotFound)
			return
		}
		f.dirs[remotePath] = true
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.files[remotePath] = body
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet:
		body, ok := f.files[remotePath]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	case r.Method == http.MethodDelete && r.URL.Query().Get("recursive") == "1":
		if n, ok := f.deleteFailOnce[remotePath]; ok {
			if n <= 1 {
				delete(f.deleteFailOnce, remotePath)
			} else {
				f.deleteFailOnce[remotePath] = n - 1
			}
			http.Error(w, "simulated transient error", http.StatusInternalServerError)
			return
		}
		for p := range f.files {
			if pathIsOrUnder(p, remotePath) {
				delete(f.files, p)
			}
		}
		for p := range f.dirs {
			if p != "/" && pathIsOrUnder(p, remotePath) {
				delete(f.dirs, p)
			}
		}
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "unsupported", http.StatusBadRequest)
	}
}

func normalizeForTest(t *testing.T, remotePath string) string {
	t.Helper()
	normalized, err := normalizeRemotePath(remotePath)
	if err != nil {
		t.Fatalf("normalize %q: %v", remotePath, err)
	}
	return normalized
}

func jsonMarshal(v any) ([]byte, error) {
	type marshaler interface {
		MarshalJSON() ([]byte, error)
	}
	if m, ok := v.(marshaler); ok {
		return m.MarshalJSON()
	}
	return json.Marshal(v)
}

// --- Multi-Pod Same PVC Tests ---

func TestControllerGetCapabilitiesIncludesMultiWriter(t *testing.T) {
	d := &Driver{}
	resp, err := d.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("ControllerGetCapabilities error = %v", err)
	}
	found := false
	for _, cap := range resp.GetCapabilities() {
		if cap.GetRpc().GetType() == csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("ControllerGetCapabilities must include SINGLE_NODE_MULTI_WRITER")
	}
}

func TestControllerGetCapabilitiesIncludesModifyVolume(t *testing.T) {
	d := &Driver{}
	resp, err := d.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("ControllerGetCapabilities error = %v", err)
	}
	found := false
	for _, cap := range resp.GetCapabilities() {
		if cap.GetRpc().GetType() == csi.ControllerServiceCapability_RPC_MODIFY_VOLUME {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("ControllerGetCapabilities must include MODIFY_VOLUME")
	}
}

func TestControllerModifyVolumeRejectsDynamicUpdates(t *testing.T) {
	d := &Driver{}
	_, err := d.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId: "drive9-root-demo",
		MutableParameters: map[string]string{
			paramAttrTTL: "5s",
		},
	})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("ControllerModifyVolume status = %s, want Unimplemented (err=%v)", status.Code(err), err)
	}
}

func TestControllerModifyVolumeRejectsUnsupportedMutableParameters(t *testing.T) {
	d := &Driver{}
	_, err := d.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId: "drive9-root-demo",
		MutableParameters: map[string]string{
			"remoteRootPrefix": "/k8s/pvc",
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ControllerModifyVolume status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

func TestNodeGetCapabilitiesIncludesMultiWriter(t *testing.T) {
	d := &Driver{}
	resp, err := d.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("NodeGetCapabilities error = %v", err)
	}
	found := false
	for _, cap := range resp.GetCapabilities() {
		if cap.GetRpc().GetType() == csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("NodeGetCapabilities must include SINGLE_NODE_MULTI_WRITER")
	}
}

func TestValidateVolumeCapabilitiesRPC(t *testing.T) {
	d := &Driver{}

	// Supported capability — should confirm.
	resp, err := d.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "vol-1",
		VolumeCapabilities: []*csi.VolumeCapability{
			multiWriterMountCapability(),
		},
	})
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities error = %v", err)
	}
	if resp.GetConfirmed() == nil {
		t.Fatal("expected confirmed for SINGLE_NODE_MULTI_WRITER capability")
	}

	// Unsupported capability — should not confirm.
	resp, err = d.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "vol-1",
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities error = %v", err)
	}
	if resp.GetConfirmed() != nil {
		t.Fatal("MULTI_NODE_MULTI_WRITER should not be confirmed")
	}
	if resp.GetMessage() == "" {
		t.Fatal("expected message for unsupported capability")
	}

	// Missing volume ID.
	_, err = d.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for missing volume id, got %v", err)
	}
}

func TestValidateVolumeCapabilitiesAcceptsMultiWriter(t *testing.T) {
	err := validateVolumeCapabilities([]*csi.VolumeCapability{multiWriterMountCapability()})
	if err != nil {
		t.Fatalf("expected SINGLE_NODE_MULTI_WRITER to be accepted, got %v", err)
	}
}

func TestPublishStateLegacyDefaults(t *testing.T) {
	s := publishState{
		VolumeID:      "vol-1",
		StagingTarget: "/stage",
		Target:        "/target",
	}
	s.applyLegacyDefaults()
	if s.Status != publishStatusPublished {
		t.Fatalf("legacy Status = %q, want %q", s.Status, publishStatusPublished)
	}
	if s.AccessMode != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER.String() {
		t.Fatalf("legacy AccessMode = %q, want SINGLE_NODE_WRITER", s.AccessMode)
	}
	if s.Layout != publishLayoutRoot {
		t.Fatalf("legacy Layout = %q, want %q", s.Layout, publishLayoutRoot)
	}
}

func TestPublishStateLegacyDefaultsDoNotOverwrite(t *testing.T) {
	s := publishState{
		VolumeID:     "vol-1",
		Status:       publishStatusPending,
		Layout:       publishLayoutSubtree,
		WorkspaceDir: defaultWorkspaceDir,
		AccessMode:   csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER.String(),
	}
	s.applyLegacyDefaults()
	if s.Status != publishStatusPending {
		t.Fatalf("Status should not be overwritten, got %q", s.Status)
	}
	if s.AccessMode != csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER.String() {
		t.Fatalf("AccessMode should not be overwritten, got %q", s.AccessMode)
	}
	if s.Layout != publishLayoutSubtree {
		t.Fatalf("Layout should not be overwritten, got %q", s.Layout)
	}
	if s.WorkspaceDir != defaultWorkspaceDir {
		t.Fatalf("WorkspaceDir should not be overwritten, got %q", s.WorkspaceDir)
	}
}

func TestHasActivePublishTargetsStaleCleanup(t *testing.T) {
	stateDir := t.TempDir()
	d := &Driver{cfg: Config{StateDir: stateDir}}

	// Write a publish state for a target that is NOT mounted (stale).
	state := publishState{
		VolumeID:      "vol-1",
		StagingTarget: "/staging",
		Target:        "/target-not-mounted",
		Status:        publishStatusPublished,
		AccessMode:    csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER.String(),
	}
	body, _ := json.MarshalIndent(state, "", "  ")
	statePath := d.publishStatePath("/target-not-mounted")
	if err := os.WriteFile(statePath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	active, err := d.hasActivePublishTargets("vol-1", "/staging")
	if err != nil {
		t.Fatalf("hasActivePublishTargets error = %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected 0 active targets (stale cleaned), got %d", len(active))
	}
	// State file should be removed.
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale state file should be removed")
	}
}

func TestHasActivePublishTargetsMalformedConservative(t *testing.T) {
	stateDir := t.TempDir()
	d := &Driver{cfg: Config{StateDir: stateDir}}

	// Write a malformed state file.
	statePath := filepath.Join(stateDir, "published-deadbeef.json")
	if err := os.WriteFile(statePath, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := d.hasActivePublishTargets("vol-1", "/staging")
	if err == nil {
		t.Fatal("expected error for malformed state file")
	}
	// File should be preserved (not cleaned).
	if _, statErr := os.Stat(statePath); errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("malformed state file should be preserved, not cleaned")
	}
}

func TestHasActivePublishTargetsMatchesStagingTarget(t *testing.T) {
	stateDir := t.TempDir()
	d := &Driver{cfg: Config{StateDir: stateDir}}

	// Write a state for a DIFFERENT staging target — should not match.
	state := publishState{
		VolumeID:      "vol-1",
		StagingTarget: "/old-staging",
		Target:        "/target-not-mounted",
		Status:        publishStatusPublished,
		AccessMode:    csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER.String(),
	}
	body, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(d.publishStatePath("/target-not-mounted"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	active, err := d.hasActivePublishTargets("vol-1", "/current-staging")
	if err != nil {
		t.Fatalf("hasActivePublishTargets error = %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected 0 active targets (different stagingTarget), got %d", len(active))
	}
}

func TestHasActivePublishTargetsLegacyState(t *testing.T) {
	stateDir := t.TempDir()
	d := &Driver{cfg: Config{StateDir: stateDir}}

	// Write a legacy state (no Status, no AccessMode).
	state := publishState{
		VolumeID:      "vol-1",
		StagingTarget: "/staging",
		Target:        "/target-not-mounted",
		PublishedAt:   "2026-01-01T00:00:00Z",
	}
	body, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(d.publishStatePath("/target-not-mounted"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	active, err := d.hasActivePublishTargets("vol-1", "/staging")
	if err != nil {
		t.Fatalf("hasActivePublishTargets error = %v", err)
	}
	// Target not mounted → stale, should be cleaned up.
	if len(active) != 0 {
		t.Fatalf("expected 0 active targets (legacy stale), got %d", len(active))
	}
}

func TestCheckMultiTargetAccessNoActiveAllowed(t *testing.T) {
	// No active targets — any mode is fine.
	for _, mode := range []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
	} {
		if err := checkMultiTargetAccess(nil, mode); err != nil {
			t.Fatalf("mode %s with no active: unexpected error %v", mode, err)
		}
	}
}

func TestCheckMultiTargetAccessSingleWriterRejectsSecondTarget(t *testing.T) {
	active := []publishState{{
		VolumeID:   "vol-1",
		AccessMode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER.String(),
		Status:     publishStatusPublished,
	}}
	// Request with SINGLE_NODE_WRITER → reject.
	err := checkMultiTargetAccess(active, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

func TestCheckMultiTargetAccessSingleSingleWriterRejectsSecondTarget(t *testing.T) {
	active := []publishState{{
		VolumeID:   "vol-1",
		AccessMode: csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER.String(),
		Status:     publishStatusPublished,
	}}
	err := checkMultiTargetAccess(active, csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

func TestCheckMultiTargetAccessMultiWriterAllowsSecondTarget(t *testing.T) {
	active := []publishState{{
		VolumeID:   "vol-1",
		AccessMode: csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER.String(),
		Status:     publishStatusPublished,
	}}
	err := checkMultiTargetAccess(active, csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER)
	if err != nil {
		t.Fatalf("expected multi-writer to allow second target, got %v", err)
	}
}

func TestCheckMultiTargetAccessMixedModesReject(t *testing.T) {
	// Existing active is SINGLE_NODE_WRITER, request is MULTI_WRITER → reject.
	active := []publishState{{
		VolumeID:   "vol-1",
		AccessMode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER.String(),
		Status:     publishStatusPublished,
	}}
	err := checkMultiTargetAccess(active, csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition for mixed modes, got %v", err)
	}
}

func TestCheckMultiTargetAccessLegacyStateBlocksMultiTarget(t *testing.T) {
	// Legacy state: AccessMode="" → after applyLegacyDefaults → SINGLE_NODE_WRITER.
	legacy := publishState{VolumeID: "vol-1"}
	legacy.applyLegacyDefaults()
	active := []publishState{legacy}

	// Even with MULTI_WRITER request, legacy state blocks.
	err := checkMultiTargetAccess(active, csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition for legacy state blocking multi-target, got %v", err)
	}
}

// TestCreateVolumeTwoPVCsIndependentLifecycle validates that two PVCs
// backed by different Secrets (same API key, same workspace root) produce
// independent volumes with independent delete lifecycles.  This is the
// unit-level analog of the one-pod multi-PVC e2e test.
func TestCreateVolumeTwoPVCsIndependentLifecycle(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("workspace-a", "default", "secret-a"),
		fd.k8sPVC("workspace-b", "default", "secret-b"),
		fd.k8sSecret("secret-a", "default"),
		fd.k8sSecret("secret-b", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	// Create two workspace-root PVCs (different names, same workspace root).
	respA, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "workspace-a",
		Parameters:         pvcParams("workspace-a", "default", nil),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume workspace-a error = %v", err)
	}
	respB, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "workspace-b",
		Parameters:         pvcParams("workspace-b", "default", nil),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume workspace-b error = %v", err)
	}

	volA := respA.GetVolume().GetVolumeId()
	volB := respB.GetVolume().GetVolumeId()
	if volA == volB {
		t.Fatalf("two PVCs must produce different volumeIDs: %q", volA)
	}
	if !isWorkspaceRootVolumeID(volA) || !isWorkspaceRootVolumeID(volB) {
		t.Fatalf("both must be workspace root volumes: A=%q B=%q", volA, volB)
	}
	// Verify different Secrets are fixated.
	if respA.GetVolume().GetVolumeContext()[attrSecretName] != "secret-a" {
		t.Fatalf("workspace-a secretName = %q, want secret-a", respA.GetVolume().GetVolumeContext()[attrSecretName])
	}
	if respB.GetVolume().GetVolumeContext()[attrSecretName] != "secret-b" {
		t.Fatalf("workspace-b secretName = %q, want secret-b", respB.GetVolume().GetVolumeContext()[attrSecretName])
	}

	// Delete A — B must remain functional (workspace-root = no-op, no credentials needed).
	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: volA,
	}); err != nil {
		t.Fatalf("DeleteVolume workspace-a error = %v", err)
	}

	// Idempotent re-create of B must succeed with the same volumeID.
	respB2, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "workspace-b",
		Parameters:         pvcParams("workspace-b", "default", nil),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("idempotent CreateVolume workspace-b error = %v", err)
	}
	if respB2.GetVolume().GetVolumeId() != volB {
		t.Fatalf("idempotent CreateVolume workspace-b volumeID = %q, want %q", respB2.GetVolume().GetVolumeId(), volB)
	}

	// Delete B.
	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: volB,
	}); err != nil {
		t.Fatalf("DeleteVolume workspace-b error = %v", err)
	}

	// Workspace root must survive both deletions.
	if !fd.exists("/") {
		t.Fatal("workspace root must not be deleted after both PVCs are removed")
	}
}

// TestCreateVolumeTwoPVCsDifferentRemoteRoots validates that two PVCs with
// the same API key but different remoteRoot values produce volumes that
// point to different mount paths.
func TestCreateVolumeTwoPVCsDifferentRemoteRoots(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()
	fd.mkdir("/projects/alpha")
	fd.mkdir("/projects/beta")

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVCWithRemoteRoot("pvc-alpha", "default", "drive9-secret", "/projects/alpha"),
		fd.k8sPVCWithRemoteRoot("pvc-beta", "default", "drive9-secret", "/projects/beta"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	respA, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-alpha",
		Parameters:         pvcParams("pvc-alpha", "default", nil),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume alpha error = %v", err)
	}

	respB, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-beta",
		Parameters:         pvcParams("pvc-beta", "default", nil),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume beta error = %v", err)
	}

	volA := respA.GetVolume().GetVolumeId()
	volB := respB.GetVolume().GetVolumeId()
	if volA == volB {
		t.Fatalf("different remoteRoot PVCs must produce different volumeIDs: %q", volA)
	}
	if respA.GetVolume().GetVolumeContext()["remoteRoot"] != "/projects/alpha" {
		t.Fatalf("alpha remoteRoot = %q, want /projects/alpha", respA.GetVolume().GetVolumeContext()["remoteRoot"])
	}
	if respB.GetVolume().GetVolumeContext()["remoteRoot"] != "/projects/beta" {
		t.Fatalf("beta remoteRoot = %q, want /projects/beta", respB.GetVolume().GetVolumeContext()["remoteRoot"])
	}
}

// --- PVC Annotation Tests ---

func TestCreateVolumeRejectsMissingAnnotation(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	// PVC without drive9.ai/secret-name annotation.
	k8s := k8sfake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pvc-no-ann",
			Namespace: "default",
		},
	})
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-no-ann",
		Parameters:         pvcParams("pvc-no-ann", "default", nil),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for missing annotation, got %v", err)
	}
	msg := status.Convert(err).Message()
	if !strings.Contains(msg, annotationSecretName) {
		t.Fatalf("error message should mention %q, got: %s", annotationSecretName, msg)
	}
}

func TestCreateVolumeRejectsSecretNotFound(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	// PVC references a Secret that doesn't exist.
	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-bad-secret", "default", "nonexistent-secret"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-bad-secret",
		Parameters:         pvcParams("pvc-bad-secret", "default", nil),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err == nil {
		t.Fatal("expected error for missing Secret")
	}
}

func TestCreateVolumeRejectsMissingPVCMetadata(t *testing.T) {
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8sfake.NewSimpleClientset()}

	// No PVC metadata params (no --extra-create-metadata).
	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-no-meta",
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for missing PVC metadata, got %v", err)
	}
}

func TestCreateVolumePVAttributesNoAPIKey(t *testing.T) {
	fd := newFakeDrive9(t)
	defer fd.close()

	k8s := k8sfake.NewSimpleClientset(
		fd.k8sPVC("pvc-safe", "default", "drive9-secret"),
		fd.k8sSecret("drive9-secret", "default"),
	)
	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}, k8s: k8s}

	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-safe",
		Parameters:         pvcParams("pvc-safe", "default", nil),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	ctx := createResp.GetVolume().GetVolumeContext()
	// Verify no credential data leaked into PV attributes.
	if err := validateNoAPIKeyInAttributes(ctx); err != nil {
		t.Fatalf("PV attributes contain credentials: %v", err)
	}
	// Verify secret reference IS present.
	if ctx[attrSecretName] == "" || ctx[attrSecretNamespace] == "" {
		t.Fatal("PV attributes must contain secretName and secretNamespace")
	}
}

func multiWriterMountCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
		},
	}
}
