package driver

import (
	"path/filepath"
	"testing"
)

func TestDrive9MountArgsIncludesMountTTLs(t *testing.T) {
	stateDir := t.TempDir()
	d := &Driver{cfg: Config{StateDir: stateDir}}
	cacheDir := filepath.Join(stateDir, "cache", "vol")

	got := d.drive9MountArgs(drive9MountRequest{
		VolumeID:      "vol",
		RemoteRoot:    "/repo",
		StagingTarget: "/stage",
		Profile:       "coding-agent",
		AttrTTL:       "1s",
		EntryTTL:      "2s",
		DirTTL:        "3s",
	}, cacheDir)

	want := []string{
		"mount",
		"--supervise-foreground",
		"--mode=fuse",
		"--direct-mount-strict",
		"--allow-other",
		"--cache-dir", cacheDir,
		"--attr-ttl", "1s",
		"--entry-ttl", "2s",
		"--dir-ttl", "3s",
		"--profile", "coding-agent",
		"--local-root", filepath.Join(stateDir, "local", "vol"),
		":/repo",
		"/stage",
	}
	assertStringSlice(t, got, want)
}

func TestDrive9MountArgsDefaultsMountTTLs(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir()}}
	cacheDir := filepath.Join(d.cfg.StateDir, "cache", "vol")

	got := d.drive9MountArgs(drive9MountRequest{
		VolumeID:      "vol",
		RemoteRoot:    "/",
		StagingTarget: "/stage",
	}, cacheDir)

	want := []string{
		"mount",
		"--supervise-foreground",
		"--mode=fuse",
		"--direct-mount-strict",
		"--allow-other",
		"--cache-dir", cacheDir,
		"--attr-ttl", "30s",
		"--entry-ttl", "30s",
		"--dir-ttl", "30s",
		":/",
		"/stage",
	}
	assertStringSlice(t, got, want)
}

func TestDrive9MountArgsPassesThroughProfileAndDurability(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir()}}
	cacheDir := filepath.Join(d.cfg.StateDir, "cache", "vol")

	got := d.drive9MountArgs(drive9MountRequest{
		VolumeID:      "vol",
		RemoteRoot:    "/",
		StagingTarget: "/stage",
		Profile:       "custom-profile",
		Durability:    "custom-durability",
	}, cacheDir)

	want := []string{
		"mount",
		"--supervise-foreground",
		"--mode=fuse",
		"--direct-mount-strict",
		"--allow-other",
		"--cache-dir", cacheDir,
		"--attr-ttl", "30s",
		"--entry-ttl", "30s",
		"--dir-ttl", "30s",
		"--profile", "custom-profile",
		"--durability", "custom-durability",
		":/",
		"/stage",
	}
	assertStringSlice(t, got, want)
}

func TestDrive9MountArgsIncludesPerfDir(t *testing.T) {
	stateDir := t.TempDir()
	d := &Driver{cfg: Config{StateDir: stateDir}}
	cacheDir := filepath.Join(stateDir, "cache", "vol")
	perfDir := filepath.Join(stateDir, "perf", "vol")

	got := d.drive9MountArgs(drive9MountRequest{
		VolumeID:      "vol",
		RemoteRoot:    "/",
		StagingTarget: "/stage",
		PerfDir:       perfDir,
	}, cacheDir)

	want := []string{
		"mount",
		"--supervise-foreground",
		"--mode=fuse",
		"--direct-mount-strict",
		"--allow-other",
		"--cache-dir", cacheDir,
		"--attr-ttl", "30s",
		"--entry-ttl", "30s",
		"--dir-ttl", "30s",
		"--perf-dir", perfDir,
		":/",
		"/stage",
	}
	assertStringSlice(t, got, want)
}

func TestDrive9MountArgsIncludesExplicitMountTuning(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir()}}
	cacheDir := filepath.Join(d.cfg.StateDir, "cache", "vol")

	got := d.drive9MountArgs(drive9MountRequest{
		VolumeID:      "vol",
		RemoteRoot:    "/",
		StagingTarget: "/stage",
		Tuning: mountTuning{
			ReaddirPrefetchGiven:        true,
			ReaddirPrefetch:             true,
			ReaddirPrefetchMaxFiles:     "64",
			ReaddirPrefetchMaxFileBytes: "50000",
			ReaddirPrefetchMaxBytes:     "4194304",
			WritebackBatchWindow:        "20ms",
		},
	}, cacheDir)

	want := []string{
		"mount",
		"--supervise-foreground",
		"--mode=fuse",
		"--direct-mount-strict",
		"--allow-other",
		"--cache-dir", cacheDir,
		"--attr-ttl", "30s",
		"--entry-ttl", "30s",
		"--dir-ttl", "30s",
		"--readdir-prefetch",
		"--readdir-prefetch-max-files", "64",
		"--readdir-prefetch-max-file-bytes", "50000",
		"--readdir-prefetch-max-bytes", "4194304",
		"--writeback-batch-window", "20ms",
		":/",
		"/stage",
	}
	assertStringSlice(t, got, want)
}

func TestDrive9MountArgsOmitsFalseReaddirPrefetch(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir()}}
	cacheDir := filepath.Join(d.cfg.StateDir, "cache", "vol")

	got := d.drive9MountArgs(drive9MountRequest{
		VolumeID:      "vol",
		RemoteRoot:    "/",
		StagingTarget: "/stage",
		Tuning: mountTuning{
			ReaddirPrefetchGiven: true,
		},
	}, cacheDir)

	for _, arg := range got {
		if arg == "--readdir-prefetch" {
			t.Fatalf("drive9MountArgs included --readdir-prefetch for explicit false: %v", got)
		}
	}
}

func assertStringSlice(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg[%d] = %q, want %q\ngot:  %v\nwant: %v", i, got[i], want[i], got, want)
		}
	}
}
