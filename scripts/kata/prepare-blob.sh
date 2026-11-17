#!/usr/bin/env bash
# Builds the trimmed Kata blob for packages/kata from an upstream kata-static release and adds it to the
# release (bosh add-blob). Keeps only what garden's VM mode with Cloud Hypervisor + runtime-rs needs.
#
#   scripts/kata/prepare-blob.sh 4.3.1 1463a2b86845f4d3d8e87f2f764ec59856fc0fee3520967a79ade32762be2e97 [tarball]
set -euo pipefail

VERSION=${1:?kata version, e.g. 4.3.1}
SHA256=${2:?sha256 of kata-static-<version>-amd64.tar.zst (from the GitHub release)}
TARBALL=${3:-}
RELEASE_DIR=$(cd "$(dirname "$0")/../.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

if [ -z "${TARBALL}" ]; then
  TARBALL="${WORK}/kata-static-${VERSION}-amd64.tar.zst"
  curl -fsSL -o "${TARBALL}" \
    "https://github.com/kata-containers/kata-containers/releases/download/${VERSION}/kata-static-${VERSION}-amd64.tar.zst"
fi
echo "${SHA256}  ${TARBALL}" | shasum -a 256 -c -

KEEP=(
  ./opt/kata/runtime-rs/bin/containerd-shim-kata-v2
  ./opt/kata/bin/cloud-hypervisor
  ./opt/kata/libexec/virtiofsd
  ./opt/kata/share/defaults/kata-containers/runtime-rs/configuration-clh-runtime-rs.toml
  ./opt/kata/share/kata-containers/vmlinux.container
  ./opt/kata/share/kata-containers/kata-containers.img
)
mkdir -p "${WORK}/root"
tar -xf "${TARBALL}" -C "${WORK}/root" "${KEEP[@]}"
# vmlinux.container and kata-containers.img are symlinks: add their targets
for link in vmlinux.container kata-containers.img; do
  target=$(readlink "${WORK}/root/opt/kata/share/kata-containers/${link}")
  tar -xf "${TARBALL}" -C "${WORK}/root" "./opt/kata/share/kata-containers/${target}"
done
echo "${VERSION}" > "${WORK}/root/VERSION"

BLOB="${WORK}/kata-clh-${VERSION}-amd64.tar.gz"
tar -czf "${BLOB}" -C "${WORK}/root" VERSION opt
ls -lh "${BLOB}"

cd "${RELEASE_DIR}"
for old in $(bosh blobs 2>/dev/null | awk '/^kata\/kata-clh-/ {print $1}'); do
  bosh remove-blob "${old}"
done
bosh add-blob "${BLOB}" "kata/kata-clh-${VERSION}-amd64.tar.gz"
