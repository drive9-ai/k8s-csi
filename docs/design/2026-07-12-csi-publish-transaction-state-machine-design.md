---
title: CSI Publish Transaction State Machine Design
status: proposed
updated: 2026-07-12
---

<!-- markdownlint-disable MD013 MD060 -->

## Scope Baseline

This design makes one change: add durable `unpublishing` intent to the existing
CSI publish state machine and make the current Node publish/unpublish paths obey
that state machine across retries and process crashes.

In scope:

1. Add `unpublishing` to the existing `publishState.Status` values.
2. Define legal transitions and their durable commit points.
3. Make NodePublishVolume resume `pending` instead of treating it as success or
   deleting it after an uncertain write.
4. Make NodeUnpublishVolume persist `unpublishing` before unmount and remove
   state only after cleanup completes.
5. Make publish-state inventory read-only and count every nonterminal state as a
   NodeUnstageVolume consumer.
6. Remove publish-target mutation from existing startup and NodeStage recovery;
   only matching CSI RPC retries may advance a publish transaction.

Out of scope:

1. Subtree layout, paths, mount propagation, or automatic workload recovery.
2. Root publish repair behavior or any new repair algorithm.
3. Layout adapters, LayoutState, mount fingerprints, or another state machine.
4. New locks, watchers, background workers, or reconciliation triggers.
5. FUSE process lifecycle or detection.
6. Readonly mount verification; that is an independent existing issue.
7. A new state-file schema, migration, downgrade, or compatibility framework.

The expected implementation size is `200-300 LoC` of production code. If the
implementation plan exceeds that range or requires an out-of-scope mechanism,
the design must return to scope review before implementation.

## Context

Current main has durable `pending` and `published` publish states, but cleanup
has no durable direction:

1. NodeUnpublishVolume unmounts the target and then removes state.
2. A crash after unmount leaves `published` plus an absent target.
3. Inventory and recovery may interpret absence as stale state and delete the
   record.

The same observation can also follow an interrupted publish. Mount presence
therefore cannot determine whether the intended direction is publish or
cleanup.

There are no production consumers requiring migration. Deployment of this
change assumes existing workloads are drained and no publish-state files remain,
so status-less state does not need compatibility handling.

## Decision

The existing state machine becomes:

~~~text
none -> pending -> published -> unpublishing -> none
          \_____________________/
             explicit unpublish
~~~

The state records desired direction:

| State | Meaning |
|---|---|
| none | No driver-owned publish transaction exists. |
| pending | Publish was requested but successful completion is not durably recorded. |
| published | Publish completion is durable and the target is expected to remain published. |
| unpublishing | Cleanup was requested and must continue only toward state removal. |

Only CSI RPCs and retries of the same RPC advance the transaction:

1. NodePublishVolume creates `pending`.
2. NodePublishVolume changes `pending` to `published` after publish completes.
3. NodeUnpublishVolume changes `pending` or `published` to `unpublishing` and
   later removes the state after cleanup completes.
4. Startup recovery and NodeStage recovery never bind, unmount, rebind, promote,
   or delete a workload publish target or its publish state.

## Alternatives

| Approach | Decision | Reason |
|---|---|---|
| Infer cleanup from an absent target | Rejected | Absence is also possible after an interrupted publish. |
| Add a second cleanup marker | Rejected | Two files create another ordering problem. |
| Add `unpublishing` to the existing record | Selected | One atomic record carries identity, status, and direction. |

## Existing Record Contract

The existing record shape and filename remain unchanged:

~~~go
const (
    publishStatusPending      = "pending"
    publishStatusPublished    = "published"
    publishStatusUnpublishing = "unpublishing"
)

type publishState struct {
    VolumeID      string `json:"volumeID"`
    StagingTarget string `json:"stagingTarget"`
    Target        string `json:"target"`
    Readonly      bool   `json:"readonly"`
    AccessMode    string `json:"accessMode,omitempty"`
    Status        string `json:"status"`
    PublishedAt   string `json:"publishedAt"`
}
~~~

This design adds no SchemaVersion, Layout, LayoutState, or fingerprint fields.
The existing atomic write implementation remains the only state writer.

Every reader validates `Status` as exactly `pending`, `published`, or
`unpublishing`. Missing or unknown status fails closed. The clean rollout removes
the need to default a missing status to `published`; existing AccessMode behavior
is otherwise unchanged.

## Transition Contract

| From | To | Writer | Durable commit point |
|---|---|---|---|
| none | pending | NodePublishVolume | After request, staging, identity, and multi-target validation; before bind. |
| pending | published | NodePublishVolume | After the existing publish operation and validation succeed. |
| pending | unpublishing | NodeUnpublishVolume | Before the first cleanup side effect. |
| published | unpublishing | NodeUnpublishVolume | Before the first cleanup side effect. |
| unpublishing | none | NodeUnpublishVolume | After the target is absent and state removal is directory-synced. |

