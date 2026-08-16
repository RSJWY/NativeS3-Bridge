# Requirement Traceability Guide

## Overview

Planning artifacts (PRD, design, implementation plan) can drift apart during task execution. When a requirement survives the PRD and design phases but vanishes from the implementation plan, it becomes invisible to verification—the task can pass all checks while missing an entire feature.

This guide provides a mechanical checklist to verify bidirectional traceability between `prd.md` Requirements and `implement.md` Steps. Applied at workflow step 1.4 (before `task.py start`), it catches requirement evaporation before implementation begins.

**Why this matters**: Verification agents walk `implement.md` as their source of truth. If a requirement never makes it into the execution plan, no amount of careful verification will catch its absence.

---

## When to Apply

**Timing**: Workflow step 1.4, immediately before `task.py start`

At this point:
- `prd.md` is complete with numbered Requirements (R1.x, R2.x...)
- `design.md` exists for complex tasks
- `implement.md` is written and ready for review

This is the **only gate** where all three artifacts are present and malleable. After `task.py start`, verification begins, and gaps become missing features.

---

## Checklist

### Forward Trace: PRD → implement.md

- [ ] **Every numbered Requirement (R1.x, R2.x...) has a corresponding Step in implement.md**
  - Explicit citation preferred: implement.md mentions "R1.1 → Step A.2"
  - Implicit coverage acceptable if obvious: search for keywords from the Requirement
- [ ] **Exception for PRD-only tasks**: If no implement.md exists, Requirements may map to design.md sections (doc tasks, spec-only work)
- [ ] **No orphans**: If a Requirement has no Step, either add the Step or remove/defer the Requirement

### Backward Trace: implement.md → PRD

- [ ] **Every Step serves at least one Requirement**
  - Steps without direct Requirement backing should be marked as scaffolding/infrastructure in design.md
  - Examples: "Set up test harness", "Add CI job", "Refactor for readability"
- [ ] **No scope creep**: If a Step feels novel or surprising, check whether the PRD needs amendment
  - New feature discovered during design? Add to PRD or split into a follow-up task
  - Don't silently expand scope in implement.md

### AC Quality: Planning vs Implementation Level

**Zero-LOC Test**: *"Can this AC pass with zero code changes?"*

- [ ] **At least one AC is implementation-level** (would fail without the feature)
  - Implementation-level: "API returns 4 fields", "expired certs are rejected", "UI shows error toast"
  - Planning-level: "design specifies 4 fields", "document rejection policy", "clarify error UX"
- [ ] **Planning ACs are insufficient alone**
  - "Document X", "establish Y flow", "clarify Z approach" → these pass when the doc is written, before any code exists
  - Valid for planning milestones, but must be paired with at least one implementation AC if code is being delivered
- [ ] **Binary discriminator**: If an AC uses "document/establish/clarify" and can be satisfied by updating a markdown file, it's planning-level

### Risk Mitigation Completeness

**Defense + Compensation Pairs**: When a risk mitigation strategy has two halves, both must have Steps.

- [ ] **Identify pairs in design.md risk section**
  - Defense: "reject expired certificates"
  - Compensation: "renew certificates before expiry"
- [ ] **Check both halves have Steps**
  - Defense without compensation → system fails when the bad state inevitably occurs
  - Compensation without defense → silent failures when mitigation is late
- [ ] **Cross-reference**: See [Tautological Test Warning](#related-tautological-test-warning) below—testing only the defense half creates false confidence

---

## Case Study: Certificate Renewal Evaporation (2026-08)

A multi-node mTLS control plane task (`07-13-multi-node-mtls`) explicitly required certificate renewal lifecycle management:

| Stage | Location | Status |
|-------|----------|--------|
| Requirement | `.trellis/tasks/07-13-multi-node-mtls/prd.md:47,53`—panel manages renewal lifecycle | ✅ Present |
| Design | `.trellis/tasks/07-13-multi-node-mtls/design.md:100-103` §3.3—detailed renewal design | ✅ Present |
| Risk mitigation | `.trellis/tasks/07-13-multi-node-mtls/design.md:216`—depends on "proactive renewal before expiry" | ✅ Present |
| **Execution plan** | `.trellis/tasks/07-13-multi-node-mtls/implement.md` 155 lines, 8 phases—**no mention of renewal** | ❌ Gap |
| Implementation | No renewal code shipped | ❌ Missing |
| Verification | `trellis-check` walked implement.md → all green | ⚠️ False pass |

**Why traceability would have caught it**:
- **Forward trace**: Requirement (renewal lifecycle) → no Step in implement.md → immediate red flag at workflow 1.4
- **AC quality check**: All 10 ACs were planning-level ("establish flow", "document approach") → zero-LOC test reveals none would fail without code
- **Risk pair check**: "reject expired" had test coverage (`pkg/panel/pki_test.go:122`), "renew before expiry" had zero → asymmetric mitigation

**Outcome**: Renewal feature was rediscovered 3 months later when an operator asked about expiry handling. Became subtask `08-16-cert-auto-renew` (commit 346943c).

**Lesson**: Requirements that survive design but vanish from execution plans are invisible to verification—unless traceability is checked before `task.py start`.

---

## Related: Tautological Test Warning

From [Quick Reference](index.md#when-verifying-ai-cross-review-results):

> **Tautological test**: Mentally delete the feature being tested—does the test still pass? If yes, the test verifies implementation details rather than requirements, creating false confidence.

The defense-only test is a variant where the test verifies the wrong half of a mitigation pair:
- Test for "expired certs rejected" passes ✓
- No test for "certs renewed before expiry" → compensation half unverified
- Result: System correctly fails closed when certs expire, but never prevents expiry in the first place

**Traceability catches this**: Risk pair check (checklist above) requires both defense and compensation to have Steps, ensuring both get implemented and tested.

---

## Core Principle

**Every requirement must have an execution step, and every step must serve a requirement—bidirectional traceability is the only defense against silent evaporation.**
