# cairn

A small content store with authoritative SQL metadata, blobs on disk, a gRPC
write surface and a **read-only OCI HTTP surface**.

The point is one query. `GET /v2/<name>/referrers/<digest>` asks "which manifests
name this subject", and a path-addressed registry cannot answer it: a filesystem
layout gives one access path per directory tree it maintains by hand, and
`subject` appears in none of them. That is why the OCI spec ships a fallback in
which the *client* maintains a `sha256-<subject>` image index — a shared mutable
document, updated by read-modify-write, with no transaction anywhere. Concurrent
writers to one subject lose each other's work, and each one still reports
success.

Here the same question is an index on `(repository, subject, artifact_type)`.

## Shape

```
proto/oci/v1/store.proto     write API, in OCI vocabulary
internal/model               stored shapes: Manifest, Blob
internal/blobstore           bytes by digest, verified on read
internal/metastore           the metadata index (interface)
internal/metastore/sqlite    schema and queries
internal/registry            service layer: parse, validate, order writes
internal/grpcapi             write surface
internal/ocihttp             read surface (end-1, end-12a, end-12b)
cmd/cairnd                   both surfaces over one store
```

Both API packages call `internal/registry` and nothing else, so there is exactly
one place that decides what may be stored. Neither protobuf nor HTTP types reach
the model.

## The data model is one table

There is no separate "referrer" entity. Per the image spec a referrer is just a
manifest whose `subject` names another manifest, so:

```sql
CREATE TABLE manifests (
    repository, digest, media_type, artifact_type,
    subject,                   -- empty when the manifest refers to nothing
    size, annotations, created_at,
    PRIMARY KEY (repository, digest)
);

CREATE INDEX manifests_by_subject
    ON manifests (repository, subject, artifact_type, digest)
    WHERE subject <> '';
```

Partial, because only manifests carrying a subject are referrers. That index is
the whole difference from a filesystem-tree registry.

## Decisions worth knowing

**The OCI surface is read-only, and that is the design.** The interop that
matters — `oras`, `crane`, `cosign verify`, peers fetching content, `regsync`
with this store as source — is all reads. Writes go through gRPC where content
can be parsed and validated before it lands. The asymmetry also skips the
fiddliest part of the spec: the resumable blob-upload session (`end-4a`, `5`,
`6`, `13`, `14`).

**Metadata is derived from the bytes, never passed alongside them.**
`PutManifest` takes raw manifest JSON and reads `mediaType`, `artifactType`,
`subject` and `annotations` out of it, so the index cannot describe something the
content does not say.

**Blobs first, metadata second.** The reverse order can leave an index row
pointing at absent content, which reads as corruption. This order can at worst
leave an unreferenced blob, which is inert.

**No foreign key from `subject` to `digest`.** The spec requires accepting a
manifest whose subject is not present — the conformance suite pushes exactly that
case — so a foreign key there would fail conformance. Cascading a subject delete
onto its referrers is a policy this schema *can* express but does not impose.

**`artifactType` is resolved at ingest.** The spec's fallback rule (empty on an
image manifest means the config descriptor's `mediaType`; empty on an index means
omit) is applied once on write, so reads stay a plain indexed filter.

## Run it

```sh
go build ./... && go test ./...
go run ./cmd/cairnd -data ./data
```

```sh
curl -i http://127.0.0.1:5000/v2/
curl -i http://127.0.0.1:5000/v2/acme/widgets/referrers/sha256:<digest>
```

Writes go to `127.0.0.1:5001` over gRPC. Regenerate stubs with `buf generate`.

## What the tests pin down

`internal/ocihttp/handler_test.go` covers the parts of `end-12a`/`12b` that are
easy to get wrong rather than just the happy path:

- an unknown-but-valid subject returns **200 with an empty list, never 404** — a
  404 is precisely the signal that sends clients to the fallback tag scheme
- `manifests` marshals as `[]`, not `null`
- `Content-Type` is `application/vnd.oci.image.index.v1+json`
- `OCI-Filters-Applied: artifactType` appears when filtering and only then
- a truncated page carries a `Link` header
- invalid digest syntax is `400` with an `errors[].code` envelope
- annotations survive onto the descriptor
- referrers do not leak across repositories
- the `artifactType` config fallback

## Not built yet

Tags and manifest-by-tag (`end-3`, `end-8a/8b`) are the obvious next endpoints
and need one more table. Blob serving (`end-2`) is nearly free because
`blobstore.FS.Open` returns a `ReadSeekCloser`, so `http.ServeContent` handles
`Range` and `416`. Authentication is out of scope in the spec itself, so the
token challenge flow is a separate concern — implement it or front this with a
proxy. Conformance can be run against the read surface with `OCI_API_PUSH=false`.
