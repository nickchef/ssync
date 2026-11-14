package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const centralLabel = "共享目录"

// SkillDir 是一个真实存在的 skill 目录（不是软链接）。
type SkillDir struct {
	Agent     string // 所属 Agent；共享目录里的为 centralLabel
	Name      string
	Path      string            // 在 Agent 目录（或共享目录）里的路径
	Real      string            // 实际读取内容的路径（共享目录里的软链接会解析到这里）
	Hash      string            // 整个目录内容的摘要
	Files     map[string]string // 相对路径 -> 单个文件摘要
	FileCount int
	ModTime   time.Time // 目录里最新的文件修改时间
}

type LinkKind int

const (
	LinkCentral  LinkKind = iota // 已经指向共享目录
	LinkExternal                 // 指向别处（例如自己的仓库），不动
	LinkBroken                   // 断掉的软链接，不动
)

type LinkEntry struct {
	Name   string
	Path   string
	Target string
	Kind   LinkKind
}

type AgentScan struct {
	Agent      Agent
	Missing    bool   // 目录不存在
	SkipReason string // 整个目录跳过的原因
	Skills     []*SkillDir
	Links      []LinkEntry
	Problems   []string
}

// hashIgnored 里的名字不参与内容比较（但会随目录一起移动）。
var hashIgnored = map[string]bool{".DS_Store": true, "__pycache__": true, ".git": true}

func hasSkillFile(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name(), "SKILL.md") {
			fi, err := os.Stat(filepath.Join(dir, e.Name()))
			if err == nil && fi.Mode().IsRegular() {
				return true
			}
		}
	}
	return false
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashDir 计算目录内容摘要：只看文件相对路径、内容、可执行位和目录内软链接的指向。
func hashDir(root string) (*SkillDir, error) {
	sd := &SkillDir{Real: root, Files: map[string]string{}}
	var lines []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		if hashIgnored[d.Name()] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		var sum string
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			sum = "link:" + t
		case info.Mode().IsRegular():
			s, err := fileSum(p)
			if err != nil {
				return err
			}
			sum = s
			if info.Mode()&0o111 != 0 {
				sum += "+x"
			}
		default:
			return nil // socket、管道之类忽略
		}
		sd.Files[rel] = sum
		sd.FileCount++
		lines = append(lines, rel+"\x00"+sum)
		if info.ModTime().After(sd.ModTime) {
			sd.ModTime = info.ModTime()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		io.WriteString(h, l)
		h.Write([]byte{'\n'})
	}
	sd.Hash = hex.EncodeToString(h.Sum(nil))
	return sd, nil
}

func realOrClean(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func scanAll(p Paths, agents []Agent) []*AgentScan {
	centralReal := realOrClean(p.Central)
	seen := map[string]string{}
	var out []*AgentScan
	for _, a := range agents {
		s := &AgentScan{Agent: a}
		out = append(out, s)
		fi, err := os.Stat(a.Dir)
		if err != nil || !fi.IsDir() {
			s.Missing = true
			continue
		}
		real := realOrClean(a.Dir)
		if real == centralReal {
			s.SkipReason = "这个目录本身就是共享目录"
			continue
		}
		if other, ok := seen[real]; ok {
			s.SkipReason = "和 " + other + " 是同一个目录"
			continue
		}
		seen[real] = a.Name
		scanAgentDir(s, centralReal)
	}
	return out
}

func scanAgentDir(s *AgentScan, centralReal string) {
	entries, err := os.ReadDir(s.Agent.Dir)
	if err != nil {
		s.SkipReason = "读不了目录：" + err.Error()
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") { // 例如 codex 的 .system，由 Agent 自己管理
			continue
		}
		path := filepath.Join(s.Agent.Dir, name)
		fi, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			s.Links = append(s.Links, classifyLink(name, path, centralReal))
			continue
		}
		if !fi.IsDir() || !hasSkillFile(path) {
			continue
		}
		sd, err := hashDir(path)
		if err != nil {
			s.Problems = append(s.Problems, fmt.Sprintf("%s：读取失败（%v），跳过", name, err))
			continue
		}
		sd.Agent, sd.Name, sd.Path = s.Agent.Name, name, path
		s.Skills = append(s.Skills, sd)
	}
}

func classifyLink(name, path, centralReal string) LinkEntry {
	le := LinkEntry{Name: name, Path: path}
	le.Target, _ = os.Readlink(path)
	if _, err := os.Stat(path); err != nil {
		le.Kind = LinkBroken
		return le
	}
	abs := le.Target
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(filepath.Dir(path), abs)
	}
	// 直接指向共享目录，或者经过别的链接最终落在共享目录里，都算已链接。
	if realOrClean(filepath.Dir(filepath.Clean(abs))) == centralReal ||
		filepath.Dir(realOrClean(path)) == centralReal {
		le.Kind = LinkCentral
		return le
	}
	le.Kind = LinkExternal
	return le
}

