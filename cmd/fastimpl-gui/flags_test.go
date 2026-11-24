package main

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"

	"github.com/yangjuncode/fastimpl/app"
)

func TestParseArgumentsIssue(t *testing.T) {
	for _, tc := range []struct {
		args []string
		iid  int
	}{
		{[]string{"42"}, 42},
		{[]string{" 7 "}, 7},
		{[]string{"-j", "3", "9"}, 9},
	} {
		opts, err := parseArguments(tc.args, new(bytes.Buffer))
		if err != nil {
			t.Errorf("parseArguments(%v) = %v, want no error", tc.args, err)
			continue
		}
		if opts.SpecIID != tc.iid {
			t.Errorf("parseArguments(%v).SpecIID = %d, want %d", tc.args, opts.SpecIID, tc.iid)
		}
	}
}

func TestParseArgumentsEmptyIssue(t *testing.T) {
	opts, err := parseArguments(nil, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("parseArguments(nil) = %v, want no error: the GUI opens its empty state", err)
	}
	if opts.SpecIID != 0 {
		t.Errorf("SpecIID = %d, want 0 for an empty state", opts.SpecIID)
	}
}

func TestParseArgumentsRejectsBadIssue(t *testing.T) {
	for _, arg := range []string{"0", "-3", "abc", "1.5", "", "#12"} {
		if _, err := parseArguments([]string{arg}, new(bytes.Buffer)); err == nil {
			t.Errorf("parseArguments(%q) accepted a bad issue", arg)
		}
	}
	if _, err := parseArguments([]string{"1", "2"}, new(bytes.Buffer)); err == nil {
		t.Error("parseArguments accepted two issue numbers")
	}
	if _, err := parseArguments([]string{"-j", "-2", "5"}, new(bytes.Buffer)); err == nil {
		t.Error("parseArguments accepted a negative -j")
	}
}

func TestParseArgumentsFlags(t *testing.T) {
	opts, err := parseArguments([]string{
		"-config", "/tmp/c.toml", "-repo-dir", "/tmp/repo", "-tree-dir", "/tmp/tree",
		"-work-dir", "/tmp/work", "-template", "/tmp/t.md", "-j", "4", "11",
	}, new(bytes.Buffer))
	if err != nil {
		t.Fatal(err)
	}
	want := app.Options{ConfigPath: "/tmp/c.toml", RepoDir: "/tmp/repo", TreeDir: "/tmp/tree",
		WorkDir: "/tmp/work", TemplatePath: "/tmp/t.md", Parallelism: 4, SpecIID: 11}
	if opts != want {
		t.Errorf("options = %+v, want %+v", opts, want)
	}
}

// -h 必须让调用方以 0 退出：它返回 flag.ErrHelp，而不是一般错误。
func TestParseArgumentsHelp(t *testing.T) {
	for _, arg := range []string{"-h", "-help", "--help"} {
		out := new(bytes.Buffer)
		_, err := parseArguments([]string{arg}, out)
		if !errors.Is(err, flag.ErrHelp) {
			t.Errorf("parseArguments(%q) = %v, want flag.ErrHelp", arg, err)
			continue
		}
		if !strings.Contains(out.String(), "fastimpl-gui") {
			t.Errorf("usage for %q = %q, want it to name the command", arg, out.String())
		}
	}
}
