# Design: Preserve HEAD Through S3 Reverse Proxy

## Root Cause

The client signs a canonical request whose method is `HEAD`. The production reverse proxy converts the upstream request to `GET`, so NativeS3 correctly computes a different canonical request and returns `SignatureDoesNotMatch`.

Evidence:

- Nginx access log: `HEAD /library/1`.
- NativeS3 auth log for the same operation: `method=GET path=/library/1`.
- A controlled probe signed as HEAD returns 403.
- The same actual HEAD request signed with canonical method GET returns 200.
- Current source tested directly with real AWS CLI handles HeadBucket, missing HeadObject 404, folder marker PUT, and existing HeadObject correctly.

## Fix

For the S3 API Nginx location:

```nginx
proxy_cache off;
proxy_cache_convert_head off;
proxy_set_header Host $http_host;
```

The documentation warns that hosting panels may enable proxy cache from an inherited or included config. S3 authenticated traffic should not use generic reverse-proxy caching.

## Code Contract

Add an auth regression test that signs HEAD and verifies it successfully, then mutates the request method to GET and verifies `SignatureDoesNotMatch`. This documents that reverse proxies must preserve the exact method.

## Compatibility

- No runtime server behavior changes.
- No database or configuration schema changes.
- Existing direct deployments remain unchanged.

## Rollback

Revert the README directives and the focused test. Production rollback is removing the added Nginx directives, though doing so reintroduces the failure when cache conversion is active.
