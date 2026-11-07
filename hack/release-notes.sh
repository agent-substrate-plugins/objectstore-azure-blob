#!/usr/bin/env bash
# Copyright 2026 The Agent Substrate Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Prints the release notes the release workflow puts above GitHub's generated
# notes: the image digest, the command that verifies its signature, and the
# release assets.
#
# Usage: hack/release-notes.sh <image>@sha256:<digest> <version> <identity>
#
# <identity> is the signing certificate's identity, the release workflow's
# https://github.com/<owner>/<repo>/.github/workflows/<file>@refs/tags/<version>.
set -euo pipefail

if [[ $# -ne 3 ]]; then
	echo "usage: $0 <image>@sha256:<digest> <version> <identity>" >&2
	exit 2
fi
image_ref=$1
version=$2
identity=$3

if [[ ! $image_ref =~ ^([a-z0-9][a-z0-9._:-]*(/[a-z0-9][a-z0-9._-]*)+)@sha256:[0-9a-f]{64}$ ]]; then
	echo "$0: image must be <name>@sha256:<64 hex digest>, got: $image_ref" >&2
	exit 1
fi
image=${BASH_REMATCH[1]}
if [[ ! $identity =~ ^https://[^/]+/[^/]+/[^/]+/\.github/workflows/[^@]+@refs/tags/ ]]; then
	echo "$0: identity must be a release workflow's https://<host>/<owner>/<repo>/.github/workflows/<file>@refs/tags/<version>, got: $identity" >&2
	exit 1
fi
issuer=https://token.actions.githubusercontent.com
repo_url=${identity%%/.github/workflows/*}

notes=$(
	cat <<'EOF'
## Image

`@IMAGE@:@VERSION@`, for linux/amd64 and linux/arm64, is:

```text
@IMAGE_REF@
```

Verify its signature:

```sh
cosign verify \
  --certificate-identity @IDENTITY@ \
  --certificate-oidc-issuer @ISSUER@ \
  @IMAGE_REF@
```

## Assets

- `objectstore-azure-blob-component.yaml`: `config/kustomization.yaml` with the
  image pinned to the digest above. Save it as `kustomization.yaml` in its own
  directory, replace the `REPLACE_WITH_*` placeholders, and list that directory
  under `components:` in your substrate install overlay.
- `azure-blob-objectstore-plugin-*.spdx.json`: SPDX SBOMs of the image index
  and of each platform image, also attested to those images with cosign.

[RELEASE.md](@REPO_URL@/blob/@VERSION@/RELEASE.md) shows how to verify the
SBOM attestations.
EOF
)
# Quoted replacements are literal, whatever they contain.
notes=${notes//@IMAGE_REF@/"$image_ref"}
notes=${notes//@IMAGE@/"$image"}
notes=${notes//@VERSION@/"$version"}
notes=${notes//@IDENTITY@/"$identity"}
notes=${notes//@ISSUER@/"$issuer"}
notes=${notes//@REPO_URL@/"$repo_url"}
printf '%s\n' "$notes"
