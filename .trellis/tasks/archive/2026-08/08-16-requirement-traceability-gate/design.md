# Design: Requirement Traceability Gate

## 1. Core Design Decisions

### 1.1 Guide Structure

The new guide (`requirement-traceability-guide.md`) follows the established pattern in `.trellis/spec/guides/`:

```
# Requirement Traceability Guide

## Overview
[Why this matters - cite the cert renewal case]

## When to Apply
[At workflow step 1.4, before task.py start]

## Checklist
### Forward Trace: PRD → implement.md
- [ ] Every numbered Requirement (R1.x, R2.x...) has a corresponding Step in implement.md
- [ ] Each Step citation is explicit (e.g., "R1.1 → Step A.2", "R2.3 → Step C.1-C.4")

### Backward Trace: implement.md → PRD
- [ ] Every Step implements at least one Requirement OR is explicitly marked as infrastructure/scaffolding
- [ ] No scope creep: Steps without PRD backing need justification in design.md

### AC Quality Check
- [ ] At least one AC will fail if the feature is not implemented (the "zero-LOC test")
- [ ] ACs with "document/clarify/establish" wording are planning-level, not implementation-level

### Risk Mitigation Completeness
- [ ] Defense + Compensation pairs both have Steps (e.g., "reject expired" + "renew before expiry")
- [ ] Cross-reference: see tautological-test section in guides/index.md

## Case Study: Certificate Renewal Evaporation
[The table from PRD background, with file:line anchors]

## Core Principle
Traceability is bidirectional: every promise must have an implementation step, and every step must serve a promise.
```

### 1.2 AC6 Self-Validation Strategy

AC6 requires applying the guide to the four completed cert subtasks:
- `08-16-cert-auto-renew` (archived, commit 346943c)
- `08-16-panel-server-cert-renew` (archived, commit 5894180)
- `08-16-cert-expiry-observability` (archived, commit e3671b0)
- `08-16-cert-docs-correction` (archived, commit eccee91)

**Decision D1: No retroactive implementation work**
If the guide reveals a Requirement→Step gap in an already-completed task:
- **Do NOT reopen the archived task or add code**
- Record the gap in a notes file (`notes/ac6-findings.md`) with severity:
  - **Critical**: Requirement has no Step, feature is missing → would require new subtask
  - **Minor**: Requirement implicit in another Step, just not explicitly cited in implement.md

Rationale: All four tasks passed AC verification and are in production. The guide's purpose is to prevent future gaps, not to rewrite history.

**Decision D2: Exception for implement.md clarity**
If a gap is **Minor** (feature exists, just not explicitly traced in implement.md), we MAY:
- Add a note in the archived task's implement.md as a comment block
- Do NOT change task.json status or create new commits

This keeps the guide's self-test honest without reopening completed work.

### 1.3 Integration with Existing Guides

**Tautological test cross-reference** (R3.2):
- `guides/index.md` Quick Reference already has: _"Tests that verify implementation details rather than requirements create false confidence"_
- The new guide's "Defense + Compensation" check is a special case of this
- Cross-reference format: _"See also: [Tautological Test Warning](index.md#quick-reference) — the defense-only test is a variant where the test verifies the wrong half of the mitigation pair."_

### 1.4 Trigger Conditions for guides/index.md

Add to the Quick Reference trigger list:
- _"Before `task.py start` (workflow 1.4): run [Requirement Traceability](requirement-traceability-guide.md) forward/backward trace"_
- _"When PRD has ≥5 Requirements: especially critical to check each has a Step"_
- _"When risk mitigation involves pairs (defense + compensation): check both halves have Steps"_

## 2. Implementation Boundaries

### 2.1 What This Task Delivers
- One new guide file: `.trellis/spec/guides/requirement-traceability-guide.md`
- Updates to `.trellis/spec/guides/index.md`: Available Guides table + Quick Reference triggers
- Notes file: `.trellis/tasks/08-16-requirement-traceability-gate/notes/ac6-findings.md` (AC6 self-test results)

### 2.2 What This Task Does NOT Deliver
- No changes to `.trellis/scripts/task.py` (AC7 constraint — if script enforcement is desired, log as future task)
- No changes to product code or test files
- No reopening of archived cert subtasks (D1)

### 2.3 Language Consistency
All new content in English, matching existing guides. No language migration of existing files.

## 3. AC6 Execution Plan

### 3.1 Self-Test Procedure
For each of the four cert subtasks, in completion order:

1. Open `prd.md`, list all Rx.y identifiers
2. Open `implement.md` (or check if it exists — `cert-docs-correction` is doc-only)
3. For each Requirement, search implement.md for explicit mention or infer which Step(s) cover it
4. Record in `notes/ac6-findings.md`:
   - **Traced OK**: R → Step citation found or obvious
   - **Gap-Minor**: Requirement clearly implemented, but implement.md doesn't cite it explicitly
   - **Gap-Critical**: Requirement has no corresponding Step or implementation

### 3.2 Expected Outcome
- `cert-auto-renew`, `panel-server-cert-renew`, `cert-expiry-observability`: all have implement.md, expect mostly "Traced OK" with possible minor gaps
- `cert-docs-correction`: PRD-only task, implement.md exists but is doc-focused — requirements should map to doc changes

If AC6 reveals ≥1 Critical gap, that validates the guide's necessity (proves the problem is real). If zero gaps found, either:
- The guide's checklist is too lenient (needs tightening), or
- The cert subtasks were exceptionally well-traced (less common)

### 3.3 AC6 Pass Criteria
- All four subtasks reviewed
- Findings recorded in notes/ac6-findings.md with file:line anchors
- At least one gap found (proving the guide catches real issues), OR explicit note that all tasks had perfect traceability (with reasoning why)

## 4. Risk Mitigation

| Risk | Mitigation |
|---|---|
| AC6 finds Critical gaps, pressure to reopen tasks | D1 decision: record but don't reopen; guide is forward-looking |
| Guide too abstract, not actionable | AC1/AC3 enforce "mechanical application" — must produce binary yes/no per Requirement |
| Scope creep into task.py scripting | AC7 red line: script ideas go to notes/future-work.md, not this task |
| Language inconsistency | All new content in English (§2.3); match existing guides tone |
| AC6 self-test is circular (we write both guide and findings) | Transparent reporting: if zero gaps found, must explain why (perfect tracing vs. lenient checklist) |

---

## 5. Open Questions for Planning Review

None — this is a lightweight spec-only task. Ready for implement.md.

## 6. Dependencies

- Reads from: all four archived cert subtask directories (prd.md, implement.md where present)
- Writes to: `.trellis/spec/guides/` only
- No code dependencies, no external tools

## 7. Rollback

Pure additive spec documentation. Rollback = `git revert` the commit. No runtime impact.