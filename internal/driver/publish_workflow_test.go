package driver

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

type publishWorkflow struct {
	Jobs map[string]publishWorkflowJob `json:"jobs"`
}

type publishWorkflowJob struct {
	Needs []string              `json:"needs"`
	Steps []publishWorkflowStep `json:"steps"`
}

type publishWorkflowStep struct {
	Name string            `json:"name"`
	Uses string            `json:"uses"`
	Run  string            `json:"run"`
	With map[string]any    `json:"with"`
	Env  map[string]string `json:"env"`
}

func TestPublishWorkflowRequiresLockedExternalCompatibilityResult(t *testing.T) {
	body := readRepoFile(t, ".github/workflows/publish-image.yml")
	workflow := decodePublishWorkflow(t, body)

	metadata := requiredWorkflowJob(t, workflow, "metadata")
	compatibility := requiredWorkflowJob(t, workflow, "compatibility")
	build := requiredWorkflowJob(t, workflow, "build")
	merge := requiredWorkflowJob(t, workflow, "merge")

	assertWorkflowNeeds(t, build, "metadata", "compatibility")
	assertWorkflowNeeds(t, merge, "metadata", "compatibility", "build")

	metadataRun := workflowRun(metadata)
	for _, want := range []string{
		"build/drive9-cli.lock.json",
		".current.version",
		".current.sourceCommit",
		"INPUT_DRIVE9_REF",
	} {
		if !strings.Contains(metadataRun, want) {
			t.Fatalf("metadata job missing locked input evidence %q", want)
		}
	}

	compatibilityRun := workflowRun(compatibility)
	for _, want := range []string{
		"COMPATIBILITY_RESULT_COMMIT",
		"COMPATIBILITY_RESULT_SHA256",
		"build/drive9-cli.lock.json",
		"https://raw.githubusercontent.com/mem9-ai/drive9/${COMPATIBILITY_RESULT_COMMIT}/release/drive9-csi-compatibility.json",
		"sha256sum -c -",
		`.schemaVersion == 2`,
		`.status == "passed"`,
		`.producer.repository == "mem9-ai/drive9"`,
		`.producer.commit == $producerCommit`,
		".compatibilityPlatform",
		".current.sourceCommit",
		".current.artifacts[$platform].sha256",
		".previous.sourceCommit",
		".previous.artifact",
		`RESULT_PAIR" != "$LOCKED_PAIR`,
	} {
		if !strings.Contains(compatibilityRun, want) {
			t.Fatalf("compatibility job missing fail-closed evidence %q", want)
		}
	}
	if !workflowUses(compatibility, "actions/checkout@") {
		t.Fatal("compatibility job must check out the locked metadata")
	}
}

func TestPublishWorkflowProducesDigestPinnedReleaseManifests(t *testing.T) {
	body := readRepoFile(t, ".github/workflows/publish-image.yml")
	workflow := decodePublishWorkflow(t, body)
	merge := requiredWorkflowJob(t, workflow, "merge")

	if !workflowUses(merge, "actions/checkout@") {
		t.Fatal("merge job must check out the source manifest base")
	}
	if !workflowUses(merge, "actions/upload-artifact@") {
		t.Fatal("merge job must upload digest-pinned Kubernetes manifests")
	}
	mergeRun := workflowRun(merge)
	for _, want := range []string{
		`docker buildx imagetools inspect`,
		`--format '{{json .}}'`,
		`.manifest.digest | select(test("^sha256:[0-9a-f]{64}$"))`,
		`registry.invalid/drive9-csi`,
		`digest: %s`,
		`drive9-csi-kubernetes`,
	} {
		if !strings.Contains(mergeRun, want) {
			t.Fatalf("merge job missing release-manifest evidence %q", want)
		}
	}
}

func TestPublishWorkflowHasNoMutableDrive9Resolution(t *testing.T) {
	body := readRepoFile(t, ".github/workflows/publish-image.yml")
	for _, forbidden := range []string{
		"https://drive9.ai/releases/drive9-",
		"Show latest drive9 CLI version",
		":latest",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("publish workflow contains mutable Drive9 resolution %q", forbidden)
		}
	}
	for _, want := range []string{
		"compatibility_result_commit:",
		"compatibility_result_sha256:",
		`DRIVE9_CSI_COMPATIBILITY_RESULT_COMMIT`,
		`DRIVE9_CSI_COMPATIBILITY_RESULT_SHA256`,
		`"build/**"`,
		`"deploy/kubernetes/**"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("publish workflow missing immutable input evidence %q", want)
		}
	}
	if strings.Contains(body, "compatibility_result_url:") {
		t.Fatal("publish workflow must not accept an arbitrary compatibility result URL")
	}
}

func decodePublishWorkflow(t *testing.T, body string) publishWorkflow {
	t.Helper()
	jsonBody, err := utilyaml.ToJSON([]byte(body))
	if err != nil {
		t.Fatalf("convert publish workflow YAML to JSON: %v", err)
	}
	var workflow publishWorkflow
	if err := json.Unmarshal(jsonBody, &workflow); err != nil {
		t.Fatalf("parse publish workflow: %v", err)
	}
	return workflow
}

func requiredWorkflowJob(t *testing.T, workflow publishWorkflow, name string) publishWorkflowJob {
	t.Helper()
	job, ok := workflow.Jobs[name]
	if !ok {
		t.Fatalf("publish workflow missing %q job", name)
	}
	return job
}

func assertWorkflowNeeds(t *testing.T, job publishWorkflowJob, names ...string) {
	t.Helper()
	for _, name := range names {
		if !slices.Contains(job.Needs, name) {
			t.Fatalf("workflow job needs = %v, missing %q", job.Needs, name)
		}
	}
}

func workflowRun(job publishWorkflowJob) string {
	var commands []string
	for _, step := range job.Steps {
		if step.Run != "" {
			commands = append(commands, step.Run)
		}
	}
	return strings.Join(commands, "\n")
}

func workflowUses(job publishWorkflowJob, prefix string) bool {
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, prefix) {
			return true
		}
	}
	return false
}
