# Benchmarks

Two questions, one per script. `scripts/bench-referrers.sh` asks what referrers cost as
they accumulate; `scripts/concurrency.sh` asks whether they survive being written at the
same time. The second is the shorter write-up and the more important result, so it comes
first.

# Concurrent attach: does a referrer survive being written?

```sh
./scripts/concurrency.sh                    # all three
ARMS="8 32 64" ./scripts/concurrency.sh cairn
```

This reproduces a test from the registry evaluation that preceded cairn, where
Distribution lost referrers silently. `oras attach` is the client, deliberately: it is
the same `oras-go` path Directory's `PushReferrer` uses, and it chooses the native or
fallback route by itself. The original arms — 6 serial, then 2/4/6/8 concurrent — are
kept verbatim, with higher ones added.

| concurrency | cairn | Zot v2.1.20 | Distribution v3.1.1 |
|---|---|---|---|
| 6 serial | 6/6 | 6/6 | 6/6 |
| 2 | 2/2 | 2/2 | **1/2** |
| 4 | 4/4 | 4/4 | **1/4** |
| 6 | 6/6 | 6/6 | **2/6** |
| 8 | 8/8 | 8/8 | **2/8** |
| 16 | 16/16 | 16/16 | **4/16** |
| 32 | 32/32 | 32/32 | **5/32** |
| 64 | 64/64 | 64/64 | **12/64** |
| 128 | 128/128 | — | — |
| 256 | 256/256 | — | — |

**Every attach exited 0.** In every Distribution row, `oras` reported success for
referrers that are not reachable afterwards. The manifest is in the registry; nothing
points at it, so `oras discover` will never return it and no caller sees an error. At 64
concurrent, 52 of them are gone and nothing anywhere says so.

The cause is the fallback index: with no referrers API, each client fetches the
`sha256-<subject>` index, appends its descriptor, and pushes it back. Concurrent clients
read the same version and each writes back one missing the others' entries. Last writer
wins the whole document, and no client can make that atomic from outside the registry.

The serial row is the control. Without it, a 1/8 could just mean the attaches never
worked.

Two honest notes. The original run reported every concurrent batch collapsing to exactly
one survivor; here the higher arms leave a handful more (2, 4, 5, 12), presumably because
more clients means more chances to interleave between a read and a write. The shape is
the same and the direction is the same, but this is not a digit-for-digit reproduction —
different Distribution version, different machine. And cairn's 128 and 256 rows have no
counterpart because they were run separately, to find cairn's limit rather than to
compare.

## What it found in cairn

cairn passes every arm now. It did not when the test was first run, and the failure was
not in the referrers path at all.

`oras push` uploads a manifest's blobs concurrently, so an ordinary push closes two
upload sessions at once. One of them returned `500 unsupported: internal error` on a
completely valid push. The cause was that `Open` applied its pragmas with `db.Exec` on a
`*sql.DB`, which is a *pool*: `busy_timeout`, `foreign_keys` and `analysis_limit`
configure a connection rather than the database, so they landed on whichever connection
ran them and no other. Every later connection had no busy timeout — a concurrent write
failed instantly with `SQLITE_BUSY` instead of waiting — and, worse, no foreign key
enforcement, so the schema's cascades were silently unenforced on most connections.

Nothing sequential could see it. One caller at a time reuses the one configured
connection, which is why the test suite, the OCI conformance suite and a 3000-push
benchmark all passed while the pool held unconfigured connections it had never needed.

The pragmas now travel in the DSN, so every connection gets them, and
`internal/metastore/sqlite/pragma_test.go` holds four connections open at once and asserts
each is configured. Finding this is the argument for the test: correctness under
concurrency is not what the referrers benchmark measures, and a sequential suite cannot
see a pool.

It also exposed a second, smaller thing. The 500 was undiagnosable because
`writeServerError` discarded the error — the access log recorded that a request failed
and the reason existed nowhere. The cause now rides on the response recorder into the
existing log record, so a failed request is still one line, and the client's response is
unchanged.

