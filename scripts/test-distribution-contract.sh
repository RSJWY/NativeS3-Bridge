#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"

require_text() {
	file="$1"
	text="$2"
	if ! grep -Fq -- "$text" "$file"; then
		printf 'distribution contract failed: %s is missing %s\n' "$file" "$text" >&2
		exit 1
	fi
}

require_absent() {
	file="$1"
	text="$2"
	if grep -Fq -- "$text" "$file"; then
		printf 'distribution contract failed: %s unexpectedly contains %s\n' "$file" "$text" >&2
		exit 1
	fi
}

require_text Dockerfile 'FROM alpine:3.20 AS panel'
require_text Dockerfile 'FROM alpine:3.20 AS node'
require_text Dockerfile 'FROM go-base AS panel-build'
require_text Dockerfile 'FROM go-base AS node-build'
require_text Dockerfile 'EXPOSE 9001 9443'
require_text Dockerfile 'EXPOSE 9000'
require_text Dockerfile 'ENTRYPOINT ["panel"]'
require_text Dockerfile 'ENTRYPOINT ["node"]'
require_text .dockerignore '**/node_modules'
require_text .dockerignore '.opencode'

require_text docker-compose.panel.yml 'ghcr.io/rsjwy/natives3-panel:${NATIVES3_TAG:-latest}'
require_text docker-compose.panel.yml '127.0.0.1:9001:9001'
require_text docker-compose.panel.yml '9443:9443'
require_text docker-compose.panel.yml 'test: ["CMD", "/usr/local/bin/panel", "-check-config"'
require_absent docker-compose.panel.yml '9000:9000'
require_absent docker-compose.panel.yml 'build:'

require_text docker-compose.node.yml 'ghcr.io/rsjwy/natives3-node:${NATIVES3_TAG:-latest}'
require_text docker-compose.node.yml '9000:9000'
require_text docker-compose.node.yml 'test: ["CMD", "/usr/local/bin/node", "-health"'
require_absent docker-compose.node.yml '9001:9001'
require_absent docker-compose.node.yml '9443:9443'
require_absent docker-compose.node.yml 'build:'

[[ ! -e docker-compose.example.yml ]] || {
	printf 'distribution contract failed: obsolete docker-compose.example.yml still exists\n' >&2
	exit 1
}

for installer in scripts/install-panel.sh scripts/install-node.sh; do
	[[ -x "$installer" ]] || {
		printf 'distribution contract failed: %s is not executable\n' "$installer" >&2
		exit 1
	}
	require_text "$installer" '--install-dir'
	require_text "$installer" '--tag'
	require_text "$installer" '--force'
	require_text "$installer" '--no-start'
	require_text "$installer" 'docker compose'
done
require_text scripts/install-panel.sh 'ghcr.io/rsjwy/natives3-panel'
require_text scripts/install-panel.sh 'openssl rand -out'
require_text scripts/install-panel.sh 'panel-ca.crt'
require_text scripts/install-panel.sh '127.0.0.1:9001:9001'
require_text scripts/install-node.sh 'ghcr.io/rsjwy/natives3-node'
require_text scripts/install-node.sh '--panel-url'
require_text scripts/install-node.sh '--registration-token'
require_text scripts/install-node.sh '--ca-file'
require_text scripts/install-node.sh '9000:9000'

require_text docs/docker-deployment.md 'install-panel.sh'
require_text docs/docker-deployment.md 'install-node.sh'
require_text docs/docker-deployment.md '完整手动部署'
require_text README.md 'docs/docker-deployment.md'
require_absent README.md 'docker-compose.example.yml'
require_absent README.md '镜像尚未正式发布'
require_absent README.md '尚未正式发布'

