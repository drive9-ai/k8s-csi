package main

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestInstallHostBinariesFirstInstall(t *testing.T) {
	options := newInstallerTestOptions(t, "first")

	result, err := installHostBinaries(options)
	if err != nil {
		t.Fatalf("installHostBinaries(): %v", err)
	}

	drive9Body, err := os.ReadFile(options.Drive9Source)
	if err != nil {
		t.Fatalf("read Drive9 source: %v", err)
	}
	sum := sha256.Sum256(drive9Body)
	wantDigest := hex.EncodeToString(sum[:])
	wantDrive9Path := filepath.Join(options.HostStateDir, "bin", "drive9-"+wantDigest)
	if result.Digest != wantDigest || result.Drive9Path != wantDrive9Path {
		t.Fatalf("result = %#v, want digest=%s path=%s", result, wantDigest, wantDrive9Path)
	}
	assertFileEquals(t, wantDrive9Path, drive9Body)
	assertExecutableRegularFile(t, wantDrive9Path)

	launcherBody, err := os.ReadFile(options.LauncherSource)
	if err != nil {
		t.Fatalf("read launcher source: %v", err)
	}
	wantLauncherPath := filepath.Join(options.HostStateDir, "bin", "drive9-csi-launcher")
	if result.LauncherPath != wantLauncherPath {
		t.Fatalf("launcher path = %q, want %q", result.LauncherPath, wantLauncherPath)
	}
	assertFileEquals(t, wantLauncherPath, launcherBody)
	assertExecutableRegularFile(t, wantLauncherPath)

	fusermountBody, err := os.ReadFile(options.FusermountSource)
	if err != nil {
		t.Fatalf("read fusermount source: %v", err)
	}
	wantFusermountPath := filepath.Join(options.HostStateDir, "bin", "fusermount3")
	if result.FusermountPath != wantFusermountPath {
		t.Fatalf("fusermount path = %q, want %q", result.FusermountPath, wantFusermountPath)
	}
	assertFileEquals(t, wantFusermountPath, fusermountBody)
	assertExecutableRegularFile(t, wantFusermountPath)

	target, err := os.Readlink(filepath.Join(options.HostStateDir, "bin", "drive9"))
	if err != nil {
		t.Fatalf("read desired symlink: %v", err)
	}
	if target != filepath.Base(wantDrive9Path) || filepath.IsAbs(target) {
		t.Fatalf("desired target = %q, want relative %q", target, filepath.Base(wantDrive9Path))
	}
	assertNoInstallerTemps(t, filepath.Join(options.HostStateDir, "bin"))
}

func TestInstallHostBinariesIdempotentReinstallAndDesiredChange(t *testing.T) {
	options := newInstallerTestOptions(t, "old")
	oldResult, err := installHostBinaries(options)
	if err != nil {
		t.Fatalf("install old: %v", err)
	}
	repeated, err := installHostBinaries(options)
	if err != nil {
		t.Fatalf("reinstall old: %v", err)
	}
	if repeated != oldResult {
		t.Fatalf("reinstall result = %#v, want %#v", repeated, oldResult)
	}

	writeSyntheticExecutable(t, options.Drive9Source, options.TargetArch, false, "new")
	newResult, err := installHostBinaries(options)
	if err != nil {
		t.Fatalf("install new: %v", err)
	}
	if newResult.Digest == oldResult.Digest {
		t.Fatal("new Drive9 content retained old digest")
	}
	assertExecutableRegularFile(t, oldResult.Drive9Path)
	assertExecutableRegularFile(t, newResult.Drive9Path)
	target, err := os.Readlink(filepath.Join(options.HostStateDir, "bin", "drive9"))
	if err != nil {
		t.Fatalf("read desired symlink: %v", err)
	}
	if target != filepath.Base(newResult.Drive9Path) {
		t.Fatalf("desired target = %q, want %q", target, filepath.Base(newResult.Drive9Path))
	}
	assertNoInstallerTemps(t, filepath.Join(options.HostStateDir, "bin"))
}

