#!/usr/bin/env bash
#
# Measures how three registries behave as referrers accumulate on one live subject.
#
# The workload is the one a scan or signing pipeline actually produces: a record that
# is re-scanned on a schedule, each run pushing a report whose bytes differ because
# they carry a timestamp. Nothing removes the previous one, so referrers-per-subject
# only ever grows. See bench/README.md for the results and what they mean.
#
# Read the slopes, not the absolute values. cairn runs natively on the host while Zot
# and Distribution run in Docker's Linux VM, which adds a fixed cost to their every
# response that has nothing to do with the registries. The control column exists to
# quantify that offset: it is a HEAD of the subject, work that does not change as
# referrers pile up, so whatever it costs at the start is the floor to subtract before
# comparing anything. What survives that subtraction is the shape of each curve, and
# the shape is the claim being tested.
#
# Usage:
#   ./scripts/bench-referrers.sh              all three, default 2000 referrers
#   ./scripts/bench-referrers.sh cairn        one registry
#   N=500 ./scripts/bench-referrers.sh        shorter run
#
# Environment:
#   N              referrers to push        (default 2000)
#   EVERY          sample interval          (default 50)
#   REPEATS        reads per sample point   (default 5)
#   OUT            results directory        (default bench/results)

set -euo pipefail

cd "$(dirname "$0")/.."

readonly ROOT="$PWD"
readonly N="${N:-2000}"
readonly EVERY="${EVERY:-50}"
readonly REPEATS="${REPEATS:-5}"
readonly OUT="${OUT:-$ROOT/bench/results}"
readonly WORK="$ROOT/.bench"

# Pinned, and both already local. Zot v2.1.20 is the version the production
# investigation ran against; Distribution 3.1.1 is the current v3.
readonly ZOT_IMAGE="ghcr.io/project-zot/zot-linux-arm64:v2.1.20"
readonly DIST_IMAGE="registry:3.1.1"

readonly CAIRN_PORT=5050
readonly ZOT_PORT=5060
readonly DIST_PORT=5070

mkdir -p "$OUT" "$WORK"

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

# --- build ------------------------------------------------------------------

log "building"

go build -o "$WORK/cairnd" ./cmd/cairnd
(cd bench && go build -o "$WORK/bench-referrers" ./referrers)

# --- teardown ---------------------------------------------------------------

CAIRN_PID=""

cleanup() {
	[[ -n "$CAIRN_PID" ]] && kill "$CAIRN_PID" 2>/dev/null || true
	docker rm -f bench-zot bench-dist >/dev/null 2>&1 || true
}

trap cleanup EXIT

# wait_ready polls the base endpoint until the registry answers, so a slow start is
# not measured as a slow registry.
wait_ready() {
	local url="$1" name="$2"

	for _ in $(seq 1 100); do
		if curl -fsS -o /dev/null "$url/v2/" 2>/dev/null; then
			return 0
		fi
		sleep 0.3
	done

	echo "$name did not become ready at $url" >&2
	return 1
}

# run_driver points the driver at one registry and records a CSV.
run_driver() {
	local label="$1" url="$2"

	log "$label: $N referrers, sampling every $EVERY"

	"$WORK/bench-referrers" \
		-registry "$url" \
		-repo "bench/records" \
		-n "$N" \
		-sample-every "$EVERY" \
		-repeats "$REPEATS" \
		-label "$label" \
		-out "$OUT/$label.csv"
}

# --- arms -------------------------------------------------------------------

arm_cairn() {
	rm -rf "$WORK/cairn-root"
	mkdir -p "$WORK/cairn-root"

	"$WORK/cairnd" \
		-addr "127.0.0.1:$CAIRN_PORT" \
		-root "$WORK/cairn-root" \
		-log-level error \
		>"$OUT/cairn.log" 2>&1 &

	CAIRN_PID=$!

	wait_ready "http://127.0.0.1:$CAIRN_PORT" cairn
	run_driver cairn "http://127.0.0.1:$CAIRN_PORT"

	kill "$CAIRN_PID" 2>/dev/null || true
	wait "$CAIRN_PID" 2>/dev/null || true
	CAIRN_PID=""
}

arm_zot() {
	# No extensions configured, and dedupe off. Both deliberate: the search extension
	# triggers a startup metadb walk and dedupe starves readers behind the store-wide
	# lock (project-zot/zot#4349). Those are known, separately measured costs, and
	# leaving them on would mean attributing them to the referrers axis. This is Zot at
	# its best on this workload, not Zot with its worst foot forward.
	cat >"$WORK/zot.json" <<-EOF
		{
		  "distSpecVersion": "1.1.0",
		  "storage": { "rootDirectory": "/var/lib/registry", "dedupe": false, "gc": false },
		  "http": { "address": "0.0.0.0", "port": "5000" },
		  "log": { "level": "warn" }
		}
	EOF

	docker rm -f bench-zot >/dev/null 2>&1 || true
	docker volume rm bench-zot-data >/dev/null 2>&1 || true

	# A named volume, not a bind mount: bind mounts on macOS cross the VM boundary per
	# operation and would make this a measurement of Docker's filesystem sharing.
	docker run -d --name bench-zot \
		-p "$ZOT_PORT:5000" \
		-v bench-zot-data:/var/lib/registry \
		-v "$WORK/zot.json:/etc/zot/config.json:ro" \
		"$ZOT_IMAGE" serve /etc/zot/config.json >/dev/null

	wait_ready "http://127.0.0.1:$ZOT_PORT" zot
	run_driver zot "http://127.0.0.1:$ZOT_PORT"

	docker logs bench-zot >"$OUT/zot.log" 2>&1 || true
	docker rm -f bench-zot >/dev/null 2>&1 || true
}

arm_distribution() {
	docker rm -f bench-dist >/dev/null 2>&1 || true
	docker volume rm bench-dist-data >/dev/null 2>&1 || true

	docker run -d --name bench-dist \
		-p "$DIST_PORT:5000" \
		-v bench-dist-data:/var/lib/registry \
		-e REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY=/var/lib/registry \
		-e REGISTRY_LOG_LEVEL=warn \
		"$DIST_IMAGE" >/dev/null

	wait_ready "http://127.0.0.1:$DIST_PORT" distribution
	run_driver distribution "http://127.0.0.1:$DIST_PORT"

	docker logs bench-dist >"$OUT/distribution.log" 2>&1 || true
	docker rm -f bench-dist >/dev/null 2>&1 || true
}

# --- main -------------------------------------------------------------------

targets=("${@:-cairn zot distribution}")

# shellcheck disable=SC2068
for target in ${targets[@]}; do
	case "$target" in
		cairn) arm_cairn ;;
		zot) arm_zot ;;
		distribution) arm_distribution ;;
		*)
			echo "unknown target: $target" >&2
			exit 1
			;;
	esac
done

log "results"
ls -1 "$OUT"/*.csv
