package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Op 是一个最小的文件操作，会写进运行记录，用来撤销。
//
//	move：把 Src 目录移到 Dst
//	link：在 Src 位置创建指向 Dst 的软链接
type Op struct {
	Kind string `json:"kind"`
	Src  string `json:"src"`
	Dst  string `json:"dst"`
	Done bool   `json:"done"`
}

// Step 是给人看的一项改动，由若干 Op 组成。
type Step struct {
	Verb string // 迁移 / 合并 / 替换 / 备份
	Desc string
	Ops  []Op
}

type Plan struct {
	RunID   string
	Steps   []Step
	Pending []string // plan 预演时还没选版本的冲突
	Auto    []string // -y 时自动保留了最近修改版本的冲突
	Skipped []string
}

func (pl *Plan) opCount() int {
	n := 0
	for _, s := range pl.Steps {
		n += len(s.Ops)
	}
	return n
}

const (
	choiceSkip    = -1
	choicePending = -2
	choiceTie     = -3 // 自动选择时，几个版本的最近修改时间一样，分不出新旧
)

var errAbort = errors.New("aborted")

// Resolver 决定冲突组保留哪个版本：返回版本下标，或 choiceSkip / choicePending。
type Resolver func(g *Group, i, n int) (int, error)

func buildPlan(p Paths, runID string, scans []*AgentScan, groups []*Group, resolve Resolver) (*Plan, error) {
	pl := &Plan{RunID: runID}
	total := 0
	for _, g := range groups {
		if g.Problem == "" && len(g.Variants) > 1 {
			total++
		}
	}

	// 1. 逐组决定保留哪个版本。
	choices := map[*Group]int{}
	stuck := map[string]bool{}   // 有 skill 会留在原处的 Agent
	waiting := map[string]bool{} // 有冲突要等 apply 时选择的 Agent（只在 plan 预演时出现）
	mark := func(g *Group, m map[string]bool) {
		for _, c := range g.Candidates {
			m[c.Agent] = true
		}
	}
	k := 0
	for _, g := range groups {
		if g.Problem != "" {
			pl.Skipped = append(pl.Skipped, g.Name+"："+g.Problem)
			mark(g, stuck)
			continue
		}
		choice := 0
		if len(g.Variants) > 1 {
			k++
			c, err := resolve(g, k, total)
			if err != nil {
				return nil, err
			}
			switch c {
			case choicePending:
				note := "最近修改时间相同，-y 时会跳过"
				if n := newestVariant(g); n >= 0 {
					note = "-y 时保留" + describeVariant(g.Variants[n])
				}
				pl.Pending = append(pl.Pending, fmt.Sprintf("%s：%d 个不同版本，%s", g.Name, len(g.Variants), note))
				mark(g, waiting)
				continue
			case choiceSkip:
				pl.Skipped = append(pl.Skipped, fmt.Sprintf("%s：%d 个不同版本，没有选择，保持原样", g.Name, len(g.Variants)))
				mark(g, stuck)
				continue
			case choiceTie:
				pl.Skipped = append(pl.Skipped, fmt.Sprintf("%s：%d 个不同版本的最近修改时间相同，分不出哪个较新，保持原样", g.Name, len(g.Variants)))
				mark(g, stuck)
				continue
			}
			choice = c
		}
		choices[g] = choice
	}

	// 2. 看哪些整目录 Agent 这次能换成整目录链接；不能的退回逐个链接。
	taken := map[string]bool{} // 共享目录里可能出现的名字
	for _, g := range groups {
		taken[g.Name] = true
	}
	noLink := map[string]bool{} // 迁移后不逐个建软链接的 Agent
	var dirSteps []Step
	for _, s := range scans {
		if s.Missing || s.SkipReason != "" {
			continue
		}
		if s.Agent.Mode == ModeNative {
			noLink[s.Agent.Name] = true
			dirSteps = append(dirSteps, oldLinkSteps(p, runID, s)...)
			continue
		}
		if s.Agent.Mode != ModeWhole {
			continue
		}
		steps, why := wholeDirSteps(p, runID, s, stuck[s.Agent.Name], taken)
		if why != "" {
			pl.Skipped = append(pl.Skipped, fmt.Sprintf("%s：暂不整目录链接（%s），先逐个链接", s.Agent.Name, why))
			continue
		}
		if waiting[s.Agent.Name] {
			steps[len(steps)-1].Desc += "（前提是 apply 时冲突都选了版本）"
		}
		noLink[s.Agent.Name] = true
		dirSteps = append(dirSteps, steps...)
	}

	// 3. 生成每组的改动；旧链接清理和整目录链接放在最后。
	for _, g := range groups {
		if c, ok := choices[g]; ok {
			pl.Steps = append(pl.Steps, stepsFor(p, runID, g, g.Variants[c], noLink)...)
		}
	}
	pl.Steps = append(pl.Steps, dirSteps...)
	return pl, nil
}

