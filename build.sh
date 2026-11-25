#!/usr/bin/env bash
# 构建 fastimpl 与 fastimpl-gui 并安装到 ~/bin/
# 用法：./build.sh [fastimpl|fastimpl-gui|all]（默认 all）
set -euo pipefail

cd "$(dirname "$0")"
mkdir -p "$HOME/bin"

# 版本注入：version.txt 是用户定义的基础版本（例如 0.1.0）；
# 能拿到 git 时追加短提交哈希，显示为 0.1.0+abcdef0。拿不到 hash 时
# 只使用基础版本——build.sh 永远不回退到 dev。直接 go build 不经过
# 本脚本时，二进制内的默认值仍是 dev。
if [ ! -f version.txt ]; then
	echo "version.txt is missing" >&2
	exit 2
fi
base_version="$(head -n 1 version.txt | tr -d '[:space:]')"
if [ -z "$base_version" ]; then
	echo "version.txt must contain a version on its first line" >&2
	exit 2
fi
version="$base_version"
short_hash="$(git rev-parse --short HEAD 2>/dev/null || true)"
if [ -n "$short_hash" ]; then
	version="$base_version+$short_hash"
fi
ldflags="-X github.com/yangjuncode/fastimpl/version.Value=$version"
echo "version: $version"

what="${1:-all}"
case "$what" in
fastimpl | fastimpl-gui | all) ;;
*)
	echo "usage: $0 [fastimpl|fastimpl-gui|all]" >&2
	exit 2
	;;
esac

if [ "$what" = "fastimpl" ] || [ "$what" = "all" ]; then
	go build -ldflags "$ldflags" -o "$HOME/bin/fastimpl" ./cmd/fastimpl
	echo "built: $HOME/bin/fastimpl"
fi

if [ "$what" = "fastimpl-gui" ] || [ "$what" = "all" ]; then
	# GUI 需要 GTK 4.14+（Linux 桌面）与 NSGo 自带的 libghostty-vt
	# （已嵌入二进制，首次运行解到 ~/.cache/nsgo）。缺依赖时构建本身
	# 仍然成功，运行时会给出具体缺失项。
	go build -ldflags "$ldflags" -o "$HOME/bin/fastimpl-gui" ./cmd/fastimpl-gui
	echo "built: $HOME/bin/fastimpl-gui"
fi
