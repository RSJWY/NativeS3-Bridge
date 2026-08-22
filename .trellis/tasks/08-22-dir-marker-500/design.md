# Technical Design

## 1. Storage Model

Keep the existing native layout for ordinary objects and represent an explicit directory marker as:

```text
bucket/dir/             # real directory, needed by child objects
bucket/dir.s3meta       # sidecar with `directory: true`, marker metadata
bucket/dir/a.txt        # ordinary child object and its sidecar
```

The marker sidecar uses the configured metadata suffix and a backward-compatible `directory` boolean in `Sidecar`. Existing sidecars without the field remain ordinary objects. The sibling sidecar is already excluded from object listing; reconcile must treat it as a valid marker sidecar rather than an orphan.

This preserves native object bytes and avoids an encoded/hidden object namespace. The trade-off is that a key ending in the configured metadata suffix is already reserved by the existing sidecar convention and cannot be used to represent a separate visible object reliably.

## 2. Path and Conflict Contracts

Add storage helpers around the existing `ResolveObjectPath` result:

- detect marker keys from the original key (`strings.HasSuffix(key, "/")`) before path cleaning;
- derive the marker sidecar from the directory target (`target + metadataSuffix`);
- inspect each parent component before `MkdirAll` so a regular file returns `ErrObjectConflict` instead of leaking an `os` error;
- reject ordinary writes to a directory target and marker writes to a regular target with `ErrObjectConflict`.

All ordinary, marker, copy, and multipart-complete writes must use the same parent/target conflict check. The existing atomic temp-file, fsync, close, and rename sequence remains unchanged for regular objects and is not used to create marker bodies.

Map `ErrObjectConflict` in the object handler storage error writer to S3 `Conflict` with HTTP 409. No internal filesystem path is exposed.

## 3. Operation Behavior

### Put / multipart complete / copy

For a trailing-slash key, stream the request only far enough to validate that it is empty (and preserve digest validation), reject non-empty marker bodies with an argument error, ensure the target is a directory, write a zero-size marker sidecar with `directory=true`, empty-object ETag, and marker metadata, then return `ObjectInfo.Key` with the trailing slash. For a regular key, reject a directory target, create parent directories, stream bytes as before, and write the ordinary sidecar. Multipart completion and copy call the same target preparation helper so they cannot recreate the 500 path; multipart completion to a trailing-slash key is rejected unless the completed size is zero.

### Head / Get / tags

`HeadObject` recognizes a marker only when the caller key ends in `/`, the target is a directory, and its marker sidecar has `directory=true`. It returns size 0 and the marker sidecar metadata. A directory without a marker sidecar is only an implicit filesystem prefix and returns `ErrNoSuchKey` for the marker key. `HeadObject("dir")` on a directory remains `ErrNoSuchKey` under the mutual-exclusion contract.

`GetObject` returns an empty `io.ReadCloser` for an explicit marker; it never attempts to stream a directory handle. Range validation still rejects ranges against size zero. Tag operations reuse the marker-aware existing-object/sidecar helper.

### Delete

For `DeleteObject("dir/")`, remove only the marker sidecar. If the directory is empty afterward, remove the directory; otherwise retain it and all children. Deleting a regular object removes its regular sidecar as before. Missing marker sidecars return `ErrNoSuchKey` rather than silently deleting an implicit prefix.

### List

Walk regular files as today, skipping sidecars and internal directories. For each non-bucket directory, inspect its marker sidecar; when `directory=true`, add an `ObjectInfo` for `<relative-path>/` with size 0. Descendant files still produce delimiter common prefixes. Explicit marker objects participate in the same sorted item/token stream as ordinary objects, so pagination remains deterministic.

### Reconcile

When scanning a metadata-suffix file whose base path is a directory with `directory=true`, count it as a marker object and do not report it as orphan. Do not count the directory itself or marker sidecar bytes as object bytes. Existing ordinary sidecars keep their current orphan rules.

## 4. Legacy Data and Rollout

Old marker writes produced a regular zero-byte file at the cleaned path and have no reliable persisted marker bit. For compatibility, a zero-byte regular file with an existing ordinary sidecar is accepted as the trailing-slash alias for `HEAD`/`GET`/`DELETE`; child writes still return `ErrObjectConflict`/409. Migration remains loss-avoiding and documented: verify the file is the known marker, delete the old object key, then issue `PUT <key>/` to create the directory marker. No startup scan guesses based only on zero-byte size.

The new sidecar field is additive JSON, so old sidecars continue to decode. Rollback is code-only: reverting the change leaves new directory markers as directories plus sidecars; old binaries may ignore the marker sidecar and list only children, so deployment should retain the new binary for marker semantics.

## 5. Risk Controls

- Centralize target preparation to keep regular, marker, copy, and multipart paths consistent.
- Test both handler-level 409 mapping and storage-level sentinel errors.
- Test marker deletion with and without children, marker pagination, and reconcile accounting.
- Preserve the existing sidecar filtering and native-byte invariants.
