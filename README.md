# cairn

A small OCI registry that keeps blobs on the filesystem and everything it needs
to *find* them in SQL.

The scope is deliberately one slice of the [OCI distribution
spec](https://github.com/opencontainers/distribution-spec/blob/main/spec.md):
push a blob, read it back. That slice is enough to exercise the whole shape of a
registry — content addressing, upload sessions, repository scoping — without the
manifest and tag surface on top.

## Running it

```sh
go run ./cmd/cairnd -addr 127.0.0.1:5050 -root data
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-addr` | `127.0.0.1:5050` | address to listen on |
| `-root` | `data` | directory holding `blobs/`, `uploads/` and `cairn.db` |
| `-max-blob-size` | `1073741824` | largest blob accepted, in bytes |
| `-log-format` | `text` | `text` or `json` |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error` |

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
| end-4a | POST   | `/v2/<name>/blobs/uploads/` |
| end-4b | POST   | `/v2/<name>/blobs/uploads/?digest=<digest>` |
| end-5  | PATCH  | `/v2/<name>/blobs/uploads/<ref>` |
| end-6  | PUT    | `/v2/<name>/blobs/uploads/<ref>?digest=<digest>` |
| end-10 | DELETE | `/v2/<name>/blobs/<digest>` |
| end-11 | POST   | `/v2/<name>/blobs/uploads/?mount=<digest>&from=<repo>` |
| end-13 | GET    | `/v2/<name>/blobs/uploads/<ref>` |

## Layout

```
cmd/cairnd            wiring and the HTTP server
internal/ocihttp      request parsing, status and error codes -- no storage logic
internal/registry     the rules: digest verification, chunk ordering, scoping
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

Worth being honest about the scope, though: **blob push and pull is the part of
the spec where SQL earns its keep least.** Content addressing already answers
most questions a path lookup would need an index for. The index becomes load
bearing for referrers, where the `subject` pointer is stored on the child and
every client wants to query it from the parent.

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
does not exist yet. Hence end-10's 202 rather than a 204.

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

- No manifests, tags or referrers. An earlier revision implemented the referrers
  endpoint over the same SQL index; see `git log`.
- Abandoned upload sessions are never swept. The schema has the index a sweeper
  would use (`uploads_by_updated_at`) but nothing runs.
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
