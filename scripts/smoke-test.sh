#!/usr/bin/env bash
set -euo pipefail

: "${EP:=--endpoint-url http://localhost:9000}"
: "${EP_HOST:=http://localhost:9000}"
: "${AWS_ACCESS_KEY_ID:=x}"
: "${AWS_SECRET_ACCESS_KEY:=x}"
: "${AWS_DEFAULT_REGION:=us-east-1}"
: "${DATA_ROOT:=./data}"

export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_DEFAULT_REGION

src="/tmp/natives3-a.txt"
dst="/tmp/natives3-b.txt"
range_dst="/tmp/natives3-range.txt"
private_dst="/tmp/natives3-private.txt"

cleanup() {
	rm -f "$src" "$dst" "$range_dst" "$private_dst"
}
trap cleanup EXIT

printf 'hello native s3\n' > "$src"
aws $EP s3 mb s3://test-bucket
aws $EP s3 cp "$src" s3://test-bucket/dir/a.txt
test -f "$DATA_ROOT/test-bucket/dir/a.txt"
aws $EP s3 cp s3://test-bucket/dir/a.txt "$dst"
diff "$src" "$dst"
aws $EP s3api head-object --bucket test-bucket --key dir/a.txt
aws $EP s3 ls s3://test-bucket/dir/
aws $EP s3 ls
aws $EP s3api get-object --bucket test-bucket --key dir/a.txt --range bytes=0-4 "$range_dst"
test "$(wc -c < "$range_dst")" -eq 5
private_status="$(curl -sS -w '%{http_code}' -o "$private_dst" -r 0-4 "$EP_HOST/test-bucket/dir/a.txt")"
test "$private_status" = "403"
aws $EP s3 rm s3://test-bucket/dir/a.txt
if curl -sS -f "$EP_HOST/test-bucket/dir/a.txt" >/dev/null; then
	printf 'expected deleted object GET to fail\n' >&2
	exit 1
fi
