package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/yangjuncode/fastimpl/files"
)

const workDirPathsHeader = `# fastimpl 项目目录配置（TOML）
# 由 fastimpl-gui 自动保存；未写的项继承程序同目录的全局 fastimpl.toml。

`

// SaveWorkDirPaths 将仓库目录与 worktree 目录持久化到 <work-dir>/fastimpl.toml。
// 它只更新目录覆盖项，保留该文件中已有的其他项目级覆盖，且绝不写入 instance_id；
// 未写入的字段继续继承 exe 同目录的全局 fastimpl.toml。
// 当 repoDir 切换到与原配置不同的仓库且 treeDir 为空时，清除原仓库遗留的 tree_dir。
func SaveWorkDirPaths(workDir, repoDir, treeDir string) (string, error) {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return "", errors.New("work directory is empty")
	}
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve work-dir %s: %w", workDir, err)
	}
	repoDir = strings.TrimSpace(repoDir)
	treeDir = strings.TrimSpace(treeDir)
	if repoDir == "" && treeDir == "" {
		return "", errors.New("repo_dir and tree_dir are both empty")
	}
	if repoDir != "" {
		if !filepath.IsAbs(repoDir) {
			return "", fmt.Errorf("repo_dir must be an absolute path, got %q", repoDir)
		}
		repoDir = filepath.Clean(repoDir)
	}
	if treeDir != "" {
		if !filepath.IsAbs(treeDir) {
			return "", fmt.Errorf("tree_dir must be an absolute path, got %q", treeDir)
		}
		treeDir = filepath.Clean(treeDir)
	}

	path := filepath.Join(absWork, DefaultFile)
	raw, _, err := readLayer(path, false)
	if err != nil {
		return "", err
	}
	// instance_id 只属于全局层，绝不写进项目目录配置。
	raw.InstanceID = nil

	prevRepo := ""
	if raw.RepoDir != nil {
		prevRepo = filepath.Clean(strings.TrimSpace(*raw.RepoDir))
	}
	repoChanged := repoDir != "" && prevRepo != "" && prevRepo != repoDir
	if repoDir != "" {
		r := repoDir
		raw.RepoDir = &r
	}
	if treeDir != "" {
		t := treeDir
		raw.TreeDir = &t
	} else if repoChanged {
		raw.TreeDir = nil
	}

	body, err := toml.Marshal(raw)
	if err != nil {
		return "", fmt.Errorf("encode work-dir paths: %w", err)
	}
	data := append([]byte(workDirPathsHeader), body...)
	if err := files.WriteFileAtomic(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write config %s: %w", path, err)
	}
	return path, nil
}
