package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Paths 是 ssync 用到的所有固定位置，都以 home 为根，方便测试时替换。
type Paths struct {
	Home    string
	Central string // 共享目录 ~/.agents/skills
	State   string // ssync 自己的状态：运行记录、备份、锁
	Config  string // 可选配置文件
}

func pathsFor(home string) Paths {
	return Paths{
		Home:    home,
		Central: filepath.Join(home, ".agents", "skills"),
		State:   filepath.Join(home, ".agents", ".ssync"),
		Config:  filepath.Join(home, ".config", "ssync", "config.json"),
	}
}

func (p Paths) backupRoot(runID string) string { return filepath.Join(p.State, "backups", runID) }
func (p Paths) runsDir() string                { return filepath.Join(p.State, "runs") }

type Agent struct {
	Name string
	Dir  string
}

// 内置的用户级 skills 目录（macOS）。目录不存在的 Agent 会被直接忽略。
var builtinAgents = []struct{ name, rel string }{
	{"claude", ".claude/skills"},
	{"codex", ".codex/skills"},
	{"cursor", ".cursor/skills"},
	{"gemini", ".gemini/skills"},
	{"antigravity", ".gemini/config/skills"},
	{"grok", ".grok/skills"},
	{"opencode", ".config/opencode/skills"},
	{"copilot", ".copilot/skills"},
	{"windsurf", ".codeium/windsurf/skills"},
	{"kiro", ".kiro/skills"},
	{"qwen", ".qwen/skills"},
	{"factory", ".factory/skills"},
	{"goose", ".config/goose/skills"},
	{"amp", ".config/amp/skills"},
	{"roo", ".roo/skills"},
	{"cline", ".cline/skills"},
	{"augment", ".augment/skills"},
	{"trae", ".trae/skills"},
	{"openclaw", ".openclaw/skills"},
}

// Config 对应 ~/.config/ssync/config.json，全部字段可选。
//
//	{
//	  "agents": {"myagent": "~/.myagent/skills"},  // 新增或覆盖 Agent 目录
//	  "ignore": ["grok"]                            // 永远跳过的 Agent
//	}
type Config struct {
	Agents map[string]string `json:"agents"`
	Ignore []string          `json:"ignore"`
}

func loadConfig(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("配置文件 %s 格式不对：%w", path, err)
	}
	return c, nil
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	if !filepath.IsAbs(p) {
		return filepath.Join(home, p)
	}
	return filepath.Clean(p)
}

// allAgents 返回内置 + 配置文件里的 Agent（不做过滤）。
func allAgents(p Paths) ([]Agent, Config, error) {
	cfg, err := loadConfig(p.Config)
	if err != nil {
		return nil, cfg, err
	}
	var list []Agent
	idx := map[string]int{}
	for _, b := range builtinAgents {
		idx[b.name] = len(list)
		list = append(list, Agent{Name: b.name, Dir: filepath.Join(p.Home, b.rel)})
	}
	var extra []string
	for name := range cfg.Agents {
		extra = append(extra, name)
	}
	sort.Strings(extra)
	for _, name := range extra {
		dir := expandHome(cfg.Agents[name], p.Home)
		if i, ok := idx[name]; ok {
			list[i].Dir = dir
			continue
		}
		idx[name] = len(list)
		list = append(list, Agent{Name: name, Dir: dir})
	}
	return list, cfg, nil
}

// selectAgents 按配置的 ignore 和命令行的 --only / --skip 过滤。
func selectAgents(p Paths, only, skip []string) ([]Agent, error) {
	list, cfg, err := allAgents(p)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, a := range list {
		known[a.Name] = true
	}
	for _, n := range append(append([]string{}, only...), skip...) {
		if !known[n] {
			return nil, fmt.Errorf("未知的 Agent：%s（用 ssync agents 查看可用名称）", n)
		}
	}
	drop := map[string]bool{}
	for _, n := range cfg.Ignore {
		drop[n] = true
	}
	for _, n := range skip {
		drop[n] = true
	}
	keep := map[string]bool{}
	for _, n := range only {
		keep[n] = true
	}
	var out []Agent
	for _, a := range list {
		if drop[a.Name] {
			continue
		}
		if len(keep) > 0 && !keep[a.Name] {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}
