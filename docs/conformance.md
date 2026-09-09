# Conformance

The OCI project maintains a conformance suite, which is the only answer to "is this
compliant" that is not an opinion:

```sh
./scripts/conformance.sh
# Ran 75 of 79 Specs in 0.164 seconds
# SUCCESS! -- 75 Passed | 0 Failed | 0 Pending | 4 Skipped
```

It clones a pinned `distribution-spec` tag, builds the suite's binary, starts a
`cairnd` on a temporary root, runs all four workflows — pull, push, content
discovery, content management — and deletes everything after. Point it at nothing
you care about: the suite pushes and deletes real content.

The version is pinned because a passing run against `main` would only mean
"compliant with whatever the spec said today", which is not a claim anyone can check
afterwards.

The four skips are configuration alternatives rather than gaps. Two are the
mutually exclusive halves of the automatic cross-mount question, and cairn declares
its side with `OCI_AUTOMATIC_CROSSMOUNT=1` — end-11 without `from` resolves the
blob itself and returns 201. The other two are opt-outs from the suite's own setup
(`OCI_TAG_NAME`, `OCI_TAG_LIST`) that only apply when pointing it at content that
already exists.

Worth knowing what a pass does and does not cover. The suite exercises referrers,
including a subject with none and the `artifactType` filter, so end-12 is checked
rather than merely written. It does not check `_catalog`, which is not in the OCI
spec at all, and it says nothing about the [decisions](decisions.md) where the spec permits a
choice.

Running it found one real bug, which is the argument for running it at all: a
reference that is neither a digest nor a legal tag was answered 400, and the suite
requires 404 — it asks for `.INVALID_MANIFEST_NAME` under the name "nonexistent
manifest".
