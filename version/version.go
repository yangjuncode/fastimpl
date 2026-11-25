// Package version 保存 fastimpl 当前构建版本。默认值供直接 go build
// 使用；正式构建脚本通过 -ldflags -X 注入 version.txt 与短 git hash
// 组成的版本。
package version

// Value 是当前构建版本。正式构建时由 -ldflags 覆盖。
var Value = "dev"

// String 返回可展示版本；空值仍回落到 dev，避免界面出现空白版本。
func String() string {
	if Value == "" {
		return "dev"
	}
	return Value
}