# Real production data

Two questions, both answered with a real Directory repository, and they need different
setups because one of them is about interoperability and the other about speed.

## Is cairn a usable mirror target? (regsync)

```sh
./scripts/regsync.sh              # sample of 300 records
MEASURE_ONLY=1 ./scripts/regsync.sh 300   # re-time without re-transferring
```

regsync is regclient, an independent implementation of the same spec, and it is the tool
Directory's own migration job runs. The config this script generates mirrors that job's:
`parallel`, `digestTags: false`, `referrers: true`. The last one matters — with referrers
off, a record arrives without its signature and the mirror is quietly incomplete.

Verification does not trust regsync's exit code, because that reports what was attempted.
Every record is re-read from both registries and compared on digest and on the set of
referrer digests attached to it.

**Result: 40/40 records mirrored, 182 referrers, zero missing, zero digest mismatches, zero
referrer gaps.** cairn accepted everything regclient sent, including manifests whose config
descriptor carries inlined `data` — regsync pushes that blob explicitly, so cairn's
requirement that referenced blobs exist is satisfied.

What this script cannot do is produce a fair timing comparison, for a reason worth
recording: regsync asks the source "what refers to this record?" once per record, and on
the source that costs ~560 ms, so the mirror is throttled by the very lookup under test.
A full mirror projects to hours and raising `parallel` from 4 to 16 changed throughput not
at all. The measurement below therefore does not use regsync.

## How fast is each registry on identical content? (the clone)

```sh
# load a byte-faithful on-disk clone of production into cairn
./scripts/load_oci_layout.py <clone>/dir 127.0.0.1:5090 dir

# time both registries on it, checking answers against the layout
./scripts/compare_registries.py --layout <clone>/dir --repo dir \
    --registry cairn=127.0.0.1:5090 --registry zot=127.0.0.1:5091
```

This is the measurement the earlier registry evaluation left unfinished. That work timed a
referrer lookup at ~6–8 s on a production Zot clone and 2 ms on Distribution, but the
second number came from a tag-addressed index clients had already built, so the two were
not answering the same question the same way — and cairn had no number at all.

Here both registries hold **the same 28,788 manifests from the same clone**, both are on
loopback, and both are asked through the referrers API. No baseline correction, no
content-volume gap, no client-built fallback index.

Loading reads the layout off disk instead of mirroring, which is what makes it practical:
a referrer is just a manifest with a subject, so the graph is already in the bytes, and
cairn builds its index as a side effect of ordinary pushes. No referrer lookup happens on
either side. **28,788 manifests and 28,796 blobs in 210 s, zero failures**, against the
36 hours regsync projected for the same data.

Ground truth comes from the layout rather than from either registry, because a fast wrong
answer is not an improvement:

```
records                                            2890
referrer manifests                                25898
distinct subjects referenced                       6615
records that have referrers                        2843
referrers whose subject no longer exists (orphaned) 14906
```

Those orphans are the point. They are more than half the referrers, their subjects were
deleted in production, and they are still tagged — so they are still scanned on every
lookup that walks the repository.

### Results

15 records sampled evenly across the repository, median of 3 reads:

| | cairn | Zot | ratio |
|---|---|---|---|
| referrer lookup | **0.43 ms** | 6642 ms | **15,411x** |
| manifest by tag | 0.79 ms | 32.5 ms | 41x |
| tag list (28,780 tags) | 11.9 ms | 42.0 ms | 3.5x |

**Every registry's referrer set matched the layout exactly.** Both are correct; they differ
only in what they do to get there. cairn reads an indexed row. Zot walks the repository
index and parses manifests to check subjects, so its cost scales with everything stored —
including the 14,906 orphans, none of which can match.

