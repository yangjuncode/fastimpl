package worktree

import "testing"

// TestForgeForHost 验证按主机名推断 forge。
func TestForgeForHost(t *testing.T) {
	cases := map[string]string{
		"github.com":         "github",
		"api.github.com":     "github",
		"gitlab.com":         "gitlab",
		"gitlab.corp.com":    "gitlab",
		"git.bfdx.com":       "", // 自建实例推断不出，走默认 gitlab
		"bitbucket.org":      "",
		"GITLAB.COM":         "gitlab",
		"Gitee.COM":          "",
		"mygithub.mirror":    "", // 非 .github.com 后缀不算
		"github.com.evil.io": "",
	}
	for host, want := range cases {
		if got := ForgeForHost(host); got != want {
			t.Errorf("ForgeForHost(%q) = %q, want %q", host, got, want)
		}
	}
}
