# Implementation Plan: Requirement Traceability Gate

## Overview

Lightweight spec documentation task. Three deliverables:
1. New traceability guide (English, follows existing guides pattern)
2. Updates to guides/index.md (table + triggers)
3. AC6 self-test findings (apply guide to four completed cert subtasks)

**Key constraint**: Pure spec work — no code, no scripts, no task.py changes (AC7).

---

## Phase A: Draft the Traceability Guide

**File**: `.trellis/spec/guides/requirement-traceability-guide.md`

### A.1 Structure Setup
Follow existing guides pattern (see `guides/index.md` for reference):
- H1 title
- Overview (why this matters)
- When to Apply (workflow 1.4 timing)
- Checklist (the actionable core)
- Case Study (cert renewal evaporation)
- Core Principle (one-sentence takeaway)

### A.2 Write the Checklist (R1.3, R2, R3)

Must be mechanically applicable (AC3). Four sections:

**Forward Trace: PRD → implement.md**
```
- [ ] Every numbered Requirement (R1.x, R2.x...) has a corresponding Step in implement.md
- [ ] Each mapping is explicit or obvious (cite in implement.md or design.md)
- [ ] Exception: PRD-only tasks may map R → design.md sections if no implement.md
```

**Backward Trace: implement.md → PRD**
```
- [ ] Every Step serves at least one Requirement
- [ ] Steps with no direct Requirement are marked as scaffolding/infrastructure in design.md
- [ ] No scope creep: if a Step appears novel, check if PRD needs amendment
```

**AC Quality: Planning vs Implementation Level** (R2.1, R2.2)
```
- [ ] Zero-LOC test: "Can this AC pass with zero code changes?" If yes → planning-level AC
- [ ] At least one AC will fail if the feature is missing (implementation-level required)
- [ ] Planning ACs ("document/establish/clarify") are fine but insufficient alone
```

**Risk Mitigation Completeness** (R3.1, R3.2)
```
- [ ] Defense + Compensation pairs: both halves have Steps
  Example: "reject expired certs" (defense) + "renew before expiry" (compensation)
- [ ] See also: Tautological Test Warning in guides/index.md — testing only the defense half
  creates false confidence
```

### A.3 Write the Case Study (R1.4, AC2)

Use the table from PRD background, with file:line anchors:

```markdown
## Case Study: Certificate Renewal Evaporation (2026-08)

A multi-node mTLS control plane task (`07-13-multi-node-mtls`) explicitly required certificate
renewal lifecycle management:

| Stage | Location | Status |
|-------|----------|--------|
| Requirement | `07-13.../prd.md:47,53` — panel manages renewal lifecycle | ✅ Present |
| Design | `07-13.../design.md:100-103` §3.3 — detailed renewal design | ✅ Present |
| Risk mitigation | `07-13.../design.md:216` — depends on "proactive renewal before expiry" | ✅ Present |
| **Execution plan** | `07-13.../implement.md` 155 lines, 8 phases — **no mention of renewal** | ❌ Gap |
| Implementation | No renewal code shipped | ❌ Missing |
| Verification | `trellis-check` walked implement.md → all green | ⚠️ False pass |

**Why traceability would have caught it**:
- Forward trace: R (renewal lifecycle) → no Step in implement.md → immediate red flag at workflow 1.4
- AC quality check: All 10 ACs were planning-level ("establish flow", "document approach") →
  zero-LOC test reveals none would fail without code
- Risk pair check: "reject expired" had test coverage (`pki_test.go:122`), "renew before expiry"
  had zero → asymmetric mitigation

**Outcome**: Renewal feature was rediscovered 3 months later when an operator asked about expiry
handling. Became subtask `08-16-cert-auto-renew`.

**Lesson**: Requirements that survive design but vanish from execution plans are invisible to
verification — unless traceability is checked before `task.py start`.
```

### A.4 Write Overview and Core Principle

**Overview**: 2-3 paragraphs
- Planning artifacts (prd/design/implement) can drift
- Implement.md is the source-of-truth for execution and verification
- Forward + backward trace at workflow 1.4 catches gaps before they become missing features

**Core Principle**: One sentence
> "Every requirement must have an execution step, and every step must serve a requirement — bidirectional traceability is the only defense against silent evaporation."

### A.5 Validation

Self-check against ACs:
- AC1: Checklist has 4 sections with mechanical yes/no items ✓
- AC2: Case study has file:line anchors to real tasks ✓
- AC3: Zero-LOC test is a binary discriminator ✓
- AC4: Risk pair check cross-references tautological test ✓

---

## Phase B: Update guides/index.md

### B.1 Add to Available Guides Table

Insert row in the table (maintain alphabetical or logical order):

```markdown
| [Requirement Traceability](requirement-traceability-guide.md) | Forward/backward trace from PRD to implement.md; AC quality checks | Workflow 1.4 (before task.py start) |
```

### B.2 Add Triggers to Quick Reference

In the Quick Reference section, add after existing triggers:

```markdown
- **Before task.py start (workflow 1.4)**: Run [Requirement Traceability](requirement-traceability-guide.md) checklist — every PRD Requirement must map to an implement.md Step
- **When PRD has ≥5 Requirements**: Traceability gaps become likely; forward trace is essential
- **Risk mitigation with pairs** (defense + compensation): Check both halves have Steps — see also Tautological Test Warning
```

### B.3 Cross-Reference in Tautological Test Section

