package driver

import (
	"path/filepath"
	"strconv"
	"strings"
)

const (
	directMountStrictFlag   = "--direct-mount-strict"
	superviseForegroundFlag = "--supervise-foreground"
)

type drive9MountRequest struct {
	VolumeID      string
	Server        string
	APIKey        string
	RemoteRoot    string
	StagingTarget string
	Profile       string
	Durability    string
	AttrTTL       string
	EntryTTL      string
	DirTTL        string
	PerfDir       string
	Tuning        mountTuning
	GVisorCompat  bool
	Policy        mountPathPolicy
}

func (d *Driver) drive9MountArgs(req drive9MountRequest, cacheDir string) []string {
	ttls := mountTTLsOrDefault(req.AttrTTL, req.EntryTTL, req.DirTTL)
	args := []string{
		"mount",
		superviseForegroundFlag,
		"--mode=fuse",
		directMountStrictFlag,
		"--gvisor-compat=" + strconv.FormatBool(req.GVisorCompat),
	}
	if req.Server != "" {
		args = append(args, "--server", req.Server)
	}
	args = append(args,
		"--allow-other",
		"--cache-dir", cacheDir,
		"--attr-ttl", ttls.AttrTTL,
		"--entry-ttl", ttls.EntryTTL,
		"--dir-ttl", ttls.DirTTL,
	)
	if req.Profile != "" {
		args = append(args, "--profile", req.Profile)
		if req.Profile == "coding-agent" {
			args = append(args, "--local-root", d.drive9LocalRoot(req.VolumeID))
		}
	}
	if req.Durability != "" {
		args = append(args, "--durability", req.Durability)
	}
	for _, pattern := range req.Policy.LocalOnlyPatterns {
		args = append(args, "--local-only="+pattern)
	}
	for _, pattern := range req.Policy.RemoteOnlyPatterns {
		args = append(args, "--remote-only="+pattern)
	}
	if req.PerfDir != "" {
		args = append(args, "--perf-dir", req.PerfDir)
	}
	args = appendMountTuningArgs(args, req.Tuning)
	return append(args, ":"+req.RemoteRoot, req.StagingTarget)
}

func mountArgsUseDirectMountStrict(args []string) bool {
	count := 0
	for _, arg := range args {
		if arg == directMountStrictFlag {
			count++
		}
	}
	return count == 1
}

func mountArgsUseInBinarySupervisor(args []string) bool {
	count := 0
	for _, arg := range args {
		if arg == superviseForegroundFlag {
			count++
			continue
		}
		if mountFlagArgument(arg, "supervise-foreground") ||
			mountFlagArgument(arg, "foreground") ||
			mountFlagArgument(arg, "supervised") ||
			mountFlagArgument(arg, "no-supervise") {
			return false
		}
	}
	return count == 1
}

func mountFlagArgument(arg string, name string) bool {
	for _, prefix := range []string{"-" + name, "--" + name} {
		if arg == prefix || strings.HasPrefix(arg, prefix+"=") {
			return true
		}
	}
	return false
}

func appendMountTuningArgs(args []string, tuning mountTuning) []string {
	if tuning.ReaddirPrefetch {
		args = append(args, "--readdir-prefetch")
	}
	if tuning.ReaddirPrefetchMaxFiles != "" {
		args = append(args, "--readdir-prefetch-max-files", tuning.ReaddirPrefetchMaxFiles)
	}
	if tuning.ReaddirPrefetchMaxFileBytes != "" {
		args = append(args, "--readdir-prefetch-max-file-bytes", tuning.ReaddirPrefetchMaxFileBytes)
	}
	if tuning.ReaddirPrefetchMaxBytes != "" {
		args = append(args, "--readdir-prefetch-max-bytes", tuning.ReaddirPrefetchMaxBytes)
	}
	if tuning.WritebackBatchWindow != "" {
		args = append(args, "--writeback-batch-window", tuning.WritebackBatchWindow)
	}
	return args
}

func (d *Driver) drive9LocalRoot(volumeID string) string {
	return filepath.Join(d.cfg.StateDir, "local", safeFileName(volumeID))
}

func (d *Driver) mountCacheDir(volumeID string) string {
	return filepath.Join(d.cfg.StateDir, "cache", safeFileName(volumeID))
}
