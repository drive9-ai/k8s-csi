package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestNodeRecoveryRereadsDurableStateAfterVolumeLock(t *testing.T) {
	stale := validActiveState(t)
	runtime := &fakeHostRuntime{}
	d := &Driver{
		cfg: Config{
			StateDir:   t.TempDir(),
			DriverName: "csi.drive9.ai",
		},
		k8s:              k8sfake.NewSimpleClientset(),
		nodeRuntime:      runtime,
		nodeCapabilities: availableNodeCapabilities(),
		nodePreflightSet: true,
	}

	d.recoverOneNodeMount(context.Background(), stale)

	if calls := runtime.Calls(); len(calls) != 0 {
		t.Fatalf("recovery acted on a deleted stale snapshot: %#v", calls)
	}
	if _, err := d.readMountState(stale.VolumeID); !os.IsNotExist(err) {
		t.Fatalf("stale recovery recreated durable state: %v", err)
	}
}

func TestNodeRecoveryCleansAbandonedStageWithoutRemoteAPI(t *testing.T) {
	fake := newFakeDrive9(t)
	secret := fake.k8sSecret("drive9-secret", "default")
	server := fake.server.URL
	fake.close()

	stateDir := t.TempDir()
	state := validStartingState(t)
	state.VolumeID = volumeIDForRemoteRoot(state.RemoteRoot)
	names, err := newVolumeHostNames(state.VolumeID, state.AttemptID)
	if err != nil {
		t.Fatalf("newVolumeHostNames(): %v", err)
	}
	state.SystemdUnit = names.SystemdUnit
	state.EnvPath = names.EnvPath
	state.ArgsPath = names.ArgsPath
	state.MountArgs = []string{
		"mount", "--foreground", "--server", server,
		":" + state.RemoteRoot, state.StagingTarget,
	}
	store := newMountStateStore(stateDir, newHostRuntime())
	if err := store.Write(state); err != nil {
		t.Fatalf("write starting state: %v", err)
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-starting-recovery"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "csi.drive9.ai",
					VolumeHandle: state.VolumeID,
					VolumeAttributes: map[string]string{
						"remoteRoot":        state.RemoteRoot,
						attrSecretName:      secret.Name,
						attrSecretNamespace: secret.Namespace,
					},
				},
			},
		},
	}
	runtime := &fakeHostRuntime{}
	processStatePath, _ := drive9ProcessStatePath(state.StagingTarget)
	runtime.isMountPointFn = func(string) (bool, error) { return false, nil }
	runtime.lstatFn = func(path string) (os.FileInfo, error) {
		if path == processStatePath {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("unexpected lstat path %s", path)
	}
	runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show" {
			return systemdShowResult(systemdUnitNotFound), nil
		}
		return hostCommandResult{}, fmt.Errorf("unexpected command %#v", command)
	}
	d := &Driver{
		cfg: Config{
			StateDir:   stateDir,
			DriverName: "csi.drive9.ai",
		},
		k8s:              k8sfake.NewSimpleClientset(secret, pv),
		nodeRuntime:      runtime,
		nodeCapabilities: availableNodeCapabilities(),
		nodePreflightSet: true,
	}

	d.recoverOneNodeMount(context.Background(), state)

	if _, readErr := store.Read(state.VolumeID); !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("abandoned stage state was not deleted: %v", readErr)
	}
	for _, call := range runtime.Calls() {
		if call.Operation != "exec" {
			continue
		}
		inner := hostInnerCommand(call.Command)
		if len(inner) > 0 && inner[0] != "systemctl" {
			t.Fatalf("abandoned stage cleanup executed unexpected command: %#v", call.Command)
		}
		if len(inner) > 1 && inner[1] != "show" {
			t.Fatalf("abandoned stage cleanup mutated systemd: %#v", call.Command)
		}
	}
}

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

func TestNodeRecoveryPolicyDoesNotRepeatPlatformPreflight(t *testing.T) {
	for _, test := range []struct {
		mode string
		want bool
	}{
		{mode: nodeRecoveryAuto, want: true},
		{mode: nodeRecoveryEnabled, want: true},
		{mode: nodeRecoveryDisabled, want: false},
	} {
		d := &Driver{cfg: Config{
			RecoverNodeMounts: test.mode,
			StateDir:          filepath.Join(t.TempDir(), "does-not-need-to-exist"),
		}}
		got, err := d.shouldRecoverNodeMounts()
		if err != nil || got != test.want {
			t.Fatalf("shouldRecoverNodeMounts(%q) = %t, %v, want %t", test.mode, got, err, test.want)
		}
	}
}

func TestListMountStatesIgnoresPublishAndMalformedState(t *testing.T) {
	stateDir := t.TempDir()
	d := &Driver{cfg: Config{StateDir: stateDir}}
	mountState := validStartingState(t)
	if err := d.writeMountState(mountState); err != nil {
		t.Fatalf("writeMountState error = %v", err)
	}
	if err := d.writePublishState(publishState{
		VolumeID:      mountState.VolumeID,
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
	if states[0].VolumeID != mountState.VolumeID {
		t.Fatalf("state volumeID = %q, want %q", states[0].VolumeID, mountState.VolumeID)
	}
}

func TestRepairPublishTargetsUsesObservedMountState(t *testing.T) {
	stateDir := t.TempDir()
	volumeID := "drive9-" + strings.Repeat("a", 32)
	stagingTarget := "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/volume/globalmount"
	targetRoot := "/var/lib/kubelet/pods/pod/volumes/kubernetes.io~csi/volume"
	tests := []struct {
		name       string
		status     string
		mounted    bool
		wantStatus string
	}{
		{name: "published-mounted", status: publishStatusPublished, mounted: true, wantStatus: publishStatusPublished},
		{name: "published-absent", status: publishStatusPublished},
		{name: "pending-mounted", status: publishStatusPending, mounted: true, wantStatus: publishStatusPublished},
		{name: "pending-absent", status: publishStatusPending},
	}
	mounted := make(map[string]bool, len(tests))
	runtime := &fakeHostRuntime{
		isMountPointFn: func(path string) (bool, error) {
			return mounted[path], nil
		},
	}
	mountOps := &fakeNodeMountOperations{}
	driver := &Driver{
		cfg:          Config{StateDir: stateDir},
		nodeRuntime:  runtime,
		nodeMountOps: mountOps,
	}
	for i, test := range tests {
		target := filepath.Join(targetRoot, test.name)
		mounted[target] = test.mounted
		if err := driver.writePublishState(publishState{
			VolumeID:      volumeID,
			StagingTarget: stagingTarget,
			Target:        target,
			Status:        test.status,
		}); err != nil {
			t.Fatalf("write publish state %d: %v", i, err)
		}
	}

	driver.repairPublishTargets(volumeID, stagingTarget)

	if mountOps.unmountCalls != 2 || mountOps.bindCalls != 2 || mountOps.lazyUnmountCalls != 0 {
		t.Fatalf("repair calls = unmount:%d bind:%d lazy:%d, want 2/2/0",
			mountOps.unmountCalls, mountOps.bindCalls, mountOps.lazyUnmountCalls)
	}
	for _, test := range tests {
		target := filepath.Join(targetRoot, test.name)
		state, err := driver.readPublishState(target)
		if !test.mounted {
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s state error = %v, want not exist", test.name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("read %s state: %v", test.name, err)
		}
		if state.Status != test.wantStatus {
			t.Fatalf("%s status = %q, want %q", test.name, state.Status, test.wantStatus)
		}
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