func TestInstallHostBinariesRejectsInvalidSources(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, installHostBinariesOptions)
	}{
		{
			name: "drive9 not executable",
			mutate: func(t *testing.T, options installHostBinariesOptions) {
				if err := os.Chmod(options.Drive9Source, 0o644); err != nil {
					t.Fatalf("chmod: %v", err)
				}
			},
		},
		{
			name: "drive9 symlink",
			mutate: func(t *testing.T, options installHostBinariesOptions) {
				replaceWithSymlink(t, options.Drive9Source)
			},
		},
		{
			name: "launcher symlink",
			mutate: func(t *testing.T, options installHostBinariesOptions) {
				replaceWithSymlink(t, options.LauncherSource)
			},
		},
		{
			name: "fusermount symlink",
			mutate: func(t *testing.T, options installHostBinariesOptions) {
				replaceWithSymlink(t, options.FusermountSource)
			},
		},
		{
			name: "fusermount not executable",
			mutate: func(t *testing.T, options installHostBinariesOptions) {
				if err := os.Chmod(options.FusermountSource, 0o644); err != nil {
					t.Fatalf("chmod: %v", err)
				}
			},
		},
		{
			name: "fusermount wrong machine",
			mutate: func(t *testing.T, options installHostBinariesOptions) {
				otherArch := "amd64"
				if options.TargetArch == "amd64" {
					otherArch = "arm64"
				}
				writeSyntheticExecutable(t, options.FusermountSource, otherArch, true, "wrong-fusermount-machine")
			},
		},
		{
			name: "wrong machine",
			mutate: func(t *testing.T, options installHostBinariesOptions) {
				otherArch := "amd64"
				if options.TargetArch == "amd64" {
					otherArch = "arm64"
				}
				writeSyntheticExecutable(t, options.Drive9Source, otherArch, false, "wrong-machine")
			},
		},
		{
			name: "wrong class",
			mutate: func(t *testing.T, options installHostBinariesOptions) {
				body, err := os.ReadFile(options.Drive9Source)
				if err != nil {
					t.Fatalf("read source: %v", err)
				}
				body[elf.EI_CLASS] = byte(elf.ELFCLASS32)
				if err := os.WriteFile(options.Drive9Source, body, 0o755); err != nil {
					t.Fatalf("write source: %v", err)
				}
			},
		},
		{
			name: "dynamic interpreter",
			mutate: func(t *testing.T, options installHostBinariesOptions) {
				writeSyntheticExecutable(t, options.Drive9Source, options.TargetArch, true, "dynamic")
			},
		},
		{
			name: "not elf",
			mutate: func(t *testing.T, options installHostBinariesOptions) {
				if err := os.WriteFile(options.Drive9Source, []byte("not-elf"), 0o755); err != nil {
					t.Fatalf("write source: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := newInstallerTestOptions(t, test.name)
			test.mutate(t, options)
			if _, err := installHostBinaries(options); err == nil {
				t.Fatal("installHostBinaries() succeeded, want error")
			}
			assertDesiredAbsent(t, filepath.Join(options.HostStateDir, "bin", "drive9"))
		})
	}
}

func TestInstallHostBinariesRejectsUnsafeDestinations(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, installHostBinariesOptions, installHostBinariesResult)
	}{
		{
			name: "versioned symlink",
			setup: func(t *testing.T, options installHostBinariesOptions, old installHostBinariesResult) {
				newBody, err := os.ReadFile(options.Drive9Source)
				if err != nil {
					t.Fatalf("read new source: %v", err)
				}
				sum := sha256.Sum256(newBody)
				path := filepath.Join(options.HostStateDir, "bin", "drive9-"+hex.EncodeToString(sum[:]))
				if err := os.Symlink(filepath.Base(old.Drive9Path), path); err != nil {
					t.Fatalf("create versioned symlink: %v", err)
				}
			},
		},
		{
			name: "launcher symlink",
			setup: func(t *testing.T, options installHostBinariesOptions, _ installHostBinariesResult) {
				path := filepath.Join(options.HostStateDir, "bin", "drive9-csi-launcher")
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove launcher: %v", err)
				}
				if err := os.Symlink(filepath.Base(options.LauncherSource), path); err != nil {
					t.Fatalf("create launcher symlink: %v", err)
				}
			},
		},
		{
			name: "fusermount symlink",
			setup: func(t *testing.T, options installHostBinariesOptions, _ installHostBinariesResult) {
				path := filepath.Join(options.HostStateDir, "bin", "fusermount3")
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove fusermount: %v", err)
				}
				if err := os.Symlink(filepath.Base(options.FusermountSource), path); err != nil {
					t.Fatalf("create fusermount symlink: %v", err)
				}
			},
		},
		{
			name: "desired regular file",
			setup: func(t *testing.T, options installHostBinariesOptions, _ installHostBinariesResult) {
				path := filepath.Join(options.HostStateDir, "bin", "drive9")
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove desired: %v", err)
				}
				if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
					t.Fatalf("write desired: %v", err)
				}
			},
		},
		{
			name: "digest mismatch",
			setup: func(t *testing.T, options installHostBinariesOptions, _ installHostBinariesResult) {
				newBody, err := os.ReadFile(options.Drive9Source)
				if err != nil {
					t.Fatalf("read new source: %v", err)
				}
				sum := sha256.Sum256(newBody)
				path := filepath.Join(options.HostStateDir, "bin", "drive9-"+hex.EncodeToString(sum[:]))
				if err := os.WriteFile(path, []byte("corrupt"), 0o755); err != nil {
					t.Fatalf("write corrupt destination: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := newInstallerTestOptions(t, "old")
			oldResult, err := installHostBinaries(options)
			if err != nil {
				t.Fatalf("install old: %v", err)
			}
			writeSyntheticExecutable(t, options.Drive9Source, options.TargetArch, false, "new")
			test.setup(t, options, oldResult)

			_, err = installHostBinaries(options)
			if err == nil {
				t.Fatal("installHostBinaries() succeeded, want error")
			}

			desiredPath := filepath.Join(options.HostStateDir, "bin", "drive9")
			if test.name == "desired regular file" {
				body, readErr := os.ReadFile(desiredPath)
				if readErr != nil || string(body) != "keep" {
					t.Fatalf("unsafe desired changed: %q, %v", body, readErr)
				}
			} else {
				target, readErr := os.Readlink(desiredPath)
				if readErr != nil || target != filepath.Base(oldResult.Drive9Path) {
					t.Fatalf("desired target = %q, %v; want %q", target, readErr, filepath.Base(oldResult.Drive9Path))
				}
			}
		})
	}
}

