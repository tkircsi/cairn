# cairn

A small OCI registry that keeps blobs on the filesystem and everything it needs
to *find* them in SQL.

The scope is deliberately one slice of the [OCI distribution
spec](https://github.com/opencontainers/distribution-spec/blob/main/spec.md):
blobs and manifests, addressed by digest. That slice exercises the whole shape of
a registry — content addressing, upload sessions, repository scoping, and a
manifest graph that has to resolve — without tags, listings or referrers on top.

**References are digests only.** A tag is recognised and refused rather than
reported as a missing manifest, so `oras manifest push` works and `oras push`
does not.

## Running it

```sh
go run ./cmd/cairnd -addr 127.0.0.1:5050 -root data
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-addr` | `127.0.0.1:5050` | address to listen on |
| `-root` | `data` | directory holding `blobs/`, `uploads/` and `cairn.db` |
| `-max-blob-size` | `1073741824` | largest blob accepted, in bytes |
| `-max-manifest-size` | `4194304` | largest manifest accepted, in bytes |
| `-log-format` | `text` | `text` or `json` |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error` |

The two size ceilings are separate because the constraint differs: a blob is
streamed to disk, while a manifest is held in memory to be hashed and parsed.

A bad `-log-format` or `-log-level` fails at startup rather than falling back to a
default, so a typo cannot silently discard the records you asked for.

## Pushing and pulling with oras

cairn speaks enough of the spec for [`oras`](https://oras.land) to use it as a
registry. It has no TLS, so `--plain-http` is required.

```sh
oras blob push --plain-http 127.0.0.1:5050/agntcy/skill ./skill_record.json
# Pushed: [registry] 127.0.0.1:5050/agntcy/skill
# Digest: sha256:492100945cb1786021c9bf867dd0f59510b1628c9510837df951328e07cdbab7
```

Reading it back takes the digest, not the path — `oras blob fetch` wants either
`--output` or `--descriptor`:

```sh
DIGEST=sha256:492100945cb1786021c9bf867dd0f59510b1628c9510837df951328e07cdbab7

oras blob fetch --plain-http --descriptor 127.0.0.1:5050/agntcy/skill@$DIGEST
# {"mediaType":"application/octet-stream","digest":"sha256:4921…","size":1181}

oras blob fetch --plain-http --output - 127.0.0.1:5050/agntcy/skill@$DIGEST
oras blob fetch --plain-http --output ./got.json 127.0.0.1:5050/agntcy/skill@$DIGEST
```

Two things about that output are worth reading closely, because both are the
data model showing through rather than quirks.

The repository in the reference is part of the address, not decoration. Fetching
the same digest from a repository it was never pushed to is a 404, even though
the bytes are on disk — a transposed `agncty` for `agntcy` looks exactly like a
missing blob, and correctly so.

And the media type moves. `oras blob push` labels its own progress line
`application/vnd.oci.image.layer.v1.tar`, but nothing of the sort reaches cairn:
a blob here is bytes plus a digest. The descriptor printed back on fetch says
`application/octet-stream` because that is simply the `Content-Type` of the
response. Media types are recorded by the *manifest* that references a blob, which
is why the round trip cannot preserve one yet.

## Manifests with oras

`oras push` is **not** usable here: it addresses its result by tag, and a client
cannot know a manifest's digest before building it. `oras manifest push` takes a
digest, so the artifact has to be assembled explicitly — which is arguably
clearer about what a push actually is.

Blobs first, because a manifest that names absent content is refused:

```sh
oras blob push --plain-http 127.0.0.1:5050/agntcy/skill ./skill_record.json
printf '{}' > empty.json
oras blob push --plain-http 127.0.0.1:5050/agntcy/skill ./empty.json
```

Then a manifest naming both, pushed at the digest of its own bytes:

```sh
MD=sha256:47cf5c4d800f3cfda50604626c5bdb78d264d31e9f2eb7bf8bc82cf1f5756b78

oras manifest push --plain-http 127.0.0.1:5050/agntcy/skill@$MD ./manifest.json
oras manifest fetch --plain-http 127.0.0.1:5050/agntcy/skill@$MD
oras manifest fetch --plain-http --descriptor 127.0.0.1:5050/agntcy/skill@$MD
```

A HEAD is answered from the index alone, without opening the content:

```sh
curl -sI "http://127.0.0.1:5050/v2/agntcy/skill/manifests/$MD"
# HTTP/1.1 200 OK
# Content-Length: 489
# Content-Type: application/vnd.oci.image.manifest.v1+json
# Docker-Content-Digest: sha256:47cf5c4d800f…
```

That `Content-Type` is the reason `media_type` is a column: it comes from the
row, so answering a HEAD never reads or parses the manifest.

The same digest is a 404 on the blob endpoints, which is not an oversight —
see below.

## Pushing and pulling with curl

One request, digest supplied up front (end-4b):

```sh
DIGEST="sha256:$(shasum -a 256 file.txt | cut -d' ' -f1)"

curl -X POST --data-binary @file.txt \
  "http://127.0.0.1:5050/v2/acme/widgets/blobs/uploads/?digest=$DIGEST"

curl "http://127.0.0.1:5050/v2/acme/widgets/blobs/$DIGEST"
```

Or as a resumable session (end-4a, 5, 6). Note that a POST *without* `?digest=`
opens a session and discards any body you send with it — that is what the spec
asks for, and it surprises people driving this by hand:

```sh
SESSION=$(curl -si -X POST http://127.0.0.1:5050/v2/acme/widgets/blobs/uploads/ \
  | awk '/^[Ll]ocation:/{print $2}' | tr -d '\r')

curl -X PATCH --data-binary @part1 -H 'Content-Range: 0-1023' \
  "http://127.0.0.1:5050$SESSION"
curl "http://127.0.0.1:5050$SESSION"                      # end-13, offset so far
curl -X PUT "http://127.0.0.1:5050$SESSION?digest=$DIGEST"
```

Granting a second repository access to bytes already stored, transferring nothing
(end-11):

```sh
curl -i -X POST \
  "http://127.0.0.1:5050/v2/agntcy/mirror/blobs/uploads/?mount=$DIGEST&from=agntcy/skill"
# HTTP/1.1 201 Created
# Location: /v2/agntcy/mirror/blobs/sha256:4921…
```

After which the digest reads 200 from both repositories, and deleting it from one
leaves the other untouched.

## Endpoints

| Spec   | Method | Path |
|--------|--------|------|
| end-1  | GET    | `/v2/` |
| end-2  | GET, HEAD | `/v2/<name>/blobs/<digest>` (supports `Range`) |
| end-3  | GET, HEAD | `/v2/<name>/manifests/<digest>` |
| end-4a | POST   | `/v2/<name>/blobs/uploads/` |
| end-4b | POST   | `/v2/<name>/blobs/uploads/?digest=<digest>` |
| end-5  | PATCH  | `/v2/<name>/blobs/uploads/<ref>` |
| end-6  | PUT    | `/v2/<name>/blobs/uploads/<ref>?digest=<digest>` |
| end-7  | PUT    | `/v2/<name>/manifests/<digest>` |
| end-9  | DELETE | `/v2/<name>/manifests/<digest>` |
| end-10 | DELETE | `/v2/<name>/blobs/<digest>` |
| end-11 | POST   | `/v2/<name>/blobs/uploads/?mount=<digest>&from=<repo>` |
| end-13 | GET    | `/v2/<name>/blobs/uploads/<ref>` |

## Layout

```
cmd/cairnd            wiring and the HTTP server
internal/ocihttp      request parsing, status and error codes -- no storage logic
internal/registry     the rules: digest verification, chunk ordering, scoping,
                      manifest validation (manifest.go)
internal/blobstore    committed bytes, addressed by digest
internal/uploadstore  staged bytes of uploads that have no digest yet
internal/metastore    the SQL index: blob membership and session state
internal/model        what a row holds
internal/logging      logger construction: format, level, destination
```

Three storage concerns, kept apart because they have different lifetimes:

- **blobstore** holds immutable, content-addressed bytes shared by all
  repositories.
- **uploadstore** holds partial content that has no digest, may never get one,
  and is the only thing here that gets deleted in the normal course of events.
- **metastore** holds no bytes at all.

## Why a content-addressed store still needs an index

A digest answers "are these the bytes I asked for". It cannot answer "may this
repository read them", and the spec requires that second answer: a blob pushed to
`acme/widgets` must be a 404 in `other/repo` even though the bytes are sitting
right there on disk.

That is the whole job of the `blobs` table — one row per (repository, digest)
membership fact. It is also what makes a cross-repository mount cheap: end-11
grants a second repository access to bytes already on disk by inserting one row,
transferring nothing.

Manifests add two more questions a digest cannot answer. **What is this?** — a
GET has to reply with the manifest's own media type, and reading and parsing the
document to find out would make every HEAD a content read; `media_type` is a
column so a HEAD touches only the index. And **what refers to this?** — the
`subject` pointer lives inside the child, but every client wants to query it from
the parent, which is a scan without an index and a seek with one. That second
question is where SQL stops being a convenience, and it is the one still
unanswered here: the column is populated, the endpoint is not written.

## Decisions worth knowing

**Digests are derived, never accepted.** `blobstore.Put` hashes what it actually
reads. A client's claimed digest is only ever compared against that, so the store
cannot be made to misdescribe its contents.

**Verification happens before promotion.** An upload is hashed while still staged,
and only moved into the blob store if it matches. Verifying afterwards would mean
either trusting the claim briefly or deleting a blob at its true digest to undo
the mistake — and those bytes may be ones another repository legitimately holds.

**Blobs first, metadata second.** The bytes are committed before the row that
points at them. The reverse order can leave an index entry referencing content
that is not there, which reads as corruption; this order can at worst leave an
unreferenced blob, which is inert.

**The staging file's length is authoritative.** The session's `received` column is
a cache of it, set from what was actually written rather than from an increment,
so a lost response cannot desynchronise the offset a client resumes from.

**Session IDs are v4 UUIDs, in canonical form only.** end-4a requires the
`Location` to contain a UUID, and randomness rather than a clock matters
independently: possession of the session URL is the only thing protecting an
in-progress upload, so a guessable ID would let a third party append to it.
Non-canonical spellings that `uuid.Parse` happens to accept — `urn:uuid:`,
braces, unhyphenated — are rejected, since the ID is the one part of a request
that becomes a filesystem path.

**Non-contiguous chunks are refused, not reconciled.** Accepting a chunk that
does not start where the last one ended would leave a gap in the middle of a
blob, and the digest check at close would then fail with nothing to point at.

**Deleting a blob deletes a row.** The bytes stay, because another repository may
hold the same digest and deciding this was the last reference needs a sweep that
does not exist yet. Hence end-10's 202 rather than a 204. The same is true of
end-9 for a manifest.

**A manifest is not a blob, though it is stored as one.** Manifest bytes go into
the same content-addressed store, so identical manifests deduplicate — but no
`blobs` membership row is written, and a manifest digest is therefore a 404 on
end-2.

The distinction is worth being precise about, because "a manifest is just a blob"
is true of *storage* and false of the *API*. The `blobs` table does not record
which bytes exist; it records which digests a repository exposes at end-2, which
is why a mount grants one by inserting a row. So the question is not whether a
manifest is bytes but whether it is a member of the repository's blob set, and the
spec says no: manifests get their own endpoints, their own error codes, and their
own descriptors that resolve *against* that set.

The model is git's. One content-addressed object store, type-blind; a reference
graph above it that is typed. Distribution is built the same way — a single
`blobs/` tree with two link namespaces, `_layers` and `_manifests/revisions` —
which is exactly `blobs` and `manifests` here.

Giving a manifest rows in both tables would buy one simpler garbage-collection
query and cost real coherence: two delete endpoints acting on one piece of content
produce four states, two of them nonsense (a manifest readable only as an
octet-stream, or a live manifest whose GC reference is gone). It would also let a
manifest pass as a *layer*, since layer descriptors resolve against `blobs`.

**A manifest may not reference content the repository lacks.** `config`, `layers`
and an index's `manifests` must all already be present, in *this* repository,
checked against their own namespace. So a manifest that exists is one that
resolves, and the failure is attributed precisely — `MANIFEST_BLOB_UNKNOWN`
naming the offending descriptor, rather than a 404 that reads as "your manifest
is missing". Content in a sibling repository does not count, which is exactly the
gap end-11 exists to close.

**Liveness is a question about every reference table, and it has a name.** Because
deleting anything removes only a row, and because content is shared, "can these
bytes go" cannot be answered from blob membership alone —
`metastore.IsReferenced` unions both tables. It exists before the sweeper that
needs it so the obligation is stated in one place rather than rediscovered; the
failure it prevents is a manifest whose HEAD succeeds and whose GET returns 404.
It is also why `manifests_by_digest` exists: the primary key is clustered on
(repository, digest) and cannot answer a query on digest alone.

Note what the predicate does *not* give you. Dedup means an unreferenced digest
becomes referenced the moment anyone pushes the same bytes, so a real sweeper
needs a grace period or a maintenance window on top.

**Reference checks are repository-scoped, which is stricter than the spec's
wording.** The spec says a registry MAY reject a manifest referencing content
absent *from the registry*; cairn rejects when it is absent from *this
repository*. Registry-wide existence is the wrong question: a blob in
`acme/other` is not readable from `acme/widgets`, so accepting the manifest would
store something that 404s on every layer fetch. Better to fail at push time, where
the client still has the bytes and the context, than at pull time in someone
else's CI. The error distinguishes "held nowhere" from "held elsewhere, mount it"
— without naming the other repository, since a mount with no `from` resolves the
source itself and naming it becomes a disclosure as soon as there is auth.

**A subject is recorded but never required to exist.** The spec is explicit that a
referrer may be pushed before, or entirely without, its subject, and signing tools
rely on it: cosign signs a digest, not a registry state. Validating it would break
them, so `subject` is stored as an uninterpreted pointer and `OCI-Subject` is
echoed back so a client knows the pointer was understood.

**Content-Type must agree with the document's `mediaType`.** Both describe the
same bytes, and which one a proxy or cache downstream believes is not knowable
from here, so a disagreement is refused instead of silently resolved in favour of
one. The value recorded always comes from the bytes.

**Manifests are returned verbatim.** The bytes are stored and served exactly as
received, never re-serialised. Canonicalising the JSON would change the digest and
invalidate every signature over it. This is also why parsing does not reject
unknown fields: annotations and later spec additions must survive the round trip.

**A tag is refused, not faked.** `UNSUPPORTED` on a well-formed tag, rather than
`MANIFEST_UNKNOWN`. A 404 would be a lie a client acts on — it would conclude the
manifest is absent and try to push one. An unsupported digest *algorithm* is
distinguished from a malformed reference for the same reason.

## Logging

Two streams, both `log/slog`. An access log records one entry per request; the
registry records the handful of domain events a status code cannot express, such
as which digest a client claimed versus what the bytes actually hashed to.

```
$ cairnd -log-format json
{"level":"INFO","msg":"blob committed","repository":"agntcy/skill","digest":"sha256:4921…","size":1181}
{"level":"INFO","msg":"request","method":"PUT","path":"/v2/agntcy/skill/blobs/uploads/3334433b-…","status":201,"bytes":0,"duration":5703083,"digest":"sha256:4921…"}
{"level":"WARN","msg":"upload digest mismatch","upload":"abd49f53-…","claimed":"sha256:0000…","actual":"sha256:0da5…","received":12}
{"level":"WARN","msg":"request","method":"GET","path":"/v2/other/repo/blobs/sha256:0da5…","status":404,"bytes":62,"duration":139958}
```

The level splits a client's mistake from the registry's: 4xx is a warning, 5xx an
error. So `-log-level error` is a usable "only tell me when *I* am broken" filter,
which is the main reason to bother mapping status onto level at all.

Access records carry `upload` and `digest` when the response headers have them.
That is what stitches the POST, PATCH and PUT of one chunked upload into
something you can grep for, since a session's steps otherwise share nothing but a
URL.

Structured output is a correctness property here, not a formatting preference.
The request path is chosen by the client, so under a printf-style log a request
carrying CRLF plus a plausible-looking line would append a *second* entry and let
a caller write whatever it liked into what you read. `slog` escapes it as a value;
`TestAccessLogResistsInjection` sends exactly that and asserts one record comes
out. Paths are also truncated, so a client cannot decide how much disk a request
costs.

Nothing emits at debug yet, so `-log-level debug` only widens the filter.

## Known gaps

- No tags, so no end-8 tag listing and no way to name a manifest by anything but
  its digest. This is what stops `oras push`, `docker pull` and most of the
  ecosystem from working end to end.
- No referrers (end-12). The `subject` and `artifact_type` columns are populated
  on every push, so the endpoint is a query away, but the index it wants is
  deliberately absent until then — an unused index is only write amplification.
  An earlier revision implemented end-12 over the same schema; see `git log`.
- Nothing is ever reclaimed. Deleting a blob or a manifest removes a row and
  leaves the bytes, and abandoned upload sessions keep their staged files. The
  schema has what a sweeper needs — `metastore.IsReferenced` for content and
  `uploads_by_updated_at` for sessions — but nothing runs, and a real sweeper also
  needs a grace period that neither provides.
- No authentication, so the blob endpoints are unauthenticated writes bounded
  only by `-max-blob-size`.
- Blob reads through `Open` are not digest-verified, because verifying would mean
  reading the whole blob before answering and would defeat `Range` support.
- No request ID, so an access record and the domain records emitted while serving
  it share only a digest or upload ID. Fine at one request at a time, thin under
  concurrency. Logs go to stderr and are not rotated.

## Tests

```sh
go test ./...
```

The tests drive the real HTTP surface over a real SQLite database and real files,
using the fixtures in `internal/ocihttp/testdata`.

One environment note: `http.ServeContent` hands `*os.File` bodies to `sendfile`,
so under a syscall sandbox that blocks it, blob reads larger than 512 bytes are
truncated and `TestChunkedPush` fails on a short body. That is the sandbox, not
the code.
