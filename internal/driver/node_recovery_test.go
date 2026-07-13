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
	kruntime "k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
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
		Status:        publishStatusPublished,
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

func TestNodeRecoveryDoesNotMutatePublishTransactions(t *testing.T) {
	for _, statusValue := range []string{
		publishStatusPending,
		publishStatusPublished,
		publishStatusUnpublishing,
	} {
		t.Run(statusValue, func(t *testing.T) {
			fixture := newStartingReconcileFixture(t)
			canonicalizeRecoveryStateIdentity(t, &fixture.state)
			fixture.mounted = true
			fixture.process = "ready"
			fixture.service = systemdUnitActive
			fixture.states.states = []mountState{fixture.state}
			fixture.installCallbacks()

			stateDir := t.TempDir()
			store := newMountStateStore(stateDir, newHostRuntime())
			if err := store.Write(fixture.state); err != nil {
				t.Fatalf("write starting state: %v", err)
			}
			mountOps := &fakeNodeMountOperations{}
			driver := &Driver{
				cfg: Config{
					StateDir:   stateDir,
					DriverName: "csi.drive9.ai",
				},
				nodeRuntime:      fixture.runtime,
				nodeMountOps:     mountOps,
				nodeCapabilities: availableNodeCapabilities(),
				nodePreflightSet: true,
			}
			driver.k8s = k8sfake.NewSimpleClientset(recoveryPV(
				fixture.state,
				[]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				nil,
			))
			publish := publishState{
				VolumeID:      fixture.state.VolumeID,
				StagingTarget: fixture.state.StagingTarget,
				Target:        filepath.Join(defaultKubeletRoot, "pods/pod/volumes/kubernetes.io~csi/volume", statusValue),
				Status:        statusValue,
			}
			if err := driver.writePublishState(publish); err != nil {
				t.Fatalf("write publish state: %v", err)
			}
			before, err := os.ReadFile(driver.publishStatePath(publish.Target))
			if err != nil {
				t.Fatalf("read publish state before recovery: %v", err)
			}

			driver.recoverOneNodeMount(context.Background(), fixture.state)

			recovered, err := store.Read(fixture.state.VolumeID)
			if err != nil || recovered.Phase != mountStatePhaseActive {
				t.Fatalf("mount recovery = %#v, %v; want active", recovered, err)
			}
			after, err := os.ReadFile(driver.publishStatePath(publish.Target))
			if err != nil || string(after) != string(before) {
				t.Fatalf("publish state changed during recovery: before=%q after=%q err=%v", before, after, err)
			}
			if mountOps.bindCalls != 0 || mountOps.unmountCalls != 0 || mountOps.lazyUnmountCalls != 0 {
				t.Fatalf("publish mount operations = bind:%d unmount:%d lazy:%d, want 0/0/0",
					mountOps.bindCalls, mountOps.unmountCalls, mountOps.lazyUnmountCalls)
			}
		})
	}
}

func TestBootRecoveryHealthyActiveResolvesPVBeforeReturn(t *testing.T) {
	for _, test := range []struct {
		name  string
		modes []corev1.PersistentVolumeAccessMode
		attrs map[string]string
		mnmw  bool
	}{
		{name: "RWO", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, attrs: map[string]string{
			paramAttrTTL: "invalid-legacy-ttl", paramPerfEnabled: "invalid-legacy-perf",
		}},
		{name: "RWX", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, attrs: map[string]string{
			paramProfile: profileNone, paramDurability: durabilityCloseSync,
		}, mnmw: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newNodeStateFirstFixtureWithActive(t, true, func(state *mountState) {
				if test.mnmw {
					state.MountArgs = recoveryMountArgs(state.MountArgs, durabilityCloseSync)
				}
			})
			k8s := k8sfake.NewSimpleClientset(recoveryPV(fixture.active, test.modes, test.attrs))
			fixture.driver.k8s = k8s
			before, err := fixture.driver.readMountState(fixture.active.VolumeID)
			if err != nil {
				t.Fatalf("read before recovery: %v", err)
			}

			fixture.driver.recoverOneNodeMount(context.Background(), fixture.active)

			after, err := fixture.driver.readMountState(fixture.active.VolumeID)
			if err != nil || !reflectMountStatesEqual(before, after) {
				t.Fatalf("healthy recovery changed state: before=%#v after=%#v err=%v", before, after, err)
			}
			assertRecoveryK8sActions(t, k8s.Actions(), 1, 0)
			assertNoNodeStateFirstDestructiveCalls(t, fixture.runtime.Calls())
		})
	}
}