func TestInstallHostBinariesPreservesDesiredAcrossFailurePoints(t *testing.T) {
	steps := []installerStep{
		installerStepDrive9Temp,
		installerStepDrive9Write,
		installerStepDrive9FileSync,
		installerStepDrive9Rename,
		installerStepDrive9DirSync,
		installerStepLauncherTemp,
		installerStepLauncherWrite,
		installerStepLauncherFileSync,
		installerStepLauncherRename,
		installerStepLauncherDirSync,
		installerStepFusermountTemp,
		installerStepFusermountWrite,
		installerStepFusermountFileSync,
		installerStepFusermountRename,
		installerStepFusermountDirSync,
		installerStepDesiredTemp,
		installerStepDesiredRename,
		installerStepDesiredDirSync,
	}

	for _, step := range steps {
		t.Run(string(step), func(t *testing.T) {
			options := newInstallerTestOptions(t, "old")
			oldResult, err := installHostBinaries(options)
			if err != nil {
				t.Fatalf("install old: %v", err)
			}
			writeSyntheticExecutable(t, options.Drive9Source, options.TargetArch, false, "new")

			injected := errors.New("injected " + string(step))
			_, err = installHostBinariesWithFault(options, func(current installerStep) error {
				if current == step {
					return injected
				}
				return nil
			})
			if !errors.Is(err, injected) {
				t.Fatalf("install error = %v, want %v", err, injected)
			}

			desiredPath := filepath.Join(options.HostStateDir, "bin", "drive9")
			target, readErr := os.Readlink(desiredPath)
			if readErr != nil {
				t.Fatalf("read desired: %v", readErr)
			}
			if target != filepath.Base(oldResult.Drive9Path) {
				t.Fatalf("desired target = %q, want %q", target, filepath.Base(oldResult.Drive9Path))
			}
			assertNoInstallerTemps(t, filepath.Join(options.HostStateDir, "bin"))
		})
	}
}

func TestInstallHostBinariesRestoresLauncherAfterDirectorySyncFailure(t *testing.T) {
	options := newInstallerTestOptions(t, "old")
	result, err := installHostBinaries(options)
	if err != nil {
		t.Fatalf("install old: %v", err)
	}
	oldLauncher, err := os.ReadFile(result.LauncherPath)
	if err != nil {
		t.Fatalf("read old launcher: %v", err)
	}
	writeSyntheticExecutable(t, options.LauncherSource, options.TargetArch, false, "new-launcher")

	injected := errors.New("launcher directory sync")
	_, err = installHostBinariesWithFault(options, func(step installerStep) error {
		if step == installerStepLauncherDirSync {
			return injected
		}
		return nil
	})
	if !errors.Is(err, injected) {
		t.Fatalf("install error = %v, want %v", err, injected)
	}
	assertFileEquals(t, result.LauncherPath, oldLauncher)
}

