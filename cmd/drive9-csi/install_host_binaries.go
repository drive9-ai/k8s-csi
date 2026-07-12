package main

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

type installHostBinariesOptions struct {
	HostStateDir   string
	Drive9Source   string
	LauncherSource string
	TargetArch     string
}

type installHostBinariesResult struct {
	Digest       string
	Drive9Path   string
	LauncherPath string
}

type installerStep string

const (
	installerStepDrive9Temp       installerStep = "drive9-temp"
	installerStepDrive9Write      installerStep = "drive9-write"
	installerStepDrive9FileSync   installerStep = "drive9-file-sync"
	installerStepDrive9Rename     installerStep = "drive9-rename"
	installerStepDrive9DirSync    installerStep = "drive9-dir-sync"
	installerStepLauncherTemp     installerStep = "launcher-temp"
	installerStepLauncherWrite    installerStep = "launcher-write"
	installerStepLauncherFileSync installerStep = "launcher-file-sync"
	installerStepLauncherRename   installerStep = "launcher-rename"
	installerStepLauncherDirSync  installerStep = "launcher-dir-sync"
	installerStepDesiredTemp      installerStep = "desired-temp"
	installerStepDesiredRename    installerStep = "desired-rename"
	installerStepDesiredDirSync   installerStep = "desired-dir-sync"
)

type installerFault func(installerStep) error

var versionedDrive9NamePattern = regexp.MustCompile("^drive9-[0-9a-f]{64}$")

func runInstallHostBinariesCommand(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("install-host-binaries", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var options installHostBinariesOptions
	flags.StringVar(&options.HostStateDir, "host-state-dir", "", "host Drive9 CSI state directory")
	flags.StringVar(&options.Drive9Source, "drive9-source", "", "Drive9 source binary")
	flags.StringVar(&options.LauncherSource, "launcher-source", "", "Drive9 CSI launcher source binary")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("install-host-binaries: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	options.TargetArch = runtime.GOARCH
	result, err := installHostBinaries(options)
	if err != nil {
		return err
	}
	if stdout == nil {
		stdout = io.Discard
	}
	_, err = fmt.Fprintf(stdout,
		"drive9_digest=%s\ndrive9_path=%s\nlauncher_path=%s\n",
		result.Digest,
		result.Drive9Path,
		result.LauncherPath,
	)
	return err
}

func installHostBinaries(options installHostBinariesOptions) (installHostBinariesResult, error) {
	return installHostBinariesWithFault(options, nil)
}

func installHostBinariesWithFault(options installHostBinariesOptions, fault installerFault) (installHostBinariesResult, error) {
	if err := validateInstallHostBinariesOptions(options); err != nil {
		return installHostBinariesResult{}, err
	}
	if err := validateDirectory(options.HostStateDir, "host state directory"); err != nil {
		return installHostBinariesResult{}, err
	}

	binDir := filepath.Join(options.HostStateDir, "bin")
	if err := ensureInstallerBinDir(binDir); err != nil {
		return installHostBinariesResult{}, err
	}
	dir, err := os.Open(binDir)
	if err != nil {
		return installHostBinariesResult{}, fmt.Errorf("open host binary directory: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := unix.Flock(int(dir.Fd()), unix.LOCK_EX); err != nil {
		return installHostBinariesResult{}, fmt.Errorf("lock host binary directory: %w", err)
	}
	defer func() { _ = unix.Flock(int(dir.Fd()), unix.LOCK_UN) }()

	drive9Body, drive9Digest, err := readValidatedELF(options.Drive9Source, options.TargetArch)
	if err != nil {
		return installHostBinariesResult{}, fmt.Errorf("validate Drive9 source: %w", err)
	}
	launcherBody, _, err := readValidatedELF(options.LauncherSource, options.TargetArch)
	if err != nil {
		return installHostBinariesResult{}, fmt.Errorf("validate launcher source: %w", err)
	}
	drive9Name := "drive9-" + drive9Digest
	drive9Path := filepath.Join(binDir, drive9Name)
	if err := installVersionedDrive9(dir, drive9Path, drive9Body, drive9Digest, fault); err != nil {
		return installHostBinariesResult{}, err
	}

	launcherPath := filepath.Join(binDir, "drive9-csi-launcher")
	if err := replaceLauncher(dir, launcherPath, launcherBody, fault); err != nil {
		return installHostBinariesResult{}, err
	}
	desiredPath := filepath.Join(binDir, "drive9")
	if err := replaceDesiredSymlink(dir, desiredPath, drive9Name, fault); err != nil {
		return installHostBinariesResult{}, err
	}

	return installHostBinariesResult{
		Digest:       drive9Digest,
		Drive9Path:   drive9Path,
		LauncherPath: launcherPath,
	}, nil
}

func validateInstallHostBinariesOptions(options installHostBinariesOptions) error {
	paths := []struct {
		name string
		path string
	}{
		{name: "host-state-dir", path: options.HostStateDir},
		{name: "drive9-source", path: options.Drive9Source},
		{name: "launcher-source", path: options.LauncherSource},
	}
	for _, value := range paths {
		if strings.TrimSpace(value.path) == "" {
			return fmt.Errorf("install-host-binaries: --%s is required", value.name)
		}
		if !filepath.IsAbs(value.path) {
			return fmt.Errorf("install-host-binaries: --%s must be absolute", value.name)
		}
	}
	switch options.TargetArch {
	case "amd64", "arm64":
		return nil
	default:
		return fmt.Errorf("install-host-binaries: unsupported target architecture %q", options.TargetArch)
	}
}

func validateDirectory(path string, description string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%s %q: %w", description, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s %q must be a real directory", description, path)
	}
	return nil
}

func ensureInstallerBinDir(path string) error {
	err := os.Mkdir(path, 0o755)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create host binary directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect host binary directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("host binary directory %q must be a real directory", path)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return fmt.Errorf("set host binary directory mode: %w", err)
	}
	return nil
}

