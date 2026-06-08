package driver

import (
	"context"
	"encoding/json"
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
			name: "mutable parameters",
			mutate: func(req *csi.CreateVolumeRequest) {
				req.MutableParameters = map[string]string{"profile": "other"}
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

	d := &Driver{cfg: Config{StateDir: t.TempDir(), DriverName: "csi.drive9.ai"}}
	remoteRoot := "/k8s/pvc/demo"
	volumeID := volumeIDForRemoteRoot(remoteRoot)
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: t.TempDir(),
		VolumeContext:     map[string]string{"remoteRoot": remoteRoot},
		Secrets:           fake.secrets(),
		VolumeCapability:  singleNodeMountCapability(),
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

func TestCreateDeleteVolumeWritesIndexAndMarker(t *testing.T) {
	fake := newFakeDrive9(t)
	defer fake.close()

	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}}
	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-demo",
		Secrets:            fake.secrets(),
		Parameters:         map[string]string{"remoteRootPrefix": "/k8s/pvc"},
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
	if !fake.exists(markerPath(remoteRoot)) {
		t.Fatalf("missing root marker %s", markerPath(remoteRoot))
	}
	if !fake.exists(indexPath(volumeID)) {
		t.Fatalf("missing index marker %s", indexPath(volumeID))
	}
	if !fake.exists(nameIndexPath("pvc-demo")) {
		t.Fatalf("missing name index marker %s", nameIndexPath("pvc-demo"))
	}

	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
		Secrets:  fake.secrets(),
	}); err != nil {
		t.Fatalf("DeleteVolume error = %v", err)
	}
	if fake.exists(remoteRoot) {
		t.Fatalf("remote root still exists after delete: %s", remoteRoot)
	}
	if fake.exists(indexPath(volumeID)) {
		t.Fatalf("index still exists after delete: %s", indexPath(volumeID))
	}
	if fake.exists(nameIndexPath("pvc-demo")) {
		t.Fatalf("name index still exists after delete: %s", nameIndexPath("pvc-demo"))
	}
}

func TestCreateVolumeIsIdempotentByName(t *testing.T) {
	fake := newFakeDrive9(t)
	defer fake.close()

	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}}
	first, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-same-name",
		Secrets:            fake.secrets(),
		Parameters:         map[string]string{"remoteRootPrefix": "/k8s/pvc"},
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("first CreateVolume error = %v", err)
	}
	second, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-same-name",
		Secrets:            fake.secrets(),
		Parameters:         map[string]string{"remoteRootPrefix": "/k8s/pvc"},
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
		Secrets:            fake.secrets(),
		Parameters:         map[string]string{"remoteRootPrefix": "/k8s/other"},
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("conflicting CreateVolume status = %s, want AlreadyExists (err=%v)", status.Code(err), err)
	}
	if fake.exists(conflictingRoot) {
		t.Fatalf("conflicting CreateVolume created remote root: %s", conflictingRoot)
	}
}

func TestCreateVolumeRecoversFromNameIndexOnly(t *testing.T) {
	fake := newFakeDrive9(t)
	defer fake.close()

	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}}
	name := "pvc-partial"
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
	fake.putJSON(nameIndexPath(name), marker)

	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               name,
		Secrets:            fake.secrets(),
		Parameters:         map[string]string{"remoteRootPrefix": "/k8s/pvc"},
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume recovery error = %v", err)
	}
	if resp.GetVolume().GetVolumeId() != volumeID {
		t.Fatalf("recovered volumeID = %q, want %q", resp.GetVolume().GetVolumeId(), volumeID)
	}
	for _, remotePath := range []string{remoteRoot, markerPath(remoteRoot), indexPath(volumeID), nameIndexPath(name)} {
		if !fake.exists(remotePath) {
			t.Fatalf("CreateVolume recovery did not create %s", remotePath)
		}
	}
}

