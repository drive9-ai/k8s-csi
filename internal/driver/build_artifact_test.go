package driver

import (
	"debug/buildinfo"
	"debug/elf"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildArtifactHasNoDrive9CLILock(t *testing.T) {
	path := filepath.Join("..", "..", "build", "drive9-cli.lock.json")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Drive9 CLI lock must not exist: %v", err)
	}
}

func TestBuildArtifactDockerfileConsumesImmutableDrive9ReleaseAndBuildsBothBinaries(t *testing.T) {
	body := readRepoFile(t, "Dockerfile")
	for _, want := range []string{
		"ARG DRIVE9_CLI_RELEASE_COMMIT",
		`release_commit="${DRIVE9_CLI_RELEASE_COMMIT}"`,
		`https://raw.githubusercontent.com/mem9-ai/drive9-fe/${release_commit}/site/releases`,
		`artifact="drive9-linux-${target_arch}"`,
		`checksums.txt`,
		`^[0-9a-f]{40}$`,
		`^[0-9a-f]{64}$`,
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
			t.Fatalf("Dockerfile missing immutable release evidence %q", want)
		}
	}
	for _, forbidden := range []string{
		"drive9-cli.lock.json",
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

func TestBuildArtifactMakeImagePassesDrive9ReleaseCommit(t *testing.T) {
	body := readRepoFile(t, "Makefile")
	for _, want := range []string{
		"DRIVE9_CLI_RELEASE_COMMIT ?=",
		"--build-arg DRIVE9_CLI_RELEASE_COMMIT=$(DRIVE9_CLI_RELEASE_COMMIT)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Makefile image targets missing Drive9 release input %q", want)
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
