# Azure Blob object-store plugin

An [Agent Substrate](https://github.com/agent-substrate/substrate) object-store
plugin that keeps actor snapshots and sandbox assets in Azure Blob Storage. It
replaces the `objectstore-plugin` sidecars next to atelet and ate-api-server
and authenticates only with Microsoft Entra workload identity.

## Compatibility

The plugin serves the `substrate.objectstore.v1` gRPC services that substrate
v1.0.0 calls. `go.mod` pins the substrate commit the plugin is built and tested
against; v1.0.0 contains it. Substrate v0.5.0 and v0.6.0 call `objectstore.v1`
and get `Unimplemented`; earlier releases call no object-store plugin.

## How it works

| Mode | Runs beside | Serves |
|---|---|---|
| `control` | ate-api-server | `ControlProvider`: copy and clean up snapshots |
| `node` | atelet | `NodeProvider`: upload and fetch snapshots; `AssetProvider` when `--asset-containers` is set |
| `healthcheck` | | Exits 0 if the plugin at `--socket` reports `SERVING` within `--timeout` (default `5s`) |

- Snapshot URIs are `azblob://<container>/<path>`, with `<container>` in
  `--containers`. `--prefix` is prepended to `<path>`.
- Each snapshot file is one block blob with its SHA-256 in metadata, which
  fetch and copy verify.
- Asset URLs are blob URLs under `--account-url`, such as
  `https://<account>.blob.core.windows.net/<container>/<blob>`, with
  `<container>` in `--asset-containers`. `--prefix` does not apply. Blobs under
  a snapshot or tag location are refused.

## Configuration

| Flag | Environment fallback | Default | Meaning |
|---|---|---|---|
| `--socket` | | required | Unix socket to serve |
| `--account-url` | `AZURE_STORAGE_ACCOUNT_URL` | required | Account endpoint, such as `https://<account>.blob.core.windows.net`; must be https; requests and tokens go only to this host, without redirects |
| `--containers` | `AZURE_STORAGE_CONTAINERS` | required | Comma-separated snapshot containers |
| `--prefix` | `AZURE_STORAGE_PREFIX` | empty | Blob name prefix for all snapshots |
| `--root` | | `/var/lib/ate` | Node mode: local directory that snapshot and asset paths must be under |
| `--asset-containers` | `AZURE_STORAGE_ASSET_CONTAINERS` | empty | Node mode: comma-separated asset containers; empty serves no assets |
| `--asset-staging-dir` | | `/var/lib/objectstore-plugin/asset-staging` | Node mode: private download directory, outside `--root` |
| `--asset-staging-capacity` | | `8Gi` | Node mode: total size of asset downloads in progress |

The plugin also reads `AZURE_CLIENT_ID`, `AZURE_TENANT_ID` and
`AZURE_FEDERATED_TOKEN_FILE`, and exits at startup if any is unset or the token
file is unreadable. It accepts no account keys, SAS tokens or client secrets.

## Deployment

### 1. Build the image

No release images are published yet. Build and push one with
[ko](https://ko.build); the last line it prints is the image reference:

```bash
KO_DOCKER_REPO=<registry> make build-image
```

### 2. Set up Azure

Create two user-assigned managed identities, each with a federated credential
for the cluster's service account issuer, audience `api://AzureADTokenExchange`.
Microsoft Entra ID must be able to fetch the issuer's OIDC discovery document.

| Identity | Federated credential subject | Roles |
|---|---|---|
| Control | `system:serviceaccount:ate-system:ate-api-server` | `Storage Blob Data Contributor` on each snapshot container |
| Node | `system:serviceaccount:ate-system:atelet` | `Storage Blob Data Contributor` on each snapshot container; `Storage Blob Data Reader` on each asset container that is not a snapshot container |

### 3. Fill in the components

[`config/`](config/kustomization.yaml) sets both sidecars' image, arguments,
workload identity and startup probe;
[`config/assets/`](config/assets/kustomization.yaml) adds asset serving to the
node sidecar. Replace the placeholders:

| Placeholder | Value |
|---|---|
| `example.invalid/replace-with-azure-blob-objectstore-plugin:latest` | Image reference from step 1 |
| `REPLACE_WITH_ACCOUNT` | Storage account name |
| `REPLACE_WITH_CONTAINERS` | Comma-separated snapshot containers |
| `REPLACE_WITH_TENANT_ID` | Entra tenant ID |
| `REPLACE_WITH_CONTROL_CLIENT_ID` | Control identity client ID |
| `REPLACE_WITH_NODE_CLIENT_ID` | Node identity client ID |
| `REPLACE_WITH_ASSET_CONTAINERS` (in `config/assets/`) | Comma-separated asset containers |

Keep `--containers` and `--prefix` (`substrate` in `config/`) the same in both
sidecars: the control sidecar copies and deletes what the node sidecar writes.

### 4. Install substrate

ate-setup has no option to add a component. Add the components, by path
relative to the overlay, to the overlay `ate-setup deploy ate-system` renders
in your substrate checkout: `manifests/ate-install/base`, or
`manifests/ate-install/kind` with `--kind`. With this repository cloned next
to substrate:

```yaml
components:
- ../../../../objectstore-azure-blob/config
- ../../../../objectstore-azure-blob/config/assets  # only to serve assets
```

Then run `ate-setup deploy ate-system` as its
[commands guide](https://github.com/agent-substrate/substrate/blob/v1.0.0/cmd/ate-setup/commands.md#deploy)
describes, with:

- `--snapshot-location=azblob://<container>/<path>/`, with `<container>` in
  `--containers` and path segments of only ASCII letters, digits, `_` and `-`
  (see [snapshot storage](https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/guides/operator-install.md#snapshot-storage)).
- `--sandbox-classes=gvisor`: the default also installs a microVM
  `SandboxConfig` whose `gs://` or `s3://` asset URLs this plugin cannot read.
- Without `--kind`, ate-setup v1.0.0 also requires
  `--snapshot-transfer-gsa-name` and `--gcp-project-id`, which the plugin
  does not use. Also pass `--context=<kubeconfig context>`, or ate-setup
  tries to fetch GKE credentials for that project.

Only `deploy ate-system` renders the overlay; `deploy atelet` and
`deploy apiserver` put the default sidecars back. To serve an asset with the
plugin, set its `url` in a `SandboxConfig` to the blob URL and its `sha256` to
that blob's SHA-256; atelet passes the configured digest to the plugin, which
refuses a blob that does not match it.

## Error codes

| Condition | Snapshot RPCs | `FetchAsset` |
|---|---|---|
| Invalid URI, local path or file list; container not configured | `InvalidArgument` | `InvalidArgument` |
| File too large for one block blob | `InvalidArgument` | |
| Blob absent on fetch | `NotFound` | `NotFound` |
| Configured container absent in Azure (cleanup succeeds) | `FailedPrecondition` | `NotFound` |
| Empty copy source | `FailedPrecondition` | |
| Asset larger than `max_bytes` or `--asset-staging-capacity`; SHA-256 mismatch; `write_path` missing or not a regular file | | `FailedPrecondition` |
| Asset under a snapshot or tag location | | `PermissionDenied` |
| Azure Storage returns 401 or 403 | `PermissionDenied` | `Internal` |
| Throttling, timeout, 5xx, connection failure or stall after retries; blob changed mid-download | `Unavailable` | `Unavailable` |
| Blob SHA-256 metadata missing or mismatched | `DataLoss` | |
| Context canceled or deadline exceeded | `Canceled`, `DeadlineExceeded` | same |
| Anything else, including Entra rejecting the token request | `Internal` | `Internal` |

## Limitations

- No per-actor authorization: the plugin ignores the actor JWT, so the node
  identity can read and write every snapshot in its containers.
- One storage account per process; no copies across accounts.
- A failed upload can leave uncommitted blocks; Azure deletes them after
  seven days.
- Native Azure blob snapshots block deletion: `CleanupSnapshot` fails with
  `Internal` until you remove them.
- A file is limited to Azure's block blob maximum, 50,000 blocks of 4,000 MiB.
- Each `CopySnapshot` holds up to 32 MiB of block data in memory; concurrent
  copies add up.

## Development

```bash
make build             # go build ./...
make test              # unit tests with -race
make verify            # gofmt, go vet with and without the integration tag, go mod tidy
make fmt               # gofmt -w
make test-integration  # integration tests against Azurite; needs Docker
```

[`hack/test-azurite.sh`](hack/test-azurite.sh) starts a pinned Azurite image on
`127.0.0.1:${AZURITE_PORT:-10000}`, runs every test with the `integration` tag
and `-race` (arguments replace the `go test` command), and removes the
container. Integration tests fail, not skip, without Azurite. PR CI runs
`make verify`, `make test` and `make test-integration`.
