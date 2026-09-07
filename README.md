# cairn

A small OCI registry that keeps blobs on the filesystem and everything it needs
to *find* them in SQL.

The scope is deliberately one slice of the [OCI distribution
spec](https://github.com/opencontainers/distribution-spec/blob/main/spec.md):
blobs, manifests, tags and referrers. That slice exercises the whole shape of a
registry — content addressing, upload sessions, repository scoping, a manifest
graph that has to resolve, mutable names over immutable content, and the one query
content addressing cannot answer on its own.

Enough for `oras push`, `oras pull`, `oras repo tags`, `oras attach` and
`oras discover` to work against it unmodified.

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

## Tags: the normal oras workflow

With tags, `oras push` and `oras pull` work the way they do against any registry,
because a tag is the only way to name content you have not built yet:

```sh
echo "hello from cairn" > note.txt
oras push --plain-http 127.0.0.1:5050/acme/widgets:v1.0.0 note.txt:text/plain
# Pushed [registry] 127.0.0.1:5050/acme/widgets:v1.0.0
# Digest: sha256:2de88a7cb2d9c05fa78ff023ca4272632f44c79f4832a0b7995160a4bcb7531c

oras repo tags --plain-http 127.0.0.1:5050/acme/widgets
# v1.0.0

oras pull --plain-http 127.0.0.1:5050/acme/widgets:v1.0.0
```

A release usually wants several names for one build, which is end-7b — one push,
`tag` repeated, and the accepted names echoed back:

```sh
curl -si -X PUT -H "Content-Type: application/vnd.oci.image.manifest.v1+json" \
  --data-binary @manifest.json \
  "http://127.0.0.1:5050/v2/acme/widgets/manifests/$MD?tag=1.2.3&tag=1.2&tag=latest"
# HTTP/1.1 201 Created
# Oci-Tag: 1.2.3, 1.2, latest
```

Listing is paginated by cursor, not by offset (end-8b). The `Link` header carries
the next request, so a client never has to build one:

```sh
curl -s "http://127.0.0.1:5050/v2/acme/widgets/tags/list"
# {"name":"acme/widgets","tags":["1.2","1.2.3","latest","v1.0.0"]}

curl -si "http://127.0.0.1:5050/v2/acme/widgets/tags/list?n=2"
# Link: </v2/acme/widgets/tags/list?last=1.2.3&n=2>; rel="next"
# {"name":"acme/widgets","tags":["1.2","1.2.3"]}

curl -s "http://127.0.0.1:5050/v2/acme/widgets/tags/list?last=1.2.3&n=2"
# {"name":"acme/widgets","tags":["latest","v1.0.0"]}
```

Note the order: `1.2` before `1.2.3`, and `latest` before `v1.0.0`. That is
ASCIIbetical, which the spec names by pointing at Go's `sort.Strings`, and it is
not version order — `v10` sorts before `v9`.

## Referrers: attaching things to a manifest

`oras attach` pushes a manifest whose `subject` names another, and `oras discover`
asks the question back:

```sh
echo '{"packages":["libfoo 1.2.3"]}' > sbom.json
echo '{"sig":"pretend"}' > sig.json

oras attach --plain-http --artifact-type application/vnd.example.sbom.v1+json \
  --annotation "org.example.tool=syft" 127.0.0.1:5050/acme/widgets:v1 sbom.json

oras attach --plain-http --artifact-type application/vnd.example.signature.v1+json \
  --annotation "org.example.signer=alice" 127.0.0.1:5050/acme/widgets:v1 sig.json

oras discover --plain-http 127.0.0.1:5050/acme/widgets:v1
# 127.0.0.1:5050/acme/widgets@sha256:681acb09…
# ├── application/vnd.example.signature.v1+json
# │   └── sha256:3884342d…
# │       └── [annotations]
# │           ├── org.example.signer: alice
# │           └── org.opencontainers.image.created: "2026-09-07T08:25:58Z"
# └── application/vnd.example.sbom.v1+json
#     └── sha256:ee37d3ea…
#         └── [annotations]
#             └── org.example.tool: syft

oras discover --plain-http \
  --artifact-type application/vnd.example.sbom.v1+json 127.0.0.1:5050/acme/widgets:v1
```

`oras attach` resolves the tag to a digest before pushing and then reports the
digest, not the tag — which is the point. The pointer a referrer stores is the
digest, so the association survives the tag moving to a different build.

The raw form (end-12a) returns an image index that was never pushed and has no
stable digest of its own, since the next signature changes it:

```sh
curl -s "http://127.0.0.1:5050/v2/acme/widgets/referrers/$SUBJECT"
```

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.index.v1+json",
  "manifests": [
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:3884342d…",
      "size": 785,
      "annotations": { "org.example.signer": "alice" },
      "artifactType": "application/vnd.example.signature.v1+json"
    }
  ]
}
```

Every field in a descriptor there comes from a column. Nothing in the loop opens a
manifest, which is the difference the `manifests` table buys: the alternative is
reading and parsing every candidate manifest to answer one query.

Filtering declares itself, so a client can tell a narrowed list from a registry
that ignored the parameter (end-12b):

```sh
curl -si "http://127.0.0.1:5050/v2/acme/widgets/referrers/$SUBJECT?artifactType=application/vnd.example.sbom.v1+json"
# HTTP/1.1 200 OK
# Content-Type: application/vnd.oci.image.index.v1+json
# Oci-Filters-Applied: artifactType
```

A subject nothing refers to is an empty list and a 200, not a 404 — "nothing has
been said about this" is an answer:

```sh
curl -s "http://127.0.0.1:5050/v2/acme/widgets/referrers/$(printf 'sha256:%064d' 0)"
# {"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}
```

## Manifests by digest with oras

`oras manifest push` addresses a manifest by its own digest, which means
assembling the artifact explicitly. Nothing requires this now that tags exist, but
it is the clearest view of what a push actually is.

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
| end-3  | GET, HEAD | `/v2/<name>/manifests/<digest\|tag>` |
| end-4a | POST   | `/v2/<name>/blobs/uploads/` |
| end-4b | POST   | `/v2/<name>/blobs/uploads/?digest=<digest>` |
| end-5  | PATCH  | `/v2/<name>/blobs/uploads/<ref>` |
| end-6  | PUT    | `/v2/<name>/blobs/uploads/<ref>?digest=<digest>` |
| end-7a | PUT    | `/v2/<name>/manifests/<digest\|tag>` |
| end-7b | PUT    | `/v2/<name>/manifests/<digest>?tag=&tag=` |
| end-8a | GET    | `/v2/<name>/tags/list` |
| end-8b | GET    | `/v2/<name>/tags/list?n=&last=` |
| end-9  | DELETE | `/v2/<name>/manifests/<digest\|tag>` |
| end-10 | DELETE | `/v2/<name>/blobs/<digest>` |
| end-11 | POST   | `/v2/<name>/blobs/uploads/?mount=<digest>&from=<repo>` |
| end-12a | GET   | `/v2/<name>/referrers/<digest>` |
| end-12b | GET   | `/v2/<name>/referrers/<digest>?artifactType=<type>` |
| end-13 | GET    | `/v2/<name>/blobs/uploads/<ref>` |

## Layout

```
cmd/cairnd            wiring and the HTTP server
internal/ocihttp      request parsing, status and error codes -- no storage logic
internal/registry     the rules: digest verification, chunk ordering, scoping,
                      manifest validation (manifest.go), tags (tag.go),
                      referrers (referrers.go)
