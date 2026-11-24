#!/usr/bin/env bash
# 构建 fastimpl 与 fastimpl-gui 并安装到 ~/bin/
# 用法：./build.sh [fastimpl|fastimpl-gui|all]（默认 all）
set -euo pipefail

cd "$(dirname "$0")"
mkdir -p "$HOME/bin"

what="${1:-all}"
case "$what" in
fastimpl | fastimpl-gui | all) ;;
*)
	echo "usage: $0 [fastimpl|fastimpl-gui|all]" >&2
	exit 2
	;;
esac

if [ "$what" = "fastimpl" ] || [ "$what" = "all" ]; then
	go build -o "$HOME/bin/fastimpl" ./cmd/fastimpl
	echo "built: $HOME/bin/fastimpl"
fi

if [ "$what" = "fastimpl-gui" ] || [ "$what" = "all" ]; then
	# GUI 需要 GTK 4.14+（Linux 桌面）与 NSGo 自带的 libghostty-vt
	# （已嵌入二进制，首次运行解到 ~/.cache/nsgo）。缺依赖时构建本身
	# 仍然成功，运行时会给出具体缺失项。
	go build -o "$HOME/bin/fastimpl-gui" ./cmd/fastimpl-gui
	echo "built: $HOME/bin/fastimpl-gui"
fi