func readValidatedELF(path string, targetArch string) ([]byte, string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, "", errors.New("source must be a regular file, not a symlink")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return nil, "", errors.New("source must be executable")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) != info.Size() {
		return nil, "", errors.New("source changed while being read")
	}
	if err := validateLinuxELF(body, targetArch); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), nil
}

func validateLinuxELF(body []byte, targetArch string) error {
	file, err := elf.NewFile(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("parse ELF: %w", err)
	}
	defer func() { _ = file.Close() }()
	if file.Class != elf.ELFCLASS64 {
		return fmt.Errorf("ELF class %s is not ELFCLASS64", file.Class)
	}
	if file.Data != elf.ELFDATA2LSB {
		return fmt.Errorf("ELF data encoding %s is not little-endian", file.Data)
	}
	if file.OSABI != elf.ELFOSABI_NONE && file.OSABI != elf.ELFOSABI_LINUX {
		return fmt.Errorf("ELF OS ABI %s is not Linux-compatible", file.OSABI)
	}
	if file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN {
		return fmt.Errorf("ELF type %s is not executable", file.Type)
	}
	wantMachine, err := elfMachineForArch(targetArch)
	if err != nil {
		return err
	}
	if file.Machine != wantMachine {
		return fmt.Errorf("ELF machine %s does not match %s", file.Machine, targetArch)
	}
	for _, program := range file.Progs {
		if program.Type == elf.PT_INTERP {
			return errors.New("ELF contains PT_INTERP and is dynamically linked")
		}
	}
	return nil
}

func elfMachineForArch(targetArch string) (elf.Machine, error) {
	switch targetArch {
	case "amd64":
		return elf.EM_X86_64, nil
	case "arm64":
		return elf.EM_AARCH64, nil
	default:
		return elf.EM_NONE, fmt.Errorf("unsupported target architecture %q", targetArch)
	}
}

func installVersionedDrive9(dir *os.File, path string, body []byte, digest string, fault installerFault) error {
	exists, err := validateExistingVersionedDrive9(path, digest)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	tempPath, err := writeInstallerTemp(
		filepath.Dir(path),
		filepath.Base(path),
		body,
		installerStepDrive9Temp,
		installerStepDrive9Write,
		installerStepDrive9FileSync,
		fault,
	)
	if err != nil {
		return fmt.Errorf("prepare versioned Drive9 binary: %w", err)
	}
	defer func() { _ = os.Remove(tempPath) }()
	if err := runInstallerStep(fault, installerStepDrive9Rename, func() error {
		return os.Rename(tempPath, path)
	}); err != nil {
		return fmt.Errorf("publish versioned Drive9 binary: %w", err)
	}
	if err := runInstallerStep(fault, installerStepDrive9DirSync, dir.Sync); err != nil {
		rollbackErr := removeAndSync(path, dir)
		return errors.Join(fmt.Errorf("sync versioned Drive9 publication: %w", err), rollbackErr)
	}
	return nil
}

