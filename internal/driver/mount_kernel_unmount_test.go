package driver

import (
	"context"
	"reflect"
	"testing"
)

func TestMountStopKernelUnmountUsesCanonicalHostNamespaceHelper(t *testing.T) {
	runtime := &fakeHostRuntime{}
	var commands []hostCommand
	runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		commands = append(commands, command)
		return hostCommandResult{}, nil
	}
	stopper := newMountStopper(runtime, &recordingMountStateStore{})
	state := validActiveState(t)
	if err := stopper.runKernelUnmount(context.Background(), state, false); err != nil {
		t.Fatalf("runKernelUnmount(false): %v", err)
	}
	if err := stopper.runKernelUnmount(context.Background(), state, true); err != nil {
		t.Fatalf("runKernelUnmount(true): %v", err)
	}
	want := []hostCommand{
		hostNamespaceCommand(hostLauncherPath, "host-unmount", "--", state.StagingTarget),
		hostNamespaceCommand(hostLauncherPath, "host-unmount", "--lazy", "--", state.StagingTarget),
	}
	if len(commands) != len(want) {
		t.Fatalf("kernel unmount commands = %#v", commands)
	}
	for i := range want {
		if !reflect.DeepEqual(commands[i], want[i]) {
			t.Fatalf("command[%d] = %#v, want %#v", i, commands[i], want[i])
		}
		inner := hostInnerCommand(commands[i])
		if len(inner) < 2 || inner[0] != hostLauncherPath || inner[1] != "host-unmount" {
			t.Fatalf("command[%d] bypassed static host unmount helper: %#v", i, commands[i])
		}
	}
}
