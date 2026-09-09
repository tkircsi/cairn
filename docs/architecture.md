# Architecture

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

## Logging

Two streams, both `log/slog`. An access log records one entry per request; the
registry records the handful of domain events a status code cannot express, such
as which digest a client claimed versus what the bytes actually hashed to.

```
$ cairnd -log-format json
{"level":"INFO","msg":"blob committed","repository":"acme/notes","digest":"sha256:4921…","size":1181}
{"level":"INFO","msg":"request","method":"PUT","path":"/v2/acme/notes/blobs/uploads/3334433b-…","status":201,"bytes":0,"duration":5703083,"digest":"sha256:4921…"}
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