All other transitions are rejected:

1. Bind failure does not delete `pending` or change it to `none`.
2. `published` never changes back to `pending`.
3. `unpublishing` never changes to `pending` or `published`.
4. Inventory and automatic recovery never change status or remove state.
5. Automatic recovery never performs mount operations on publish targets.

## Writer Matrix

| Durable state | NodePublishVolume | NodeUnpublishVolume | Inventory and automatic recovery |
|---|---|---|---|
| none | Validate, write `pending`, run the existing bind path, then write `published`. | Succeed only if the target is also absent; a mounted target without state fails closed. | No action. |
| pending | Require a matching request, resume the existing publish path, and write `published` only after success. | Write `unpublishing`, then clean up. | Preserve state and target; perform no publish operation. |
| published | Return idempotent success only when the existing validation succeeds; otherwise preserve state. | Write `unpublishing`, then clean up. | Preserve state and target; perform no repair operation. |
| unpublishing | Return FailedPrecondition. | Resume cleanup and remove state last. | Preserve state and target; perform no cleanup operation. |
| malformed, unknown, or mismatched | Fail closed and preserve state and resources. | Fail closed and preserve state and resources. | Report and preserve. |

## NodePublishVolume

NodePublishVolume keeps the existing request, healthy staging, identity,
capability, and multi-target validation. It reads publish state before treating
an existing mount as idempotent success.

### No state

1. A mounted target without matching state fails closed.
2. Run the existing multi-target check.
3. Write the complete existing record with `Status=pending` before bind.
4. Run the existing bind path.
5. After existing validation succeeds, atomically write `Status=published`.
6. Return success only after the `published` write succeeds.

### Matching pending

1. Require VolumeID, StagingTarget, Target, Readonly, and AccessMode to match the
   request.
2. If the target is absent, resume the existing bind path without creating a new
   record or rerunning the new-target multi-target decision.
3. If the target is mounted, run the existing published-mount validation.
4. Write `published` only after the existing validation succeeds.

### Matching published

1. A mounted target that passes existing validation is idempotent success.
2. An absent target preserves `published` and returns FailedPrecondition. This
   design does not add root repair or infer a new publish transaction.

### Matching unpublishing

Return FailedPrecondition. Publish never reverses cleanup direction.

### Failure rules

1. Bind failure preserves `pending`; it does not remove state.
2. If the promotion write returns an error, do not unmount or delete state.
   Atomic rename may already have made either the old or new complete record
   visible. The next NodePublishVolume retry re-reads the visible state.
3. This design continues to use the existing published-mount validation. It does
   not add Readonly mount-option verification.

## NodeUnpublishVolume

NodeUnpublishVolume reads and validates state before any unmount:

1. Missing state plus an absent target is idempotent success.
2. Missing state plus a mounted target fails closed because no complete record is
   available to authorize cleanup.
3. Mismatched, malformed, or unknown state fails closed.
4. Matching `pending` or `published` is atomically rewritten as
   `unpublishing` before unmount.
5. Matching `unpublishing` resumes cleanup without another direction change.
6. After durable `unpublishing`, unmount the target if mounted.
7. After the target is verified absent, remove the state file and sync the state
   directory.

Any unmount or state-removal failure retains the last durable direction.
Only a NodeUnpublishVolume retry continues cleanup. Cleanup does not require
healthy staging.

If the `unpublishing` write returns an error, no unmount follows. The retry reads
the complete visible record and follows its status.

## Inventory, Unstage, and Recovery Isolation

Publish-state scans become read-only:

1. Do not call IsMountPoint to decide whether a valid record should be removed.
2. Never remove `pending`, `published`, or `unpublishing` from inventory.
3. Return every matching nonterminal record as a publish consumer, even when its
   target is absent.
4. Treat unreadable, malformed, or unknown-status state as an error rather than
   cleanup permission; existing identity checks remain unchanged.

NodeUnstageVolume remains blocked while any matching `pending`, `published`, or
`unpublishing` record exists.

Automatic mount recovery is isolated from publish targets:

1. Startup recovery and NodeStage recovery may recover the staging FUSE mount,
   but they do not operate on workload publish targets afterward.
2. They do not bind, unmount, or rebind a publish target.
3. They do not promote `pending`, resume `unpublishing`, or delete any publish
   state.
4. The existing `repairPublishTargets` path is not used. Publish repair remains
   unavailable until the subtree design is implemented.

Normal CSI RPC retry is the only convergence mechanism for an unfinished
publish or unpublish transaction. With the root layout, a previously completed
workload mount that becomes unusable after FUSE reconstruction is recovered only
by rebuilding the workload Pod.

