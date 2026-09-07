#!/usr/bin/env python3
"""Verifies a mirror and times both registries on the same content.

Two questions, in order, because the second is meaningless without the first.

Did everything arrive? A mirror that dropped a signature is not slow, it is wrong, and a
sync tool's exit code does not settle it: regsync reports what it attempted, not what the
target kept. So each record is re-read from both sides and compared -- digest, and the set
of referrer digests attached to it.

How long does each side take to answer? The measurement the earlier registry evaluation
left unfinished. A referrer lookup on a production Zot clone took 7.9 seconds because Zot
reads the repository index and parses every manifest to check its subject. Comparing
cairn against the live source on the same records puts cairn's number on the same axis --
with the caveat, stated in the output, that one side is a loopback and the other is over
the internet, so the fixed cost differs and only the shape of the curve transfers.
"""

import concurrent.futures
import csv
import hashlib
import http.client
import json
import os
import statistics
import sys
import threading
import time
import urllib.parse

SOURCE = os.environ["SOURCE"]
REPO = os.environ["REPO"]
LOCAL = os.environ["LOCAL"]
OUT = os.environ["OUT"]
STARTED = float(os.environ["STARTED"])
FINISHED = float(os.environ["FINISHED"])

MANIFEST_ACCEPT = ",".join(
    [
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.oci.image.index.v1+json",
    ]
)

# Enough reads to have a median that is not one outlier, few enough not to make a load
# test out of a measurement against production.
REPEATS = 5

# How many records to time. Verification covers every record; timing does not need to,
# and each timed record is REPEATS requests to production.
TIMED = 25

# Verification runs in parallel because it is thousands of round trips across the internet
# and none of them is a measurement -- it only has to establish that the content matches.
# The timings below stay strictly sequential: concurrent requests would measure queueing.
VERIFY_WORKERS = 8


class Pool:
    """One Client per thread.

    http.client connections are not safe to share, and the point of the Client is that it
    keeps one connection alive, so each worker needs its own rather than a lock around a
    single connection -- which would serialise exactly what this is here to parallelise.
    """

    def __init__(self, base: str) -> None:
        self.base = base
        self.local = threading.local()

    def get(self) -> "Client":
        client = getattr(self.local, "client", None)

        if client is None:
            client = Client(self.base)
            self.local.client = client

        return client


class Client:
    """A registry client that reuses one connection.

    Connection reuse is the difference between measuring a registry and measuring the
    network. urllib opens a fresh connection per request, so every timing against a remote
    host over TLS carried a handshake -- and against this source that is around 700 ms,
    which swamped the thing being measured and made the source look uniformly slow
    regardless of what was asked of it.
    """

    def __init__(self, base: str) -> None:
        parts = urllib.parse.urlsplit(base)

        self.host = parts.netloc
        self.secure = parts.scheme == "https"
        self.conn = self._connect()

    def _connect(self) -> http.client.HTTPConnection:
        if self.secure:
            return http.client.HTTPSConnection(self.host, timeout=120)

        return http.client.HTTPConnection(self.host, timeout=120)

    def get(self, path: str, headers: dict | None = None) -> tuple[int, bytes, dict]:
        """Fetches a path, returning status, body and headers rather than raising on 404."""
        for attempt in (1, 2):
            try:
                self.conn.request("GET", path, headers=headers or {})
                response = self.conn.getresponse()
                body = response.read()

                return response.status, body, dict(response.getheaders())
            except (http.client.HTTPException, OSError):
                # A reused connection can be closed by the far side between requests. One
                # reconnect, then give up rather than retrying into a real outage.
                self.conn.close()
                self.conn = self._connect()

                if attempt == 2:
                    raise

        raise AssertionError("unreachable")

    def repo(self, path: str, headers: dict | None = None) -> tuple[int, bytes, dict]:
        return self.get(f"/v2/{REPO}/{path}", headers)

    def time_repo(self, path: str, headers: dict | None = None) -> float:
        """Seconds for one repository request, body fully read."""
        start = time.perf_counter()
        self.repo(path, headers)

        return time.perf_counter() - start

    def baseline_ms(self) -> float:
        """Median latency of the cheapest endpoint, as a floor for everything else.

        GET /v2/ does no lookup and returns almost nothing, so what it measures is the
        round trip plus request handling. Subtracting it from another endpoint's latency
        leaves roughly the cost of the work that endpoint actually did, which is the only
        number comparable between a loopback registry and one across the internet.
        """
        samples = []
        for _ in range(REPEATS):
            start = time.perf_counter()
            self.get("/v2/")
            samples.append(time.perf_counter() - start)

        return statistics.median(samples) * 1000


