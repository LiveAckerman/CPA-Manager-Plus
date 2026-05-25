#!/bin/sh
# init-auth-key.sh
#
# Generates a per-container-boot random 64-byte hex key at
#   /run/chatgpt2api_internal_key
# /run is tmpfs, so the key never lands in image layers or volumes, and it
# rotates on every container restart.
#
# Why not just an env var? Anything in the container's main env shows up in
# `docker inspect`. Process-level env set inside an s6 run script does not.
# Both services read this file (chatgpt2api exports it as CHATGPT2API_AUTH_KEY
# right before exec; cpa-manager reads it via Config.ChatGPT2APIInternalKey),
# and `docker exec` only sees the container's outer env — never this secret.
#
# Idempotent: if the file already exists for this boot, leave it alone so the
# two services don't drift if init-auth-key is restarted by s6.

set -eu

KEY_FILE="${CHATGPT2API_INTERNAL_KEY_FILE:-/run/chatgpt2api_internal_key}"

if [ -s "${KEY_FILE}" ]; then
    echo "[init-auth-key] key already present at ${KEY_FILE} (size=$(wc -c <"${KEY_FILE}" | tr -d ' '))"
    exit 0
fi

mkdir -p "$(dirname "${KEY_FILE}")"
# 32 random bytes -> 64 hex chars. /dev/urandom is fine for a per-boot ephemeral
# secret; no need for getrandom() ceremony here.
od -vN 32 -An -tx1 < /dev/urandom | tr -d ' \n' > "${KEY_FILE}"
chmod 0400 "${KEY_FILE}"

echo "[init-auth-key] wrote ${KEY_FILE} (64-char hex)"
