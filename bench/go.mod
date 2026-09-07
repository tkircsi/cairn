// A separate module so the benchmark's dependencies stay out of cairn's, and so
// `go build ./...` and `go test ./...` at the repo root do not pick it up. Nothing
// in cairn imports this, and it talks to every registry over plain HTTP -- including
// cairn -- so that all three are driven identically.
module github.com/tkircsi/cairn/bench

go 1.24

require (
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.1.1
)