require_text .github/workflows/release.yml 'component: [panel, node]'
require_text .github/workflows/release.yml 'target: ${{ matrix.component }}'
require_text .github/workflows/release.yml '/natives3-${{ matrix.component }}:${{ needs.prepare.outputs.docker_tag }}'
require_text .github/workflows/release.yml 'cache-from: type=gha,scope=natives3-${{ matrix.component }}'
require_text .github/workflows/release.yml 'cache-to: type=gha,mode=max,scope=natives3-${{ matrix.component }}'
require_text .github/workflows/release.yml 'provenance: mode=min'
require_text .github/workflows/release.yml 'sbom: false'
require_text .github/workflows/release.yml 'e2e:'
require_text .github/workflows/release.yml 'needs: [prepare, quality]'
require_text .github/workflows/release.yml 'E2E_MODE: local'
require_text .github/workflows/release.yml 'E2E_MODE: docker'
require_text .github/workflows/release.yml 'if: ${{ failure() }}'
require_text .github/workflows/release.yml 'needs: [prepare, quality, e2e]'
require_text .github/workflows/release.yml 'needs: [prepare, artifacts, images, e2e]'
[[ -x scripts/test-panel-node-e2e.sh ]] || {
	printf 'distribution contract failed: scripts/test-panel-node-e2e.sh is not executable\n' >&2
	exit 1
}
require_text scripts/test-panel-node-e2e.sh '/api/admin/auth-settings'
require_text scripts/test-panel-node-e2e.sh '--aws-sigv4'
require_text scripts/test-panel-node-e2e.sh 'wrong-CA registration failed closed'
require_text scripts/test-panel-node-e2e.sh 'browser-report.json'
require_text scripts/test-panel-node-e2e.sh '/api/admin/nodes/1/tasks'
require_text scripts/test-panel-node-e2e.sh 'bounded structured Node ring log query over mTLS'
require_text scripts/test-panel-node-e2e.sh 'authenticated bounded Panel local log query'
[[ -x scripts/internal/e2e-browser.py ]] || {
	printf 'distribution contract failed: scripts/internal/e2e-browser.py is not executable\n' >&2
	exit 1
}
require_text scripts/internal/e2e-browser.py 'no same-origin /api/admin/nodes request was observed'
require_text scripts/internal/e2e-browser.py 'panel_nodes_api'
require_text scripts/internal/e2e-browser.py 'no same-origin /api/admin/logs request was observed'
require_text scripts/internal/e2e-browser.py 'panel_logs_api'
require_text scripts/internal/e2e-browser.py 'no same-origin Node task dispatch request was observed'
require_text scripts/internal/e2e-browser.py 'node_logs_ui'

e2e_line="$(grep -n '^  e2e:' .github/workflows/release.yml | head -n 1 | cut -d: -f1)"
artifacts_line="$(grep -n '^  artifacts:' .github/workflows/release.yml | head -n 1 | cut -d: -f1)"
images_line="$(grep -n '^  images:' .github/workflows/release.yml | head -n 1 | cut -d: -f1)"
release_line="$(grep -n '^  release:' .github/workflows/release.yml | head -n 1 | cut -d: -f1)"
if [[ -z "$e2e_line" || -z "$artifacts_line" || -z "$images_line" || -z "$release_line" ]] || \
	((e2e_line >= artifacts_line || e2e_line >= images_line || e2e_line >= release_line)); then
	printf 'distribution contract failed: e2e job must precede artifacts/images/release\n' >&2
	exit 1
fi

if grep -Fq 'cmd/natives3bridge' .github/workflows/release.yml; then
	printf 'distribution contract failed: release workflow still builds cmd/natives3bridge\n' >&2
	exit 1
fi

node_build_stage="$(awk '/^FROM go-base AS node-build$/{in_stage=1} in_stage{print} in_stage && /^FROM / && $0 !~ /AS node-build$/{exit}' Dockerfile)"
if grep -Eq -- '--from=web|cmd/panel' <<<"$node_build_stage"; then
	printf 'distribution contract failed: node-build depends on the web or panel build\n' >&2
	exit 1
fi

printf 'distribution contract passed\n'
