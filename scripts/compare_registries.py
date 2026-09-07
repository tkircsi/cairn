#!/usr/bin/env python3
"""Times two registries holding identical content, and checks their answers agree.

This is the comparison the earlier registry evaluation could not make directly. That work
measured a referrer lookup at ~7.9 s on a production Zot clone and ~2 ms on Distribution,
but the second number came from a tag-addressed index clients had already built, so the two
were not answering the same question the same way. And cairn had no number at all.

Here both registries hold the same 28,788 manifests from the same on-disk clone, both are
on loopback, and both are asked through the referrers API. No baseline correction, no
content-volume gap, no fallback index -- the remaining difference is what each registry
does to answer.

Ground truth comes from the layout on disk rather than from either registry, because a fast
wrong answer is not an improvement: the expected referrer count for a subject is computed
by reading the manifests, and both registries are checked against it.

Usage:
  ./scripts/compare_registries.py --layout DIR --repo dir \\
      --registry cairn=127.0.0.1:5090 --registry zot=127.0.0.1:5091
"""

import argparse
import collections
import hashlib
import http.client
import json
import os
import statistics
import sys
import time

MANIFEST_ACCEPT = ",".join(
    [
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.oci.image.index.v1+json",
    ]
)


class Client:
    def __init__(self, host: str) -> None:
        self.host = host
        self.conn = http.client.HTTPConnection(host, timeout=300)

    def get(self, path: str, headers: dict | None = None) -> tuple[int, bytes]:
        for attempt in (1, 2):
            try:
                self.conn.request("GET", path, headers=headers or {})
                response = self.conn.getresponse()

                return response.status, response.read()
            except (http.client.HTTPException, OSError):
                self.conn.close()
                self.conn = http.client.HTTPConnection(self.host, timeout=300)

                if attempt == 2:
                    raise

        raise AssertionError("unreachable")

    def time(self, path: str, headers: dict | None = None) -> tuple[float, int, bytes]:
        start = time.perf_counter()
        status, body = self.get(path, headers)

        return time.perf_counter() - start, status, body


def ground_truth(layout: str) -> tuple[dict[str, set[str]], list[tuple[str, str]]]:
    """Reads the layout and returns subject -> referrer digests, and (tag, digest) records.

    A record is a manifest with no subject; a referrer is one with a subject. Both are
    tagged by their own CID in this dataset, so the registry cannot classify them and the
    manifests have to be read.
    """
    with open(os.path.join(layout, "index.json")) as f:
        entries = json.load(f)["manifests"]

    referrers: dict[str, set[str]] = collections.defaultdict(set)
    records: list[tuple[str, str]] = []

    for entry in entries:
        digest = entry["digest"]
        algorithm, hex_digest = digest.split(":", 1)

        with open(os.path.join(layout, "blobs", algorithm, hex_digest), "rb") as f:
            manifest = json.loads(f.read())

        subject = manifest.get("subject")

        if subject:
            referrers[subject["digest"]].add(digest)
        else:
            tag = (entry.get("annotations") or {}).get("org.opencontainers.image.ref.name")
            if tag:
                records.append((tag, digest))

    return referrers, records


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--layout", required=True)
    parser.add_argument("--repo", default="dir")
    parser.add_argument("--registry", action="append", required=True,
                        help="name=host, repeatable")
    parser.add_argument("--samples", type=int, default=15)
    parser.add_argument("--repeats", type=int, default=3)
    parser.add_argument("--out")
    args = parser.parse_args()

    registries = {}
    for spec in args.registry:
        name, host = spec.split("=", 1)
        registries[name] = Client(host)

    print("reading ground truth from the layout on disk")
    referrers, records = ground_truth(args.layout)

    total_referrers = sum(len(v) for v in referrers.values())
    live = [(tag, digest) for tag, digest in records if digest in referrers]
    orphaned = total_referrers - sum(len(referrers[d]) for _, d in live)

    print(f"  records                     : {len(records)}")
    print(f"  referrer manifests          : {total_referrers}")
    print(f"  distinct subjects referenced: {len(referrers)}")
    print(f"  records that have referrers : {len(live)}")
    print(f"  referrers whose subject is not a record (orphaned): {orphaned}")

    # Evenly spaced across the list rather than the first N, so the sample is not one
    # region of the CID alphabet -- which, since CIDs are content hashes, tends to mean
    # content written at one time.
    step = max(len(live) // args.samples, 1)
    sample = live[::step][: args.samples]

    print(f"\ntiming {len(sample)} records, median of {args.repeats} reads, all on loopback")

    rows = []
    disagreements = []

    for tag, digest in sample:
        expected = referrers[digest]
        row = {"record": tag, "expected_referrers": len(expected)}

        for name, client in registries.items():
            samples = []
            got: set[str] = set()

            for _ in range(args.repeats):
                elapsed, status, body = client.time(f"/v2/{args.repo}/referrers/{digest}")
                samples.append(elapsed)

                if status == 200:
                    got = {m["digest"] for m in json.loads(body).get("manifests", [])}

            row[f"{name}_ms"] = round(statistics.median(samples) * 1000, 3)

            if got != expected:
                disagreements.append(
                    f"{name} on {tag}: returned {len(got)}, layout says {len(expected)}"
                )

        rows.append(row)

    names = list(registries)

    print("\n  referrer lookup, median ms")
    for name in names:
        values = [r[f"{name}_ms"] for r in rows]
        print(f"    {name:6}: median {statistics.median(values):9.2f}   "
              f"min {min(values):9.2f}   max {max(values):9.2f}")

    if len(names) == 2:
        a, b = names
        ratio = statistics.median([r[f"{b}_ms"] for r in rows]) / max(
            statistics.median([r[f"{a}_ms"] for r in rows]), 1e-9
        )
        print(f"\n    {b} takes {ratio:,.0f}x as long as {a}")

    print("\n  tag list, median ms")
    for name, client in registries.items():
        samples = []
        count = 0

        for _ in range(args.repeats):
            elapsed, status, body = client.time(f"/v2/{args.repo}/tags/list")
            samples.append(elapsed)

            if status == 200:
                count = len(json.loads(body).get("tags") or [])

        print(f"    {name:6}: {statistics.median(samples) * 1000:9.2f} for {count} tags")

    print("\n  manifest by tag, median ms")
    for name, client in registries.items():
        samples = []

        for tag, _ in sample:
            elapsed, _, _ = client.time(
                f"/v2/{args.repo}/manifests/{tag}", {"Accept": MANIFEST_ACCEPT}
            )
            samples.append(elapsed)

        print(f"    {name:6}: {statistics.median(samples) * 1000:9.2f}")

    if disagreements:
        print(f"\n  DISAGREEMENTS WITH THE LAYOUT ({len(disagreements)}):")
        for line in disagreements[:10]:
            print(f"    {line}")
    else:
        print("\n  every registry's referrer set matched the layout exactly")

    if args.out:
        import csv

        with open(args.out, "w", newline="") as f:
            writer = csv.DictWriter(f, fieldnames=list(rows[0]))
            writer.writeheader()
            writer.writerows(rows)

        print(f"\n  wrote {args.out}")

    return 1 if disagreements else 0


if __name__ == "__main__":
    sys.exit(main())
