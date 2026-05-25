#!/bin/sh
# init-data.sh
#
# Runs once per container start (s6 oneshot). Responsible for:
#   1. Making sure /data and /data/chatgpt2api exist on the persistent volume.
#   2. Symlinking chatgpt2api's hardcoded ./data dir to /data/chatgpt2api so its
#      accounts.json / images / backup-state / etc. land on the shared volume.
#      (Vendor hardcodes BASE_DIR/data with no env override — we patch via symlink
#       instead of touching their code, per Phase 1 rules.)
#
# Safe to run repeatedly: all operations are idempotent.

set -eu

DATA_ROOT="${USAGE_DATA_DIR:-/data}"
CHATGPT2API_DATA="${CHATGPT2API_DATA_DIR:-${DATA_ROOT}/chatgpt2api}"
APP_DATA_LINK="/opt/chatgpt2api/data"

mkdir -p "${DATA_ROOT}" "${CHATGPT2API_DATA}"

# If a previous image left a real ./data dir (or this is the first boot after an
# upgrade), migrate its contents into the persistent volume before symlinking.
if [ -d "${APP_DATA_LINK}" ] && [ ! -L "${APP_DATA_LINK}" ]; then
    cp -an "${APP_DATA_LINK}/." "${CHATGPT2API_DATA}/" 2>/dev/null || true
    rm -rf "${APP_DATA_LINK}"
fi

ln -sfn "${CHATGPT2API_DATA}" "${APP_DATA_LINK}"

echo "[init-data] /data ready; chatgpt2api data -> ${CHATGPT2API_DATA}"
