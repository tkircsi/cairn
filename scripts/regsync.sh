#!/usr/bin/env bash
#
# Mirrors real Directory data from a Zot registry into cairn with regsync, then measures
# both registries on the same content.
#
# Two things are under test, and only one of them is speed.
#
# The first is whether cairn is a usable mirror target at all, for a client that is not
# oras. regsync is regclient, an independent implementation of the same spec, and it is
# the tool Directory's own migration job runs -- so this exercises cairn's API the way a
# migration actually would rather than the way its test suite does. Anything regclient
# expects that cairn gets wrong shows up here as a failed sync.
#
# The second is the measurement the earlier registry evaluation left unfinished. A
# referrer lookup against a production Zot clone with ~25k manifests took 7.9 seconds,
# because Zot answers it by reading the repository index and then parsing every manifest
# to check its subject. Distribution answered the same question in 2 ms, but only because
# clients had already built the index for it. Both numbers are in that evaluation; cairn's
# is not, so this fills it in against the same data rather than against a synthetic
# workload.
#
# Directory's shape matters for reading the results. Every record lives in one repository
# as a tag named by its CID, and every referrer is *also* tagged by its own CID, so the
# tag namespace is a flat mix of the two, distinguishable only by whether a manifest has a
# subject. One repository with tens of thousands of tags is the case Zot is least suited
# to, and it is not a synthetic case -- it is production.
#
# Usage:
#   ./scripts/regsync.sh              sample of 300 tags
#   ./scripts/regsync.sh 2000         larger sample
#   ./scripts/regsync.sh all          everything the source has
#
# Environment:
#   SOURCE     source registry host   (default prod.zot.ads.outshift.io)
#   REPO       repository to mirror   (default dir)
#   PARALLEL   regsync concurrency    (default 4, matching Directory's migration job)
#   OUT        results directory      (default bench/results)

set -euo pipefail

cd "$(dirname "$0")/.."

readonly ROOT="$PWD"
readonly SOURCE="${SOURCE:-prod.zot.ads.outshift.io}"
readonly REPO="${REPO:-dir}"
readonly PARALLEL="${PARALLEL:-4}"
readonly OUT="${OUT:-$ROOT/bench/results}"
readonly WORK="$ROOT/.bench/regsync"
readonly PORT="${PORT:-5080}"
readonly LOCAL="127.0.0.1:$PORT"

readonly SAMPLE="${1:-300}"

mkdir -p "$OUT" "$WORK"

export PATH="$PATH:$(go env GOPATH)/bin"

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

CAIRN_PID=""

cleanup() {
	[[ -n "$CAIRN_PID" ]] && kill "$CAIRN_PID" 2>/dev/null || true
}

trap cleanup EXIT

for tool in regsync python3; do
	if ! command -v "$tool" >/dev/null; then
		echo "$tool is required" >&2
		echo "regsync: go install github.com/regclient/regclient/cmd/regsync@latest" >&2
		exit 1
	fi
done

log "source inventory"

# Cached: the tag list is the one request whose size scales with the whole repository, and
# it does not change during a run.
readonly TAGS_JSON="$WORK/source-tags.json"
readonly RECORDS_TXT="$WORK/source-records.txt"

if [[ ! -s "$TAGS_JSON" ]]; then
	curl -fsS --max-time 120 "https://$SOURCE/v2/$REPO/tags/list" -o "$TAGS_JSON"
fi

# The record list comes from Directory rather than from the registry, because the registry
# cannot tell the two apart. Records and referrers are both tagged by their own CID, in one
# flat namespace, so the only way to classify a tag from the registry side is to fetch its
# manifest and look for a subject -- one request per tag. dirctl already knows.
if [[ ! -s "$RECORDS_TXT" ]]; then
	if ! command -v dirctl >/dev/null; then
		echo "dirctl is required to enumerate records, or seed $RECORDS_TXT yourself" >&2
		exit 1
	fi

	dirctl search --limit 100000 --context "${DIRCTL_CONTEXT:-prod}" \
		>"$WORK/dirctl-search.out" 2>&1

	grep -o 'baeare[a-z0-9]\{53\}' "$WORK/dirctl-search.out" | sort -u >"$RECORDS_TXT"
fi

