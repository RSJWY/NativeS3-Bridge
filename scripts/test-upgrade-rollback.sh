#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$root_dir/scripts/lib/integration-test-helpers.sh"
cd "$root_dir"

: "${LEGACY_REF:=5f0be5c}"

for command_name in git go aws curl python3; do
	if ! command -v "$command_name" >/dev/null 2>&1; then
		printf 'upgrade/rollback test requires %s\n' "$command_name" >&2
		exit 1
	fi
done

tmp_dir="$(mktemp -d /tmp/natives3-upgrade-rollback.XXXXXX)"
legacy_worktree="$tmp_dir/legacy-src"
legacy_pid=""
node_pid=""
rollback_pid=""

cleanup() {
	stop_process "$rollback_pid"
	stop_process "$node_pid"
	stop_process "$legacy_pid"
	if [ -d "$legacy_worktree" ]; then
		git worktree remove --force "$legacy_worktree" >/dev/null 2>&1 || true
	fi
	rm -rf "$tmp_dir"
}
trap cleanup EXIT

git cat-file -e "$LEGACY_REF^{commit}"
git worktree add --detach "$legacy_worktree" "$LEGACY_REF" >/dev/null

printf 'building legacy standalone from %s\n' "$LEGACY_REF"
(cd "$legacy_worktree" && GOWORK=off go build -o "$tmp_dir/natives3bridge-legacy" ./cmd/natives3bridge)
printf 'building current node\n'
GOWORK=off go build -o "$tmp_dir/natives3-node-current" ./cmd/node

s3_port="$(pick_free_port)"
admin_port="$(pick_free_port)"
data_root="$tmp_dir/data"
db_path="$tmp_dir/natives3.db"
legacy_config="$tmp_dir/legacy.yaml"
node_config="$tmp_dir/node.yaml"
bucket="upgrade-bucket"
access_key="AKIAUPGRADEE2E"
secret_key="upgrade-secret-key-e2e"
endpoint="http://127.0.0.1:$s3_port"

mkdir -p "$data_root"

cat > "$legacy_config" <<EOF
server:
  s3_addr: "127.0.0.1:$s3_port"
  admin_addr: "127.0.0.1:$admin_port"
storage:
  data_root: "$data_root"
  multipart_tmp: "$data_root/.multipart"
  metadata_suffix: ".s3meta"
database:
  driver: "sqlite"
  dsn: "$db_path"
hooks:
  queue_size: 16
  workers: 1
  max_retry: 1
  timeout: "1s"
webadmin:
  admin_bootstrap_password: "upgrade-admin-password"
  session_secret: "upgrade-test-session-secret-32-bytes"
rate_limit:
  anonymous_rps: 10
  anonymous_burst: 20
region: "us-east-1"
log_level: "info"
EOF

cat > "$node_config" <<EOF
server:
  s3_addr: "127.0.0.1:$s3_port"
  admin_addr: "127.0.0.1:$admin_port"
storage:
  data_root: "$data_root"
  multipart_tmp: "$data_root/.multipart"
  metadata_suffix: ".s3meta"
database:
  driver: "sqlite"
  dsn: "$db_path"
panel:
  node_id: 1
  agent_url: "wss://127.0.0.1:1/agent"
  register_url: ""
  registration_token: ""
  cert_file: "$tmp_dir/pki/node.crt"
  key_file: "$tmp_dir/pki/node.key"
  ca_file: "$tmp_dir/pki/panel-ca.crt"
  heartbeat_interval: 1s
webadmin:
  password_hash: "legacy-field-must-be-ignored"
  session_secret: "legacy-field-must-be-ignored"
rate_limit:
  anonymous_rps: 99
  anonymous_burst: 99
region: "us-east-1"
log_level: "info"
EOF

export AWS_ACCESS_KEY_ID="$access_key"
export AWS_SECRET_ACCESS_KEY="$secret_key"
export AWS_DEFAULT_REGION="us-east-1"
export AWS_EC2_METADATA_DISABLED=true
export AWS_PAGER=""

aws_s3() {
	aws --no-cli-pager --endpoint-url "$endpoint" "$@"
}

printf 'legacy-object-from-standalone\n' > "$tmp_dir/legacy-object.txt"
printf 'node-object-after-upgrade\n' > "$tmp_dir/node-object.txt"
printf 'rollback-object-after-downgrade\n' > "$tmp_dir/rollback-object.txt"

