package driver

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUploadPerfScriptRequiresCaseID(t *testing.T) {
	skipUploadPerfScriptTestOnWindows(t)

	stateDir := t.TempDir()
	stdout, stderr, err := runUploadPerfScript(t, stateDir, "token", "--token-stdin")
	if err == nil {
		t.Fatal("expected upload helper to fail without --case-id")
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "--case-id is required") {
		t.Fatalf("stderr = %q, want missing --case-id error", stderr)
	}
}

func TestUploadPerfScriptAutoSelectsSingleVolumeAndUploads(t *testing.T) {
	skipUploadPerfScriptTestOnWindows(t)

	stateDir := t.TempDir()
	perfRoot := filepath.Join(stateDir, "perf")
	volumeID := "drive9-volume-1"
	if err := os.MkdirAll(filepath.Join(perfRoot, volumeID), 0o755); err != nil {
		t.Fatalf("create perf dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(perfRoot, volumeID, "sample.txt"),
		[]byte("sample"),
		0o644,
	); err != nil {
		t.Fatalf("write sample: %v", err)
	}
	logPath := installFakeDrive9(t)
	t.Setenv("DRIVE9_CSI_NODE_NAME", "node-a")

	token := "support-token-secret"
	stdout, stderr, err := runUploadPerfScript(
		t,
		stateDir,
		token,
		"--case-id",
		"CASE-123",
		"--token-stdin",
	)
	if err != nil {
		t.Fatalf("upload helper error = %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}

	bundle := filepath.Join(perfRoot, "CASE-123.tgz")
	if _, err := os.Stat(bundle); err != nil {
		t.Fatalf("expected bundle %s: %v", bundle, err)
	}
	destination := ":/support-inbox/CASE-123/node-a/" + volumeID + ".tgz"
	for _, want := range []string{
		"Uploaded perf bundle: " + destination,
		"Local perf bundle: " + bundle,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}

	logBody := readTextFile(t, logPath)
	for _, want := range []string{
		"cmd=fs cp",
		"cmd=fs stat",
		"server=https://api.drive9.ai",
		"token=present",
		"case=CASE-123",
		"source=k8s-csi",
		"node=node-a",
		"volume=" + volumeID,
		"Drive9 CSI perf bundle",
		bundle,
		destination,
	} {
		if !strings.Contains(logBody, want) {
			t.Fatalf("fake drive9 log missing %q:\n%s", want, logBody)
		}
	}
	for _, body := range []string{stdout, stderr, logBody} {
		if strings.Contains(body, token) {
			t.Fatalf("support token leaked into output/logs:\n%s", body)
		}
	}
	if strings.Contains(readTextFile(t, bundle), token) {
		t.Fatalf("support token leaked into local bundle %s", bundle)
	}
}

func TestUploadPerfScriptRequiresVolumeIDWhenMultipleDirs(t *testing.T) {
	skipUploadPerfScriptTestOnWindows(t)

	stateDir := t.TempDir()
	perfRoot := filepath.Join(stateDir, "perf")
	for _, name := range []string{"drive9-a", "drive9-b"} {
		if err := os.MkdirAll(filepath.Join(perfRoot, name), 0o755); err != nil {
			t.Fatalf("create perf dir: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(perfRoot, "old.tgz"), []byte("bundle"), 0o644); err != nil {
		t.Fatalf("write old bundle: %v", err)
	}

	stdout, stderr, err := runUploadPerfScript(
		t,
		stateDir,
		"token",
		"--case-id",
		"CASE-123",
		"--token-stdin",
	)
	if err == nil {
		t.Fatal("expected upload helper to require --volume-id")
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	for _, want := range []string{"drive9-a", "drive9-b", "rerun with --volume-id"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "old.tgz") {
		t.Fatalf("stderr should ignore bundle files, got:\n%s", stderr)
	}
}

func TestUploadPerfScriptRejectsMissingTokenInNonInteractiveMode(t *testing.T) {
	skipUploadPerfScriptTestOnWindows(t)

	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "perf", "drive9-vol"), 0o755); err != nil {
		t.Fatalf("create perf dir: %v", err)
	}
	logPath := installFakeDrive9(t)

	stdout, stderr, err := runUploadPerfScript(t, stateDir, "", "--case-id", "CASE-123")
	if err == nil {
		t.Fatal("expected upload helper to reject non-interactive missing token")
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "pass --token-stdin") {
		t.Fatalf("stderr = %q, want --token-stdin guidance", stderr)
	}
	if logBody := readTextFile(t, logPath); logBody != "" {
		t.Fatalf("drive9 should not be called without token, log:\n%s", logBody)
	}
}

func TestUploadPerfScriptRejectsUnsafePathComponents(t *testing.T) {
	skipUploadPerfScriptTestOnWindows(t)

	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "perf", "drive9-vol"), 0o755); err != nil {
		t.Fatalf("create perf dir: %v", err)
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "case traversal",
			args: []string{"--case-id", "CASE..123", "--token-stdin"},
			want: "--case-id must not contain '..'",
		},
		{
			name: "volume path",
			args: []string{"--case-id", "CASE-123", "--volume-id", "bad/vol", "--token-stdin"},
			want: "--volume-id contains invalid characters",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, err := runUploadPerfScript(t, stateDir, "token", tt.args...)
			if err == nil {
				t.Fatal("expected upload helper to reject unsafe input")
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, tt.want) {
				t.Fatalf("stderr = %q, want %q", stderr, tt.want)
			}
		})
	}
}

func runUploadPerfScript(
	t *testing.T,
	stateDir string,
	stdin string,
	args ...string,
) (string, string, error) {
	t.Helper()
	t.Setenv("DRIVE9_CSI_STATE_DIR", stateDir)

	script := filepath.Join("..", "..", "hack", "drive9-csi-upload-perf.sh")
	cmdArgs := append([]string{script}, args...)
	cmd := exec.Command("/bin/sh", cmdArgs...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func installFakeDrive9(t *testing.T) string {
	t.Helper()

	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "drive9.log")
	fake := `#!/bin/sh
{
	printf 'cmd=%s %s\n' "$1" "$2"
	shift 2
	i=1
	for arg do
		printf 'arg%d=%s\n' "${i}" "${arg}"
		i=$((i + 1))
	done
	printf 'server=%s\n' "${DRIVE9_SERVER:-}"
	if [ -n "${DRIVE9_API_KEY:-}" ]; then
		printf 'token=present\n'
	else
		printf 'token=missing\n'
		exit 17
	fi
} >> "${DRIVE9_FAKE_LOG}"
`
	fakePath := filepath.Join(binDir, "drive9")
	if err := os.WriteFile(fakePath, []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake drive9: %v", err)
	}
	t.Setenv("DRIVE9_FAKE_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func readTextFile(t *testing.T, name string) string {
	t.Helper()

	body, err := os.ReadFile(name)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

func skipUploadPerfScriptTestOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("upload helper is a Unix shell script")
	}
}
