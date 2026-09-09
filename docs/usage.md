# Usage

## Pushing and pulling with oras

cairn speaks enough of the spec for [`oras`](https://oras.land) to use it as a
registry. It has no TLS, so `--plain-http` is required.

```sh
oras blob push --plain-http 127.0.0.1:5050/acme/notes ./note.json
# Pushed: [registry] 127.0.0.1:5050/acme/notes
# Digest: sha256:492100945cb1786021c9bf867dd0f59510b1628c9510837df951328e07cdbab7
```

Reading it back takes the digest, not the path — `oras blob fetch` wants either
`--output` or `--descriptor`:

```sh
DIGEST=sha256:492100945cb1786021c9bf867dd0f59510b1628c9510837df951328e07cdbab7

oras blob fetch --plain-http --descriptor 127.0.0.1:5050/acme/notes@$DIGEST
# {"mediaType":"application/octet-stream","digest":"sha256:4921…","size":1181}

oras blob fetch --plain-http --output - 127.0.0.1:5050/acme/notes@$DIGEST
oras blob fetch --plain-http --output ./got.json 127.0.0.1:5050/acme/notes@$DIGEST
```

Two things about that output are worth reading closely, because both are the
data model showing through rather than quirks.

The repository in the reference is part of the address, not decoration. Fetching
the same digest from a repository it was never pushed to is a 404, even though
the bytes are on disk — a transposed `acem` for `acme` looks exactly like a
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
oras blob push --plain-http 127.0.0.1:5050/acme/notes ./note.json
printf '{}' > empty.json
oras blob push --plain-http 127.0.0.1:5050/acme/notes ./empty.json
```

Then a manifest naming both, pushed at the digest of its own bytes:

```sh
MD=sha256:47cf5c4d800f3cfda50604626c5bdb78d264d31e9f2eb7bf8bc82cf1f5756b78

oras manifest push --plain-http 127.0.0.1:5050/acme/notes@$MD ./manifest.json
oras manifest fetch --plain-http 127.0.0.1:5050/acme/notes@$MD
oras manifest fetch --plain-http --descriptor 127.0.0.1:5050/acme/notes@$MD
```

A HEAD is answered from the index alone, without opening the content:

```sh
curl -sI "http://127.0.0.1:5050/v2/acme/notes/manifests/$MD"
# HTTP/1.1 200 OK
# Content-Length: 489
# Content-Type: application/vnd.oci.image.manifest.v1+json
# Docker-Content-Digest: sha256:47cf5c4d800f…
```

That `Content-Type` is the reason `media_type` is a column: it comes from the
row, so answering a HEAD never reads or parses the manifest.

The same digest is a 404 on the blob endpoints, which is not an oversight —
see [decisions](decisions.md).

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
  "http://127.0.0.1:5050/v2/acme/mirror/blobs/uploads/?mount=$DIGEST&from=acme/notes"
# HTTP/1.1 201 Created
# Location: /v2/acme/mirror/blobs/sha256:4921…
```

After which the digest reads 200 from both repositories, and deleting it from one
leaves the other untouched.