Two sanity checks on the harness rather than the registries. Zot's 6.6 s reproduces the
5,983 ms the earlier evaluation measured on this same clone, so the setup is consistent
with prior work. And cairn's 0.43 ms at 28,788 manifests matches its 0.49 ms at 40 — the
flatness under accumulation the synthetic benchmark below predicts, now confirmed on real
data three orders of magnitude larger.

One honest note on the load: throughput drifted from 160/s to 137/s across the run, a 14%
decline as the index grew. Small, but it is not perfectly flat and it is measured here
rather than assumed.

# Referrers under accumulation

cairn keeps an SQL index for one reason: "what refers to this manifest" is not
derivable from the bytes being asked about, and its answer changes every time someone
signs them. Every other table here is a convenience that a slow walk of the store could
reconstruct. This benchmark is the argument for that one table, measured rather than
asserted.

Run it:

```sh
./scripts/bench-referrers.sh                # all three, 3000 referrers
N=500 ./scripts/bench-referrers.sh cairn    # one arm, shorter
```

Results land in `bench/results/<registry>.csv`, one row per sample point.

## The workload

One repository, one subject manifest, and referrers pushed onto it one at a time. Each
referrer is shaped like a scan report: a small JSON blob and a manifest naming the
subject, with an `artifactType` and annotations.

The shape is not invented for the benchmark. It is what a scan or signing pipeline
produces against a record that is re-scanned on a schedule: the report embeds the time
it was produced, so re-scanning yields different bytes, a different digest, and one more
referrer rather than a no-op. Nothing collects the superseded one, and the subject is
alive, so no garbage collector would. Referrers per subject is therefore an axis that
only grows in production — which is what makes it worth knowing the cost of.

Three measurements at each sample point:

| Column | What it is |
|---|---|
| `push_ms` | PUT of the referrer manifest — where a registry maintaining a per-repository index does its work |
| `fallback_ms` | the read-modify-write a registry without a referrers API forces on the client |
| `query_ms` | GET of the referrers list, the read the accumulation degrades |
| `control_ms` | HEAD of the subject, unrelated to referrers — whether the cost stays contained |

Blob uploads are deliberately not timed. They are identical work for every registry
(bytes to disk), so including them would dilute the thing being measured.

## Why the driver probes rather than being told

