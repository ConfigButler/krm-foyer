#!/usr/bin/env bash
# Writes krm-stream's single-file browser bundle, from the npm package of the given
# version, to the given file, after checking the package against npm's integrity
# string. No npm needed: the registry serves the package as a tarball.
#
#   test/vendor-krm-stream.sh 0.4.0 sha512-... examples/hello/web/krm-stream.js
set -euo pipefail

version="$1" integrity="$2" out="$3"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL -o "$tmp/package.tgz" "https://registry.npmjs.org/@configbutler/krm-stream/-/krm-stream-$version.tgz"
got="sha512-$(openssl dgst -sha512 -binary "$tmp/package.tgz" | base64 -w0)"
if [ "$got" != "$integrity" ]; then
  echo "krm-stream $version: integrity $got, want $integrity" >&2
  exit 1
fi
tar -xzf "$tmp/package.tgz" -C "$tmp" package/dist/krm-stream.js
cp "$tmp/package/dist/krm-stream.js" "$out"
