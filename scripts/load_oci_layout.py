#!/usr/bin/env python3
"""Loads an on-disk OCI image layout into a registry over HTTP.

Why this exists rather than a sync tool: mirroring this data with regsync means asking the
source "what refers to this?" once per record, and against a repository of this size that
call costs hundreds of milliseconds to several seconds -- the very cost under study. A
36-hour mirror ends up measuring the source's referrer lookup rather than the target.

Reading the layout directly avoids the question entirely. A referrer is just a manifest
with a subject field, so the graph is already in the bytes on disk. cairn builds its
referrer index as it ingests manifests, which means the index arrives as a side effect of
an ordinary push and no referrer lookup happens on either side.

Order is the one real constraint. cairn requires a manifest's referenced blobs to exist in
the repository before it will accept the manifest, so config and layers go first. Subjects
are exempt: a subject need not exist, which is what lets this dataset's orphaned referrers
load at all -- their subjects were deleted in production and never came back.

Usage:
  ./scripts/load_oci_layout.py <layout-dir> <host> <repository> [--workers N] [--limit N]
"""

import argparse
import concurrent.futures
import http.client
import json
import os
import sys
import threading
import time


class Client:
    """One connection, reused. Not thread-safe: use one per worker."""

    def __init__(self, host: str) -> None:
        self.host = host
        self.conn = http.client.HTTPConnection(host, timeout=120)

    def request(
        self,
        method: str,
        path: str,
        body: bytes | None = None,
        headers: dict | None = None,
    ) -> tuple[int, bytes, dict]:
        for attempt in (1, 2):
            try:
                self.conn.request(method, path, body=body, headers=headers or {})
                response = self.conn.getresponse()
                data = response.read()

                return response.status, data, dict(response.getheaders())
            except (http.client.HTTPException, OSError):
                # A kept-alive connection can be closed between requests. Reconnect once,
                # then let a real outage surface rather than retrying into it.
                self.conn.close()
                self.conn = http.client.HTTPConnection(self.host, timeout=120)

                if attempt == 2:
                    raise

        raise AssertionError("unreachable")


class Loader:
    def __init__(self, layout: str, host: str, repo: str) -> None:
        self.layout = layout
        self.host = host
        self.repo = repo

        self.local = threading.local()

        # Blobs already known to be in the target. Shared, because this dataset has one
        # empty config blob referenced by all 28,788 manifests and checking it once per
        # manifest would be most of the run.
        self.present: set[str] = set()
        self.present_lock = threading.Lock()

        self.counters = {
            "manifests": 0,
            "blobs_uploaded": 0,
            "blobs_skipped": 0,
            "failures": 0,
        }
        self.counters_lock = threading.Lock()
        self.failures: list[str] = []

    def client(self) -> Client:
        client = getattr(self.local, "client", None)

        if client is None:
            client = Client(self.host)
            self.local.client = client

        return client

    def blob_path(self, digest: str) -> str:
        algorithm, hex_digest = digest.split(":", 1)

        return os.path.join(self.layout, "blobs", algorithm, hex_digest)

    def read_blob(self, digest: str) -> bytes:
        with open(self.blob_path(digest), "rb") as f:
            return f.read()

    def count(self, key: str, delta: int = 1) -> None:
        with self.counters_lock:
            self.counters[key] += delta

    def ensure_blob(self, digest: str) -> None:
        """Uploads a blob unless the target already has it."""
        with self.present_lock:
            if digest in self.present:
                self.count("blobs_skipped")

                return

        client = self.client()

        status, _, _ = client.request("HEAD", f"/v2/{self.repo}/blobs/{digest}")
        if status == 200:
            with self.present_lock:
                self.present.add(digest)

            self.count("blobs_skipped")

            return

        body = self.read_blob(digest)

        # Monolithic upload: one request instead of a session's three. cairn supports it,
        # and at this manifest count the saved round trips dominate.
        status, response, _ = client.request(
            "POST",
            f"/v2/{self.repo}/blobs/uploads/?digest={digest}",
            body=body,
            headers={
                "Content-Type": "application/octet-stream",
                "Content-Length": str(len(body)),
            },
        )

        if status != 201:
            raise RuntimeError(f"blob {digest}: {status} {response[:200]!r}")

        with self.present_lock:
            self.present.add(digest)

        self.count("blobs_uploaded")

    def load_manifest(self, entry: dict) -> None:
        digest = entry["digest"]
        tag = (entry.get("annotations") or {}).get("org.opencontainers.image.ref.name")

        try:
            raw = self.read_blob(digest)
            manifest = json.loads(raw)

            # Config first, then layers: both must exist before the manifest is accepted.
            # The subject is deliberately not pushed -- it may not exist, and requiring it
            # would drop every orphaned referrer.
            config = manifest.get("config")
            if config and config.get("digest"):
                self.ensure_blob(config["digest"])

            for layer in manifest.get("layers") or []:
                self.ensure_blob(layer["digest"])

            media_type = manifest.get("mediaType") or entry["mediaType"]

            # By tag when there is one, so the target ends up with the same tag namespace
            # as the source; by digest otherwise, which is how an untagged referrer lands.
            reference = tag or digest

            status, response, _ = self.client().request(
                "PUT",
                f"/v2/{self.repo}/manifests/{reference}",
                body=raw,
                headers={"Content-Type": media_type, "Content-Length": str(len(raw))},
            )

            if status != 201:
                raise RuntimeError(f"manifest {reference}: {status} {response[:200]!r}")

            self.count("manifests")
        except Exception as err:  # noqa: BLE001 - one bad manifest should not end the load
            self.count("failures")

            with self.counters_lock:
                if len(self.failures) < 20:
                    self.failures.append(f"{digest}: {err}")

    def run(self, entries: list[dict], workers: int) -> None:
        started = time.time()
        total = len(entries)

        print(f"loading {total} manifests into {self.host}/{self.repo} with {workers} workers")

        with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
            futures = [pool.submit(self.load_manifest, e) for e in entries]

            for done, _ in enumerate(concurrent.futures.as_completed(futures), start=1):
                if done % 2000 == 0 or done == total:
                    elapsed = time.time() - started
                    rate = done / elapsed if elapsed else 0
                    print(
                        f"  {done}/{total}  {rate:.0f}/s  "
                        f"blobs {self.counters['blobs_uploaded']} uploaded, "
                        f"{self.counters['failures']} failures",
                        flush=True,
                    )

        elapsed = time.time() - started

        print(f"\ndone in {elapsed:.1f}s")
        for key, value in self.counters.items():
            print(f"  {key}: {value}")

        for failure in self.failures:
            print(f"  failure: {failure}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("layout")
    parser.add_argument("host")
    parser.add_argument("repo")
    parser.add_argument("--workers", type=int, default=8)
    parser.add_argument("--limit", type=int, default=0)
    args = parser.parse_args()

    with open(os.path.join(args.layout, "index.json")) as f:
        entries = json.load(f)["manifests"]

    if args.limit:
        entries = entries[: args.limit]

    loader = Loader(args.layout, args.host, args.repo)
    loader.run(entries, args.workers)

    return 1 if loader.counters["failures"] else 0


if __name__ == "__main__":
    sys.exit(main())
