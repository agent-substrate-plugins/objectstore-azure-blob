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

# Renders the release install asset: config/kustomization.yaml with both
# sidecar image fields pinned to an image digest. The release workflow runs it
# through `make install-asset`.
#
# Usage: hack/render-install-asset.sh <image>@sha256:<digest> <output file>
#
# A tag in the reference (as `make build-image` prints) is dropped, so the
# asset pins the image by digest only.
set -euo pipefail

if [[ $# -ne 2 ]]; then
	echo "usage: $0 <image>@sha256:<digest> <output file>" >&2
	exit 2
fi
ref=$1
out=$2

# <output file> is relative to the working directory, not the repository.
src_name=config/kustomization.yaml
src="$(dirname "${BASH_SOURCE[0]}")/../$src_name"
placeholder=example.invalid/replace-with-azure-blob-objectstore-plugin:latest
# Both sidecars, ate-api-server's and atelet's, run the plugin image.
want_images=2

# <name>[:<tag>]@sha256:<64 hex>, where <name> may carry a registry port.
if [[ ! $ref =~ ^([a-z0-9][a-z0-9._:-]*(/[a-z0-9][a-z0-9._-]*)+)(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@(sha256:[0-9a-f]{64})$ ]]; then
	echo "$0: image must be <name>@sha256:<64 hex digest>, got: $ref" >&2
	exit 1
fi
pinned="${BASH_REMATCH[1]}@${BASH_REMATCH[4]}"

got=$(grep -c -F "image: $placeholder" "$src" || true)
if [[ $got -ne $want_images ]]; then
	echo "$0: want $want_images '$placeholder' image fields in $src_name, found $got" >&2
	exit 1
fi

mkdir -p "$(dirname "$out")"
tmp=$(mktemp "$out.XXXXXX")
trap 'rm -f "$tmp"' EXIT

{
	echo "# Rendered from $src_name by hack/render-install-asset.sh, with the plugin"
	echo "# image pinned to $pinned."
	echo "# Save it as kustomization.yaml in its own directory and replace the"
	echo "# REPLACE_WITH_* placeholders before listing it as a component."
	sed "s|image: $placeholder\$|image: $pinned|" "$src"
} >"$tmp"

# Every image field must now be the pinned reference, and only that.
if grep -q -F "$placeholder" "$tmp"; then
	echo "$0: $out still references $placeholder" >&2
	exit 1
fi
images=$(grep -c -E '^[[:space:]]*image:' "$tmp" || true)
pinned_images=$(grep -c -F "image: $pinned" "$tmp" || true)
if [[ $images -ne $want_images || $pinned_images -ne $want_images ]]; then
	echo "$0: $out must have exactly $want_images image fields, all $pinned" >&2
	exit 1
fi

chmod 0644 "$tmp"
mv "$tmp" "$out"
trap - EXIT
echo "$out"
