#!/usr/bin/env bash
#
# Attaches referrers to one subject concurrently and counts how many survive.
#
# This reproduces a test from the registry evaluation that preceded cairn, where
# Distribution lost data silently: every concurrent batch collapsed to exactly one
# survivor, and every attach still exited 0. Cause is that a registry without a
# referrers API makes the client maintain a shared index document under a tag derived
# from the subject, so two attaches read the same version and each writes back one
# missing the other's entry. Last writer wins the whole document.
#
# The original arms were 6 serial and 2/4/6/8 concurrent. They are kept verbatim so the
# numbers are comparable, and higher arms are added because a registry that answers
# end-12 has no shared document to race on and should clear the original bar trivially
# -- the interesting question for cairn is not whether it passes at 8 but where it stops
# passing, and whether the failure is loud when it comes.
#
# oras attach is the client, deliberately: it is the same oras-go path Directory's
# PushReferrer uses, and it picks the native or fallback route by itself. Reproducing
# the test with a bespoke client would prove something about the client instead.
#
# Usage:
#   ./scripts/concurrency.sh                    all three registries
#   ./scripts/concurrency.sh cairn              one registry
#   ARMS="8 32 64" ./scripts/concurrency.sh cairn
#
# Environment:
#   ARMS   concurrency levels to run   (default "2 4 6 8 16 32 64")
#   OUT    results directory           (default bench/results)

set -euo pipefail

cd "$(dirname "$0")/.."

readonly ROOT="$PWD"
readonly ARMS="${ARMS:-2 4 6 8 16 32 64}"
readonly OUT="${OUT:-$ROOT/bench/results}"
readonly WORK="$ROOT/.bench"

# Relative, and deliberately so: oras refuses an absolute file argument unless told to
# skip path validation, and skipping a safety check to satisfy a test script is the wrong
# trade. The script cd's to the repo root above, so this resolves the same either way.
readonly ATTACH=".bench/attach"

readonly ZOT_IMAGE="ghcr.io/project-zot/zot-linux-arm64:v2.1.20"
readonly DIST_IMAGE="registry:3.1.1"

readonly CAIRN_PORT=5050
readonly ZOT_PORT=5060
readonly DIST_PORT=5070

mkdir -p "$OUT" "$ATTACH"

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

CAIRN_PID=""

cleanup() {
	[[ -n "$CAIRN_PID" ]] && kill "$CAIRN_PID" 2>/dev/null || true
	docker rm -f conc-zot conc-dist >/dev/null 2>&1 || true
}

trap cleanup EXIT

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

# push_subject creates the artifact everything will attach to, and prints its digest.
push_subject() {
	local host="$1" repo="$2"

	printf 'the record itself\n' >"$ATTACH/record.json"

	# Output goes to a log rather than /dev/null: a subject that fails to push makes every
	# arm below it meaningless, so this is the one step whose errors must survive.
	oras push --plain-http --no-tty \
		"$host/$repo:v1" \
		"$ATTACH/record.json:application/json" \
		>"$ATTACH/push-subject.log" 2>&1

	oras manifest fetch --plain-http --descriptor "$host/$repo:v1" \
		2>>"$ATTACH/push-subject.log" |
		sed -n 's/.*"digest":"\([^"]*\)".*/\1/p'
}

# survivors counts the referrers the registry will admit to having, by the same route a
# client would use. oras discover follows the native API or the fallback tag on its own,
# which is the point: a referrer that exists but is unreachable is lost, and this counts
# reachable ones.
survivors() {
	local host="$1" repo="$2" subject="$3"

	oras discover --plain-http --format json "$host/$repo@$subject" 2>/dev/null |
		grep -c '"artifactType"' || true
}

