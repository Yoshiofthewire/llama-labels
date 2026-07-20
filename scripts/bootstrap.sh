#!/bin/sh
set -eu

mkdir -p "${CONFIG_DIR:-/kypost/config}" "${LOG_DIR:-/kypost/logs}" "${STATE_DIR:-/kypost/state}"

# users.json is the multi-user store; admin.env is only the legacy seed that
# the backend imports into users.json on first start. Skip if either exists.
if [ ! -f "${CONFIG_DIR:-/kypost/config}/users.json" ] && [ ! -f "${CONFIG_DIR:-/kypost/config}/admin.env" ]; then
  user="${BOOTSTRAP_ADMIN_USER:-admin}"
  # Generate a random, install-specific password when the operator hasn't set
  # one, instead of a fixed publicly-known default. The value is printed once
  # to the logs below and a first-login change is still required.
  pass="${BOOTSTRAP_ADMIN_PASS:-$(node -e 'process.stdout.write(require("crypto").randomBytes(18).toString("base64url"))')}"
  pass_hash="$(PASS="$pass" node -e '
const crypto = require("crypto");
const pass = process.env.PASS || "";
const N = 16384;
const r = 8;
const p = 1;
const keyLen = 32;
const salt = crypto.randomBytes(16);
const hash = crypto.scryptSync(pass, salt, keyLen, { N, r, p, maxmem: 64 * 1024 * 1024 });
process.stdout.write(`scrypt$${N}$${r}$${p}$${salt.toString("base64")}$${hash.toString("base64")}`);
')"
  {
    echo "ADMIN_USER=${user}"
    echo "ADMIN_PASS_HASH=${pass_hash}"
    echo "MUST_CHANGE_PASSWORD=true"
  } >"${CONFIG_DIR:-/kypost/config}/admin.env"
  chmod 600 "${CONFIG_DIR:-/kypost/config}/admin.env"
  echo "Generated first-run admin credentials in config volume"
  echo "Username: ${user}"
  echo "Password: ${pass}"
  echo "Password change is required on first login"
fi
