#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$project_root"
bash examples/plugin/session-quota-control/build.sh
exec ./bin/session-quota-control/cli-proxy-api --config config.yaml "$@"