// modTime 是这个版本所有副本里最新的文件修改时间。
func (v *Variant) modTime() time.Time {
	var t time.Time
	for _, m := range v.Members {
		if m.ModTime.After(t) {
			t = m.ModTime
		}
	}
	return t
}

// newestVariant 返回最近修改的版本下标；最新的几个修改时间完全相同时返回 -1。
func newestVariant(g *Group) int {
	best, tie := -1, false
	var bt time.Time
	for i, v := range g.Variants {
		switch t := v.modTime(); {
		case best < 0 || t.After(bt):
			best, bt, tie = i, t, false
		case t.Equal(bt):
			tie = true
		}
	}
	if tie {
		return -1
	}
	return best
}

// newestResolver 是 -y 时的冲突处理：保留最近修改的版本，记进 pl.Auto。
func newestResolver(auto *[]string) Resolver {
	return func(g *Group, _, _ int) (int, error) {
		n := newestVariant(g)
		if n < 0 {
			return choiceTie, nil
		}
		*auto = append(*auto, fmt.Sprintf("%s：保留%s，其余 %d 个版本移到备份", g.Name, describeVariant(g.Variants[n]), len(g.Variants)-1))
		return n, nil
	}
}

// describeVariant 形如「claude、kiro 的版本（最近修改 2026-11-17 09:00:01）」。
func describeVariant(v *Variant) string {
	var owners []string
	for _, m := range v.Members {
		owners = append(owners, m.Agent)
	}
	return fmt.Sprintf(" %s 的版本（最近修改 %s）", strings.Join(owners, "、"), v.modTime().Local().Format("2006-01-02 15:04:05"))
}

// wholeDirSteps 生成把整个 Agent 目录换成共享目录软链接的改动。
// 返回非空原因表示这次做不了。调用前，目录里的 skill 已由各组的改动移走。
func wholeDirSteps(p Paths, runID string, s *AgentScan, stuck bool, taken map[string]bool) ([]Step, string) {
	dir := s.Agent.Dir
	if fi, err := os.Lstat(dir); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		t, _ := os.Readlink(dir)
		return nil, "目录本身是指向 " + t + " 的软链接"
	}
	if stuck || len(s.Problems) > 0 {
		return nil, "还有 skill 留在原处"
	}
	if len(s.Hidden) > 0 {
		return nil, "里面有隐藏项 " + strings.Join(s.Hidden, ", ") + "，可能由 Agent 自己管理"
	}
	var steps []Step
	var leftovers []string
	for _, l := range s.Links {
		switch l.Kind {
		case LinkBroken:
			leftovers = append(leftovers, l.Name+"（断链）")
			continue
		case LinkCentral:
			continue
		}
		// 外部链接（例如指向自己的仓库）挪到共享目录里，整目录链接后照样能用。
		dst := filepath.Join(p.Central, l.Name)
		if _, err := os.Lstat(dst); err == nil {
			if realOrClean(dst) == realOrClean(l.Path) {
				continue // 共享目录里已经有指向同一处的
			}
			return nil, "外部链接 " + l.Name + " 和共享目录里的同名项冲突"
		}
		if taken[l.Name] {
			return nil, "外部链接 " + l.Name + " 和别处的同名 skill 冲突"
		}
		taken[l.Name] = true
		target := l.Target
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		target = filepath.Clean(target)
		steps = append(steps, Step{
			Verb: "外链",
			Desc: fmt.Sprintf("%s 在共享目录里重建为 %s → %s", disp(p, l.Path), disp(p, dst), disp(p, target)),
			Ops:  []Op{{Kind: "link", Src: dst, Dst: target}},
		})
	}
	leftovers = append(leftovers, s.Others...)
	desc := fmt.Sprintf("%s 移到备份，原处换成指向 %s 的软链接", disp(p, dir), disp(p, p.Central))
	if len(leftovers) > 0 {
		desc += "；一起移到备份的还有：" + strings.Join(leftovers, ", ")
	}
	steps = append(steps, Step{
		Verb: "整目录",
		Desc: desc,
		Ops: []Op{
			{Kind: "move", Src: dir, Dst: filepath.Join(p.backupRoot(runID), "_dirs", s.Agent.Name)},
			{Kind: "link", Src: dir, Dst: p.Central},
		},
	})
	return steps, ""
}

