---
name: k8s-csi-e2e
description: |
  Safety and contribution contract for Drive9 CSI real-cluster E2E cases.
  Applies recursively to every file under e2e/.
---

# Drive9 CSI E2E Agent Reference

## Scope

The `e2e/` directory contains manually invoked tests against a real Kubernetes
cluster and a real Drive9 server. Preparation and cases are not executed by
`make test`, `make check`, or the image publishing workflow. `make check` only
includes the non-mutating `e2e-check` static validation.

## Hard Safety Rules

1. Preparation and every case must require `DRIVE9_CSI_E2E_CONTEXT` and
   `DRIVE9_CSI_E2E_DRIVER_NAMESPACE`; never use the current kubectl context
   implicitly and never run `kubectl config use-context`.
   Cases additionally require `DRIVE9_CSI_E2E_SECRET_NAME`; their optional
   `DRIVE9_CSI_E2E_NAMESPACE` defaults to the Driver namespace.
2. Every cluster operation in preparation or a case must call the shared
   `kube` wrapper. Only `lib/common.sh` may invoke `kubectl` directly.
3. Reject context names containing `prod` or `production`.
4. Require `DRIVE9_CSI_E2E_CONFIRM=1` before the first cluster mutation.
5. `prepare.sh` must require the CSI image as either the publishing workflow's
   `drive9-<cli-sha7>-csi-<csi-sha7>` trace tag or an immutable
   `@sha256:<digest>` reference. Cases must verify the prepared Driver keeps
   one accepted image reference, but must not require or deploy
   `DRIVE9_CSI_IMAGE`.
6. Print the selected context and API server before mutation, but never print,
   render, copy, or delete pre-provisioned Secret data.
7. `prepare.sh` owns the persistent Driver resources and never deletes them.
   Cases must never render, apply, or delete Driver resources.
8. Register case cleanup before the first mutation. Create case resources
   without replacement, track ownership, and delete only resources whose
   per-run ownership metadata still matches.
9. Cases reuse an existing workload namespace and pre-provisioned Secret. The
   workload namespace may equal the Driver namespace and must never be created
   or deleted by a case. Use explicit timeouts and unique test filenames.
10. Do not add preparation or E2E execution to
    `.github/workflows/publish-image.yml`.
11. Do not run a real E2E as implementation acceptance. Use `make e2e-check`,
    `make test`, and `make check` locally on macOS.

## Layout

1. `prepare.sh`: idempotently create or update the persistent Driver
   environment and requested validation image.
2. `basic-lifecycle.sh`: provisioning, mount, read/write, remount, multi-Pod,
   multi-PVC, unpublish, unstage, and delete behavior.
3. `mount-survival.sh`: live mount identity and workload I/O across CSI node
   Pod replacement.
4. `lib/common.sh`: safety gates, prepared-Driver checks, ownership-aware
   cleanup, explicit-context kubectl wrapper, and polling helpers.
5. `lib/manifests.sh`: separate Driver and case manifest rendering; no cluster
   operations.

## Resource Ownership

1. Preparation owns the Driver namespace, CSIDriver, RBAC, controller
   Deployment, and node DaemonSet. These resources persist across case runs.
2. The environment owns the case namespace and pre-provisioned Secret. Each
   case owns only its labeled StorageClass, VolumeAttributesClass, PVCs, and
   Pods.
3. `mount-survival.sh` may delete one node Pod as its explicit test action; the
   owning DaemonSet and all other Driver resources remain persistent.

## Shell Contract

1. Use Bash, tabs, quoted variables, `[[ ]]`, explicit error handling, and
   functions without the `function` keyword.
2. Avoid `set -e`, `eval`, command aliases, GNU-only behavior on the macOS
   coordinator, and hidden reliance on the current working directory.
3. Case scripts must be directly executable. Library scripts must reject direct
   execution.
4. Do not accept credentials through environment variables or render them into
   temporary files. Cases receive only the non-sensitive Secret name.

## Codex Execution

1. Before asking Codex to run an E2E entrypoint, make every required environment
   variable available to its command subprocess. If a variable is missing,
   Codex must stop instead of constructing a wrapped command to supply it.
2. From the repository root, invoke exactly one entrypoint directly per command:

   ```sh
   e2e/prepare.sh
   e2e/basic-lifecycle.sh
   e2e/mount-survival.sh
   ```

3. Do not prefix an entrypoint with `bash`, `sh`, `zsh`, `env`, or `./`. Do not
   use `zsh -lc`, inline environment assignments, or command substitutions.
   These forms do not match `.codex/rules/e2e.rules` and can persist sensitive
   command text in approval rules.
4. The project rule removes repeated command-execution approval only for the
   three direct entrypoints. It does not waive the required environment,
   explicit real-cluster authorization, or any Hard Safety Rule above.
5. Never request Secret values or print or persist Secret data in a prompt,
   command line, approval rule, log, or Codex configuration.

## Mount-Survival Evidence

The survival case must compare the following values before and after CSI node
Pod replacement:

1. mount PID;
2. PID start time;
3. host mount ID;
4. systemd unit;
5. content-addressed Drive9 binary path;
6. workload I/O progress and absence of recorded I/O failures.
