# Design: Atomic quota reservations

## Contract

Separate quota capacity accounting from request statistics. A write first creates a database-backed reservation by conditionally incrementing `credentials.used_bytes`. The write then either settles against the actual stored size or releases the reservation on every failure path.

## Flows

- PUT Object: reserve declared decoded length, wrap the body in a hard limit, write through the backend's temporary-file path, then settle to actual size.
- Copy Object: inspect source size, reserve it, copy, then settle using actual destination size.
- Complete Multipart Upload: compute selected part size, reserve it, merge, then settle using actual object size.
- Delete: atomically decrease usage after successful deletion.
- GET: update statistics only.

## Safety

The reservation SQL update includes `quota_bytes = 0 OR used_bytes + amount <= quota_bytes`, so competing requests cannot both reserve the same remaining capacity. A request-scoped reservation token guarantees release when a handler exits without settlement.

Body limiting returns `quota.ErrQuotaExceeded` from the reader after the declared byte budget. The file backend treats this as a copy failure, removes its temporary file, and leaves an existing destination untouched.

## Multipart temporary storage

`MultipartStore` serializes pending-byte accounting and enforces `storage.multipart_max_pending_bytes`. Replacement uploads credit the old part size; rejected replacements leave the old part intact.