printf 'phase 1/3: starting legacy standalone\n'
"$tmp_dir/natives3bridge-legacy" \
	-config "$legacy_config" \
	-seed-access-key "$access_key" \
	-seed-secret-key "$secret_key" \
	>"$tmp_dir/legacy.log" 2>&1 &
legacy_pid=$!
wait_http "$endpoint/" "$legacy_pid" "$tmp_dir/legacy.log"
aws_s3 s3api create-bucket --bucket "$bucket" >/dev/null
aws_s3 s3api put-object --bucket "$bucket" --key legacy.txt --body "$tmp_dir/legacy-object.txt" >/dev/null
aws_s3 s3api get-object --bucket "$bucket" --key legacy.txt "$tmp_dir/legacy-before-upgrade.txt" >/dev/null
cmp "$tmp_dir/legacy-object.txt" "$tmp_dir/legacy-before-upgrade.txt"
stop_process "$legacy_pid"
legacy_pid=""

go run ./scripts/internal/upgrade-inspect \
	-db "$db_path" -access-key "$access_key" -secret-key "$secret_key" -bucket "$bucket" -expect-agent=false

printf 'phase 2/3: starting current node with panel unavailable\n'
"$tmp_dir/natives3-node-current" -config "$node_config" >"$tmp_dir/node.log" 2>&1 &
node_pid=$!
wait_http "$endpoint/" "$node_pid" "$tmp_dir/node.log"
aws_s3 s3api get-object --bucket "$bucket" --key legacy.txt "$tmp_dir/legacy-after-upgrade.txt" >/dev/null
cmp "$tmp_dir/legacy-object.txt" "$tmp_dir/legacy-after-upgrade.txt"
aws_s3 s3api put-object --bucket "$bucket" --key node.txt --body "$tmp_dir/node-object.txt" >/dev/null
aws_s3 s3api get-object --bucket "$bucket" --key node.txt "$tmp_dir/node-after-upgrade.txt" >/dev/null
cmp "$tmp_dir/node-object.txt" "$tmp_dir/node-after-upgrade.txt"
stop_process "$node_pid"
node_pid=""

go run ./scripts/internal/upgrade-inspect \
	-db "$db_path" -access-key "$access_key" -secret-key "$secret_key" -bucket "$bucket" -expect-agent=true

shopt -s nullglob
upgrade_backups=("$db_path".pre-upgrade-*.bak*)
shopt -u nullglob
if [ "${#upgrade_backups[@]}" -lt 1 ]; then
	printf 'node upgrade did not create a SQLite pre-upgrade backup\n' >&2
	exit 1
fi
upgrade_backup="${upgrade_backups[0]}"
go run ./scripts/internal/upgrade-inspect \
	-db "$upgrade_backup" -access-key "$access_key" -secret-key "$secret_key" -bucket "$bucket" -expect-agent=false

printf 'phase 3/3: rolling back to legacy standalone\n'
"$tmp_dir/natives3bridge-legacy" -config "$legacy_config" >"$tmp_dir/rollback.log" 2>&1 &
rollback_pid=$!
wait_http "$endpoint/" "$rollback_pid" "$tmp_dir/rollback.log"
aws_s3 s3api get-object --bucket "$bucket" --key legacy.txt "$tmp_dir/legacy-after-rollback.txt" >/dev/null
cmp "$tmp_dir/legacy-object.txt" "$tmp_dir/legacy-after-rollback.txt"
aws_s3 s3api get-object --bucket "$bucket" --key node.txt "$tmp_dir/node-after-rollback.txt" >/dev/null
cmp "$tmp_dir/node-object.txt" "$tmp_dir/node-after-rollback.txt"
aws_s3 s3api put-object --bucket "$bucket" --key rollback.txt --body "$tmp_dir/rollback-object.txt" >/dev/null
aws_s3 s3api get-object --bucket "$bucket" --key rollback.txt "$tmp_dir/rollback-after-rollback.txt" >/dev/null
cmp "$tmp_dir/rollback-object.txt" "$tmp_dir/rollback-after-rollback.txt"
stop_process "$rollback_pid"
rollback_pid=""

go run ./scripts/internal/upgrade-inspect \
	-db "$db_path" -access-key "$access_key" -secret-key "$secret_key" -bucket "$bucket" -expect-agent=true

printf 'upgrade and rollback integration test passed\n'
