#!/usr/bin/env bash
# Build the TARily plugin as an isolated OCI image.
# Run from this directory inside WSL (or any Linux/Docker environment):
#   ./build.sh
set -euo pipefail

IMAGE="weknora-plugin-tarily:latest"

echo "Vendoring dependencies (uses the local WeKnora-fork checkout via go.mod replace)..."
go mod vendor

echo "Building ${IMAGE} ..."
docker build -t "${IMAGE}" .

echo
echo "Done. Image: ${IMAGE}"
echo "Next steps:"
echo "  1. Run the WeKnora host on a Linux/WSL-native filesystem (NOT /mnt/d,"
echo "     unix sockets do not work across the Windows mount)."
echo "  2. Set WEKNORA_PLUGIN_DIR_SEARCH to this plugin's parent dir (Linux path)."
echo "  3. Set trust level 'isolated' for weknora.tarily123."
echo "  4. Trigger a plugin rescan."