func TestBootRecoveryActiveSkipKeepsCredentialsLazy(t *testing.T) {
	fixture := newNodeStateFirstFixtureWithActive(t, true, func(state *mountState) {
		state.MountArgs = recoveryMountArgs(state.MountArgs, durabilityCloseSync)
	})
	originalIsMountPoint := fixture.runtime.isMountPointFn
	mountChecks := 0
	fixture.runtime.isMountPointFn = func(path string) (bool, error) {
		mountChecks++
		if mountChecks == 1 {
			return false, nil
		}
		return originalIsMountPoint(path)
	}
	k8s := k8sfake.NewSimpleClientset(recoveryPV(
		fixture.active,
		[]corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		map[string]string{
			paramProfile: profileNone, paramDurability: durabilityCloseSync,
			attrSecretName: "missing", attrSecretNamespace: "default",
		},
	))
	fixture.driver.k8s = k8s
	before, _ := fixture.driver.readMountState(fixture.active.VolumeID)

	fixture.driver.recoverOneNodeMount(context.Background(), fixture.active)

	after, err := fixture.driver.readMountState(fixture.active.VolumeID)
	if err != nil || !reflectMountStatesEqual(before, after) {
		t.Fatalf("active skip changed state: before=%#v after=%#v err=%v", before, after, err)
	}
	assertRecoveryK8sActions(t, k8s.Actions(), 1, 0)
	assertNoNodeStateFirstDestructiveCalls(t, fixture.runtime.Calls())
	if mountChecks < 2 {
		t.Fatalf("active recovery made %d mount checks, want fast-path plus skip observation", mountChecks)
	}
}

func TestBootRecoveryStartingPromotionResolvesPVWithoutSecret(t *testing.T) {
	fixture := newStartingReconcileFixture(t)
	canonicalizeRecoveryStateIdentity(t, &fixture.state)
	fixture.mounted = true
	fixture.process = "ready"
	fixture.service = systemdUnitActive
	fixture.state.MountArgs = recoveryMountArgs(fixture.state.MountArgs, durabilityCloseSync)
	fixture.states.states = []mountState{fixture.state}
	fixture.installCallbacks()
	stateDir := t.TempDir()
	store := newMountStateStore(stateDir, newHostRuntime())
	if err := store.Write(fixture.state); err != nil {
		t.Fatalf("write starting state: %v", err)
	}
	k8s := k8sfake.NewSimpleClientset(recoveryPV(
		fixture.state,
		[]corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		map[string]string{paramProfile: profileNone, paramDurability: durabilityCloseSync},
	))
	driver := &Driver{
		cfg: Config{StateDir: stateDir, DriverName: "csi.drive9.ai"},
		k8s: k8s, nodeRuntime: fixture.runtime,
		nodeCapabilities: availableNodeCapabilities(), nodePreflightSet: true,
	}

	driver.recoverOneNodeMount(context.Background(), fixture.state)

	recovered, err := store.Read(fixture.state.VolumeID)
	if err != nil || recovered.Phase != mountStatePhaseActive {
		t.Fatalf("starting recovery = %#v, %v; want active", recovered, err)
	}
	assertRecoveryK8sActions(t, k8s.Actions(), 1, 0)
}

func TestBootRecoveryUnhealthyActiveFetchesSecretAfterSinglePVList(t *testing.T) {
	for _, test := range []struct {
		name  string
		modes []corev1.PersistentVolumeAccessMode
		mnmw  bool
	}{
		{name: "RWO", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
		{name: "RWX", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, mnmw: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newNodeStateFirstFixtureWithActive(t, false, func(state *mountState) {
				if test.mnmw {
					state.MountArgs = recoveryMountArgs(state.MountArgs, durabilityCloseSync)
				}
			})
			attrs := map[string]string{
				attrSecretName: "missing", attrSecretNamespace: "default", paramDurability: durabilityCloseSync,
			}
			if test.mnmw {
				attrs[paramProfile] = profileNone
			}
			k8s := k8sfake.NewSimpleClientset(recoveryPV(fixture.active, test.modes, attrs))
			fixture.driver.k8s = k8s
			before, _ := fixture.driver.readMountState(fixture.active.VolumeID)

			fixture.driver.recoverOneNodeMount(context.Background(), fixture.active)

			after, err := fixture.driver.readMountState(fixture.active.VolumeID)
			if err != nil || !reflectMountStatesEqual(before, after) {
				t.Fatalf("credential failure changed active state: before=%#v after=%#v err=%v", before, after, err)
			}
			assertRecoveryK8sActions(t, k8s.Actions(), 1, 1)
			assertNoNodeStateFirstDestructiveCalls(t, fixture.runtime.Calls())
		})
	}
}

