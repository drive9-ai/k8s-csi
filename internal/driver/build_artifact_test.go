package driver

import (
	"debug/buildinfo"
	"debug/elf"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type drive9CLILock struct {
	SchemaVersion         int                      `json:"schemaVersion"`
	SourceRepository      string                   `json:"sourceRepository"`
	ReleaseRepository     string                   `json:"releaseRepository"`
	CompatibilityPlatform string                   `json:"compatibilityPlatform"`
	Current               drive9CLIGeneration      `json:"current"`
	Previous              drive9PreviousGeneration `json:"previous"`
}

type drive9CLIGeneration struct {
	Version       string                       `json:"version"`
	SourceCommit  string                       `json:"sourceCommit"`
	ReleaseCommit string                       `json:"releaseCommit"`
	Artifacts     map[string]drive9CLIArtifact `json:"artifacts"`
}

type drive9CLIArtifact struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type drive9PreviousGeneration struct {
	Version      string                 `json:"version"`
	SourceCommit string                 `json:"sourceCommit"`
	Artifact     drive9PreviousArtifact `json:"artifact"`
}

type drive9PreviousArtifact struct {
	Kind            string `json:"kind"`
	Platform        string `json:"platform"`
	Repository      string `json:"repository"`
	IndexDigest     string `json:"indexDigest"`
	Digest          string `json:"digest"`
	ConfigDigest    string `json:"configDigest"`
	Path            string `json:"path"`
	TraceTag        string `json:"traceTag"`
	CSISourceCommit string `json:"csiSourceCommit"`
}

var (
	fullCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func TestBuildArtifactLockIsImmutableAndArchitectureSpecific(t *testing.T) {
	lock := readDrive9CLILock(t)
	if lock.SchemaVersion != 2 {
		t.Fatalf("lock schemaVersion = %d, want 2", lock.SchemaVersion)
	}
	if lock.SourceRepository != "mem9-ai/drive9" {
		t.Fatalf("lock sourceRepository = %q", lock.SourceRepository)
	}
	if lock.ReleaseRepository != "mem9-ai/drive9-fe" {
		t.Fatalf("lock releaseRepository = %q", lock.ReleaseRepository)
	}
	if lock.Current.SourceCommit == lock.Previous.SourceCommit {
		t.Fatal("current and previous Drive9 source commits must differ")
	}
	if lock.CompatibilityPlatform != "linux/amd64" {
		t.Fatalf("compatibilityPlatform = %q, want linux/amd64", lock.CompatibilityPlatform)
	}
	assertImmutableDrive9Generation(t, lock.Current)
	if _, ok := lock.Current.Artifacts[lock.CompatibilityPlatform]; !ok {
		t.Fatalf("current generation lacks compatibility platform %q", lock.CompatibilityPlatform)
	}
	assertImmutablePreviousDrive9Generation(t, lock.Previous, lock.CompatibilityPlatform)
	if lock.Previous.SourceCommit != "3aed9d09d3288e5edbd83239e7588594c3c39417" {
		t.Fatalf("previous sourceCommit = %q, want adjacent mainline predecessor 3aed9d0", lock.Previous.SourceCommit)
	}
	if lock.Previous.Artifact.CSISourceCommit != "766bc136761c6acbf727d2709656a2f0223a396d" {
		t.Fatalf("previous CSI source commit = %q, want adjacent mainline predecessor 766bc13", lock.Previous.Artifact.CSISourceCommit)
	}
	if lock.Previous.Artifact.IndexDigest != "sha256:93e628e1196ea542595d8c20f98224456e77e5d4149fa1a4d96db1968839c78c" {
		t.Fatalf("previous image index digest = %q, want adjacent predecessor index", lock.Previous.Artifact.IndexDigest)
	}
	if lock.Previous.Artifact.Digest != "sha256:8c0f72260e22cfb22f9da2559748e4d6130500e1948641e1be2e0fff121d2ab3" {
		t.Fatalf("previous image digest = %q, want adjacent predecessor platform digest", lock.Previous.Artifact.Digest)
	}
	if lock.Previous.Artifact.ConfigDigest != "sha256:7d76b0a721bc187a54d93b2999051c44dc2dc19faa4fa1074f1a477cd706f0bd" {
		t.Fatalf("previous image config digest = %q, want adjacent predecessor config", lock.Previous.Artifact.ConfigDigest)
	}
}

func assertImmutablePreviousDrive9Generation(
	t *testing.T,
	generation drive9PreviousGeneration,
	compatibilityPlatform string,
) {
	t.Helper()
	if !fullCommitPattern.MatchString(generation.SourceCommit) {
		t.Fatalf("previous sourceCommit = %q, want full commit SHA", generation.SourceCommit)
	}
	if generation.Version != generation.SourceCommit[:7] {
		t.Fatalf("previous version = %q, want source commit prefix %q", generation.Version, generation.SourceCommit[:7])
	}
	artifact := generation.Artifact
	if artifact.Kind != "oci-image-file" {
		t.Fatalf("previous artifact kind = %q, want oci-image-file", artifact.Kind)
	}
	if artifact.Platform != compatibilityPlatform {
		t.Fatalf("previous artifact platform = %q, want %q", artifact.Platform, compatibilityPlatform)
	}
	if artifact.Repository != "ghcr.io/drive9-ai/drive9-csi" {
		t.Fatalf("previous artifact repository = %q", artifact.Repository)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(artifact.Digest) {
		t.Fatalf("previous artifact digest = %q", artifact.Digest)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(artifact.IndexDigest) ||
		!regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(artifact.ConfigDigest) {
		t.Fatalf("previous OCI provenance is incomplete: index=%q config=%q", artifact.IndexDigest, artifact.ConfigDigest)
	}
	if artifact.Path != "/usr/local/bin/drive9" {
		t.Fatalf("previous artifact path = %q", artifact.Path)
	}
	if !fullCommitPattern.MatchString(artifact.CSISourceCommit) {
		t.Fatalf("previous CSI source commit = %q", artifact.CSISourceCommit)
	}
	wantTraceTag := fmt.Sprintf(
		"drive9-%s-csi-%s",
		generation.SourceCommit[:7],
		artifact.CSISourceCommit[:7],
	)
	if artifact.TraceTag != wantTraceTag {
		t.Fatalf("previous artifact traceTag = %q, want %q", artifact.TraceTag, wantTraceTag)
	}
}

func TestBuildArtifactDockerfileConsumesLockAndBuildsBothBinaries(t *testing.T) {
	body := readRepoFile(t, "Dockerfile")
	for _, want := range []string{
		"COPY build/drive9-cli.lock.json",
		".current.artifacts[$platform].url",
		".current.artifacts[$platform].sha256",
		"sha256sum -c -",
		"go build -o /out/drive9-csi ./cmd/drive9-csi",
		"go build -o /out/drive9-csi-launcher ./cmd/drive9-csi-launcher",
		"verify-host-binary --path=/out/drive9-csi",
		"verify-host-binary --path=/out/drive9-csi-launcher",
		"verify-host-binary --path=/out/drive9",
		"COPY --from=csi-builder /out/drive9-csi-launcher /usr/local/bin/drive9-csi-launcher",
		"fuse3",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Dockerfile missing locked build evidence %q", want)
		}
	}
	for _, forbidden := range []string{
		"https://drive9.ai/releases/drive9-",
		"/main/site/releases/drive9-",
		":latest",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("Dockerfile contains mutable Drive9 input %q", forbidden)
		}
	}
}

func TestBuildArtifactMakeBuildProducesBothBinaries(t *testing.T) {
	body := readRepoFile(t, "Makefile")
	for _, want := range []string{
		"go build -o bin/drive9-csi ./cmd/drive9-csi",
		"go build -o bin/drive9-csi-launcher ./cmd/drive9-csi-launcher",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Makefile build target missing %q", want)
		}
	}
}

func TestBuildArtifactLinuxBinariesAreStaticForSupportedArchitectures(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	commands := []struct {
		name   string
		pkg    string
		goPath string
	}{
		{name: "drive9-csi", pkg: "./cmd/drive9-csi", goPath: "github.com/drive9-ai/csi/cmd/drive9-csi"},
		{name: "drive9-csi-launcher", pkg: "./cmd/drive9-csi-launcher", goPath: "github.com/drive9-ai/csi/cmd/drive9-csi-launcher"},
	}
	architectures := []struct {
		name    string
		machine elf.Machine
	}{
		{name: "amd64", machine: elf.EM_X86_64},
		{name: "arm64", machine: elf.EM_AARCH64},
	}

	for _, architecture := range architectures {
		for _, command := range commands {
			t.Run(architecture.name+"/"+command.name, func(t *testing.T) {
				output := filepath.Join(t.TempDir(), command.name)
				build := exec.Command("go", "build", "-trimpath", "-o", output, command.pkg)
				build.Dir = repoRoot
				build.Env = environmentWith(map[string]string{
					"CGO_ENABLED": "0",
					"GOARCH":      architecture.name,
					"GOOS":        "linux",
					"GOPROXY":     "off",
					"GOSUMDB":     "off",
					"GOTOOLCHAIN": "local",
				})
				if outputBody, err := build.CombinedOutput(); err != nil {
					t.Fatalf("build %s for linux/%s: %v\n%s", command.name, architecture.name, err, outputBody)
				}

				file, err := elf.Open(output)
				if err != nil {
					t.Fatalf("open ELF: %v", err)
				}
				defer func() { _ = file.Close() }()
				if file.Machine != architecture.machine {
					t.Fatalf("ELF machine = %s, want %s", file.Machine, architecture.machine)
				}
				for _, program := range file.Progs {
					if program.Type == elf.PT_INTERP {
						t.Fatal("binary contains PT_INTERP")
					}
				}

				info, err := buildinfo.ReadFile(output)
				if err != nil {
					t.Fatalf("read Go build metadata: %v", err)
				}
				if info.Path != command.goPath {
					t.Fatalf("Go build path = %q, want %q", info.Path, command.goPath)
				}
			})
		}
	}
}

func readDrive9CLILock(t *testing.T) drive9CLILock {
	t.Helper()
	body := readRepoFile(t, "build/drive9-cli.lock.json")
	var lock drive9CLILock
	if err := json.Unmarshal([]byte(body), &lock); err != nil {
		t.Fatalf("parse build/drive9-cli.lock.json: %v", err)
	}
	return lock
}

func assertImmutableDrive9Generation(t *testing.T, generation drive9CLIGeneration) {
	t.Helper()
	if !fullCommitPattern.MatchString(generation.SourceCommit) {
		t.Fatalf("sourceCommit = %q, want full commit SHA", generation.SourceCommit)
	}
	if !fullCommitPattern.MatchString(generation.ReleaseCommit) {
		t.Fatalf("releaseCommit = %q, want full commit SHA", generation.ReleaseCommit)
	}
	if generation.Version != generation.SourceCommit[:7] {
		t.Fatalf("version = %q, want source commit prefix %q", generation.Version, generation.SourceCommit[:7])
	}
	if len(generation.Artifacts) != 2 {
		t.Fatalf("artifact count = %d, want 2", len(generation.Artifacts))
	}
	for _, arch := range []string{"amd64", "arm64"} {
		platform := "linux/" + arch
		artifact, ok := generation.Artifacts[platform]
		if !ok {
			t.Fatalf("missing artifact %q", platform)
		}
		if artifact.GOOS != "linux" || artifact.GOARCH != arch {
			t.Fatalf("artifact %q target = %s/%s", platform, artifact.GOOS, artifact.GOARCH)
		}
		wantURL := fmt.Sprintf(
			"https://raw.githubusercontent.com/mem9-ai/drive9-fe/%s/site/releases/drive9-linux-%s",
			generation.ReleaseCommit,
			arch,
		)
		if artifact.URL != wantURL {
			t.Fatalf("artifact %q URL = %q, want %q", platform, artifact.URL, wantURL)
		}
		if !sha256Pattern.MatchString(artifact.SHA256) {
			t.Fatalf("artifact %q sha256 = %q", platform, artifact.SHA256)
		}
	}
}

func environmentWith(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if _, overridden := overrides[key]; !overridden {
			environment = append(environment, value)
		}
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment
}
