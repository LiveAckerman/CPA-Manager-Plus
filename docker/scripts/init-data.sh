#!/bin/sh
# init-data.sh — make sure /data and image-service's data subdir exist on
# the persistent volume.
#
# Phase 1/2 also symlinked the vendored chatgpt2api's hardcoded ./data path
# into /data/chatgpt2api. That's gone in the in-project image-service rewrite:
# the new service reads its data dir directly from CHATGPT2API_DATA_DIR
# (default /data/chatgpt-image), so a plain mkdir is enough.
#
# Safe to run repeatedly: all operations are idempotent.

set -eu

DATA_ROOT="${USAGE_DATA_DIR:-/data}"
IMAGE_DATA="${CHATGPT2API_DATA_DIR:-${DATA_ROOT}/chatgpt-image}"

mkdir -p "${DATA_ROOT}" "${IMAGE_DATA}/images"

echo "[init-data] /data ready; image-service data -> ${IMAGE_DATA}"
