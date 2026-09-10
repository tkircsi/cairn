# Decisions worth knowing

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

`sqlite.Store.Optimize` is exported because the schedule cannot be decided here.
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

**Every write goes through one connection, on purpose.** SQLite has a single write
lock, so a pool of thirty-two connections does not write in parallel — it decides
how many goroutines contend for that lock. Contending has a price: the loser waits
inside SQLite's busy handler, which retries with backoff rather than queueing, so
the wait is unfair and requests that were entirely valid start failing once its
tail crosses `busy_timeout`. `database/sql` hands connections out FIFO, so capping
the pool at one turns that race into a queue.

It is a throughput decision, not a cautious one — the concurrency was never buying
parallelism, only spending it on backoff. The cost is that reads serialise onto the
same connection, which gives up the concurrent readers WAL exists to provide, and
that is the reason to eventually queue writes at `PutBlob` and `PutManifest`
instead of capping the pool. It also makes one bug possible that was not before:
holding a transaction open while issuing another query on `s.db` now deadlocks,
because the transaction owns the only connection.

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

**A reference that could not exist is a 404 on read and a 400 on write.** A string
that is neither a digest nor a legal tag names nothing, and what that means depends
on the method. A GET, HEAD or DELETE is answered `MANIFEST_UNKNOWN`: the client's
position is the same as for any missing manifest, and a 400 would invite a retry of
a request that cannot succeed. A PUT is refused, because it is not asking whether
the name resolves but for the name to be assigned, and that is not a name a tag may
have.

Calling it malformed on read was the original behaviour and it failed conformance —
the suite requests `.INVALID_MANIFEST_NAME` under the name "nonexistent manifest".

**An unsupported digest algorithm is the one exception, and stays a 400.**
`UNSUPPORTED` rather than a 404, because "I do not implement md5" is a different
statement from "there is nothing here": the reference is well formed, so naming the
algorithm as the problem tells the client something it can act on, and `go-digest`
separates the two cases for free.
