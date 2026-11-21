# Releasing

Pushing a `v<major>.<minor>.<patch>[-<pre-release>]` tag runs the
[release workflow](.github/workflows/release.yaml). It:

- runs `make verify` and `make test`;
- builds the plugin with ko for linux/amd64 and linux/arm64 and pushes
  `ghcr.io/agent-substrate-plugins/objectstore-azure-blob:<version>`;
- signs the image index and each platform image with keyless cosign, as the
  workflow's GitHub identity;
- attests ko's SPDX SBOM of the index and of each platform image to that image;
- renders the install asset with `make install-asset`;
- creates a GitHub release whose notes give the image digest and the verify
  command, followed by GitHub's generated notes, with the install asset and the
  SBOMs attached. A version with a pre-release suffix, such as `v0.2.0-rc.1`, is
  marked as a pre-release.

## Cutting a release

From a clone with this repository as `upstream`, tag the commit on `main` and
push the tag:

```sh
VERSION=v0.1.0
git fetch upstream
git tag -a "${VERSION}" -m "${VERSION}" upstream/main
git push upstream "${VERSION}"
```

Then follow the run on the repository's Actions tab.

If the run fails, re-run the failed jobs once the cause is fixed outside the
repository, for example a registry outage. A re-run pushes the image again and
adds another signature; ko builds are reproducible, so the digest should not
change. If `gh release create` failed after creating the release, delete the
release, not the tag, before re-running. If the fix needs a code change, do not
move the tag; release the next patch version instead.

After the first release, check that the `objectstore-azure-blob` package on
GHCR is public and linked to this repository: a new organization package can
start out private.

### The install asset

`objectstore-azure-blob-component.yaml` is `config/kustomization.yaml` with
both image fields pinned to the released image's digest. The workflow renders
it with the same targets a contributor can run against any registry:

```sh
KO_DOCKER_REPO=<registry>/objectstore-azure-blob make build-image KO_FLAGS=--bare
make install-asset IMAGE=<the reference build-image printed>
```

`make install-asset` writes `_output/objectstore-azure-blob-component.yaml`
and fails unless both image fields end up pinned by digest.

To use the asset, save it as `kustomization.yaml` in its own directory, replace
the `REPLACE_WITH_*` placeholders as the README's Deployment section describes,
and list that directory under `components:` in your substrate install overlay.
The optional `config/assets` component has no image; take it from the same tag.

## Verifying a release

You need cosign v3 (the workflow signs with v3.1.4), jq and curl. Set:

```sh
VERSION=v0.1.0
REPO=https://github.com/agent-substrate-plugins/objectstore-azure-blob
IMAGE=ghcr.io/agent-substrate-plugins/objectstore-azure-blob
IDENTITY=${REPO}/.github/workflows/release.yaml@refs/tags/${VERSION}
ISSUER=https://token.actions.githubusercontent.com
```

`IDENTITY` and `ISSUER` pin the signing certificate to this repository's
release workflow, run for that tag.

### Image signature

Take the digest from the install asset, so you verify the image you install:

```sh
curl -fsSLO "${REPO}/releases/download/${VERSION}/objectstore-azure-blob-component.yaml"
DIGEST=$(sed -n 's/^ *image: .*@\(sha256:[0-9a-f]\{64\}\)$/\1/p' objectstore-azure-blob-component.yaml | sort -u)
cosign verify \
  --certificate-identity "${IDENTITY}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "${IMAGE}@${DIGEST}"
```

The release notes give the same digest. Each platform image is signed too, so
`cosign verify` also accepts a platform image's digest.

### SBOM

Each `azure-blob-objectstore-plugin-*.spdx.json` asset is ko's SPDX SBOM of
one image: `-index` of the index, the others of one platform image. It lists
the Go modules built into the plugin and the base image by digest, not the base
image's own packages. Its `name` is `sbom-<digest of that image>`.

To check that an SBOM asset is the one the workflow attested to its image:

```sh
SBOM=azure-blob-objectstore-plugin-linux-amd64.spdx.json
curl -fsSLO "${REPO}/releases/download/${VERSION}/${SBOM}"
SUBJECT=$(jq -r '.name | sub("^sbom-"; "")' "${SBOM}")
cosign verify-attestation --type spdxjson \
  --certificate-identity "${IDENTITY}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "${IMAGE}@${SUBJECT}" > attestation.json
jq -r '.payload' attestation.json | head -n 1 | base64 -d | jq -S '.predicate' > attested.spdx.json
jq -S . "${SBOM}" | diff - attested.spdx.json && echo "SBOM matches the attestation"
```

The identity names the workflow file, so renaming `release.yaml` changes the
identity of every later release; update these commands with it.