internal/blobstore    committed bytes, addressed by digest
internal/uploadstore  staged bytes of uploads that have no digest yet
internal/metastore    the SQL index: membership, manifests, tags, referrers,
                      session state
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
column so a HEAD touches only the index.

And **what refers to this?** — which is where SQL stops being a convenience. The
`subject` pointer is stored on the child, because a signature knows what it signs,
but every client asks from the other end: given an image, what has been said about
it? That answer is not derivable from the subject's bytes and changes every time
someone signs it, so content addressing cannot produce it at all. Something has to
keep a mapping from subject back to the manifests naming it, and that something is
`manifests_by_subject` — a scan of every manifest in the repository without it, a
seek with it.

The same reasoning decides how much of a manifest gets columns. A referrers
response has to carry each referrer's `artifactType` and its annotations, since
those are how a client picks one signature out of twenty without fetching them all.
Leaving them in the bytes would mean reading and parsing every candidate manifest
to answer one listing — the read amplification the endpoint exists to prevent — so
`artifact_type` and `annotations` are columns too.

Tags add the question that runs the other way. Content names itself, but nobody
wants to type a digest, so **what does this name point at right now?** needs a
place to live — and unlike everything else here, the answer changes. The `tags`
table is the only mutable naming in the store, and it is clustered on
`(repository, name)` precisely because that is the order end-8a must return, which
turns listing into an ordered range scan and pagination into a seek.

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
rely on it: cosign signs a digest, not a registry state. Validating existence would
break them, so `subject` is stored as an uninterpreted pointer and `OCI-Subject` is
echoed back so a client knows the pointer was understood.

