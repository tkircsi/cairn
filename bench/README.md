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