python3 - "$TAGS_JSON" "$RECORDS_TXT" "$SAMPLE" "$WORK/selected.txt" <<'PY'
import json, random, sys

tags_path, records_path, sample, out = sys.argv[1:5]

tags = set(json.load(open(tags_path)).get("tags") or [])
records = {line.strip() for line in open(records_path) if line.strip()}

# Only tagged records can be mirrored by tag. The remainder is a gap between Directory's
# search index and the registry's contents, which this script reports and does not try to
# explain.
mirrorable = sorted(records & tags)
referrer_tags = len(tags) - len(mirrorable)

print(f"  tags in {records_path.split('/')[-1]}'s repository : {len(tags)}")
print(f"  records known to Directory              : {len(records)}")
print(f"  records that are tagged (mirrorable)    : {len(mirrorable)}")
print(f"  records known but not tagged            : {len(records - tags)}")
print(f"  tags that are self-tagged referrers     : {referrer_tags}")

if mirrorable:
    print(f"  referrers per record                    : {referrer_tags / len(mirrorable):.2f}")

if sample == "all":
    selected = mirrorable
else:
    # Seeded, so a rerun mirrors the same records and is a no-op rather than a different
    # sample. Random rather than the first N because the list is sorted by CID and CIDs are
    # content hashes: the first N is arbitrary, and sampling across the range avoids
    # selecting a region of the alphabet that happens to have been written at one time.
    random.seed(1)
    selected = sorted(random.sample(mirrorable, min(int(sample), len(mirrorable))))

print(f"\n  selected {len(selected)} records to mirror")

with open(out, "w") as f:
    for tag in selected:
        f.write(tag + "\n")
PY

log "building"
go build -o "$WORK/cairnd" ./cmd/cairnd

# A fresh root, so a sync duration is a sync and not a resume. MEASURE_ONLY keeps the
# previous run's data, for re-timing without paying for the transfer again.
if [[ -z "${MEASURE_ONLY:-}" ]]; then
	rm -rf "$WORK/root"
fi

mkdir -p "$WORK/root"

"$WORK/cairnd" -addr "$LOCAL" -root "$WORK/root" -log-level warn \
	>"$OUT/regsync-cairn.log" 2>&1 &

CAIRN_PID=$!

for _ in $(seq 1 100); do
	curl -fsS -o /dev/null "http://$LOCAL/v2/" 2>/dev/null && break
	sleep 0.3
done

log "generating regsync config"

# Mirrors the config Directory's migration job generates: parallel 4, digestTags off,
# referrers on. referrers is the setting that matters -- with it off, a record arrives
# without its signature and the mirror is quietly incomplete.
{
	cat <<-EOF
		version: 1
		creds:
		  - registry: $SOURCE
		  - registry: $LOCAL
		    tls: disabled
		defaults:
		  parallel: $PARALLEL
		  digestTags: false
		  referrers: true
		sync:
		  - source: $SOURCE/$REPO
		    target: $LOCAL/$REPO
		    type: repository
		    tags:
		      allow:
	EOF

	while read -r tag; do
		printf '        - %s\n' "$tag"
	done <"$WORK/selected.txt"
} >"$WORK/regsync.yml"

log "syncing $(wc -l <"$WORK/selected.txt" | tr -d ' ') records and their referrers"

started=$(python3 -c 'import time; print(time.time())')

if [[ -z "${MEASURE_ONLY:-}" ]]; then
	# Not fatal: a partial sync is a result too, and the verification below reports what
	# actually landed rather than trusting the exit code.
	regsync once -c "$WORK/regsync.yml" -v info >"$OUT/regsync.log" 2>&1 || {
		echo "regsync exited non-zero; see $OUT/regsync.log" >&2
		tail -20 "$OUT/regsync.log" >&2
	}
else
	echo "  MEASURE_ONLY: reusing the previous run's data"
fi

finished=$(python3 -c 'import time; print(time.time())')

log "verifying and measuring"

# Exported rather than passed as an env prefix: these are readonly, and assigning them
# again in a command prefix is an error even when the value is identical.
export SOURCE REPO LOCAL OUT
export STARTED="$started" FINISHED="$finished"

python3 "$ROOT/scripts/regsync_report.py" "$WORK/selected.txt"

log "results in $OUT/regsync-report.csv"