Its *syntax* is checked, though, which is not the same thing. An unparseable
subject would be filed under a key no query could produce, so the referrer would be
silently unfindable by the only endpoint that looks for it — a client error is far
better than a signature that vanishes.

**A referrers response is assembled, not stored.** The index end-12 returns was
never pushed and has no stable digest, because the next signature pushed changes
it. It is the one place this API steps outside content addressing, and that is
precisely why the endpoint has to exist: the answer is a property of the store at a
moment, not of any bytes in it.

**`artifactType` in a response is not always what the manifest said.** The spec
defines a fallback — an image manifest that declares none is described by its
config's media type, an index that declares none has the field omitted entirely —
which keeps the pre-1.1 convention of carrying the artifact's type in the config
descriptor working.

Resolving it happens once, at push time, so `artifact_type` holds the *effective*
type. That is what makes the end-12b filter a column comparison instead of a rule
reapplied to every row on every query, and it means the value you can filter on is
the same value the listing shows.

**A tag is not accepted as a subject.** Every other manifest endpoint takes a tag
or a digest; end-12 takes only a digest. Resolving a tag would answer about
whatever it points at *now*, while the referrers were filed against what it pointed
at *then*, so every signature would appear to vanish the moment a tag moved.

**`+` in the `artifactType` query is a plus, not a space.** Go's query parser
follows the HTML form convention where `+` means a space, and almost every artifact
type ends in `+json` — so a client sending the perfectly legal
`?artifactType=application/vnd.example.sbom.v1+json` would be filtering on a type
with a space in it. cairn percent-decodes without that substitution.

It is safe to reinterpret because only one reading can ever be right: a media type
cannot contain a space, so there is no input for which form decoding would have been
correct, and an escaped `%2B` still arrives as `+`. The failure it avoids is the
dangerous kind — an empty list is a well-formed answer meaning "nothing has been
said about this", so a mangled filter reads as *unsigned* rather than as an error.

**An unknown repository is a 404 from end-12, which is a reading of the spec rather
than a transcription.** The spec says a registry supporting this API "MUST NOT
return a 404 Not Found to a referrers API request", yet lists 404 among end-12a's
failure codes, so the prohibition cannot be absolute. Taking it to mean "not for a
subject with no referrers" satisfies both sentences.

The alternative is worse where it matters: a mistyped repository would answer 200
with an empty list, which a verification tool cannot distinguish from "this image is
unsigned". And the cost of being wrong is bounded — a client that reads 404 as "no
referrers API here" falls back to the referrers tag schema, which in a repository
that does not exist is also a 404.

**The referrers index is partial, and the query says so out loud.** Most manifests
are not referrers, so `manifests_by_subject` covers only rows `WHERE subject != ''`
— keeping it proportional to the number of signatures rather than to everything ever
pushed, and keeping a plain image push from maintaining an entry it would never
appear in.

The query repeats that predicate, which looks redundant next to `subject = ?` and is
not. SQLite chooses a partial index only when the WHERE clause implies the index's
own predicate, and a bound parameter cannot be known to be non-empty, so removing
the term turns the seek back into a scan. It also stops a caller passing an empty
digest from matching every plain manifest in the repository. There is a test that
reads `EXPLAIN QUERY PLAN`, because both mistakes return identical rows.

