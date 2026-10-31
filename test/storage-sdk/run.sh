#!/usr/bin/env bash
# Create the venv if needed, then drive Nimbus with the real Azure SDKs.
#
#   ./bin/nimbus start --persist     # terminal 1
#   cd test/storage-sdk && ./run.sh  # terminal 2
set -euo pipefail
cd "$(dirname "$0")"

ACCOUNT="${1:-stnimbusdata}"
VENV="${VENV:-.venv}"

if [ ! -d "$VENV" ]; then
  echo "creating $VENV (needs the network once)"
  python3 -m venv "$VENV"
  "$VENV/bin/pip" install -q --disable-pip-version-check -r requirements.txt
fi

exec "$VENV/bin/python" check.py --account "$ACCOUNT" "${@:2}"
