# Contributing to Disk Recovery Lab

Thanks for helping! This project recovers people's data, so **correctness and safety come first**.

## Ground rules

1. **Never write to the source.** Code that reads a drive must go through `internal/rawdisk`, which opens
   devices read-only. Repairs only ever modify a copy (see `detect.RepairedCopy`).
2. **Clean-room only.** Write parsers from public specifications (vendor technical notes, published format
   documents, your own analysis of disks you created). Do not copy or translate code from closed-source recovery
   tools, and do not submit material obtained by decompiling them.
3. **No new runtime dependencies** without discussion: the app should stay a small, auditable single file. Go
   team modules (`golang.org/x/...`) are fine.
4. Explain uncertainty to the user. If a recovered file may be incomplete or overwritten, say so in the item's
   `note`; don't hide it.

## Getting started

```sh
go test ./...
go run ./cmd/disk-recovery-lab --no-elevate
```

`--no-elevate` runs without administrator rights, which is enough to work with the disk images in `testdata/` and
with images you create yourself.

## Adding things

- **A file signature:** add a `Carver` to `internal/carve/carve.go` with a function that validates the structure and
  returns the exact length. Return 0 for anything that does not look right; false positives waste users' time.
- **A filesystem reader:** follow `internal/hfs` as the model. Expose a way to list records (including stale or
  deleted ones when the format allows) and map them to byte ranges, then wire it into `internal/scan/run.go`.
- **Tests:** add a small, compressible disk image to `testdata/` (a few tens of KB once gzipped) and a test that
  scans it. `scripts/make-fixtures.sh` shows how the existing images are produced on macOS. Describe how you made
  new images in the pull request.
- **Platform code:** OS-specific behaviour lives in `internal/platform` behind build tags. Each OS must compile:
  `GOOS=windows go vet ./...` and `GOOS=linux go vet ./...` run in CI.

## Pull requests

- Run `gofmt`, `go vet ./...` and `go test ./...`.
- Keep changes focused, and describe how you tested them (which OS, which kind of drive or image).
- UI changes: include a screenshot.

By contributing you agree that your contribution is licensed under the Apache License 2.0.