Distribution v3.1.1 has no `/v2/<name>/referrers/` route at all —
[PR #4828](https://github.com/distribution/distribution/pull/4828) implements end-12 and
is unmerged, targeted at 3.2.0. Timing only endpoints would therefore report it as the
fastest of the three, by leaving out everything the missing endpoint delegates.

It delegates a lot. The spec makes a client whose registry 404s maintain an image index
under a tag derived from the subject, which means fetching that index, appending one
descriptor, and pushing it back on every push. The document grows by a descriptor each
time, so each push moves and re-parses everything pushed before it — linear per push,
quadratic over a run — and it is paid whether or not anyone ever asks for the list.

So the driver probes end-12 once and takes the branch a conformant client would. The
spec defines that probe precisely so it is unambiguous: a registry supporting the API
returns 200 with an index even when there are no referrers, so a 404 means the API is
absent rather than the subject unknown. What the driver measures is then the cost of
recording one referrer and of listing them, against each registry behaving as it really
does, which is the number a client experiences.

It also checks that each registry returns exactly the referrers pushed before recording
any timing. A lossy or silently paginating implementation fails the run rather than
scoring well for being fast.

## Results

3000 referrers on one subject, sampled every 100, median of 5 reads, one sequential
client. Apple silicon, 2026-09-07. Zot v2.1.20, Distribution v3.1.1.

Cost to record referrer number N, in milliseconds:

| N | cairn | Zot | Distribution |
|---|---|---|---|
| 100 | 0.52 | 2.02 | 9.34 |
| 1000 | 0.57 | 4.36 | 25.43 |
| 2000 | 0.56 | 8.22 | 50.23 |
| 3000 | 0.54 | 11.46 | 60.45 |
| **growth** | **1.0×** | **5.7×** | **6.5×** |

Marginal cost, from a least-squares fit — what one more accumulated referrer adds to
every subsequent write:

| | µs added per referrer |
|---|---|
| cairn | 0.01 |
| Zot | 3.14 |
| Distribution (client fallback) | 18.57 |

Cumulative write time over the run: **1.75 s** for cairn, **19.80 s** for Zot,
**106.50 s** for Distribution.

An unrelated `HEAD` of the subject, which should not move at all:

| | at 100 | at 3000 | growth |
|---|---|---|---|
| cairn | 0.162 ms | 0.161 ms | 1.0× |
| Zot | 0.444 ms | 1.545 ms | 3.5× |
| Distribution | 0.315 ms | 0.447 ms | 1.4× |

Listing the referrers:

| | at 100 | at 3000 | µs per referrer, fitted slope |
|---|---|---|---|
| cairn | 0.61 ms | 10.27 ms | 3.28 |
| Zot | 1.20 ms | 26.77 ms | 8.73 |
| Distribution | 1.11 ms | 19.35 ms | 7.03 |

## Reading them

**cairn's write cost is flat because a referrer is a row.** The insert does not touch
the other 2999, and `manifests_by_subject` is a partial index over rows that have a
subject, so it stays proportional to the number of signatures rather than to everything
ever pushed. This is the whole claim, and it is the one line in the table that does not
move.

**Zot's write cost is linear because every manifest push rewrites the repository's
`index.json`**, which by then holds an entry per referrer. Its unrelated `HEAD` getting
3.5× slower is the same cause seen from the other side, and it is the more damaging
result: the cost does not stay inside the feature that caused it. That is the mechanism
behind the 5–6 second reads in the original production investigation, reproduced at 3000
referrers on a laptop against a repository that held ~27,000 tags in production.

Worth being clear that this is Zot at its best on this workload. Dedupe is off and no
extensions are configured, so neither the startup dedupe walk
([zot#4349](https://github.com/project-zot/zot/issues/4349)) nor the metadb walk is in
these numbers. Both are real and additive; leaving them in would have meant attributing
known separate bugs to the referrers axis.

**Distribution's own storage layer is the best of the three at this.** Its `push_ms`
is genuinely flat — around 2.7 ms at every N — because it writes a subject link file and
shares no document between referrers. Every millisecond of its growth is the client
rebuilding an answer the registry declines to keep. So the honest reading is not that
Distribution scales badly here but that end-12 is not optional: without it the work does
not disappear, it moves somewhere with less information and no way to be atomic. The
fallback index is racy for exactly that reason — two clients that read it concurrently
each write back a version missing the other's entry.

**Everyone's read cost grows, and should.** At 3000 referrers the answer is an 831 KB
index; no implementation makes that constant-time. cairn's constant is the lowest of the
three, which is the least interesting result here. The real answer to that axis is
pagination, which cairn does not yet implement on end-12.

## What it does not show

**Absolute numbers are not comparable.** cairn runs natively on the host while Zot and
Distribution run in Docker's Linux VM, which adds a fixed cost to every response of
theirs. The control column at low N measures that floor — 0.16 ms against 0.44 and 0.32.
Subtract it and what remains is the slope of each curve, which is why the slopes are the
claim and the absolute values are not.

**cairn does far less.** No authentication, no garbage collection, no dedupe accounting,
no replication, no search index, no scale-out. Some of what the other two spend time on
is work cairn has not implemented, and a production comparison would have to add it back.

**One client, so no contention.** Sequential by design, which measures work rather than
lock contention. That understates the gap for Zot specifically, whose store-wide mutex
serialises operations — the axis it was already known to degrade worst on.

**3000 referrers is small**, chosen so all three arms finish in about three minutes. The
curves are linear and unbroken across the range, so extrapolating is reasonable, but
nothing here locates the cliff each implementation eventually has, and a linear fit
cannot predict one.