func TestCreateVolumeDefaultPrefixIsValid(t *testing.T) {
	fake := newFakeDrive9(t)
	defer fake.close()

	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}}
	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-default",
		Secrets:            fake.secrets(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume with default prefix error = %v", err)
	}
	remoteRoot := createResp.GetVolume().GetVolumeContext()["remoteRoot"]
	if !pathIsOrUnder(remoteRoot, defaultRemoteRootPrefix) {
		t.Fatalf("default remote root = %q, want under %q", remoteRoot, defaultRemoteRootPrefix)
	}
}

func TestCreateVolumeCreatesRemoteParents(t *testing.T) {
	fake := newFakeDrive9(t)
	fake.requireMkdirParents()
	defer fake.close()

	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}}
	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-parent-demo",
		Secrets:            fake.secrets(),
		Parameters:         map[string]string{"remoteRootPrefix": "/k8s/pvc"},
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	remoteRoot := createResp.GetVolume().GetVolumeContext()["remoteRoot"]
	for _, remotePath := range []string{"/k8s", "/k8s/pvc", "/k8s/.drive9-csi", metadataRoot, nameIndexRoot, remoteRoot} {
		if !fake.exists(remotePath) {
			t.Fatalf("expected parent path to exist: %s", remotePath)
		}
	}
}

func TestDeleteVolumeRejectsTamperedRootIndex(t *testing.T) {
	fake := newFakeDrive9(t)
	defer fake.close()

	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}}
	volumeID := volumeIDForRemoteRoot("/")
	fake.putJSON(indexPath(volumeID), volumeMarker{
		Version:    1,
		Driver:     "csi.drive9.ai",
		VolumeID:   volumeID,
		Name:       "tampered",
		RemoteRoot: "/",
	})
	_, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
		Secrets:  fake.secrets(),
	})
	if err == nil {
		t.Fatal("expected DeleteVolume to reject tampered root index")
	}
	if status.Code(err).String() != "FailedPrecondition" {
		t.Fatalf("DeleteVolume status = %s, want FailedPrecondition", status.Code(err))
	}
}

func TestDeleteVolumeRejectsTamperedRootMarkerName(t *testing.T) {
	fake := newFakeDrive9(t)
	defer fake.close()

	d := &Driver{cfg: Config{DriverName: "csi.drive9.ai"}}
	createResp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-delete-safe",
		Secrets:            fake.secrets(),
		Parameters:         map[string]string{"remoteRootPrefix": "/k8s/pvc"},
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeMountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume error = %v", err)
	}
	volumeID := createResp.GetVolume().GetVolumeId()
	remoteRoot := createResp.GetVolume().GetVolumeContext()["remoteRoot"]
	fake.putJSON(markerPath(remoteRoot), volumeMarker{
		Version:    1,
		Driver:     "csi.drive9.ai",
		VolumeID:   volumeID,
		Name:       "other-name",
		RemoteRoot: remoteRoot,
	})

	_, err = d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
		Secrets:  fake.secrets(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("DeleteVolume status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if !fake.exists(remoteRoot) {
		t.Fatalf("DeleteVolume removed remote root despite tampered marker: %s", remoteRoot)
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
	}, "vol", "/stage", "/target", true) {
		t.Fatal("expected matching publish state")
	}
	if publishStateMatches(publishState{
		VolumeID:      "vol",
		StagingTarget: "/stage",
		Target:        "/target",
		Readonly:      false,
	}, "vol", "/stage", "/target", true) {
		t.Fatal("expected readonly mismatch to fail")
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

func (f *fakeDrive9) close() {
	f.server.Close()
}

func (f *fakeDrive9) secrets() map[string]string {
	return map[string]string{
		"server": f.server.URL,
		"apiKey": "test-key",
	}
}

func (f *fakeDrive9) exists(remotePath string) bool {
	remotePath = normalizeForTest(f.t, remotePath)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dirs[remotePath] || f.files[remotePath] != nil
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
