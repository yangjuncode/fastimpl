#!/usr/bin/env bash
# 构建 fastimpl 并安装到 ~/bin/
set -euo pipefail

cd "$(dirname "$0")"
mkdir -p "$HOME/bin"
go build -o "$HOME/bin/fastimpl" ./cmd/fastimpl
echo "built: $HOME/bin/fastimpl"