func TestBootRecoveryStartingResumeFetchesSecretAfterSinglePVList(t *testing.T) {
	fixture := newStartingReconcileFixture(t)
	canonicalizeRecoveryStateIdentity(t, &fixture.state)
	fixture.state.MountArgs = recoveryMountArgs(fixture.state.MountArgs, durabilityCloseSync)
	fixture.useRecoveryDesired()
	fixture.service = systemdUnitNotFound
	fixture.installCallbacks()
	stateDir := t.TempDir()
	store := newMountStateStore(stateDir, newHostRuntime())
	if err := store.Write(fixture.state); err != nil {
		t.Fatalf("write starting state: %v", err)
	}
	k8s := k8sfake.NewSimpleClientset(recoveryPV(
		fixture.state,
		[]corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		map[string]string{
			paramProfile: profileNone, paramDurability: durabilityCloseSync,
			attrSecretName: "missing", attrSecretNamespace: "default",
		},
	))
	driver := &Driver{
		cfg: Config{StateDir: stateDir, DriverName: "csi.drive9.ai"},
		k8s: k8s, nodeRuntime: fixture.runtime,
		nodeCapabilities: availableNodeCapabilities(), nodePreflightSet: true,
	}

	driver.recoverOneNodeMount(context.Background(), fixture.state)

	recovered, err := store.Read(fixture.state.VolumeID)
	if err != nil || !reflectMountStatesEqual(fixture.state, recovered) {
		t.Fatalf("credential failure changed starting state: before=%#v after=%#v err=%v", fixture.state, recovered, err)
	}
	assertRecoveryK8sActions(t, k8s.Actions(), 1, 1)
	if runs := countMountSystemdRuns(fixture.runtime.Calls()); runs != 0 {
		t.Fatalf("credential failure launched %d mount services", runs)
	}
}

func TestBootRecoveryRejectsUnprovedModeAndMNMWArgvWithoutMutation(t *testing.T) {
	tests := []struct {
		name    string
		objects func(mountState) []kruntime.Object
		mutate  func(*mountState)
	}{
		{name: "missing PV", objects: func(mountState) []kruntime.Object { return nil }},
		{name: "duplicate PV", objects: func(state mountState) []kruntime.Object {
			return []kruntime.Object{
				recoveryPV(state, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil),
				recoveryPVWithName("duplicate", state, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil),
			}
		}},
		{name: "wrong driver", objects: func(state mountState) []kruntime.Object {
			pv := recoveryPV(state, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
			pv.Spec.CSI.Driver = "other.example.com"
			return []kruntime.Object{pv}
		}},
		{name: "unsupported mode", objects: func(state mountState) []kruntime.Object {
			return []kruntime.Object{recoveryPV(state, []corev1.PersistentVolumeAccessMode{"UnknownWriter"}, nil)}
		}},
		{name: "mixed RWX and read only", objects: func(state mountState) []kruntime.Object {
			return []kruntime.Object{recoveryPV(state, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany, corev1.ReadOnlyMany}, nil)}
		}},
		{name: "MNMW primary mismatch", objects: func(state mountState) []kruntime.Object {
			return []kruntime.Object{recoveryPV(state, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, map[string]string{
				paramProfile: profileNone, paramDurability: durabilityCloseSync,
			})}
		}},
		{name: "MNMW invalid profile", mutate: func(state *mountState) {
			state.MountArgs = recoveryMountArgs(state.MountArgs, durabilityCloseSync)
			for i := range state.MountArgs {
				if i > 0 && state.MountArgs[i-1] == "--profile" {
					state.MountArgs[i] = "coding-agent"
				}
			}
		}, objects: func(state mountState) []kruntime.Object {
			return []kruntime.Object{recoveryPV(state, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, map[string]string{
				paramProfile: "coding-agent", paramDurability: durabilityCloseSync,
			})}
		}},
		{name: "MNMW durability tuning conflict", mutate: func(state *mountState) {
			state.MountArgs = recoveryMountArgs(state.MountArgs, durabilityCloseSync)
		}, objects: func(state mountState) []kruntime.Object {
			return []kruntime.Object{recoveryPV(state, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, map[string]string{
				paramProfile: profileNone, paramDurability: durabilityCloseSync, paramWritebackBatchWindow: "20ms",
			})}
		}},
		{name: "MNMW fallback mismatch", mutate: func(state *mountState) {
			state.MountArgs = recoveryMountArgs(state.MountArgs, durabilityCloseSync)
			state.Reason = mountStartReasonRecovery
			state.FallbackBinaryPath = "/var/lib/drive9-csi/bin/drive9-" + strings.Repeat("b", 64)
			state.FallbackMountArgs = withoutMountArg(state.MountArgs, "--durability")
		}, objects: func(state mountState) []kruntime.Object {
			return []kruntime.Object{recoveryPV(state, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, map[string]string{
				paramProfile: profileNone, paramDurability: durabilityCloseSync,
			})}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newStartingReconcileFixture(t)
			canonicalizeRecoveryStateIdentity(t, &fixture.state)
			fixture.mounted = true
			fixture.process = "ready"
			fixture.service = systemdUnitActive
			if test.mutate != nil {
				test.mutate(&fixture.state)
			}
			fixture.states.states = []mountState{fixture.state}
			fixture.installCallbacks()
			stateDir := t.TempDir()
			store := newMountStateStore(stateDir, newHostRuntime())
			if err := store.Write(fixture.state); err != nil {
				t.Fatalf("write starting state: %v", err)
			}
			statePath, _ := store.statePath(fixture.state.VolumeID)
			before, _ := os.ReadFile(statePath)
			k8s := k8sfake.NewSimpleClientset(test.objects(fixture.state)...)
			driver := &Driver{
				cfg: Config{StateDir: stateDir, DriverName: "csi.drive9.ai"},
				k8s: k8s, nodeRuntime: fixture.runtime,
				nodeCapabilities: availableNodeCapabilities(), nodePreflightSet: true,
			}

			driver.recoverOneNodeMount(context.Background(), fixture.state)

			after, _ := os.ReadFile(statePath)
			if string(after) != string(before) {
				t.Fatalf("fail-closed recovery changed durable state: before=%q after=%q", before, after)
			}
			assertRecoveryK8sActions(t, k8s.Actions(), 1, 0)
			assertNoNodeStateFirstDestructiveCalls(t, fixture.runtime.Calls())
		})
	}
}

