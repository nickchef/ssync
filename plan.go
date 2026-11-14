package main

import (
	"errors"
	"fmt"
	"path/filepath"
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
)

var errAbort = errors.New("aborted")

// Resolver 决定冲突组保留哪个版本：返回版本下标，或 choiceSkip / choicePending。
type Resolver func(g *Group, i, n int) (int, error)

func buildPlan(p Paths, runID string, groups []*Group, resolve Resolver) (*Plan, error) {
	pl := &Plan{RunID: runID}
	total := 0
	for _, g := range groups {
		if g.Problem == "" && len(g.Variants) > 1 {
			total++
		}
	}
	k := 0
	for _, g := range groups {
		if g.Problem != "" {
			pl.Skipped = append(pl.Skipped, g.Name+"："+g.Problem)
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
				pl.Pending = append(pl.Pending, fmt.Sprintf("%s：%d 个不同版本", g.Name, len(g.Variants)))
				continue
			case choiceSkip:
				pl.Skipped = append(pl.Skipped, fmt.Sprintf("%s：%d 个不同版本，没有选择，保持原样", g.Name, len(g.Variants)))
				continue
			}
			choice = c
		}
		pl.Steps = append(pl.Steps, stepsFor(p, runID, g, g.Variants[choice])...)
	}
	return pl, nil
}

func stepsFor(p Paths, runID string, g *Group, chosen *Variant) []Step {
	central := filepath.Join(p.Central, g.Name)
	backup := func(owner string) string { return filepath.Join(p.backupRoot(runID), owner, g.Name) }
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
		steps = append(steps, Step{
			Verb: "迁移",
			Desc: fmt.Sprintf("%s → %s，原处换成软链接", disp(p, keeper.Path), disp(p, central)),
			Ops: []Op{
				{Kind: "move", Src: keeper.Path, Dst: central},
				{Kind: "link", Src: keeper.Path, Dst: central},
			},
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
		steps = append(steps, Step{
			Verb: verb,
			Desc: fmt.Sprintf("%s（%s）移到备份，原处换成软链接", disp(p, c.Path), why),
			Ops: []Op{
				{Kind: "move", Src: c.Path, Dst: backup(c.Agent)},
				{Kind: "link", Src: c.Path, Dst: central},
			},
		})
	}
	return steps
}
