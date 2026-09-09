# Contributing

cairn is a small experiment. Patches that keep it small are welcome.

## Setup

Go version is the `go` line in `go.mod`.

```sh
go test ./...
```

`gofmt` the files you touch. Add or extend tests for the behavior you change.
Keep PRs focused.

The tests drive the real HTTP surface over SQLite and the filesystem. Under a
syscall sandbox that blocks `sendfile`, blob reads larger than 512 bytes can
fail — that is the sandbox, not the code. See the Tests section of the
[README](README.md).

## Conduct

See [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
