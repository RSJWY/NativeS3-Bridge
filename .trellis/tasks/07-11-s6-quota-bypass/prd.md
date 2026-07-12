# S6: Close quota bypass vectors

## Goal

Fix TOCTOU race, client-declared size underreport, and multipart temp-data disk exhaustion.

## Requirements

- Quota-consuming writes must atomically reserve capacity in the database before modifying object state; concurrent writes may not collectively exceed a finite credential quota.
- PUT request bodies must not exceed the signed/declared decoded content length. An underreported stream must fail before the storage backend replaces the destination object.
- Successful writes must settle reservations using actual stored byte counts and release unused capacity; failed writes must release their reservation.
- CopyObject and CompleteMultipartUpload must use the same atomic reservation contract based on source/assembled size.
- Pending multipart part data must have a configurable global byte cap and rejected writes must not destroy an existing part.
- Usage statistics must record actual transferred bytes and remain separate from reservation accounting.

## Acceptance Criteria

- [x] Concurrent reservations against one finite quota allow only capacity-fitting writes.
- [x] A body larger than `x-amz-decoded-content-length` or `Content-Length` returns `QuotaExceeded` and preserves an existing object.
- [x] Failed writes release reserved bytes; shorter successful writes release the unused remainder.
- [x] Copy and multipart completion cannot race past quota.
- [x] Multipart pending bytes are capped and over-limit replacement preserves the prior part.
- [x] Quota, handler, server, and storage tests pass, including concurrency coverage.

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