def resolve(client: Client, reference: str) -> str | None:
    """Returns a reference's digest, or None if the registry does not have it."""
    status, body, headers = client.repo(
        f"manifests/{reference}", {"Accept": MANIFEST_ACCEPT}
    )

    if status != 200:
        return None

    # Prefer the registry's own digest header, falling back to computing it, so a registry
    # that omits the header is still comparable.
    digest = headers.get("Docker-Content-Digest")
    if not digest:
        digest = "sha256:" + hashlib.sha256(body).hexdigest()

    return digest


def referrers(client: Client, digest: str) -> set[str]:
    """Returns the set of referrer digests attached to a subject."""
    status, body, _ = client.repo(f"referrers/{digest}")

    if status != 200:
        return set()

    return {m["digest"] for m in json.loads(body).get("manifests", [])}


def median_referrers(client: Client, digest: str) -> float:
    """Median referrer lookup latency in milliseconds."""
    samples = [client.time_repo(f"referrers/{digest}") for _ in range(REPEATS)]

    return statistics.median(samples) * 1000


def median_tags(client: Client) -> tuple[float, int]:
    """Median tag list latency in milliseconds, and the number of tags returned."""
    samples = []
    count = 0

    for _ in range(REPEATS):
        start = time.perf_counter()
        status, body, _ = client.repo("tags/list")
        samples.append(time.perf_counter() - start)

        if status == 200:
            count = len(json.loads(body).get("tags") or [])

    return statistics.median(samples) * 1000, count


