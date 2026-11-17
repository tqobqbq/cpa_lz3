#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$project_root"
target_os="$(go env GOOS)"
target_arch="$(go env GOARCH)"
if [[ "$(go env CGO_ENABLED)" != "1" ]]; then
  echo "This plugin requires CGO_ENABLED=1 and a C compiler." >&2
  exit 1
fi
case "$target_os" in
  darwin) extension=dylib ;;
  windows) extension=dll ;;
  *) extension=so ;;
esac
artifact_dir="$project_root/plugins/$target_os/$target_arch"
server_dir="$project_root/bin/session-quota-control"
mkdir -p "$artifact_dir" "$server_dir"
go build -buildmode=c-shared -o "$server_dir/session-quota-control.$extension" ./examples/plugin/session-quota-control
go build -o "$server_dir/cli-proxy-api" ./cmd/server
plugin_copy="$(mktemp "$artifact_dir/.session-quota-control.XXXXXX")"
trap 'rm -f -- "$plugin_copy"' EXIT
cp -p -- "$server_dir/session-quota-control.$extension" "$plugin_copy"
cp -- "$server_dir/session-quota-control.h" "$artifact_dir/session-quota-control.h"
mv -f -- "$plugin_copy" "$artifact_dir/session-quota-control.$extension"
echo "Plugin: $artifact_dir/session-quota-control.$extension"
echo "Plugin copy: $server_dir/session-quota-control.$extension"
echo "Server: $server_dir/cli-proxy-api"
