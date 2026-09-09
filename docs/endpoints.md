# Endpoints

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
