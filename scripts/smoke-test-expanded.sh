#!/usr/bin/env bash
set -euo pipefail

: "${EP:=--endpoint-url http://localhost:9000}"
: "${EP_HOST:=http://localhost:9000}"
: "${AWS_ACCESS_KEY_ID:=x}"
: "${AWS_SECRET_ACCESS_KEY:=x}"
: "${AWS_DEFAULT_REGION:=us-east-1}"
: "${DATA_ROOT:=./data}"
: "${SMOKE_BUCKET:=test-bucket}"
: "${SMOKE_PREFIX:=expanded-smoke-$$}"
: "${EXPECT_MULTIPART_EMPTY:=1}"
: "${ENABLE_WEBHOOK_CHECK:=0}"
: "${WEBHOOK_ADDR:=127.0.0.1:18080}"

export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_DEFAULT_REGION

tmp_dir="$(mktemp -d /tmp/natives3-expanded-smoke.XXXXXX)"
receiver_pid=""

cleanup() {
	if [ -n "$receiver_pid" ]; then
		kill "$receiver_pid" >/dev/null 2>&1 || true
		wait "$receiver_pid" >/dev/null 2>&1 || true
	fi
	rm -rf "$tmp_dir"
}
trap cleanup EXIT

fail() {
	printf 'expanded smoke failed: %s\n' "$*" >&2
	exit 1
}

object_path() {
	printf '%s/%s/%s\n' "$DATA_ROOT" "$SMOKE_BUCKET" "$1"
}

wait_for_event() {
	event_type="$1"
	key="$2"
	events_file="$3"
	for _ in $(seq 1 50); do
		if grep -q "\"type\":\"$event_type\"" "$events_file" && grep -q "\"key\":\"$key\"" "$events_file"; then
			return 0
		fi
		sleep 0.1
	done
	printf 'webhook events received:\n' >&2
	cat "$events_file" >&2 || true
	fail "missing webhook event $event_type for $key"
}

ensure_bucket() {
	if ! aws $EP s3api head-bucket --bucket "$SMOKE_BUCKET" >/dev/null 2>&1; then
		aws $EP s3 mb "s3://$SMOKE_BUCKET" >/dev/null
	fi
}

ensure_bucket

meta_key="$SMOKE_PREFIX/meta.txt"
meta_src="$tmp_dir/meta.txt"
printf 'metadata smoke\n' > "$meta_src"
aws $EP s3api put-object \
	--bucket "$SMOKE_BUCKET" \
	--key "$meta_key" \
	--body "$meta_src" \
	--content-type text/plain \
	--metadata author=alice,team=release >/dev/null

head_json="$tmp_dir/head.json"
aws $EP s3api head-object --bucket "$SMOKE_BUCKET" --key "$meta_key" > "$head_json"
grep -Eiq '"author"[[:space:]]*:[[:space:]]*"alice"' "$head_json" || fail "metadata author was not returned by head-object"
grep -Eiq '"team"[[:space:]]*:[[:space:]]*"release"' "$head_json" || fail "metadata team was not returned by head-object"

aws $EP s3api put-object-tagging \
	--bucket "$SMOKE_BUCKET" \
	--key "$meta_key" \
	--tagging 'TagSet=[{Key=env,Value=smoke},{Key=kind,Value=e2e}]' >/dev/null
tags_json="$tmp_dir/tags.json"
aws $EP s3api get-object-tagging --bucket "$SMOKE_BUCKET" --key "$meta_key" > "$tags_json"
grep -Eq '"Key"[[:space:]]*:[[:space:]]*"env"' "$tags_json" || fail "tag key env was not returned"
grep -Eq '"Value"[[:space:]]*:[[:space:]]*"smoke"' "$tags_json" || fail "tag value smoke was not returned"
aws $EP s3api delete-object-tagging --bucket "$SMOKE_BUCKET" --key "$meta_key" >/dev/null
aws $EP s3api get-object-tagging --bucket "$SMOKE_BUCKET" --key "$meta_key" > "$tags_json"
if grep -Eq '"Key"[[:space:]]*:[[:space:]]*"env"' "$tags_json"; then
	fail "tag env remained after delete-object-tagging"
fi

multipart_key="$SMOKE_PREFIX/multipart.bin"
multipart_src="$tmp_dir/multipart.bin"
multipart_dst="$tmp_dir/multipart-downloaded.bin"
dd if=/dev/zero of="$multipart_src" bs=1M count=10 status=none
aws $EP s3 cp "$multipart_src" "s3://$SMOKE_BUCKET/$multipart_key" >/dev/null
test -f "$(object_path "$multipart_key")" || fail "multipart final native file is missing"
cmp "$multipart_src" "$(object_path "$multipart_key")" || fail "multipart native file bytes differ"
aws $EP s3 cp "s3://$SMOKE_BUCKET/$multipart_key" "$multipart_dst" >/dev/null
cmp "$multipart_src" "$multipart_dst" || fail "multipart downloaded bytes differ"

if [ "$EXPECT_MULTIPART_EMPTY" = "1" ] && [ -d "$DATA_ROOT/.multipart" ]; then
	if find "$DATA_ROOT/.multipart" -mindepth 1 -maxdepth 1 | grep -q .; then
		find "$DATA_ROOT/.multipart" -mindepth 1 -maxdepth 2 >&2
		fail "multipart temp directory was not cleaned"
	fi
fi

presigned_url="$(aws $EP s3 presign "s3://$SMOKE_BUCKET/$meta_key" --expires-in 60)"
presigned_dst="$tmp_dir/presigned.txt"
curl -fsS "$presigned_url" -o "$presigned_dst"
cmp "$meta_src" "$presigned_dst" || fail "presigned GET bytes differ"

if [ "$ENABLE_WEBHOOK_CHECK" = "1" ]; then
	events_file="$tmp_dir/webhook-events.jsonl"
	: > "$events_file"
	go run ./scripts/internal/smoke/webhook-receiver -addr "$WEBHOOK_ADDR" -out "$events_file" >/dev/null 2>&1 &
	receiver_pid=$!
	for _ in $(seq 1 40); do
		if curl -fsS "http://$WEBHOOK_ADDR/healthz" >/dev/null; then
			break
		fi
		if ! kill -0 "$receiver_pid" >/dev/null 2>&1; then
			fail "webhook receiver exited early"
		fi
		sleep 0.1
	done

	hook_key="$SMOKE_PREFIX/hook.txt"
	hook_src="$tmp_dir/hook.txt"
	printf 'webhook smoke\n' > "$hook_src"
	aws $EP s3api put-object --bucket "$SMOKE_BUCKET" --key "$hook_key" --body "$hook_src" >/dev/null
	wait_for_event ObjectCreated "$hook_key" "$events_file"
	aws $EP s3api delete-object --bucket "$SMOKE_BUCKET" --key "$hook_key" >/dev/null
	wait_for_event ObjectDeleted "$hook_key" "$events_file"
fi

printf 'expanded smoke passed\n'
