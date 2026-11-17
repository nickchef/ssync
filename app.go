package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type App struct {
	Paths       Paths
	Out         io.Writer
	In          *bufio.Reader
	Interactive bool // stdin 是终端，才会提问
	Color       bool
}

func (a *App) c(code, s string) string {
	if !a.Color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
func (a *App) bold(s string) string   { return a.c("1", s) }
func (a *App) green(s string) string  { return a.c("32", s) }
func (a *App) yellow(s string) string { return a.c("33", s) }
func (a *App) red(s string) string    { return a.c("31", s) }
func (a *App) dim(s string) string    { return a.c("2", s) }

func (a *App) printf(format string, args ...any) { fmt.Fprintf(a.Out, format, args...) }

func (a *App) scan(o options) ([]*AgentScan, error) {
	agents, err := selectAgents(a.Paths, o.only, o.skip)
	if err != nil {
		return nil, err
	}
	return scanAll(a.Paths, agents), nil
}

// ---------- agents ----------

func (a *App) Agents() error {
	list, cfg, err := allAgents(a.Paths)
	if err != nil {
		return err
	}
	ignored := map[string]bool{}
	for _, n := range cfg.Ignore {
		ignored[n] = true
	}
	for _, ag := range list {
		state := a.dim("不存在")
		if fi, err := os.Stat(ag.Dir); err == nil && fi.IsDir() {
			state = a.green("存在")
		}
		if ignored[ag.Name] {
			state += a.yellow("（配置里已忽略）")
		}
		a.printf("%-12s %-32s %s %s\n", ag.Name, disp(a.Paths, ag.Dir), padWide(ag.Mode.label(), 10), state)
	}
	a.printf("\n共享目录：%s\n", disp(a.Paths, a.Paths.Central))
	a.printf("配置文件：%s\n", disp(a.Paths, a.Paths.Config))
	return nil
}

// padWide 按终端显示宽度补空格（中文占两格），让中文列对齐。
func padWide(s string, width int) string {
	w := 0
	for _, r := range s {
		if r >= 0x2E80 {
			w += 2
		} else {
			w++
		}
	}
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// ---------- status ----------

func (a *App) Status(o options) error {
	scans, err := a.scan(o)
	if err != nil {
		return err
	}
	a.printf("共享目录 %s：%d 个 skill\n", disp(a.Paths, a.Paths.Central), countSkills(a.Paths.Central))
	pending, oldLinks, pendingDirs := 0, 0, 0
	for _, s := range scans {
		if s.Missing {
			continue
		}
		a.printf("\n%s  %s\n", a.bold(s.Agent.Name), a.dim(disp(a.Paths, s.Agent.Dir)))
		if s.DirLinked {
			a.printf("  %s 整个目录已链接到共享目录\n", a.green("✓"))
			continue
		}
		if s.SkipReason != "" {
			a.printf("  跳过：%s\n", s.SkipReason)
			continue
		}
		var linked, local []string
		for _, l := range s.Links {
			if l.Kind == LinkCentral {
				linked = append(linked, l.Name)
			}
		}
		for _, sd := range s.Skills {
			local = append(local, sd.Name)
		}
		pending += len(local)
		native := s.Agent.Mode == ModeNative
		if native {
			a.printf("  %s\n", a.green("✓ 直接读共享目录"))
			if len(linked) > 0 {
				oldLinks += len(linked)
				a.printf("  %s %s（apply 时移到备份）\n", a.yellow("• 旧链接  "), strings.Join(linked, ", "))
			}
		} else if len(linked) > 0 {
			a.printf("  %s %s\n", a.green("✓ 已链接  "), strings.Join(linked, ", "))
		}
		if len(local) > 0 {
			a.printf("  %s %s\n", a.yellow("• 未迁移  "), strings.Join(local, ", "))
		}
		for _, l := range s.Links {
			switch l.Kind {
			case LinkExternal:
				a.printf("  %s %s → %s\n", a.dim("↗ 外部链接"), l.Name, l.Target)
			case LinkBroken:
				a.printf("  %s %s → %s\n", a.red("✗ 断链    "), l.Name, l.Target)
			}
		}
		for _, pr := range s.Problems {
			a.printf("  %s %s\n", a.red("! 问题    "), pr)
		}
		if !native && len(s.Links) == 0 && len(s.Skills) == 0 && len(s.Problems) == 0 {
			a.printf("  %s\n", a.dim("（空）"))
		}
		if s.Agent.Mode == ModeWhole {
			pendingDirs++
			a.printf("  %s\n", a.dim("apply 后整个目录会换成指向共享目录的软链接"))
		}
	}
	var todo []string
	if pending > 0 {
		todo = append(todo, fmt.Sprintf("%d 个 skill 没迁移", pending))
	}
	if oldLinks > 0 {
		todo = append(todo, fmt.Sprintf("%d 个旧链接要清理", oldLinks))
	}
	if pendingDirs > 0 {
		todo = append(todo, fmt.Sprintf("%d 个目录没换成整目录链接", pendingDirs))
	}
	if len(todo) > 0 {
		a.printf("\n还有 %s，运行 ssync plan 看具体改动。\n", strings.Join(todo, "、"))
	}
	return nil
}

func countSkills(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") && hasSkillFile(filepath.Join(dir, e.Name())) {
			n++
		}
	}
	return n
}

