package driver

import (
	"context"
	"errors"
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

func TestRepairPublishTargetUsesRepeatedBindWhenTargetMounted(t *testing.T) {
	state := publishState{
		VolumeID: "vol-a",
		Target:   "/var/lib/kubelet/pods/pod/volumes/kubernetes.io~csi/pv/mount",
		Readonly: true,
	}
	var repeated bool
	err := repairRootPublishTargetWithOps("/stage", state,
		func(target string) (bool, error) {
			if target != state.Target {
				t.Fatalf("isMounted target = %q, want %q", target, state.Target)
			}
			return true, nil
		},
		func(source string, target string) (bool, error) {
			if source != "/stage" || target != state.Target {
				t.Fatalf("sameTopMount args = (%q, %q)", source, target)
			}
			return false, nil
		},
		func(string, string, bool) error {
			t.Fatal("fresh bind must not be used for an existing publish mount")
			return nil
		},
		func(source string, target string, readonly bool) error {
			repeated = true
			if source != "/stage" || target != state.Target || !readonly {
				t.Fatalf("repeated bind args = (%q, %q, %v)", source, target, readonly)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("repairPublishTargetWithOps error = %v", err)
	}
	if !repeated {
		t.Fatal("expected repeated bind")
	}
}

func TestRepairPublishTargetSkipsRepeatedBindWhenTargetAlreadyMatchesStaging(t *testing.T) {
	state := publishState{
		VolumeID: "vol-a",
		Target:   "/var/lib/kubelet/pods/pod/volumes/kubernetes.io~csi/pv/mount",
	}
	err := repairRootPublishTargetWithOps("/stage", state,
		func(string) (bool, error) {
			return true, nil
		},
		func(source string, target string) (bool, error) {
			if source != "/stage" || target != state.Target {
				t.Fatalf("sameTopMount args = (%q, %q)", source, target)
			}
			return true, nil
		},
		func(string, string, bool) error {
			t.Fatal("fresh bind must not be used when target already matches staging")
			return nil
		},
		func(string, string, bool) error {
			t.Fatal("repeated bind must not be used when target already matches staging")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("repairPublishTargetWithOps error = %v", err)
	}
}

func TestRepairPublishTargetUsesFreshBindWhenTargetUnmounted(t *testing.T) {
	state := publishState{
		VolumeID: "vol-a",
		Target:   "/var/lib/kubelet/pods/pod/volumes/kubernetes.io~csi/pv/mount",
	}
	var fresh bool
	err := repairRootPublishTargetWithOps("/stage", state,
		func(string) (bool, error) {
			return false, nil
		},
		func(string, string) (bool, error) {
			t.Fatal("sameTopMount must not be called for an unmounted publish target")
			return false, nil
		},
		func(source string, target string, readonly bool) error {
			fresh = true
			if source != "/stage" || target != state.Target || readonly {
				t.Fatalf("fresh bind args = (%q, %q, %v)", source, target, readonly)
			}
			return nil
		},
		func(string, string, bool) error {
			t.Fatal("repeated bind must not be used for an unmounted publish target")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("repairPublishTargetWithOps error = %v", err)
	}
	if !fresh {
		t.Fatal("expected fresh bind")
	}
}

func TestRepairPublishTargetWrapsRepeatedBindError(t *testing.T) {
	state := publishState{
		VolumeID: "vol-a",
		Target:   "/var/lib/kubelet/pods/pod/volumes/kubernetes.io~csi/pv/mount",
	}
	errBind := errors.New("mount failed")
	err := repairRootPublishTargetWithOps("/stage", state,
		func(string) (bool, error) {
			return true, nil
		},
		func(string, string) (bool, error) {
			return false, nil
		},
		func(string, string, bool) error {
			t.Fatal("fresh bind must not be used for an existing publish mount")
			return nil
		},
		func(string, string, bool) error {
			return errBind
		},
	)
	if !errors.Is(err, errBind) {
		t.Fatalf("error = %v, want wrapping %v", err, errBind)
	}
}

func TestRepairSubtreePublishTargetBindsMissingWorkspaceChild(t *testing.T) {
	state := publishState{
		VolumeID:     "vol-a",
		Target:       "/var/lib/kubelet/pods/pod/volumes/kubernetes.io~csi/pv/mount",
		Layout:       publishLayoutSubtree,
		WorkspaceDir: defaultWorkspaceDir,
		Readonly:     true,
	}
	var ensured bool
	var bound bool
	err := repairSubtreePublishTargetWithOps("/stage", state,
		func(target string) (bool, error) {
			if target != state.workspaceTarget() {
				t.Fatalf("isMounted target = %q, want workspace target %q", target, state.workspaceTarget())
			}
			return false, nil
		},
		func(string, string) (bool, error) {
			t.Fatal("sameTopMount must not be called for a missing workspace mount")
			return false, nil
		},
		func(target string) error {
			ensured = true
			if target != state.Target {
				t.Fatalf("ensure anchor target = %q, want %q", target, state.Target)
			}
			return nil
		},
		func(source string, target string, readonly bool) error {
			bound = true
			if source != "/stage" || target != state.workspaceTarget() || !readonly {
				t.Fatalf("bind args = (%q, %q, %v)", source, target, readonly)
			}
			return nil
		},
		func(string) error {
			t.Fatal("unmountAll must not be called for a missing workspace mount")
			return nil
		},
		func(string) error {
			t.Fatal("lazyUnmount must not be called for a missing workspace mount")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("repairSubtreePublishTargetWithOps error = %v", err)
	}
	if !ensured || !bound {
		t.Fatalf("ensured=%v bound=%v, want both true", ensured, bound)
	}
}

func TestRepairSubtreePublishTargetSkipsMatchingWorkspaceChild(t *testing.T) {
	state := publishState{
		VolumeID:     "vol-a",
		Target:       "/var/lib/kubelet/pods/pod/volumes/kubernetes.io~csi/pv/mount",
		Layout:       publishLayoutSubtree,
		WorkspaceDir: defaultWorkspaceDir,
	}
	err := repairSubtreePublishTargetWithOps("/stage", state,
		func(target string) (bool, error) {
			if target != state.workspaceTarget() {
				t.Fatalf("isMounted target = %q, want workspace target %q", target, state.workspaceTarget())
			}
			return true, nil
		},
		func(source string, target string) (bool, error) {
			if source != "/stage" || target != state.workspaceTarget() {
				t.Fatalf("sameTopMount args = (%q, %q)", source, target)
			}
			return true, nil
		},
		func(string) error {
			return nil
		},
		func(string, string, bool) error {
			t.Fatal("bindChild must not be called when workspace already matches")
			return nil
		},
		func(string) error {
			t.Fatal("unmountAll must not be called when workspace already matches")
			return nil
		},
		func(string) error {
			t.Fatal("lazyUnmount must not be called when workspace already matches")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("repairSubtreePublishTargetWithOps error = %v", err)
	}
}

func TestRepairSubtreePublishTargetUnmountsStaleWorkspaceChild(t *testing.T) {
	state := publishState{
		VolumeID:     "vol-a",
		Target:       "/var/lib/kubelet/pods/pod/volumes/kubernetes.io~csi/pv/mount",
		Layout:       publishLayoutSubtree,
		WorkspaceDir: defaultWorkspaceDir,
	}
	var unmounted bool
	var bound bool
	err := repairSubtreePublishTargetWithOps("/stage", state,
		func(string) (bool, error) {
			return true, nil
		},
		func(string, string) (bool, error) {
			return false, nil
		},
		func(string) error {
			return nil
		},
		func(source string, target string, readonly bool) error {
			bound = true
			if source != "/stage" || target != state.workspaceTarget() || readonly {
				t.Fatalf("bind args = (%q, %q, %v)", source, target, readonly)
			}
			return nil
		},
		func(target string) error {
			unmounted = true
			if target != state.workspaceTarget() {
				t.Fatalf("unmount target = %q, want %q", target, state.workspaceTarget())
			}
			return nil
		},
		func(string) error {
			t.Fatal("lazyUnmount must not be called when regular unmount succeeds")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("repairSubtreePublishTargetWithOps error = %v", err)
	}
	if !unmounted || !bound {
		t.Fatalf("unmounted=%v bound=%v, want both true", unmounted, bound)
	}
}
