#!/usr/bin/env bash
set -euo pipefail

: "${EP:=--endpoint-url http://localhost:9000}"
: "${EP_HOST:=http://localhost:9000}"
: "${AWS_ACCESS_KEY_ID:=x}"
: "${AWS_SECRET_ACCESS_KEY:=x}"
: "${AWS_DEFAULT_REGION:=us-east-1}"

export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_DEFAULT_REGION

src="/tmp/natives3-a.txt"
dst="/tmp/natives3-b.txt"
range_dst="/tmp/natives3-range.txt"

printf 'hello native s3\n' > "$src"
aws $EP s3 cp "$src" s3://test-bucket/dir/a.txt
test -f ./data/test-bucket/dir/a.txt
aws $EP s3 cp s3://test-bucket/dir/a.txt "$dst"
diff "$src" "$dst"
aws $EP s3api head-object --bucket test-bucket --key dir/a.txt
aws $EP s3 ls s3://test-bucket/dir/
aws $EP s3 ls
range_status="$(curl -fsS -w '%{http_code}' -o "$range_dst" -r 0-4 "$EP_HOST/test-bucket/dir/a.txt")"
test "$range_status" = "206"
test "$(wc -c < "$range_dst")" -eq 5
aws $EP s3 rm s3://test-bucket/dir/a.txt
if curl -fsS "$EP_HOST/test-bucket/dir/a.txt" >/dev/null; then
	printf 'expected deleted object GET to fail\n' >&2
	exit 1
fi
