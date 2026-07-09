// This is a self-contained module so `docker build examples/hello` needs only this directory
// (no dependency on the parent burrow module). It has no requires — hello imports only the Go
// standard library. As a nested module it is excluded from the repo's `go build ./...`/`go test
// ./...`; its SPDX header is still checked and it is still gofmt'd.
module hello

go 1.26