def main() -> int:
    records = [line.strip() for line in open(sys.argv[1]) if line.strip()]

    source = Client(f"https://{SOURCE}")
    local = Client(f"http://{LOCAL}")

    print(f"  sync took {FINISHED - STARTED:.1f}s for {len(records)} records")

    source_pool = Pool(f"https://{SOURCE}")
    local_pool = Pool(f"http://{LOCAL}")

    def verify(tag: str) -> tuple[str, str, int, list[str], list[str]]:
        """Compares one record across both registries. Returns a verdict and the detail."""
        src = source_pool.get()
        dst = local_pool.get()

        want_digest = resolve(src, tag)
        got_digest = resolve(dst, tag)

        if got_digest is None:
            return tag, "missing", 0, [], []

        if want_digest != got_digest:
            return tag, "mismatch", 0, [], []

        want_refs = referrers(src, want_digest)
        got_refs = referrers(dst, got_digest)

        verdict = "ok" if want_refs == got_refs else "gap"

        return tag, verdict, len(want_refs), sorted(want_refs - got_refs), sorted(got_refs - want_refs)

    missing, mismatched, referrer_gaps = [], [], []
    total_referrers = 0

    print(f"  verifying {len(records)} records with {VERIFY_WORKERS} workers")

    with concurrent.futures.ThreadPoolExecutor(max_workers=VERIFY_WORKERS) as pool:
        for tag, verdict, count, absent, extra in pool.map(verify, records):
            total_referrers += count

            if verdict == "missing":
                missing.append(tag)
            elif verdict == "mismatch":
                mismatched.append(tag)
            elif verdict == "gap":
                referrer_gaps.append((tag, absent, extra))

    print(f"\n  records mirrored     : {len(records) - len(missing) - len(mismatched)}/{len(records)}")
    print(f"  records missing      : {len(missing)}")
    print(f"  digest mismatches    : {len(mismatched)}")
    print(f"  referrers expected   : {total_referrers}")
    print(f"  records with referrer gaps: {len(referrer_gaps)}")

    for tag, absent, extra in referrer_gaps[:5]:
        print(f"    {tag}: missing {len(absent)}, unexpected {len(extra)}")
        for digest in absent[:3]:
            print(f"      absent: {digest}")

    for tag in missing[:5]:
        print(f"    absent record: {tag}")

    # Time the records that actually have referrers: a lookup returning nothing measures
    # the empty case, and the whole question is what happens as referrers accumulate.
    timed = []
    for tag in records:
        digest = resolve(local, tag)
        if digest is None:
            continue

        if referrers(local, digest):
            timed.append((tag, digest, len(referrers(local, digest))))

        if len(timed) >= TIMED:
            break

    # Measured before the timings and reported alongside them, because without it the two
    # sides are not comparable at all: one is a loopback and the other is across the
    # internet.
    cairn_floor = local.baseline_ms()
    source_floor = source.baseline_ms()

    print(f"\n  baseline GET /v2/, cairn : {cairn_floor:8.2f} ms")
    print(f"  baseline GET /v2/, source: {source_floor:8.2f} ms")
    print(f"\n  timing {len(timed)} records with referrers, median of {REPEATS} reads")

    rows = []
    for tag, digest, count in timed:
        cairn_ms = median_referrers(local, digest)
        source_ms = median_referrers(source, digest)

        rows.append(
            {
                "record": tag,
                "referrers": count,
                "cairn_ms": round(cairn_ms, 3),
                "source_ms": round(source_ms, 3),
            }
        )

    cairn_median = source_median = 0.0

    if rows:
        cairn_median = statistics.median(r["cairn_ms"] for r in rows)
        source_median = statistics.median(r["source_ms"] for r in rows)

        print(f"    referrer lookup, cairn : {cairn_median:8.2f} ms  "
              f"({max(cairn_median - cairn_floor, 0):7.2f} ms over baseline)")
        print(f"    referrer lookup, source: {source_median:8.2f} ms  "
              f"({max(source_median - source_floor, 0):7.2f} ms over baseline)")

    cairn_tags_ms, cairn_tags = median_tags(local)
    source_tags_ms, source_tags = median_tags(source)

    print(f"\n    tag list, cairn  : {cairn_tags_ms:8.2f} ms for {cairn_tags:6} tags  "
          f"({max(cairn_tags_ms - cairn_floor, 0):7.2f} ms over baseline)")
    print(f"    tag list, source : {source_tags_ms:8.2f} ms for {source_tags:6} tags  "
          f"({max(source_tags_ms - source_floor, 0):7.2f} ms over baseline)")

    print(
        "\n  Read the over-baseline column, not the raw one. cairn is loopback and the\n"
        "  source is across the internet, so the raw numbers differ by a round trip\n"
        "  before either registry does any work.\n"
        f"\n  Also note the two are not holding the same content: cairn has {cairn_tags}\n"
        f"  tags here and the source has {source_tags}. That is the comparison's main\n"
        "  limitation -- run with a larger sample, or 'all', to close it."
    )

    path = os.path.join(OUT, "regsync-report.csv")
    with open(path, "w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=["record", "referrers", "cairn_ms", "source_ms"])
        writer.writeheader()
        writer.writerows(rows)

    summary = {
        "source": SOURCE,
        "repository": REPO,
        "records_requested": len(records),
        "records_missing": len(missing),
        "digest_mismatches": len(mismatched),
        "referrers_expected": total_referrers,
        "records_with_referrer_gaps": len(referrer_gaps),
        "sync_seconds": round(FINISHED - STARTED, 1),
        "cairn_tags": cairn_tags,
        "cairn_tag_list_ms": round(cairn_tags_ms, 3),
        "source_tags": source_tags,
        "source_tag_list_ms": round(source_tags_ms, 3),
        "cairn_baseline_ms": round(cairn_floor, 3),
        "source_baseline_ms": round(source_floor, 3),
        "cairn_referrers_ms": round(cairn_median, 3),
        "source_referrers_ms": round(source_median, 3),
    }

    with open(os.path.join(OUT, "regsync-summary.json"), "w") as f:
        json.dump(summary, f, indent=2)
        f.write("\n")

    # A mirror that lost content is a failure, and the exit code should say so.
    return 1 if missing or mismatched or referrer_gaps else 0


if __name__ == "__main__":
    sys.exit(main())
