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
	Needs   []string              `json:"needs"`
	Outputs map[string]string     `json:"outputs"`
	Steps   []publishWorkflowStep `json:"steps"`
}

type publishWorkflowStep struct {
	ID   string            `json:"id"`
	Name string            `json:"name"`
	If   string            `json:"if"`
	Uses string            `json:"uses"`
	Run  string            `json:"run"`
	With map[string]any    `json:"with"`
	Env  map[string]string `json:"env"`
}

func TestPublishWorkflowIsManualOnlyAndResolvesLatestImmutableRelease(t *testing.T) {
	body := readRepoFile(t, ".github/workflows/publish-image.yml")
	if !strings.Contains(body, "on:\n  workflow_dispatch:") {
		t.Fatal("publish workflow must expose workflow_dispatch")
	}
	for _, forbidden := range []string{
		"\n  push:",
		"inputs:",
		"compatibility",
		"drive9-cli.lock.json",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("publish workflow contains forbidden trigger or gate %q", forbidden)
		}
	}

	workflow := decodePublishWorkflow(t, body)
	metadata := requiredWorkflowJob(t, workflow, "metadata")
	build := requiredWorkflowJob(t, workflow, "build")
	merge := requiredWorkflowJob(t, workflow, "merge")
	if len(workflow.Jobs) != 3 {
		t.Fatalf("publish workflow job count = %d, want metadata, build, merge", len(workflow.Jobs))
	}

	assertWorkflowNeeds(t, build, "metadata")
	assertWorkflowNeeds(t, merge, "metadata", "build")

	metadataRun := workflowRun(metadata)
	for _, want := range []string{
		"repos/mem9-ai/drive9-fe/commits",
		"path=site/releases/checksums.txt",
		"https://raw.githubusercontent.com/mem9-ai/drive9-fe/${release_commit}/site/releases",
		"checksums.txt",
		"drive9-linux-amd64",
		"drive9-linux-arm64",
		"repos/mem9-ai/drive9/commits/${version}",
		"release_commit=%s",
		"source_commit=%s",
		"version=%s",
	} {
		if !strings.Contains(metadataRun, want) {
			t.Fatalf("metadata job missing immutable release evidence %q", want)
		}
	}
}

func TestPublishWorkflowPassesPinnedReleaseToEveryImageBuild(t *testing.T) {
	body := readRepoFile(t, ".github/workflows/publish-image.yml")
	workflow := decodePublishWorkflow(t, body)
	build := requiredWorkflowJob(t, workflow, "build")

	step := requiredWorkflowStep(t, build, "Build and push digest")
	buildArgs, ok := step.With["build-args"].(string)
	if !ok {
		t.Fatalf("build-args type = %T, want string", step.With["build-args"])
	}
	if !strings.Contains(
		buildArgs,
		"DRIVE9_CLI_RELEASE_COMMIT=${{ needs.metadata.outputs.drive9_release_commit }}",
	) {
		t.Fatal("image build does not receive the pinned Drive9 release commit")
	}
	labels, ok := step.With["labels"].(string)
	if !ok {
		t.Fatalf("labels type = %T, want string", step.With["labels"])
	}
	for _, want := range []string{
		"ai.drive9.cli.version=${{ needs.metadata.outputs.drive9_version }}",
		"ai.drive9.cli.source-commit=${{ needs.metadata.outputs.drive9_source_commit }}",
		"ai.drive9.cli.release-commit=${{ needs.metadata.outputs.drive9_release_commit }}",
	} {
		if !strings.Contains(labels, want) {
			t.Fatalf("image labels missing release provenance %q", want)
		}
	}
}

func TestPublishWorkflowOutputsImageMetadataWithoutDeploymentArtifacts(t *testing.T) {
	body := readRepoFile(t, ".github/workflows/publish-image.yml")
	workflow := decodePublishWorkflow(t, body)
	merge := requiredWorkflowJob(t, workflow, "merge")

	if workflowUses(merge, "actions/checkout@") {
		t.Fatal("merge job must not check out deployment manifests")
	}
	if workflowUses(merge, "actions/upload-artifact@") {
		t.Fatal("merge job must not upload deployment artifacts")
	}

	output := requiredWorkflowStep(t, merge, "Output published image")
	if output.ID != "image" {
		t.Fatalf("image output step id = %q, want image", output.ID)
	}
	for _, want := range []string{
		`docker buildx imagetools inspect`,
		`--format '{{json .}}'`,
		`.manifest.digest | select(test("^sha256:[0-9a-f]{64}$"))`,
		`tag=%s`,
		`digest=%s`,
		`reference=%s@%s`,
		`GITHUB_OUTPUT`,
		`GITHUB_STEP_SUMMARY`,
	} {
		if !strings.Contains(output.Run, want) {
			t.Fatalf("image output step missing %q", want)
		}
	}
	for name, want := range map[string]string{
		"tag":       "${{ steps.image.outputs.tag }}",
		"digest":    "${{ steps.image.outputs.digest }}",
		"reference": "${{ steps.image.outputs.reference }}",
	} {
		if merge.Outputs[name] != want {
			t.Fatalf("merge output %q = %q, want %q", name, merge.Outputs[name], want)
		}
	}
	for _, forbidden := range []string{
		"Create digest-pinned Kubernetes manifests",
		"Upload digest-pinned Kubernetes manifests",
		"kubernetes-manifests-",
		"drive9-csi-kubernetes",
		"deploy/kubernetes",
		"registry.invalid/drive9-csi",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("publish workflow contains deployment responsibility %q", forbidden)
		}
	}
}

func TestPublishWorkflowHasNoLockMutableBinaryOrDeploymentResponsibilities(t *testing.T) {
	body := readRepoFile(t, ".github/workflows/publish-image.yml")
	for _, forbidden := range []string{
		"https://drive9.ai/releases/drive9-",
		"/main/site/releases/drive9-",
		":latest",
		"drive9-cli.lock.json",
		"compatibility_result",
		"DRIVE9_CSI_COMPATIBILITY",
		"Create digest-pinned Kubernetes manifests",
		"Upload digest-pinned Kubernetes manifests",
		"deploy/kubernetes",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("publish workflow contains forbidden responsibility %q", forbidden)
		}
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

func requiredWorkflowStep(t *testing.T, job publishWorkflowJob, name string) publishWorkflowStep {
	t.Helper()
	for _, step := range job.Steps {
		if step.Name == name {
			return step
		}
	}
	t.Fatalf("publish workflow missing step %q", name)
	return publishWorkflowStep{}
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