// ---------- plan / apply ----------

func (a *App) Plan(o options) error {
	scans, err := a.scan(o)
	if err != nil {
		return err
	}
	groups := buildGroups(a.Paths, scans)
	pl, err := buildPlan(a.Paths, "<本次运行>", scans, groups, func(*Group, int, int) (int, error) {
		return choicePending, nil
	})
	if err != nil {
		return err
	}
	a.printPlan(scans, pl)
	return nil
}

func (a *App) Apply(o options) error {
	if !a.Interactive && !o.yes {
		return fmt.Errorf("当前不是交互终端，请加 -y 确认执行（有冲突的 skill 会被跳过）")
	}
	unlock, err := acquireLock(a.Paths)
	if err != nil {
		return err
	}
	defer unlock()

	scans, err := a.scan(o)
	if err != nil {
		return err
	}
	groups := buildGroups(a.Paths, scans)
	pl, err := buildPlan(a.Paths, newRunID(a.Paths), scans, groups, a.resolveConflict)
	if err != nil {
		return err
	}
	a.printPlan(scans, pl)
	if len(pl.Steps) == 0 {
		return nil
	}
	if !o.yes && !a.confirm(fmt.Sprintf("\n确认执行以上 %d 项改动？[y/N] ", len(pl.Steps))) {
		return errAbort
	}
	r, err := executePlan(a.Paths, pl)
	if err != nil {
		a.printf("\n%s %v\n", a.red("出错："), err)
		if r != nil && r.doneCount() > 0 {
			a.printf("已完成 %d/%d 个操作。运行 ssync undo 可以把已完成的部分撤回。\n", r.doneCount(), len(r.Ops))
		}
		return fmt.Errorf("迁移没有全部完成")
	}
	a.printf("\n%s 完成 %d 项改动。撤销：ssync undo\n", a.green("✓"), len(pl.Steps))
	if hasBackups(a.Paths, pl) {
		a.printf("被替换下来的副本在 %s\n", disp(a.Paths, a.Paths.backupRoot(pl.RunID)))
	}
	return nil
}

func hasBackups(p Paths, pl *Plan) bool {
	root := p.backupRoot(pl.RunID) + string(filepath.Separator)
	for _, s := range pl.Steps {
		for _, op := range s.Ops {
			if op.Kind == "move" && strings.HasPrefix(op.Dst, root) {
				return true
			}
		}
	}
	return false
}

func (a *App) printPlan(scans []*AgentScan, pl *Plan) {
	var dirs, linked []string
	for _, s := range scans {
		if s.DirLinked {
			linked = append(linked, s.Agent.Name)
		} else if !s.Missing && s.SkipReason == "" {
			dirs = append(dirs, s.Agent.Name)
		}
	}
	if len(dirs) == 0 && len(linked) == 0 {
		a.printf("没有找到任何 Agent 的 skills 目录。\n")
	}
	if len(dirs) > 0 {
		a.printf("扫描：%s\n", strings.Join(dirs, ", "))
	}
	if len(linked) > 0 {
		a.printf("整个目录已链接到共享目录：%s\n", strings.Join(linked, ", "))
	}
	for _, s := range scans {
		if s.SkipReason != "" && !s.DirLinked {
			a.printf("跳过 %s：%s\n", s.Agent.Name, s.SkipReason)
		}
		for _, pr := range s.Problems {
			a.printf("%s %s/%s\n", a.red("!"), s.Agent.Name, pr)
		}
	}

	if len(pl.Steps) == 0 {
		a.printf("\n没有需要迁移的 skill。\n")
	} else {
		a.printf("\n%s\n", a.bold(fmt.Sprintf("将执行 %d 项改动：", len(pl.Steps))))
		for _, st := range pl.Steps {
			verb := st.Verb
			switch verb {
			case "迁移":
				verb = a.green(verb)
			case "合并":
				verb = a.dim(verb)
			default:
				verb = a.yellow(verb)
			}
			a.printf("  %s  %s\n", verb, st.Desc)
		}
	}
	if len(pl.Pending) > 0 {
		a.printf("\n%s\n", a.bold("同名但内容不同（apply 时会逐个问你保留哪个）："))
		for _, s := range pl.Pending {
			a.printf("  %s\n", s)
		}
	}
	if len(pl.Skipped) > 0 {
		a.printf("\n%s\n", a.bold("跳过："))
		for _, s := range pl.Skipped {
			a.printf("  %s\n", s)
		}
	}
	if hasBackups(a.Paths, pl) {
		a.printf("\n被替换下来的副本会移到 %s，不会删除。\n", disp(a.Paths, filepath.Join(a.Paths.State, "backups")))
	}
}