func TestBootRecoveryRelaunchPublishesCanonicalDurability(t *testing.T) {
	for _, test := range []struct {
		name       string
		modes      []corev1.PersistentVolumeAccessMode
		profile    string
		durability string
	}{
		{name: "RWO absent", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
		{name: "RWO explicit", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, durability: durabilityWriteSync},
		{name: "MNMW", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, profile: profileNone, durability: durabilityCloseSync},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeDrive9(t)
			defer fake.close()
			secret := fake.k8sSecret("drive9-secret", "default")
			starting := validStartingState(t)
			active := validActiveState(t)
			canonicalizeRecoveryStateIdentity(t, &starting)
			canonicalizeRecoveryStateIdentity(t, &active)
			addContract := func(args []string) []string {
				if test.profile != "" {
					return recoveryMountArgs(args, test.durability)
				}
				if test.durability == "" {
					return args
				}
				return append(args[:len(args)-2], "--durability", test.durability,
					args[len(args)-2], args[len(args)-1])
			}
			starting.MountArgs = addContract(starting.MountArgs)
			active.MountArgs = addContract(active.MountArgs)
			fake.mkdir(active.RemoteRoot)
			fake.putJSON(markerPath(active.RemoteRoot), volumeMarker{
				Version: 1, Driver: "csi.drive9.ai",
				VolumeID: active.VolumeID, RemoteRoot: active.RemoteRoot,
			})

			stateDir := t.TempDir()
			store := newMountStateStore(stateDir, newHostRuntime())
			if err := store.Write(starting); err != nil {
				t.Fatalf("write starting state: %v", err)
			}
			if err := store.Write(active); err != nil {
				t.Fatalf("write active state: %v", err)
			}
			attrs := map[string]string{
				attrSecretName: secret.Name, attrSecretNamespace: secret.Namespace,
			}
			if test.profile != "" {
				attrs[paramProfile] = test.profile
			}
			if test.durability != "" {
				attrs[paramDurability] = test.durability
			}
			k8s := k8sfake.NewSimpleClientset(secret, recoveryPV(active, test.modes, attrs))
			launch := newMountLaunchFixture(t, "")
			launch.attemptNumber = 1
			originalExec := launch.runtime.execFn
			launch.runtime.execFn = func(ctx context.Context, command hostCommand) (hostCommandResult, error) {
				inner := hostInnerCommand(command)
				if containsArgument(inner, "--property=Description") {
					current, err := store.Read(active.VolumeID)
					if err != nil {
						return hostCommandResult{}, err
					}
					return systemdDescriptionResult(current), nil
				}
				return originalExec(ctx, command)
			}
			originalRead := launch.runtime.readFileFn
			launch.runtime.readFileFn = func(path string) ([]byte, error) {
				if path == hostProcPIDPath(launch.pid, "cgroup") {
					current, err := store.Read(active.VolumeID)
					if err != nil {
						return nil, err
					}
					return []byte("0::/system.slice/" + current.SystemdUnit + "\n"), nil
				}
				return originalRead(path)
			}
			driver := &Driver{
				cfg: Config{StateDir: stateDir, DriverName: "csi.drive9.ai"},
				k8s: k8s, nodeRuntime: launch.runtime,
				nodeCapabilities: availableNodeCapabilities(), nodePreflightSet: true,
			}

			driver.recoverOneNodeMount(context.Background(), active)

			recovered, err := store.Read(active.VolumeID)
			if err != nil || recovered.Phase != mountStatePhaseActive || recovered.BinaryPath != launch.drive9Path {
				t.Fatalf("boot relaunch = %#v, %v; want desired active mount", recovered, err)
			}
			assertRecoveryK8sActions(t, k8s.Actions(), 1, 1)
			if runs := countMountSystemdRuns(launch.runtime.Calls()); runs != 1 {
				t.Fatalf("boot relaunch systemd-run count = %d, want 1", runs)
			}
			wantArgsBody := encodeNULTerminated(append([]string{launch.drive9Path}, recovered.MountArgs...))
			if got := launch.publishedBody(t, ".args."); string(got) != string(wantArgsBody) {
				t.Fatalf("published recovery argv = %q, want %q", got, wantArgsBody)
			}
			if test.durability == "" {
				if stringSliceContains(recovered.MountArgs, "--durability") {
					t.Fatalf("absent RWO durability emitted argv: %q", recovered.MountArgs)
				}
			} else if err := validateCanonicalMountOption(
				recovered.MountArgs, "--durability", test.durability,
			); err != nil {
				t.Fatalf("recovered argv: %v (args=%q)", err, recovered.MountArgs)
			}
			if test.profile != "" {
				if err := validateCanonicalMountOption(recovered.MountArgs, "--profile", test.profile); err != nil {
					t.Fatalf("recovered argv: %v (args=%q)", err, recovered.MountArgs)
				}
			}
		})
	}
}