// oldLinkSteps 把直接读取共享目录的 Agent 里、指向共享目录的旧软链接移到备份，
// 免得同一个 skill 被它从两条路径各读一次。外部链接和断链不动。
func oldLinkSteps(p Paths, runID string, s *AgentScan) []Step {
	var steps []Step
	for _, l := range s.Links {
		if l.Kind != LinkCentral {
			continue
		}
		steps = append(steps, Step{
			Verb: "清理",
			Desc: fmt.Sprintf("%s（指向共享目录的旧链接，%s 会直接读共享目录）移到备份", disp(p, l.Path), s.Agent.Name),
			Ops:  []Op{{Kind: "move", Src: l.Path, Dst: filepath.Join(p.backupRoot(runID), s.Agent.Name, l.Name)}},
		})
	}
	return steps
}

// stepsFor 生成一组同名 skill 的改动。noLink 里的 Agent（直接读取或稍后整目录链接）不逐个建软链接。
func stepsFor(p Paths, runID string, g *Group, chosen *Variant, noLink map[string]bool) []Step {
	central := filepath.Join(p.Central, g.Name)
	backup := func(owner string) string { return filepath.Join(p.backupRoot(runID), owner, g.Name) }
	linkBack := func(sd *SkillDir) ([]Op, string) {
		if noLink[sd.Agent] {
			return nil, ""
		}
		return []Op{{Kind: "link", Src: sd.Path, Dst: central}}, "，原处换成软链接"
	}
	var steps []Step
	var keeper *SkillDir
	if g.Central == nil || !chosen.has(g.Central) {
		if g.Central != nil {
			steps = append(steps, Step{
				Verb: "备份",
				Desc: fmt.Sprintf("%s（共享目录里未选中的旧版本）移到备份", disp(p, central)),
				Ops:  []Op{{Kind: "move", Src: central, Dst: backup("_central")}},
			})
		}
		keeper = chosen.Members[0]
		link, tail := linkBack(keeper)
		steps = append(steps, Step{
			Verb: "迁移",
			Desc: fmt.Sprintf("%s → %s%s", disp(p, keeper.Path), disp(p, central), tail),
			Ops:  append([]Op{{Kind: "move", Src: keeper.Path, Dst: central}}, link...),
		})
	}
	for _, c := range g.Candidates {
		if c == keeper {
			continue
		}
		verb, why := "合并", "内容相同"
		if !chosen.has(c) {
			verb, why = "替换", "未选中的版本"
		}
		link, tail := linkBack(c)
		steps = append(steps, Step{
			Verb: verb,
			Desc: fmt.Sprintf("%s（%s）移到备份%s", disp(p, c.Path), why, tail),
			Ops:  append([]Op{{Kind: "move", Src: c.Path, Dst: backup(c.Agent)}}, link...),
		})
	}
	return steps
}
