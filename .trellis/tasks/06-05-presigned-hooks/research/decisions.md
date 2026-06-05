# Decisions

- Hook queue size defaults to 1024, matching the frozen design. `Emit` drops and logs when full so S3 request latency is never blocked by webhook backpressure.
- Hook workers default to 4, max retry defaults to 3, and webhook timeout defaults to 5s. These are exposed under `hooks.queue_size`, `hooks.workers`, `hooks.max_retry`, and `hooks.timeout` in config.
- Presigned URL verification uses `UNSIGNED-PAYLOAD` for the canonical request payload hash, matching the task design and AWS S3 query-presign convention.