// Group 是所有 Agent 里同名的 skill。
type Group struct {
	Name       string
	Central    *SkillDir   // 共享目录里已有的同名 skill，没有则为 nil
	Candidates []*SkillDir // 各 Agent 目录里的真实目录
	Variants   []*Variant  // 按内容分组；有共享目录版本时它排第一
	Problem    string      // 非空表示这一组无法自动处理
}

type Variant struct {
	Hash    string
	Members []*SkillDir
}

func (v *Variant) has(sd *SkillDir) bool {
	for _, m := range v.Members {
		if m == sd {
			return true
		}
	}
	return false
}

func buildGroups(p Paths, scans []*AgentScan) []*Group {
	byName := map[string]*Group{}
	var names []string
	for _, s := range scans {
		for _, sd := range s.Skills {
			g, ok := byName[sd.Name]
			if !ok {
				g = &Group{Name: sd.Name}
				byName[sd.Name] = g
				names = append(names, sd.Name)
			}
			g.Candidates = append(g.Candidates, sd)
		}
	}
	sort.Strings(names)
	var groups []*Group
	for _, n := range names {
		g := byName[n]
		inspectCentral(p, g)
		if g.Problem == "" {
			g.Variants = makeVariants(g)
		}
		groups = append(groups, g)
	}
	return groups
}

func inspectCentral(p Paths, g *Group) {
	cp := filepath.Join(p.Central, g.Name)
	fi, err := os.Lstat(cp)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		g.Problem = "读取共享目录里的同名项失败：" + err.Error()
		return
	}
	real := cp
	if fi.Mode()&fs.ModeSymlink != 0 {
		r, err := filepath.EvalSymlinks(cp)
		if err != nil {
			g.Problem = "共享目录里的同名项是断掉的软链接，请先处理 " + disp(p, cp)
			return
		}
		for _, c := range g.Candidates {
			if realOrClean(c.Path) == r {
				g.Problem = fmt.Sprintf("共享目录里的 %s 反过来指向了 %s，请手动处理", disp(p, cp), disp(p, c.Path))
				return
			}
		}
		real = r
	} else if !fi.IsDir() {
		g.Problem = "共享目录里有同名文件 " + disp(p, cp)
		return
	}
	if !hasSkillFile(real) {
		g.Problem = "共享目录里的同名目录没有 SKILL.md：" + disp(p, cp)
		return
	}
	sd, err := hashDir(real)
	if err != nil {
		g.Problem = "读取共享目录里的同名 skill 失败：" + err.Error()
		return
	}
	sd.Agent, sd.Name, sd.Path = centralLabel, g.Name, cp
	g.Central = sd
}

func makeVariants(g *Group) []*Variant {
	var vs []*Variant
	byHash := map[string]*Variant{}
	add := func(sd *SkillDir) {
		v, ok := byHash[sd.Hash]
		if !ok {
			v = &Variant{Hash: sd.Hash}
			byHash[sd.Hash] = v
			vs = append(vs, v)
		}
		v.Members = append(v.Members, sd)
	}
	if g.Central != nil {
		add(g.Central)
	}
	for _, c := range g.Candidates {
		add(c)
	}
	return vs
}

// diffSummary 列出各版本之间不一样的文件。
func diffSummary(vs []*Variant) []string {
	paths := map[string]bool{}
	for _, v := range vs {
		for f := range v.Members[0].Files {
			paths[f] = true
		}
	}
	var sorted []string
	for f := range paths {
		sorted = append(sorted, f)
	}
	sort.Strings(sorted)
	var out []string
	for _, f := range sorted {
		var present []int
		sums := map[string]bool{}
		for i, v := range vs {
			if s, ok := v.Members[0].Files[f]; ok {
				present = append(present, i)
				sums[s] = true
			}
		}
		if len(present) == len(vs) && len(sums) == 1 {
			continue
		}
		desc := "内容不同"
		if len(present) < len(vs) {
			var labels []string
			for _, i := range present {
				labels = append(labels, fmt.Sprintf("[%d]", i+1))
			}
			desc = "只有 " + strings.Join(labels, " ") + " 有"
			if len(sums) > 1 {
				desc += "，且内容不同"
			}
		}
		out = append(out, f+"  "+desc)
	}
	return out
}

func disp(p Paths, path string) string {
	if path == p.Home {
		return "~"
	}
	if strings.HasPrefix(path, p.Home+string(filepath.Separator)) {
		return "~" + path[len(p.Home):]
	}
	return path
}