Locate the existing tautological test warning (likely in Quick Reference). Add:

```markdown
**Related**: [Requirement Traceability Guide](requirement-traceability-guide.md) — Defense-only testing (e.g., "expired certs rejected" without "renewal before expiry") is a variant where the test verifies the wrong half of the mitigation pair.
```

---

## Phase C: AC6 Self-Validation

**File**: `.trellis/tasks/08-16-requirement-traceability-gate/notes/ac6-findings.md`

### C.1 Setup Findings Template

```markdown
# AC6 Self-Test: Applying Traceability Guide to Cert Subtasks

## Methodology

Applied the requirement-traceability-guide.md checklist to four completed cert lifecycle subtasks:
1. `08-16-cert-auto-renew` (commit 346943c)
2. `08-16-panel-server-cert-renew` (commit 5894180)
3. `08-16-cert-expiry-observability` (commit e3671b0)
4. `08-16-cert-docs-correction` (commit eccee91)

For each task:
- List all PRD Requirements (Rx.y)
- Check implement.md (or design.md if PRD-only) for corresponding Steps
- Apply Zero-LOC test to ACs
- Record findings: Traced OK / Gap-Minor / Gap-Critical

## Findings

[Fill in C.2-C.5 results]

## Summary

[Total gaps found, severity distribution, verdict on guide effectiveness]
```

### C.2 Trace 08-16-cert-auto-renew

Read:
- `.trellis/tasks/archive/2026-08/08-16-cert-auto-renew/prd.md` (Requirements)
- `.trellis/tasks/archive/2026-08/08-16-cert-auto-renew/implement.md` (Steps)

For each Requirement:
- Search implement.md for explicit mention or obvious coverage
- Note any gaps

Record in findings.md:
```markdown
### 08-16-cert-auto-renew

| Requirement | implement.md Step | Status |
|-------------|-------------------|--------|
| R1.1 ... | Step A.2 | Traced OK |
| R1.2 ... | Step B.1-B.3 | Traced OK |
| ... | ... | ... |

**AC Quality Check**: [Apply zero-LOC test to the task's ACs]

**Risk Pair Check**: [If applicable]

**Gaps Found**: [None / list with severity]
```

### C.3 Trace 08-16-panel-server-cert-renew

Same procedure as C.2, using archived task files.

### C.4 Trace 08-16-cert-expiry-observability

Same procedure as C.2.

### C.5 Trace 08-16-cert-docs-correction

Note: This is a doc-only task. Check if Requirements map to:
- implement.md sections (doc changes), or
- design.md sections if implement.md is lightweight

### C.6 Write Summary

Answer:
- Total Requirements checked across four tasks: [N]
- Traced OK: [X]
- Gap-Minor (feature exists, not explicitly cited): [Y]
- Gap-Critical (no Step, feature missing): [Z]

**Verdict on guide effectiveness**:
- If Z > 0: Guide catches real gaps — validated ✓
- If Z = 0 and Y > 0: Guide reveals citation gaps — useful for future tasks ✓
- If Z = 0 and Y = 0: Either (a) cert tasks had exceptional traceability, or (b) guide checklist too lenient — explain which

---

## Phase D: Final Validation

### D.1 AC Checklist

Run through all ACs:
- AC1: Guide has mechanical checklist (4 sections) ✓
- AC2: Case study with file:line anchors ✓
- AC3: Zero-LOC test is a binary discriminator ✓
- AC4: Cross-reference to tautological test ✓
- AC5: guides/index.md updated (table + triggers) ✓
- AC6: Self-test complete, findings recorded ✓
- AC7: No script changes, only spec docs ✓

### D.2 File Inventory

Changed files (all under `.trellis/`):
```
spec/guides/requirement-traceability-guide.md (new)
spec/guides/index.md (modified: table + triggers + cross-ref)
tasks/08-16-requirement-traceability-gate/notes/ac6-findings.md (new)
tasks/08-16-requirement-traceability-gate/design.md (already exists)
tasks/08-16-requirement-traceability-gate/implement.md (this file)
```

No product code, no scripts, no test files.

### D.3 Language Check

All new content in English? (guides must match existing style)
- requirement-traceability-guide.md: English ✓
- index.md additions: English ✓
- ac6-findings.md: English ✓

### D.4 Git Readiness

```bash
git add .trellis/spec/guides/ .trellis/tasks/08-16-requirement-traceability-gate/
git diff --cached --stat  # Should show ~3 files, +200-300 lines
```

---

## Common Pitfalls

1. **AC6 circular reasoning**: Don't gloss over gaps to make the guide "pass". If cert tasks were perfectly traced, that's fine — but explain why (they were recent, had explicit review, etc.). Honesty validates the guide.

2. **Scope creep into scripting**: If during AC6 you think "this should be automated", note it in findings.md under Future Work. Do NOT add validation to task.py in this task (violates AC7).

3. **Language drift**: guides/index.md is English. Don't insert Chinese fragments.

4. **Case study blame**: The case study (§A.3) describes a process failure, not a person failure. Keep it factual and file:line anchored.

5. **Zero-LOC test confusion**: An AC like "API returns 4 fields" is implementation-level (would fail without code). An AC like "Design document specifies 4 fields" is planning-level (passes with zero code). The discriminator is whether *implementation* is required to satisfy it.

---

## Completion Signal

When all of D.1-D.4 pass, report to verification session. Do not call `task.py start/finish`, do not commit.