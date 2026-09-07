#!/usr/bin/env bash
#
# Run the OCI distribution-spec conformance suite against a throwaway cairn.
#
# The suite is the only answer to "is this compliant" that is not an opinion: it is
# maintained by the same people as the spec, and it is what a registry claiming
# conformance is expected to publish. Everything here is disposable -- a fresh
# database, a fresh blob store, both deleted on exit -- because the suite pushes and
# deletes real content and must never be pointed at a store you care about.
#
# Usage:
#   scripts/conformance.sh              # run everything, report to ./conformance-report
#   CAIRN_PORT=5099 scripts/conformance.sh
#
# The spec version is pinned. An unpinned suite would make a passing run mean
# "compliant with whatever main said today", which is not a claim anyone can check
# later.

set -euo pipefail

SPEC_VERSION="${SPEC_VERSION:-v1.1.1}"
CAIRN_PORT="${CAIRN_PORT:-5060}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="${repo_root}/.conformance"
report_dir="${CONFORMANCE_REPORT_DIR:-${repo_root}/conformance-report}"

mkdir -p "${work}" "${report_dir}"

# The suite is cloned rather than vendored, and at a depth of one: it is a test
# dependency of a test, and nothing in cairn imports it.
if [ ! -d "${work}/spec" ]; then
  echo "==> fetching distribution-spec ${SPEC_VERSION}"
  git clone --quiet --depth 1 --branch "${SPEC_VERSION}" \
    https://github.com/opencontainers/distribution-spec.git "${work}/spec"
fi

if [ ! -x "${work}/conformance.test" ]; then
  echo "==> building the conformance binary"
  (cd "${work}/spec/conformance" && go test -c -o "${work}/conformance.test" .)
fi

echo "==> building cairnd"
go build -o "${work}/cairnd" ./cmd/cairnd

data="$(mktemp -d)"
daemon_log="${report_dir}/daemon.log"

cleanup() {
  if [ -n "${daemon_pid:-}" ]; then
    kill "${daemon_pid}" 2>/dev/null || true
    wait "${daemon_pid}" 2>/dev/null || true
  fi

  rm -rf "${data}"
}

trap cleanup EXIT

echo "==> starting cairnd on 127.0.0.1:${CAIRN_PORT} (root ${data})"

# warn, not info: the suite deliberately provokes 4xx responses, and an access log
# of every one of them buries the result. The log is kept so a failure can be
# explained afterwards.
"${work}/cairnd" \
  --addr "127.0.0.1:${CAIRN_PORT}" \
  --root "${data}" \
  --log-format json \
  --log-level warn > "${daemon_log}" 2>&1 &
daemon_pid=$!

for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:${CAIRN_PORT}/v2/" > /dev/null 2>&1; then
    break
  fi

  sleep 0.1
done

if ! curl -fsS "http://127.0.0.1:${CAIRN_PORT}/v2/" > /dev/null 2>&1; then
  echo "cairnd did not come up; see ${daemon_log}" >&2
  exit 1
fi

export OCI_ROOT_URL="http://127.0.0.1:${CAIRN_PORT}"
export OCI_NAMESPACE="myorg/myrepo"
export OCI_CROSSMOUNT_NAMESPACE="myorg/other"

export OCI_TEST_PULL=1
export OCI_TEST_PUSH=1
export OCI_TEST_CONTENT_DISCOVERY=1
export OCI_TEST_CONTENT_MANAGEMENT=1

# Not a preference but a declaration of what cairn does: end-11 without "from"
# resolves the blob itself and returns 201. Setting this to 0 asserts the opposite
# and fails, which is the suite working correctly.
export OCI_AUTOMATIC_CROSSMOUNT=1

# Manifests before blobs, because a manifest may not reference content this
# repository lacks -- so removing a layer first would leave a manifest that no
# longer resolves, and the teardown would be exercising a state a push could never
# have produced.
export OCI_DELETE_MANIFEST_BEFORE_BLOBS=1

export OCI_HIDE_SKIPPED_WORKFLOWS=1
export OCI_REPORT_DIR="${report_dir}"

echo "==> running the suite"

status=0
(cd "${report_dir}" && "${work}/conformance.test") || status=$?

echo
echo "==> report: ${report_dir}/report.html"
echo "==> daemon log: ${daemon_log}"

exit "${status}"