func TestInstallHostBinariesRestoresFusermountAfterDirectorySyncFailure(t *testing.T) {
	options := newInstallerTestOptions(t, "old")
	result, err := installHostBinaries(options)
	if err != nil {
		t.Fatalf("install old: %v", err)
	}
	oldFusermount, err := os.ReadFile(result.FusermountPath)
	if err != nil {
		t.Fatalf("read old fusermount: %v", err)
	}
	writeSyntheticExecutable(t, options.FusermountSource, options.TargetArch, true, "new-fusermount")

	injected := errors.New("fusermount directory sync")
	_, err = installHostBinariesWithFault(options, func(step installerStep) error {
		if step == installerStepFusermountDirSync {
			return injected
		}
		return nil
	})
	if !errors.Is(err, injected) {
		t.Fatalf("install error = %v, want %v", err, injected)
	}
	assertFileEquals(t, result.FusermountPath, oldFusermount)
}

func TestInstallHostBinariesConcurrentInstallers(t *testing.T) {
	options := newInstallerTestOptions(t, "concurrent")
	const installers = 12
	var wg sync.WaitGroup
	errs := make(chan error, installers)
	results := make(chan installHostBinariesResult, installers)
	wg.Add(installers)
	for i := 0; i < installers; i++ {
		go func() {
			defer wg.Done()
			result, err := installHostBinaries(options)
			errs <- err
			results <- result
		}()
	}
	wg.Wait()
	close(errs)
	close(results)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent install: %v", err)
		}
	}
	var first installHostBinariesResult
	for result := range results {
		if first == (installHostBinariesResult{}) {
			first = result
			continue
		}
		if result != first {
			t.Fatalf("result = %#v, want %#v", result, first)
		}
	}
	assertExecutableRegularFile(t, first.Drive9Path)
	assertExecutableRegularFile(t, first.LauncherPath)
	assertExecutableRegularFile(t, first.FusermountPath)
	assertNoInstallerTemps(t, filepath.Join(options.HostStateDir, "bin"))
}