**Opening and closing the database run `PRAGMA optimize`.** Not tuning: without
statistics SQLite's planner rates a prefix seek of the primary key on `repository`
alone as competitive with the subject index, and picks it — reading every manifest
in the repository. The index is present and simply not chosen.

`metastore.Optimize` is exported because the schedule cannot be decided here.
Statistics are only as current as the last call, so a daemon that opens an empty
database and then serves a million pushes holds an empty database's statistics for
the whole run.

**Column additions migrate; nothing else needs to.** `CREATE TABLE IF NOT EXISTS`
silently does nothing when the table already exists, so a column added later never
reaches a database an earlier build created — which is what the `annotations` column
was. Indexes and new tables need no migration, since `CREATE ... IF NOT EXISTS` is
reapplied on every open.

Each migration asks the schema whether it has already been applied rather than
consulting a recorded version, because a version number can drift out of step with
the database it claims to describe.

**Content-Type must agree with the document's `mediaType`.** Both describe the
same bytes, and which one a proxy or cache downstream believes is not knowable
from here, so a disagreement is refused instead of silently resolved in favour of
one. The value recorded always comes from the bytes.

**Manifests are returned verbatim.** The bytes are stored and served exactly as
received, never re-serialised. Canonicalising the JSON would change the digest and
invalidate every signature over it. This is also why parsing does not reject
unknown fields: annotations and later spec additions must survive the round trip.

**A tag resolves to a digest and then behaves identically.** A GET by tag returns
the same body and the same `Docker-Content-Digest` as a GET by the digest behind
it. The tag is an indirection in the lookup, not a different kind of response, so
nothing downstream needs to know which form the client used.

**Deleting a tag and deleting a manifest are different operations at one URL.**
end-9 on a tag withdraws a name and leaves the content; end-9 on a digest removes
the content and takes every name for it along too. The cascade is the spec's
requirement — a tag must stop resolving once its manifest is gone — and it is the
only transaction in the store, because a listing that advertised a tag which
cannot be fetched would be worse than one that omitted it.

**Pagination is by cursor, never by offset.** `last` names a position in the
ordering rather than a count into it, so a tag pushed or deleted mid-walk cannot
shift the window and make a client skip or repeat one. The cursor need not still
exist, which is exactly the case that makes offsets wrong.

**A repository exists if anything is filed under it.** There is no repositories
table and no create step; existence is derived from the other tables. But the
distinction is still worth deriving, because the spec requires a pull of a
nonexistent repository to 404, and a typo would otherwise be indistinguishable
from a repository that simply has no tags yet. The check runs only when a page
comes back empty, so a repository with tags pays nothing for it.

**An unsupported digest algorithm is distinguished from a malformed reference.**
`UNSUPPORTED` rather than `MANIFEST_INVALID`, because "I do not implement md5" and
"that is not a reference" call for different responses from the client.

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

- end-8a returns every tag in a repository, with no ceiling. A client that sends
  `n` gets a bounded page and a `Link`; one that does not gets the whole list,
  which for a repository with a very large number of tags is a response nobody
  wants. A default page size would fix it and is deliberately not invented here.
- end-12 is not paginated. The spec requires a `Link` header when the descriptor
  list will not fit in one response and leaves the threshold to the registry;
  cairn returns all of them. In practice a subject accumulates signatures and
  attestations rather than thousands of referrers, so this is a smaller hazard than
  the tag listing above, but it is the same missing ceiling.
- No backfill from the referrers tag schema. A registry enabling end-12 is meant
  to surface referrers that clients previously recorded by pushing an index to a
  `sha256-<subject>` tag. cairn never advertised the API as absent — it returns
  `OCI-Subject`, which tells a client the API is live — so there should be no such
  data, but a client that pushed to this store between the tag and referrer commits
  could have created some, and nothing goes looking for it.
- Statistics can go stale within a long run, degrading the referrers query plan.
  See `metastore.Optimize`; a real deployment wants it on a timer.
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