# run_arm attaches $count referrers to a fresh subject, either serially or all at once,
# and reports failures and survivors.
run_arm() {
	local label="$1" host="$2" count="$3" mode="$4"

	# A fresh repository per arm, so one arm's survivors cannot be counted in another's
	# and a lost referrer cannot be masked by an earlier success.
	local repo="conc/$mode-$count"
	local subject
	subject="$(push_subject "$host" "$repo")"

	if [[ -z "$subject" ]]; then
		echo "could not resolve subject for $label $mode-$count" >&2
		return 1
	fi

	local pids=() failures=0 i

	for ((i = 1; i <= count; i++)); do
		# Distinct bytes per attach, so each is a genuinely different manifest. Identical
		# payloads would collapse to one digest and the test would pass by accident, which
		# is the failure mode it exists to detect.
		printf '{"run":%d,"nonce":"%s-%s"}\n' "$i" "$(date -u +%s)" "$RANDOM" \
			>"$ATTACH/report-$i.json"

		if [[ "$mode" == serial ]]; then
			oras attach --plain-http --no-tty \
				--artifact-type "application/vnd.example.scanreport.v1+json" \
				"$host/$repo@$subject" \
				"$ATTACH/report-$i.json:application/json" \
				>"$ATTACH/out-$i.log" 2>&1 || failures=$((failures + 1))
		else
			oras attach --plain-http --no-tty \
				--artifact-type "application/vnd.example.scanreport.v1+json" \
				"$host/$repo@$subject" \
				"$ATTACH/report-$i.json:application/json" \
				>"$ATTACH/out-$i.log" 2>&1 &
			pids+=($!)
		fi
	done

	for pid in ${pids[@]+"${pids[@]}"}; do
		wait "$pid" || failures=$((failures + 1))
	done

	local found
	found="$(survivors "$host" "$repo" "$subject")"

	local verdict="OK"
	if ((found != count)); then
		verdict="LOST $((count - found))"
	fi

	printf '%-13s %-12s %6d %14d %10s/%-6s %s\n' \
		"$label" "$mode" "$failures" "$found" "$found" "$count" "$verdict"

	printf '%s,%s,%d,%d,%d,%s\n' \
		"$label" "$mode" "$count" "$failures" "$found" "$verdict" >>"$OUT/concurrency.csv"
}

run_registry() {
	local label="$1" host="$2"

	log "$label"
	printf '%-13s %-12s %6s %14s %17s %s\n' \
		registry mode errors reachable survived verdict

	# The serial arm is the control. Without it, a zero in a concurrent arm cannot be
	# distinguished from the attaches never having worked at all.
	run_arm "$label" "$host" 6 serial

	for count in $ARMS; do
		run_arm "$label" "$host" "$count" concurrent
	done
}

start_cairn() {
	rm -rf "$WORK/cairn-conc"
	mkdir -p "$WORK/cairn-conc"

	"$WORK/cairnd" \
		-addr "127.0.0.1:$CAIRN_PORT" \
		-root "$WORK/cairn-conc" \
		-log-level warn \
		>"$OUT/cairn-concurrency.log" 2>&1 &

	CAIRN_PID=$!

	wait_ready "http://127.0.0.1:$CAIRN_PORT" cairn
}

start_zot() {
	cat >"$WORK/zot.json" <<-EOF
		{
		  "distSpecVersion": "1.1.0",
		  "storage": { "rootDirectory": "/var/lib/registry", "dedupe": false, "gc": false },
		  "http": { "address": "0.0.0.0", "port": "5000" },
		  "log": { "level": "warn" }
		}
	EOF

	docker rm -f conc-zot >/dev/null 2>&1 || true
	docker volume rm conc-zot-data >/dev/null 2>&1 || true

	docker run -d --name conc-zot \
		-p "$ZOT_PORT:5000" \
		-v conc-zot-data:/var/lib/registry \
		-v "$WORK/zot.json:/etc/zot/config.json:ro" \
		"$ZOT_IMAGE" serve /etc/zot/config.json >/dev/null

	wait_ready "http://127.0.0.1:$ZOT_PORT" zot
}

start_distribution() {
	docker rm -f conc-dist >/dev/null 2>&1 || true
	docker volume rm conc-dist-data >/dev/null 2>&1 || true

	docker run -d --name conc-dist \
		-p "$DIST_PORT:5000" \
		-v conc-dist-data:/var/lib/registry \
		-e REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY=/var/lib/registry \
		-e REGISTRY_LOG_LEVEL=warn \
		"$DIST_IMAGE" >/dev/null

	wait_ready "http://127.0.0.1:$DIST_PORT" distribution
}

log "building"
go build -o "$WORK/cairnd" ./cmd/cairnd

printf 'registry,mode,attempted,errors,reachable,verdict\n' >"$OUT/concurrency.csv"

targets=("${@:-cairn zot distribution}")

# shellcheck disable=SC2068
for target in ${targets[@]}; do
	case "$target" in
		cairn)
			start_cairn
			run_registry cairn "127.0.0.1:$CAIRN_PORT"
			kill "$CAIRN_PID" 2>/dev/null || true
			wait "$CAIRN_PID" 2>/dev/null || true
			CAIRN_PID=""
			;;
		zot)
			start_zot
			run_registry zot "127.0.0.1:$ZOT_PORT"
			docker rm -f conc-zot >/dev/null 2>&1 || true
			;;
		distribution)
			start_distribution
			run_registry distribution "127.0.0.1:$DIST_PORT"
			docker rm -f conc-dist >/dev/null 2>&1 || true
			;;
		*)
			echo "unknown target: $target" >&2
			exit 1
			;;
	esac
done

log "results in $OUT/concurrency.csv"