func TestInstallHostBinariesDispatchesBeforeKubernetesConfiguration(t *testing.T) {
	options := newInstallerTestOptions(t, "dispatch")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Setenv("CSI_ENDPOINT", "not-a-csi-endpoint")
	t.Setenv("NODE_ID", "")
	t.Setenv("DRIVE9_API_KEY", "")

	var stdout bytes.Buffer
	err := run([]string{
		"install-host-binaries",
		"--host-state-dir=" + options.HostStateDir,
		"--drive9-source=" + options.Drive9Source,
		"--launcher-source=" + options.LauncherSource,
		"--fusermount-source=" + options.FusermountSource,
	}, &stdout)
	if err != nil {
		t.Fatalf("run install-host-binaries: %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "drive9_digest=") ||
		!strings.Contains(output, "drive9_path=") ||
		!strings.Contains(output, "launcher_path=") ||
		!strings.Contains(output, "fusermount_path=") {
		t.Fatalf("installer output = %q", output)
	}
	if strings.Contains(output, string(syntheticELF64(machineForArch(t, options.TargetArch), false, "dispatch"))) {
		t.Fatal("installer output contains source bytes")
	}
}

func TestVerifyHostBinaryCommandUsesStaticELFValidation(t *testing.T) {
	fixture := newInstallerTestOptions(t, "verify")
	if err := run([]string{
		"verify-host-binary",
		"--path=" + fixture.Drive9Source,
		"--target-arch=" + fixture.TargetArch,
	}, nil); err != nil {
		t.Fatalf("verify-host-binary: %v", err)
	}

	writeSyntheticExecutable(t, fixture.Drive9Source, fixture.TargetArch, true, "dynamic")
	if err := run([]string{
		"verify-host-binary",
		"--path=" + fixture.Drive9Source,
		"--target-arch=" + fixture.TargetArch,
	}, nil); err == nil || !strings.Contains(err.Error(), "PT_INTERP") {
		t.Fatalf("dynamic verify-host-binary error = %v, want PT_INTERP rejection", err)
	}
}

func newInstallerTestOptions(t *testing.T, payload string) installHostBinariesOptions {
	t.Helper()
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	if err := os.Mkdir(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	options := installHostBinariesOptions{
		HostStateDir:     filepath.Join(root, "host-state"),
		Drive9Source:     filepath.Join(sourceDir, "drive9"),
		LauncherSource:   filepath.Join(sourceDir, "drive9-csi-launcher"),
		FusermountSource: filepath.Join(sourceDir, "fusermount3"),
		TargetArch:       runtime.GOARCH,
	}
	if err := os.Mkdir(options.HostStateDir, 0o755); err != nil {
		t.Fatalf("mkdir host state: %v", err)
	}
	writeSyntheticExecutable(t, options.Drive9Source, options.TargetArch, false, "drive9-"+payload)
	writeSyntheticExecutable(t, options.LauncherSource, options.TargetArch, false, "launcher-"+payload)
	writeSyntheticExecutable(t, options.FusermountSource, options.TargetArch, true, "fusermount-"+payload)
	return options
}

func writeSyntheticExecutable(t *testing.T, path string, arch string, interpreter bool, payload string) {
	t.Helper()
	body := syntheticELF64(machineForArch(t, arch), interpreter, payload)
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatalf("write synthetic ELF: %v", err)
	}
}

func machineForArch(t *testing.T, arch string) elf.Machine {
	t.Helper()
	switch arch {
	case "amd64":
		return elf.EM_X86_64
	case "arm64":
		return elf.EM_AARCH64
	default:
		t.Fatalf("unsupported test architecture %q", arch)
		return elf.EM_NONE
	}
}

func syntheticELF64(machine elf.Machine, interpreter bool, payload string) []byte {
	const (
		elfHeaderSize     = 64
		programHeaderSize = 56
	)
	programCount := 0
	total := elfHeaderSize
	if interpreter {
		programCount = 1
		total += programHeaderSize + len("/lib64/ld-linux.so.2\x00")
	}
	body := make([]byte, total)
	copy(body[:4], []byte{0x7f, 'E', 'L', 'F'})
	body[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	body[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	body[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(body[16:18], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(body[18:20], uint16(machine))
	binary.LittleEndian.PutUint32(body[20:24], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(body[52:54], elfHeaderSize)
	binary.LittleEndian.PutUint16(body[54:56], programHeaderSize)
	binary.LittleEndian.PutUint16(body[56:58], uint16(programCount))
	binary.LittleEndian.PutUint16(body[58:60], 64)
	if interpreter {
		interpreterOffset := elfHeaderSize + programHeaderSize
		binary.LittleEndian.PutUint64(body[32:40], elfHeaderSize)
		binary.LittleEndian.PutUint32(body[elfHeaderSize:elfHeaderSize+4], uint32(elf.PT_INTERP))
		binary.LittleEndian.PutUint64(body[elfHeaderSize+8:elfHeaderSize+16], uint64(interpreterOffset))
		binary.LittleEndian.PutUint64(body[elfHeaderSize+32:elfHeaderSize+40], uint64(len("/lib64/ld-linux.so.2\x00")))
		copy(body[interpreterOffset:], []byte("/lib64/ld-linux.so.2\x00"))
	}
	return append(body, []byte(payload)...)
}

func replaceWithSymlink(t *testing.T, path string) {
	t.Helper()
	target := path + ".target"
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	if err := os.Rename(path, target); err != nil {
		t.Fatalf("rename source: %v", err)
	}
	if err := os.WriteFile(target, body, 0o755); err != nil {
		t.Fatalf("rewrite target: %v", err)
	}
	if err := os.Symlink(filepath.Base(target), path); err != nil {
		t.Fatalf("create source symlink: %v", err)
	}
}

func assertFileEquals(t *testing.T, path string, want []byte) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("%s content differs", path)
	}
}

func assertExecutableRegularFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("%s mode = %s, want executable regular file", path, info.Mode())
	}
}

func assertDesiredAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("desired path exists or lstat failed: %v", err)
	}
}

func assertNoInstallerTemps(t *testing.T, binDir string) {
	t.Helper()
	entries, err := os.ReadDir(binDir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("read bin dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temporary installer artifact remains: %s", entry.Name())
		}
	}
}
