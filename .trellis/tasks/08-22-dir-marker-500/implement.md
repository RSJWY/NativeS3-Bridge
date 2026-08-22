# Implementation Plan

## Ordered Steps

1. **R1/R3: Define storage contracts and shared path preparation**
   - Add `ErrObjectConflict` and the additive `Sidecar.Directory` field.
   - Add helpers for trailing-slash detection, marker sidecar lookup, directory-marker detection, parent-file checks, and regular-vs-directory target conflicts.
   - Ensure `PutObjectWithOptions`, `CopyObject`, and multipart `Complete` use the shared preparation path.

2. **R1/R2: Implement marker writes and reads**
   - Make trailing-slash PUT create a real directory and marker sidecar with empty-object metadata.
   - Validate that marker request bodies are empty and preserve MD5/SHA256 validation; reject non-empty trailing-slash objects explicitly.
   - Make `HeadObject`, `GetObject`, and tag sidecar lookup marker-aware while preserving ordinary-object behavior and Range errors.

3. **R2/R5: Implement marker delete, list, and reconcile semantics**
   - Delete only marker metadata and remove an empty directory when safe.
   - Extend `ListObjects` sorting, delimiter handling, and continuation tokens to include explicit marker objects.
   - Update reconcile to count marker objects and avoid false orphan sidecars.

4. **R3/R4: Wire conflict errors and legacy behavior**
   - Map `ErrObjectConflict` to HTTP 409 `Conflict` in object handlers.
   - Verify legacy file-shaped markers remain listable/readable/deletable as ordinary objects and return 409 for child writes; add migration documentation in the storage spec.

5. **R5: Add focused tests**
   - Add table-driven storage cases for marker PUT/HEAD/GET/DELETE/LIST, child writes, marker pagination/delimiter, conflicts, marker tags, and legacy files.
   - Add handler/server coverage for HTTP 409 and the end-to-end trailing-slash flow.
   - Add reconcile coverage for valid marker sidecars and object/byte counts.

6. **R5: Validate and review**
   - Run `gofmt` on changed Go files.
   - Run `go test ./pkg/storage/... ./pkg/server/...`.
   - Run `go vet ./pkg/storage/... ./pkg/server/...`.
   - Run `git diff --check` and review each acceptance criterion against the tests.

## Risky Files / Rollback Points

- `pkg/storage/file_backend.go`: core path and object semantics; revert this file plus tests if native object behavior regresses.
- `pkg/storage/metadata.go`: additive sidecar schema only; old JSON must remain readable.
- `pkg/storage/multipart.go`: target conflict handling must not alter part validation or cleanup.
- `pkg/handlers/object.go`: only add a specific 409 mapping; retain all existing error mappings.
- `pkg/storage/reconcile.go`: marker sidecars must not inflate byte counts or hide genuine orphan sidecars.

## Requirement Traceability

- R1 -> Steps 1-2 and marker flow tests in Step 5.
- R2 -> Step 3 and list/delete/reconcile tests in Step 5.
- R3 -> Steps 1 and 4 plus conflict tests in Step 5.
- R4 -> Step 4 plus legacy compatibility tests in Step 5.
- R5 -> Steps 5-6.