## Crash Convergence

| Crash or error point | Durable observation | Retry behavior |
|---|---|---|
| Before `pending` write | none | A later NodePublishVolume starts normally. |
| After `pending` write, before bind | pending plus absent target | NodePublishVolume resumes bind. |
| Bind returns an error | pending plus absent or partial existing result | Preserve state; NodePublishVolume retries the existing publish path. |
| After bind, before promotion | pending plus mounted target | Validate and promote. |
| Promotion write returns an error | complete pending or complete published | Re-read; never roll back blindly. |
| After promotion, before response | published | Return idempotent success. |
| Before `unpublishing` write | pending or published | No cleanup side effect has occurred. |
| After `unpublishing` write | unpublishing | NodeUnpublishVolume continues cleanup only. |
| After unmount, before state removal | unpublishing plus absent target | Remove and directory-sync state. |
| State removal or directory sync returns an error | complete unpublishing or none | Retry cleanup/removal; both observations converge. |
| After durable removal, before response | none plus absent target | Return idempotent success. |

## Error Policy

1. Invalid request fields keep their existing InvalidArgument behavior.
2. Mismatched, malformed, missing-state-plus-mounted, unknown status, published
   plus absent, and publish-during-unpublishing return FailedPrecondition.
3. A state I/O or mount operation failure after validated intent returns Internal
   and preserves the last durable state.
4. Inventory errors preserve publish state and resources.

## Implementation Surfaces and Budget

1. `internal/driver/driver.go`
   - Add `publishStatusUnpublishing` and strict status validation.
   - Make NodePublishVolume dispatch on existing status before mount fast paths.
   - Preserve `pending` after bind or promotion failure.
   - Write `unpublishing` before NodeUnpublishVolume cleanup.
   - Remove state only after verified absence and directory sync.
   - Make publish-state inventory read-only and count all three statuses.
2. `internal/driver/node_recovery.go`
   - Remove calls that repair publish targets after staging recovery.
   - Remove the publish-target repair path; automatic recovery does not mutate
     publish mounts or state.
3. `internal/driver/active_recovery.go`
   - Remove the now-unused publish-state selection helper.
4. Existing publish-state tests
   - Extend current fixtures and fault injection; add no production framework.

Expected production change:

| Surface | Net LoC |
|---|---:|
| Existing record helpers and durable removal | 40-70 |
| NodePublishVolume and NodeUnpublishVolume | 100-150 |
| Inventory changes and recovery isolation | 40-70 |
| Total | 200-300 |

## Test Plan

1. Table-test all legal transitions and reject every other transition.
2. Prove `pending` plus mounted does not return success before durable promotion.
3. Prove bind failure retains `pending`.
4. Prove promotion write failure never unmounts or deletes state.
5. Prove NodeUnpublishVolume durably writes `unpublishing` before unmount.
6. Prove unmount failure retains `unpublishing`.
7. Prove absent target plus `unpublishing` removes and directory-syncs state.
8. Prove a removal or directory-sync error converges on retry.
9. Prove inventory never deletes a valid state because the target is absent.
10. Prove all three statuses block NodeUnstageVolume.
11. Prove startup and NodeStage recovery never bind, unmount, rebind, promote,
    or delete publish targets or publish state in any status.
12. Prove missing state plus mounted target fails NodeUnpublishVolume without
    unmounting it.
13. Prove malformed and unknown status fail closed.
14. Run local `make check`.

## Acceptance Criteria

1. The only production state addition is `unpublishing`.
2. No cleanup side effect occurs before durable `unpublishing`.
3. State is removed only after cleanup completes.
4. `pending` never returns publish success before durable `published`.
5. Bind and promotion failures preserve a retryable `pending` direction.
6. `unpublishing` never returns to `pending` or `published`.
7. Inventory never mutates publish state.
8. `pending`, `published`, and `unpublishing` all block NodeUnstageVolume.
9. Startup and NodeStage recovery never operate on any workload publish target
   or advance any publish state.
10. No Layout adapter, LayoutState, fingerprint, new lock, or new reconciliation
    trigger is introduced.
11. Production Net LoC remains within `200-300 LoC`; exceeding the range requires
    scope re-approval.
12. Local `make check` passes.

## Deferred Work

1. Subtree publish and automatic recovery remain in
   [the subtree design](./2026-07-09-csi-subtree-mount-recovery-design.md).
2. Readonly mount-option verification is an independent existing issue and is
   not a blocker for this state-machine change.
3. Publish-target repair, including any future Layout adapter, versioned payload,
   fingerprint, or generalized repair framework, belongs to the subtree phase
   and requires separate approval.
