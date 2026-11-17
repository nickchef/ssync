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

// Mode 是 skill 迁到共享目录后，Agent 怎么继续看到它们。
type Mode string

const (
	ModeWhole  Mode = "whole"  // 整个 skills 目录换成指向共享目录的软链接
	ModeNative Mode = "native" // Agent 自己会读 ~/.agents/skills，不建任何链接
	ModeLink   Mode = "link"   // 每个 skill 目录各自换成软链接
)

func (m Mode) label() string {
	switch m {
	case ModeWhole:
		return "整目录链接"
	case ModeNative:
		return "直接读取"
	}
	return "逐个链接"
}

type Agent struct {
	Name string
	Dir  string
	Mode Mode
}

// 内置的用户级 skills 目录（macOS）。目录不存在的 Agent 会被直接忽略。
//
// native：官方文档写明会读 ~/.agents/skills。本机另外核实过：grok 用 grok inspect，
// opencode 用 opencode debug skill（按名字去重），cursor-agent 看了加载代码（始终读
// ~/.agents/skills，按真实路径去重）。
// whole：不读 ~/.agents/skills，只好把整个目录链过去。
// link：还没核实，保守起见逐个链接；确认后可在配置文件 modes 里改成 native。
var builtinAgents = []struct {
	name, rel string
	mode      Mode
}{
	{"claude", ".claude/skills", ModeWhole},
	{"codex", ".codex/skills", ModeNative},
	{"cursor", ".cursor/skills", ModeNative},
	{"gemini", ".gemini/skills", ModeNative},
	{"antigravity", ".gemini/config/skills", ModeWhole},
	{"grok", ".grok/skills", ModeNative},
	{"opencode", ".config/opencode/skills", ModeNative},
	{"copilot", ".copilot/skills", ModeLink},
	{"windsurf", ".codeium/windsurf/skills", ModeLink},
	{"kiro", ".kiro/skills", ModeLink},
	{"qwen", ".qwen/skills", ModeLink},
	{"factory", ".factory/skills", ModeLink},
	{"goose", ".config/goose/skills", ModeLink},
	{"amp", ".config/amp/skills", ModeLink},
	{"roo", ".roo/skills", ModeLink},
	{"cline", ".cline/skills", ModeLink},
	{"augment", ".augment/skills", ModeLink},
	{"trae", ".trae/skills", ModeLink},
	{"openclaw", ".openclaw/skills", ModeLink},
}

// Config 对应 ~/.config/ssync/config.json，全部字段可选。
//
//	{
//	  "agents": {"myagent": "~/.myagent/skills"},  // 新增或覆盖 Agent 目录
//	  "ignore": ["grok"],                           // 永远跳过的 Agent
//	  "modes": {"kiro": "native"}                   // 改链接方式：whole / native / link
//	}
type Config struct {
	Agents map[string]string `json:"agents"`
	Ignore []string          `json:"ignore"`
	Modes  map[string]Mode   `json:"modes"`
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
		list = append(list, Agent{Name: b.name, Dir: filepath.Join(p.Home, b.rel), Mode: b.mode})
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
		list = append(list, Agent{Name: name, Dir: dir, Mode: ModeLink})
	}
	for name, m := range cfg.Modes {
		i, ok := idx[name]
		if !ok {
			return nil, cfg, fmt.Errorf("配置文件 modes 里的 Agent 不存在：%s", name)
		}
		if m != ModeWhole && m != ModeNative && m != ModeLink {
			return nil, cfg, fmt.Errorf("配置文件 modes 里 %s 的值 %q 不对，只能是 whole / native / link", name, m)
		}
		list[i].Mode = m
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
