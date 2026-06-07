package driver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
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
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	dirs   map[string]bool
	files  map[string][]byte
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