func (a *App) confirm(prompt string) bool {
	if !a.Interactive {
		return false
	}
	a.printf("%s", prompt)
	line, _ := a.In.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func (a *App) resolveConflict(g *Group, i, n int) (int, error) {
	if !a.Interactive {
		return choiceSkip, nil
	}
	a.printf("\n%s\n", a.bold(fmt.Sprintf("冲突 %d/%d：%s 有 %d 个不同版本", i, n, g.Name, len(g.Variants))))
	for vi, v := range g.Variants {
		rep := v.Members[0]
		a.printf("  [%d] %d 个文件，最近修改 %s\n", vi+1, rep.FileCount, rep.ModTime.Local().Format("2006-01-02 15:04"))
		for _, m := range v.Members {
			a.printf("      %-12s %s\n", m.Agent, disp(a.Paths, m.Path))
		}
	}
	diffs := diffSummary(g.Variants)
	a.printf("  不同的文件：\n")
	for k, d := range diffs {
		if k == 15 {
			a.printf("    …还有 %d 个\n", len(diffs)-k)
			break
		}
		a.printf("    %s\n", d)
	}
	for {
		a.printf("保留哪个版本？输入 1-%d；d 看完整差异；s 跳过（都不动）；q 中止：", len(g.Variants))
		line, err := a.In.ReadString('\n')
		line = strings.ToLower(strings.TrimSpace(line))
		if err != nil && line == "" {
			return 0, errAbort
		}
		switch line {
		case "s":
			return choiceSkip, nil
		case "q":
			return 0, errAbort
		case "d":
			a.showDiff(g)
			continue
		}
		if k, err := strconv.Atoi(line); err == nil && k >= 1 && k <= len(g.Variants) {
			return k - 1, nil
		}
		a.printf("没看懂，请重新输入。\n")
	}
}

func (a *App) showDiff(g *Group) {
	base := g.Variants[0].Members[0].Real
	for i := 1; i < len(g.Variants); i++ {
		other := g.Variants[i].Members[0].Real
		a.printf("\n%s\n", a.bold(fmt.Sprintf("===== [1] 和 [%d] 的差异 =====", i+1)))
		cmd := exec.Command("diff", "-ru", "-x", ".DS_Store", "-x", "__pycache__", "-x", ".git", base, other)
		cmd.Stdout = a.Out
		cmd.Stderr = a.Out
		_ = cmd.Run() // 有差异时 diff 返回 1，属正常
	}
	a.printf("\n")
}

// ---------- undo ----------

func (a *App) Undo(o options) error {
	if !a.Interactive && !o.yes {
		return fmt.Errorf("当前不是交互终端，请加 -y 确认撤销")
	}
	unlock, err := acquireLock(a.Paths)
	if err != nil {
		return err
	}
	defer unlock()

	r, err := latestUndoable(a.Paths)
	if err != nil {
		return err
	}
	if r == nil {
		a.printf("没有可以撤销的记录。\n")
		return nil
	}
	links, moves := 0, 0
	for _, op := range r.Ops {
		if !op.Done {
			continue
		}
		if op.Kind == "link" {
			links++
		} else {
			moves++
		}
	}
	a.printf("将撤销 %s 的运行：删除 %d 个软链接，把 %d 个目录移回原处。\n",
		r.Created.Local().Format("2006-01-02 15:04:05"), links, moves)
	if !o.yes && !a.confirm("确认撤销？[y/N] ") {
		return errAbort
	}
	warns, err := undoRun(a.Paths, r)
	for _, w := range warns {
		a.printf("%s %s\n", a.yellow("注意："), w)
	}
	if err != nil {
		return err
	}
	a.printf("%s 已撤销。\n", a.green("✓"))
	return nil
}
