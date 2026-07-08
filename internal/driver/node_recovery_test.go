package driver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestNodeRecoveryModeValidation(t *testing.T) {
	for _, mode := range []string{"", "auto", "enabled", "disabled", " AUTO "} {
		normalized := normalizeNodeRecoveryMode(mode)
		if err := validateNodeRecoveryMode(normalized); err != nil {
			t.Fatalf("validateNodeRecoveryMode(%q) error = %v", mode, err)
		}
	}
	if normalizeNodeRecoveryMode("") != nodeRecoveryAuto {
		t.Fatal("empty node recovery mode should default to auto")
	}
	if err := validateNodeRecoveryMode("sometimes"); err == nil {
		t.Fatal("expected invalid recovery mode to fail")
	}
}

func TestListMountStatesIgnoresPublishAndMalformedState(t *testing.T) {
	stateDir := t.TempDir()
	d := &Driver{cfg: Config{StateDir: stateDir}}
	if err := d.writeMountState(mountState{
		VolumeID:      "vol-a",
		RemoteRoot:    "/k8s/a",
		StagingTarget: "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/vol-a/globalmount",
	}); err != nil {
		t.Fatalf("writeMountState error = %v", err)
	}
	if err := d.writePublishState(publishState{
		VolumeID:      "vol-a",
		StagingTarget: "/stage",
		Target:        "/target",
	}); err != nil {
		t.Fatalf("writePublishState error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("write malformed state: %v", err)
	}

	states := d.listMountStates()
	if len(states) != 1 {
		t.Fatalf("len(listMountStates) = %d, want 1: %#v", len(states), states)
	}
	if states[0].VolumeID != "vol-a" {
		t.Fatalf("state volumeID = %q, want vol-a", states[0].VolumeID)
	}
}

func TestResolveVolumeContextFromPV(t *testing.T) {
	attrs := map[string]string{
		"remoteRoot":        "/k8s/demo",
		attrSecretName:      "drive9-secret",
		attrSecretNamespace: "default",
	}
	k8s := k8sfake.NewSimpleClientset(&corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-drive9"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:           "csi.drive9.ai",
					VolumeHandle:     "vol-drive9",
					VolumeAttributes: attrs,
				},
			},
		},
	})

	got, err := resolveVolumeContextFromPV(context.Background(), k8s, "csi.drive9.ai", "vol-drive9")
	if err != nil {
		t.Fatalf("resolveVolumeContextFromPV error = %v", err)
	}
	if got["remoteRoot"] != "/k8s/demo" {
		t.Fatalf("remoteRoot = %q, want /k8s/demo", got["remoteRoot"])
	}
	got["remoteRoot"] = "/mutated"
	if attrs["remoteRoot"] != "/k8s/demo" {
		t.Fatal("resolveVolumeContextFromPV must return a copy of volumeAttributes")
	}
}

func TestResolveVolumeContextFromPVRejectsWrongDriver(t *testing.T) {
	k8s := k8sfake.NewSimpleClientset(&corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-other"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:           "other.example.com",
					VolumeHandle:     "vol-drive9",
					VolumeAttributes: map[string]string{"remoteRoot": "/k8s/demo"},
				},
			},
		},
	})

	_, err := resolveVolumeContextFromPV(context.Background(), k8s, "csi.drive9.ai", "vol-drive9")
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
}

func TestPathUnderRoot(t *testing.T) {
	for _, path := range []string{
		"/var/lib/kubelet",
		"/var/lib/kubelet/plugins/kubernetes.io/csi/pv/vol/globalmount",
		"/var/lib/kubelet/pods/pod/volumes/kubernetes.io~csi/vol/mount",
	} {
		if !pathUnderRoot(path, defaultKubeletRoot) {
			t.Fatalf("pathUnderRoot(%q) = false, want true", path)
		}
	}
	for _, path := range []string{
		"/var/lib/kubelet-other",
		"/tmp/target",
		"relative/path",
	} {
		if pathUnderRoot(path, defaultKubeletRoot) {
			t.Fatalf("pathUnderRoot(%q) = true, want false", path)
		}
	}
}