func recoveryPV(
	state mountState,
	modes []corev1.PersistentVolumeAccessMode,
	extra map[string]string,
) *corev1.PersistentVolume {
	return recoveryPVWithName("pv-"+safeFileName(state.VolumeID), state, modes, extra)
}

func recoveryPVWithName(
	name string,
	state mountState,
	modes []corev1.PersistentVolumeAccessMode,
	extra map[string]string,
) *corev1.PersistentVolume {
	attrs := map[string]string{"remoteRoot": state.RemoteRoot}
	for key, value := range extra {
		attrs[key] = value
	}
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: modes,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver: "csi.drive9.ai", VolumeHandle: state.VolumeID, VolumeAttributes: attrs,
				},
			},
		},
	}
}

func recoveryMountArgs(args []string, durability string) []string {
	args = append([]string(nil), args...)
	return append(args[:len(args)-2],
		"--profile", profileNone, "--durability", durability,
		args[len(args)-2], args[len(args)-1],
	)
}

func canonicalizeRecoveryStateIdentity(t *testing.T, state *mountState) {
	t.Helper()
	state.VolumeID = volumeIDForRemoteRoot(state.RemoteRoot)
	names, err := newVolumeHostNames(state.VolumeID, state.AttemptID)
	if err != nil {
		t.Fatalf("newVolumeHostNames(): %v", err)
	}
	state.SystemdUnit = names.SystemdUnit
	if state.Phase == mountStatePhaseStarting {
		state.EnvPath, state.ArgsPath = names.EnvPath, names.ArgsPath
	}
}

func assertRecoveryK8sActions(t *testing.T, actions []k8stesting.Action, pvLists int, secretGets int) {
	t.Helper()
	gotPVLists := 0
	gotSecretGets := 0
	for _, action := range actions {
		if action.GetVerb() == "list" && action.GetResource().Resource == "persistentvolumes" {
			gotPVLists++
		}
		if action.GetVerb() == "get" && action.GetResource().Resource == "secrets" {
			gotSecretGets++
		}
	}
	if gotPVLists != pvLists || gotSecretGets != secretGets {
		t.Fatalf("Kubernetes actions: PV lists=%d Secret gets=%d, want %d/%d",
			gotPVLists, gotSecretGets, pvLists, secretGets)
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
