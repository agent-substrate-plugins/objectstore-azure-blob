# Azure Blob object-store plugin

An out-of-tree [Agent Substrate](https://github.com/agent-substrate/substrate)
object-store plugin backed by Azure Blob Storage.

The plugin code is landing in follow-up pull requests. This repository
currently holds the Go module, its build targets and CI.

## Development

```bash
make build
make test
make verify
make fmt
```

The integration tests use the real Azure Blob SDK and the gRPC services over
a Unix socket, against Azurite. They fail rather than skip when their Azurite
configuration is absent. `make test-integration` runs
`hack/test-azurite.sh`, which starts a pinned Azurite image with Docker on
`127.0.0.1:${AZURITE_PORT:-10000}`, runs every test in the module with the
`integration` tag and `-race`, and removes the container; PR CI runs the same
script.

The test uploads and restores a 320 MiB file with distinct per-block patterns,
retries a partial block upload, verifies corrupt data is rejected, exercises
concurrent uploads, and checks both control RPCs. Another uploads and copies
blocks larger than 32 MiB, which are streamed rather than buffered.

The import-boundary test rejects imports of substrate packages outside `api/`
and `pkg/`.