func validateExistingVersionedDrive9(path string, digest string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect versioned Drive9 destination: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, errors.New("versioned Drive9 destination must be a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return false, errors.New("versioned Drive9 destination is not executable")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read versioned Drive9 destination: %w", err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != digest {
		return false, errors.New("versioned Drive9 destination digest mismatch")
	}
	return true, nil
}

type regularFileSnapshot struct {
	exists bool
	body   []byte
	mode   fs.FileMode
}

func replaceLauncher(dir *os.File, path string, body []byte, fault installerFault) error {
	return replaceRegularBinary(dir, path, body, "launcher", regularBinaryInstallerSteps{
		temp:     installerStepLauncherTemp,
		write:    installerStepLauncherWrite,
		fileSync: installerStepLauncherFileSync,
		rename:   installerStepLauncherRename,
		dirSync:  installerStepLauncherDirSync,
	}, fault)
}

type regularBinaryInstallerSteps struct {
	temp     installerStep
	write    installerStep
	fileSync installerStep
	rename   installerStep
	dirSync  installerStep
}

func replaceRegularBinary(
	dir *os.File,
	path string,
	body []byte,
	description string,
	steps regularBinaryInstallerSteps,
	fault installerFault,
) error {
	previous, err := snapshotRegularDestination(path)
	if err != nil {
		return fmt.Errorf("inspect %s destination: %w", description, err)
	}
	tempPath, err := writeInstallerTemp(
		filepath.Dir(path),
		filepath.Base(path),
		body,
		steps.temp,
		steps.write,
		steps.fileSync,
		fault,
	)
	if err != nil {
		return fmt.Errorf("prepare %s: %w", description, err)
	}
	defer func() { _ = os.Remove(tempPath) }()
	if err := runInstallerStep(fault, steps.rename, func() error {
		return os.Rename(tempPath, path)
	}); err != nil {
		return fmt.Errorf("publish %s: %w", description, err)
	}
	if err := runInstallerStep(fault, steps.dirSync, dir.Sync); err != nil {
		rollbackErr := restoreRegularDestination(path, previous, description, dir)
		return errors.Join(fmt.Errorf("sync %s publication: %w", description, err), rollbackErr)
	}
	return nil
}

func snapshotRegularDestination(path string) (regularFileSnapshot, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return regularFileSnapshot{}, nil
	}
	if err != nil {
		return regularFileSnapshot{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return regularFileSnapshot{}, errors.New("destination must be a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return regularFileSnapshot{}, errors.New("destination must be executable")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return regularFileSnapshot{}, err
	}
	return regularFileSnapshot{exists: true, body: body, mode: info.Mode().Perm()}, nil
}

func restoreRegularDestination(path string, previous regularFileSnapshot, description string, dir *os.File) error {
	if !previous.exists {
		return removeAndSync(path, dir)
	}
	temp, err := writeInstallerTemp(
		filepath.Dir(path),
		filepath.Base(path)+".rollback",
		previous.body,
		"",
		"",
		"",
		nil,
	)
	if err != nil {
		return fmt.Errorf("prepare %s rollback: %w", description, err)
	}
	defer func() { _ = os.Remove(temp) }()
	if err := os.Chmod(temp, previous.mode); err != nil {
		return fmt.Errorf("restore %s mode: %w", description, err)
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("restore %s: %w", description, err)
	}
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync %s rollback: %w", description, err)
	}
	return nil
}

func replaceDesiredSymlink(dir *os.File, path string, target string, fault installerFault) error {
	if !versionedDrive9NamePattern.MatchString(target) || filepath.IsAbs(target) {
		return fmt.Errorf("invalid desired Drive9 target %q", target)
	}
	previousTarget, previousExists, err := snapshotDesiredSymlink(path)
	if err != nil {
		return err
	}
	if previousExists && previousTarget == target {
		return nil
	}

	tempPath, err := newInstallerTempPath(filepath.Dir(path), ".drive9-link.tmp-")
	if err != nil {
		return fmt.Errorf("allocate desired symlink temp path: %w", err)
	}
	if err := runInstallerStep(fault, installerStepDesiredTemp, func() error {
		return os.Symlink(target, tempPath)
	}); err != nil {
		return fmt.Errorf("create desired symlink: %w", err)
	}
	defer func() { _ = os.Remove(tempPath) }()
	if err := runInstallerStep(fault, installerStepDesiredRename, func() error {
		return os.Rename(tempPath, path)
	}); err != nil {
		return fmt.Errorf("publish desired symlink: %w", err)
	}
	if err := runInstallerStep(fault, installerStepDesiredDirSync, dir.Sync); err != nil {
		rollbackErr := restoreDesiredSymlink(path, previousTarget, previousExists, dir)
		return errors.Join(fmt.Errorf("sync desired symlink publication: %w", err), rollbackErr)
	}
	return nil
}

func snapshotDesiredSymlink(path string) (string, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("inspect desired symlink: %w", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", false, errors.New("desired Drive9 destination must be a symlink")
	}
	target, err := os.Readlink(path)
	if err != nil {
		return "", false, fmt.Errorf("read desired symlink: %w", err)
	}
	if filepath.IsAbs(target) || !versionedDrive9NamePattern.MatchString(target) {
		return "", false, fmt.Errorf("existing desired symlink target %q is unsafe", target)
	}
	targetInfo, err := os.Lstat(filepath.Join(filepath.Dir(path), target))
	if err != nil {
		return "", false, fmt.Errorf("inspect existing desired target: %w", err)
	}
	if targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.Mode().IsRegular() {
		return "", false, errors.New("existing desired target must be a regular file")
	}
	return target, true, nil
}

func restoreDesiredSymlink(path string, target string, existed bool, dir *os.File) error {
	if !existed {
		return removeAndSync(path, dir)
	}
	tempPath, err := newInstallerTempPath(filepath.Dir(path), ".drive9-link.rollback.tmp-")
	if err != nil {
		return fmt.Errorf("allocate desired rollback path: %w", err)
	}
	defer func() { _ = os.Remove(tempPath) }()
	if err := os.Symlink(target, tempPath); err != nil {
		return fmt.Errorf("create desired rollback symlink: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("restore desired symlink: %w", err)
	}
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync desired symlink rollback: %w", err)
	}
	return nil
}

func writeInstallerTemp(
	dir string,
	base string,
	body []byte,
	tempStep installerStep,
	writeStep installerStep,
	syncStep installerStep,
	fault installerFault,
) (string, error) {
	var file *os.File
	if err := runInstallerStep(fault, tempStep, func() error {
		var err error
		file, err = os.CreateTemp(dir, "."+base+".tmp-")
		return err
	}); err != nil {
		return "", err
	}
	path := file.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o755); err != nil {
		return "", err
	}
	if err := runInstallerStep(fault, writeStep, func() error {
		return writeFull(file, body)
	}); err != nil {
		return "", err
	}
	if err := runInstallerStep(fault, syncStep, file.Sync); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	cleanup = false
	return path, nil
}

func writeFull(writer io.Writer, body []byte) error {
	for len(body) > 0 {
		written, err := writer.Write(body)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		body = body[written:]
	}
	return nil
}

func newInstallerTempPath(dir string, pattern string) (string, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	path := file.Name()
	closeErr := file.Close()
	removeErr := os.Remove(path)
	if closeErr != nil || removeErr != nil {
		return "", errors.Join(closeErr, removeErr)
	}
	return path, nil
}

func removeAndSync(path string, dir *os.File) error {
	removeErr := os.Remove(path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	syncErr := dir.Sync()
	return errors.Join(removeErr, syncErr)
}

func runInstallerStep(fault installerFault, step installerStep, action func() error) error {
	if fault != nil && step != "" {
		if err := fault(step); err != nil {
			return err
		}
	}
	return action()
}
