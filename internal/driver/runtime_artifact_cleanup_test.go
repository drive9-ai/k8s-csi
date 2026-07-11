package driver

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveVerifiedDeadRuntimeArtifacts(t *testing.T) {
	tests := []struct {
		name               string
		mounted            bool
		processMode        os.FileMode
		processPresent     bool
		socketMode         os.FileMode
		socketPresent      bool
		livePID            bool
		wrongProcessTarget bool
		differentDeadPID   bool
		wantError          bool
		wantRemoved        int
	}{
		{
			name:           "validated dead process state and socket",
			processMode:    0o600,
			processPresent: true,
			socketMode:     os.ModeSocket | 0o600,
			socketPresent:  true,
			wantRemoved:    2,
		},
		{
			name:          "orphan socket without process state",
			socketMode:    os.ModeSocket | 0o600,
			socketPresent: true,
			wantRemoved:   1,
		},
		{
			name:             "stale dead process state with replaced PID",
			processMode:      0o600,
			processPresent:   true,
			socketMode:       os.ModeSocket | 0o600,
			socketPresent:    true,
			differentDeadPID: true,
			wantRemoved:      2,
		},
		{
			name:           "live recorded PID",
			processMode:    0o600,
			processPresent: true,
			socketMode:     os.ModeSocket | 0o600,
			socketPresent:  true,
			livePID:        true,
			wantError:      true,
		},
		{
			name:           "process state symlink",
			processMode:    os.ModeSymlink | 0o777,
			processPresent: true,
			socketMode:     os.ModeSocket | 0o600,
			socketPresent:  true,
			wantError:      true,
		},
		{
			name:           "socket wrong type",
			processMode:    0o600,
			processPresent: true,
			socketMode:     0o600,
			socketPresent:  true,
			wantError:      true,
		},
		{
			name:               "process state target mismatch",
			processMode:        0o600,
			processPresent:     true,
			socketMode:         os.ModeSocket | 0o600,
			socketPresent:      true,
			wrongProcessTarget: true,
			wantError:          true,
		},
		{
			name:           "mount still present",
			mounted:        true,
			processMode:    0o600,
			processPresent: true,
			socketMode:     os.ModeSocket | 0o600,
			socketPresent:  true,
			wantError:      true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := validActiveState(t)
			processPath := state.ProcessStatePath
			socketPath := state.ControlSocketPath
			var removed []string
			runtime := &fakeHostRuntime{}
			runtime.isMountPointFn = func(string) (bool, error) {
				return test.mounted, nil
			}
			runtime.lstatFn = func(path string) (os.FileInfo, error) {
				switch path {
				case processPath:
					if !test.processPresent {
						return nil, os.ErrNotExist
					}
					return fakeHostFileInfo{name: filepath.Base(path), mode: test.processMode}, nil
				case socketPath:
					if !test.socketPresent {
						return nil, os.ErrNotExist
					}
					return fakeHostFileInfo{name: filepath.Base(path), mode: test.socketMode}, nil
				default:
					return nil, errors.New("unexpected lstat")
				}
			}
			runtime.readFileFn = func(path string) ([]byte, error) {
				if path == processPath {
					target := state.StagingTarget
					if test.wrongProcessTarget {
						target += "-other"
					}
					pid := state.PID
					if test.differentDeadPID {
						pid++
					}
					return json.Marshal(drive9ProcessState{
						PID:           pid,
						Component:     "drive9-fuse",
						MountKind:     "fuse",
						MountPoint:    target,
						ControlSocket: socketPath,
					})
				}
				if path == hostProcPIDPath(state.PID, "stat") && test.livePID {
					return []byte(hostProcStatLine(state.PID, "drive9 mount", state.PIDStartTime)), nil
				}
				return nil, os.ErrNotExist
			}
			runtime.removeFn = func(path string) error {
				removed = append(removed, path)
				return nil
			}

			err := removeVerifiedDeadRuntimeArtifacts(runtime, state)
			if test.wantError && err == nil {
				t.Fatal("removeVerifiedDeadRuntimeArtifacts() succeeded")
			}
			if !test.wantError && err != nil {
				t.Fatalf("removeVerifiedDeadRuntimeArtifacts(): %v", err)
			}
			if len(removed) != test.wantRemoved {
				t.Fatalf("removed paths = %v, want %d", removed, test.wantRemoved)
			}
		})
	}
}
