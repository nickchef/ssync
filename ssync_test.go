package main

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type env struct {
	t    *testing.T
	home string
	out  *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	// macOS 的临时目录在 /var 下，而 /var 是软链接；解析掉，路径比较更直观。
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, home: home, out: &bytes.Buffer{}}
}

func (e *env) app(input string) *App {
	return &App{
		Paths:       pathsFor(e.home),
		Out:         e.out,
		In:          bufio.NewReader(strings.NewReader(input)),
		Interactive: true,
	}
}

func (e *env) p(rel string) string { return filepath.Join(e.home, rel) }

func (e *env) skill(rel string, files map[string]string) {
	e.t.Helper()
	dir := e.p(rel)
	if _, ok := files["SKILL.md"]; !ok {
		files["SKILL.md"] = "# " + filepath.Base(dir)
	}
	for name, body := range files {
		f := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) symlink(rel, target string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.p(rel)), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Symlink(target, e.p(rel)); err != nil {
		e.t.Fatal(err)
	}
}

// snapshot 记录 home 下（不含 ssync 状态目录）每个文件和软链接，用来比对撤销是否还原。
func (e *env) snapshot() map[string]string {
	e.t.Helper()
	snap := map[string]string{}
	state := e.p(".agents/.ssync")
	filepath.WalkDir(e.home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			e.t.Fatal(err)
		}
		if p == state {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(e.home, p)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			t, _ := os.Readlink(p)
			snap[rel] = "link:" + t
		case d.Type().IsRegular():
			b, _ := os.ReadFile(p)
			snap[rel] = "file:" + string(b)
		}
		return nil
	})
	return snap
}

func (e *env) mustLink(rel, wantTarget string) {
	e.t.Helper()
	got, err := os.Readlink(e.p(rel))
	if err != nil {
		e.t.Fatalf("%s 应该是软链接：%v", rel, err)
	}
	if got != e.p(wantTarget) {
		e.t.Fatalf("%s → %s，期望 → %s", rel, got, e.p(wantTarget))
	}
}

func (e *env) mustGone(rel string) {
	e.t.Helper()
	if _, err := os.Lstat(e.p(rel)); err == nil {
		e.t.Fatalf("%s 应该已经不在了", rel)
	}
}

func (e *env) mustRealDir(rel string) {
	e.t.Helper()
	fi, err := os.Lstat(e.p(rel))
	if err != nil || !fi.IsDir() {
		e.t.Fatalf("%s 应该是真实目录（err=%v）", rel, err)
	}
}

func (e *env) mustFile(rel, body string) {
	e.t.Helper()
	b, err := os.ReadFile(e.p(rel))
	if err != nil {
		e.t.Fatalf("读取 %s：%v", rel, err)
	}
	if string(b) != body {
		e.t.Fatalf("%s 内容是 %q，期望 %q", rel, b, body)
	}
}

// apply 模拟在终端里交互运行：input 先回答冲突，最后自动补一个 y 确认执行。
func (e *env) apply(input string) {
	e.t.Helper()
	if err := e.app(input + "y\n").Apply(options{}); err != nil {
		e.t.Fatalf("apply 出错：%v\n%s", err, e.out)
	}
}

// mtime 把 rel 目录里所有文件的修改时间设成 t。
func (e *env) mtime(rel string, t time.Time) {
	e.t.Helper()
	filepath.WalkDir(e.p(rel), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			e.t.Fatal(err)
		}
		return os.Chtimes(p, t, t)
	})
}

func (e *env) backups() []string {
	var out []string
	root := e.p(".agents/.ssync/backups")
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && hasSkillFile(p) {
			rel, _ := filepath.Rel(root, p)
			parts := strings.SplitN(rel, string(filepath.Separator), 2) // 去掉 runID
			out = append(out, parts[1])
			return filepath.SkipDir
		}
		return nil
	})
	return out
}

func TestMigrateSingleSkill(t *testing.T) {
	e := newEnv(t)
	e.skill(".kiro/skills/foo", map[string]string{"SKILL.md": "foo v1", "ref/a.md": "a"})
	before := e.snapshot()

	e.apply("")

	e.mustRealDir(".agents/skills/foo")
	e.mustFile(".agents/skills/foo/ref/a.md", "a")
	e.mustLink(".kiro/skills/foo", ".agents/skills/foo")
	e.mustFile(".kiro/skills/foo/SKILL.md", "foo v1") // 透过软链接仍可读

	if err := e.app("").Undo(options{yes: true}); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); !equalMap(got, before) {
		t.Fatalf("撤销后没有还原：\n前 %v\n后 %v", before, got)
	}
}

func TestIdenticalCopiesAreMerged(t *testing.T) {
	e := newEnv(t)
	e.skill(".copilot/skills/foo", map[string]string{"SKILL.md": "same"})
	e.skill(".kiro/skills/foo", map[string]string{"SKILL.md": "same", ".DS_Store": "junk"})
	e.skill(".agents/skills/bar", map[string]string{"SKILL.md": "bar"})
	e.skill(".qwen/skills/bar", map[string]string{"SKILL.md": "bar"})
	before := e.snapshot()

	e.apply("")

	e.mustLink(".copilot/skills/foo", ".agents/skills/foo")
	e.mustLink(".kiro/skills/foo", ".agents/skills/foo")
	e.mustLink(".qwen/skills/bar", ".agents/skills/bar")
	e.mustRealDir(".agents/skills/foo")
	e.mustRealDir(".agents/skills/bar")
	got := strings.Join(e.backups(), ",")
	if got != "kiro/foo,qwen/bar" {
		t.Fatalf("备份应为 kiro/foo,qwen/bar，实际 %s", got)
	}

	if err := e.app("").Undo(options{yes: true}); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); !equalMap(got, before) {
		t.Fatalf("撤销后没有还原：\n前 %v\n后 %v", before, got)
	}
	if _, err := os.Stat(e.p(".agents/.ssync/backups")); err == nil {
		entries, _ := os.ReadDir(e.p(".agents/.ssync/backups"))
		if len(entries) != 0 {
			t.Fatalf("撤销后备份目录应清空，实际还有 %d 项", len(entries))
		}
	}
}

func TestConflictChooseNonCentralVersion(t *testing.T) {
	e := newEnv(t)
	e.skill(".agents/skills/foo", map[string]string{"SKILL.md": "old"})
	e.skill(".kiro/skills/foo", map[string]string{"SKILL.md": "old"})
	e.skill(".copilot/skills/foo", map[string]string{"SKILL.md": "new", "extra.md": "x"})
	before := e.snapshot()

	// 先输入一次看不懂的内容，再看 diff，最后选 [2]
	e.apply("zzz\nd\n2\n")

	out := e.out.String()
	for _, want := range []string{"冲突 1/1：foo 有 2 个不同版本", "extra.md  只有 [2] 有", "SKILL.md  内容不同", "没看懂"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出里缺少 %q：\n%s", want, out)
		}
	}
	e.mustFile(".agents/skills/foo/SKILL.md", "new")
	e.mustLink(".copilot/skills/foo", ".agents/skills/foo")
	e.mustLink(".kiro/skills/foo", ".agents/skills/foo")
	if got := strings.Join(e.backups(), ","); got != "_central/foo,kiro/foo" {
		t.Fatalf("备份应为 _central/foo,kiro/foo，实际 %s", got)
	}

	if err := e.app("").Undo(options{yes: true}); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); !equalMap(got, before) {
		t.Fatalf("撤销后没有还原：\n前 %v\n后 %v", before, got)
	}
}

func TestConflictKeepCentral(t *testing.T) {
	e := newEnv(t)
	e.skill(".agents/skills/foo", map[string]string{"SKILL.md": "central"})
	e.skill(".kiro/skills/foo", map[string]string{"SKILL.md": "codex"})
	e.apply("1\n")
	e.mustFile(".agents/skills/foo/SKILL.md", "central")
	e.mustLink(".kiro/skills/foo", ".agents/skills/foo")
	if got := strings.Join(e.backups(), ","); got != "kiro/foo" {
		t.Fatalf("备份应为 kiro/foo，实际 %s", got)
	}
}

func TestConflictSkipLeavesEverything(t *testing.T) {
	e := newEnv(t)
	e.skill(".claude/skills/foo", map[string]string{"SKILL.md": "a"})
	e.skill(".codex/skills/foo", map[string]string{"SKILL.md": "b"})
	e.skill(".claude/skills/ok", map[string]string{"SKILL.md": "ok"})
	before := e.snapshot()

	e.apply("s\n")

	e.mustLink(".claude/skills/ok", ".agents/skills/ok") // 有冲突没解决，claude 退回逐个链接
	if !strings.Contains(e.out.String(), "claude：暂不整目录链接（还有 skill 留在原处）") {
		t.Fatalf("应提示 claude 暂不整目录链接：\n%s", e.out)
	}
	e.mustRealDir(".claude/skills/foo")
	e.mustRealDir(".codex/skills/foo")
	if _, err := os.Lstat(e.p(".agents/skills/foo")); err == nil {
		t.Fatal("跳过的冲突不应出现在共享目录")
	}
	delete(before, ".claude/skills/ok/SKILL.md")
	after := e.snapshot()
	delete(after, ".claude/skills/ok")
	delete(after, ".agents/skills/ok/SKILL.md")
	if !equalMap(after, before) {
		t.Fatalf("跳过的冲突被改动了：\n前 %v\n后 %v", before, after)
	}
}

func TestQuitAbortsWithoutChanges(t *testing.T) {
	e := newEnv(t)
	e.skill(".claude/skills/foo", map[string]string{"SKILL.md": "a"})
	e.skill(".codex/skills/foo", map[string]string{"SKILL.md": "b"})
	e.skill(".claude/skills/ok", map[string]string{"SKILL.md": "ok"})
	before := e.snapshot()
	err := e.app("q\n").Apply(options{})
	if err != errAbort {
		t.Fatalf("期望中止，实际 %v", err)
	}
	if got := e.snapshot(); !equalMap(got, before) {
		t.Fatal("中止后不应有任何改动")
	}
}

func TestNonInteractiveNeedsYes(t *testing.T) {
	e := newEnv(t)
	e.skill(".codex/skills/foo", map[string]string{})
	a := e.app("")
	a.Interactive = false
	if err := a.Apply(options{}); err == nil {
		t.Fatal("非交互且没有 -y 时应拒绝执行")
	}
	e.mustRealDir(".codex/skills/foo")
}

func TestYesKeepsNewestVersion(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.skill(".agents/skills/foo", map[string]string{"SKILL.md": "old"})
	e.skill(".claude/skills/foo", map[string]string{"SKILL.md": "old"})
	e.skill(".kiro/skills/foo", map[string]string{"SKILL.md": "new", "extra.md": "x"})
	e.mtime(".agents/skills/foo", now.Add(-2*time.Hour))
	e.mtime(".claude/skills/foo", now.Add(-3*time.Hour))
	e.mtime(".kiro/skills/foo", now.Add(-time.Hour))

	if err := e.app("").Plan(options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "foo：2 个不同版本，-y 时保留 kiro 的版本") {
		t.Fatalf("plan 应预告 -y 会保留哪个版本：\n%s", e.out)
	}

	// 在终端里加 -y 也不应提问：输入为空，一提问就会因为读到 EOF 而中止。
	e.out.Reset()
	if err := e.app("").Apply(options{yes: true}); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if s := e.out.String(); strings.Contains(s, "保留哪个版本") || !strings.Contains(s, "foo：保留 kiro 的版本") {
		t.Fatalf("应自动保留 kiro 的版本且不提问：\n%s", s)
	}
	e.mustFile(".agents/skills/foo/SKILL.md", "new")
	e.mustFile(".agents/skills/foo/extra.md", "x")
	e.mustLink(".kiro/skills/foo", ".agents/skills/foo")
	e.mustLink(".claude/skills", ".agents/skills") // 冲突都解决了，claude 照常整目录链接
	if got := strings.Join(e.backups(), ","); got != "_central/foo,claude/foo" {
		t.Fatalf("备份应为 _central/foo,claude/foo，实际 %s", got)
	}
}

func TestYesKeepsNewerCentral(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.skill(".agents/skills/foo", map[string]string{"SKILL.md": "central"})
	e.skill(".codex/skills/foo", map[string]string{"SKILL.md": "codex"})
	e.mtime(".agents/skills/foo", now)
	e.mtime(".codex/skills/foo", now.Add(-time.Minute))
	a := e.app("")
	a.Interactive = false
	if err := a.Apply(options{yes: true}); err != nil {
		t.Fatal(err)
	}
	e.mustFile(".agents/skills/foo/SKILL.md", "central")
	e.mustGone(".codex/skills/foo")
	if got := strings.Join(e.backups(), ","); got != "codex/foo" {
		t.Fatalf("备份应为 codex/foo，实际 %s", got)
	}
}

func TestYesSkipsWhenModTimesTie(t *testing.T) {
	e := newEnv(t)
	same := time.Now().Add(-time.Hour)
	e.skill(".claude/skills/foo", map[string]string{"SKILL.md": "a"})
	e.skill(".codex/skills/foo", map[string]string{"SKILL.md": "b"})
	e.mtime(".claude/skills/foo", same)
	e.mtime(".codex/skills/foo", same)
	a := e.app("")
	a.Interactive = false
	if err := a.Apply(options{yes: true}); err != nil {
		t.Fatal(err)
	}
	e.mustRealDir(".claude/skills/foo")
	e.mustRealDir(".codex/skills/foo")
	if !strings.Contains(e.out.String(), "分不出哪个较新，保持原样") {
		t.Fatalf("修改时间相同时应跳过并说明：\n%s", e.out)
	}
}

func TestIgnoresHiddenNonSkillAndExistingLinks(t *testing.T) {
	e := newEnv(t)
	e.skill(".codex/skills/.system/imagegen", map[string]string{})
	e.skill(".codex/skills/.hidden", map[string]string{})
	if err := os.MkdirAll(e.p(".codex/skills/notes"), 0o755); err != nil { // 没有 SKILL.md
		t.Fatal(err)
	}
	e.skill("dev/my-skill", map[string]string{})
	e.symlink(".kiro/skills/my-skill", e.p("dev/my-skill"))           // 指向自己的仓库
	e.symlink(".kiro/skills/gone", e.p("nowhere"))                    // 断链
	e.skill(".agents/skills/shared", map[string]string{})             //
	e.symlink(".copilot/skills/shared", e.p(".agents/skills/shared")) // 已链接
	before := e.snapshot()

	e.apply("")

	if got := e.snapshot(); !equalMap(got, before) {
		t.Fatalf("这些都不该被改动：\n前 %v\n后 %v", before, got)
	}
	if !strings.Contains(e.out.String(), "没有需要迁移的 skill") {
		t.Fatalf("应提示没有需要迁移的：\n%s", e.out)
	}

	e.out.Reset()
	if err := e.app("").Status(options{}); err != nil {
		t.Fatal(err)
	}
	out := e.out.String()
	for _, want := range []string{"已链接   shared", "外部链接 my-skill", "断链     gone"} {
		if !strings.Contains(strings.ReplaceAll(out, "  ", " "), strings.ReplaceAll(want, "  ", " ")) {
			t.Fatalf("status 输出缺少 %q：\n%s", want, out)
		}
	}
}

func TestChainedLinkStillWorksAfterMigration(t *testing.T) {
	e := newEnv(t)
	e.skill(".claude/skills/foo", map[string]string{"SKILL.md": "foo"})
	e.symlink(".kiro/skills/foo", e.p(".claude/skills/foo")) // kiro 借用 claude 的
	e.apply("")
	e.mustFile(".kiro/skills/foo/SKILL.md", "foo")
	e.out.Reset()
	e.app("").Status(options{})
	if strings.Contains(e.out.String(), "外部链接") {
		t.Fatalf("迁移后 kiro/foo 最终指向共享目录，应算已链接：\n%s", e.out)
	}
}

func TestAgentDirPointingAtCentralIsSkipped(t *testing.T) {
	e := newEnv(t)
	e.skill(".agents/skills/foo", map[string]string{})
	e.symlink(".cursor/skills", e.p(".agents/skills"))
	e.skill(".claude/skills/bar", map[string]string{})
	e.symlink(".grok/skills", e.p(".claude/skills")) // 和 claude 共用
	e.apply("")
	out := e.out.String()
	if !strings.Contains(out, "整个目录已链接到共享目录：cursor") ||
		!strings.Contains(out, "跳过 grok：和 claude 是同一个目录") {
		t.Fatalf("应跳过 cursor 和 grok：\n%s", out)
	}
	e.mustLink(".claude/skills", ".agents/skills")
	e.mustRealDir(".agents/skills/bar")
	e.mustFile(".grok/skills/bar/SKILL.md", "# bar") // grok 借道 claude 的目录，仍然能读到
}

func TestSecondApplyIsNoop(t *testing.T) {
	e := newEnv(t)
	e.skill(".claude/skills/foo", map[string]string{})
	e.apply("")
	e.out.Reset()
	e.apply("")
	if !strings.Contains(e.out.String(), "没有需要迁移的 skill") {
		t.Fatalf("第二次应无事可做：\n%s", e.out)
	}
}

func TestUndoIsLayered(t *testing.T) {
	e := newEnv(t)
	e.skill(".claude/skills/foo", map[string]string{})
	s0 := e.snapshot()
	e.apply("")
	e.skill(".codex/skills/foo", map[string]string{}) // 之后又装了一份相同的
	s1 := e.snapshot()
	e.apply("")

	if err := e.app("").Undo(options{yes: true}); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); !equalMap(got, s1) {
		t.Fatalf("第一次撤销应回到第二次 apply 之前：\n期望 %v\n实际 %v", s1, got)
	}
	if err := e.app("").Undo(options{yes: true}); err != nil {
		t.Fatal(err)
	}
	got := e.snapshot()
	delete(got, ".codex/skills/foo/SKILL.md")
	if !equalMap(got, s0) {
		t.Fatalf("第二次撤销应回到最初：\n期望 %v\n实际 %v", s0, got)
	}
	e.out.Reset()
	e.app("").Undo(options{yes: true})
	if !strings.Contains(e.out.String(), "没有可以撤销的记录") {
		t.Fatalf("应无可撤销：\n%s", e.out)
	}
}

func TestUndoDoesNotClobberUserChanges(t *testing.T) {
	e := newEnv(t)
	e.skill(".kiro/skills/foo", map[string]string{})
	e.apply("")
	// 用户自己把软链接换成了别的东西
	os.Remove(e.p(".kiro/skills/foo"))
	e.skill(".kiro/skills/foo", map[string]string{"SKILL.md": "mine"})
	if err := e.app("").Undo(options{yes: true}); err != nil {
		t.Fatal(err)
	}
	e.mustFile(".kiro/skills/foo/SKILL.md", "mine")
	e.mustRealDir(".agents/skills/foo")
	if !strings.Contains(e.out.String(), "已经不是 ssync 创建的软链接") {
		t.Fatalf("应提示跳过：\n%s", e.out)
	}
}

func TestOnlyAndSkipAndConfig(t *testing.T) {
	e := newEnv(t)
	e.skill(".claude/skills/a", map[string]string{})
	e.skill(".codex/skills/b", map[string]string{})
	e.skill("custom/agent-skills/c", map[string]string{})
	os.MkdirAll(e.p(".config/ssync"), 0o755)
	os.WriteFile(e.p(".config/ssync/config.json"), []byte(`{"agents":{"mine":"~/custom/agent-skills"},"ignore":["codex"]}`), 0o644)

	if err := e.app("").Apply(options{only: []string{"nope"}, yes: true}); err == nil {
		t.Fatal("未知 Agent 应报错")
	}
	e.apply("")
	e.mustLink(".claude/skills", ".agents/skills")
	e.mustRealDir(".agents/skills/a")
	e.mustLink("custom/agent-skills/c", ".agents/skills/c")
	e.mustRealDir(".codex/skills/b")
}

func TestWholeDirMigrationAndUndo(t *testing.T) {
	e := newEnv(t)
	e.skill(".claude/skills/foo", map[string]string{"SKILL.md": "foo"})
	e.skill(".claude/skills/bar", map[string]string{"SKILL.md": "bar"})
	e.skill(".codex/skills/bar", map[string]string{"SKILL.md": "bar"})   // 内容相同
	e.skill(".gemini/config/skills/qux", map[string]string{})            // antigravity
	e.skill(".agents/skills/baz", map[string]string{})                   //
	e.symlink(".claude/skills/baz", e.p(".agents/skills/baz"))           // 旧版 ssync 留下的逐个链接
	e.skill("dev/my", map[string]string{"SKILL.md": "mine"})             //
	e.symlink(".claude/skills/my", "../../dev/my")                       // 相对路径的外部链接
	e.symlink(".claude/skills/gone", e.p("nowhere"))                     // 断链
	os.WriteFile(e.p(".claude/skills/notes.txt"), []byte("n"), 0o644)    // 不是 skill
	os.WriteFile(e.p(".claude/skills/.DS_Store"), []byte("junk"), 0o644) // 不算隐藏项
	before := e.snapshot()

	e.out.Reset()
	if err := e.app("").Plan(options{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.out.String(), "~/.claude/skills/foo → ~/.agents/skills/foo，原处换成软链接") {
		t.Fatalf("整目录 Agent 不该逐个建软链接：\n%s", e.out)
	}
	for _, want := range []string{
		"迁移  ~/.claude/skills/foo → ~/.agents/skills/foo\n",
		"合并  ~/.codex/skills/bar（内容相同）移到备份\n",
		"整目录  ~/.claude/skills 移到备份，原处换成指向 ~/.agents/skills 的软链接；一起移到备份的还有：gone（断链）, notes.txt",
		"整目录  ~/.gemini/config/skills 移到备份",
		"外链  ~/.claude/skills/my 在共享目录里重建为 ~/.agents/skills/my → ~/dev/my",
	} {
		if !strings.Contains(e.out.String(), want) {
			t.Fatalf("plan 输出缺少 %q：\n%s", want, e.out)
		}
	}

	e.apply("")

	e.mustLink(".claude/skills", ".agents/skills")
	e.mustLink(".gemini/config/skills", ".agents/skills")
	e.mustGone(".codex/skills/bar") // codex 直接读共享目录，不建链接
	e.mustLink(".agents/skills/my", "dev/my")
	for _, n := range []string{"foo", "bar", "qux", "baz"} {
		e.mustRealDir(".agents/skills/" + n)
	}
	// 透过整目录链接，claude 和 antigravity 都能看到全部 skill
	e.mustFile(".claude/skills/foo/SKILL.md", "foo")
	e.mustFile(".claude/skills/qux/SKILL.md", "# qux")
	e.mustFile(".claude/skills/my/SKILL.md", "mine")
	e.mustFile(".gemini/config/skills/foo/SKILL.md", "foo")
	if got := strings.Join(e.backups(), ","); got != "codex/bar" {
		t.Fatalf("备份应为 codex/bar，实际 %s", got)
	}
	matches, _ := filepath.Glob(e.p(".agents/.ssync/backups/*/_dirs/claude/notes.txt"))
	if len(matches) != 1 {
		t.Fatal("claude 原目录里剩下的内容应在备份里")
	}

	e.out.Reset()
	if err := e.app("").Status(options{}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(e.out.String(), "整个目录已链接到共享目录") != 2 {
		t.Fatalf("status 应显示 claude 和 antigravity 已整目录链接：\n%s", e.out)
	}
	e.out.Reset()
	e.apply("")
	if !strings.Contains(e.out.String(), "没有需要迁移的 skill") {
		t.Fatalf("第二次应无事可做：\n%s", e.out)
	}

	if err := e.app("").Undo(options{yes: true}); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); !equalMap(got, before) {
		t.Fatalf("撤销后没有还原：\n前 %v\n后 %v", before, got)
	}
	e.mustRealDir(".claude/skills")
	e.mustRealDir(".gemini/config/skills")
}

func TestWholeDirConvertsOldPerSkillLinks(t *testing.T) {
	e := newEnv(t)
	e.skill(".agents/skills/foo", map[string]string{})
	e.symlink(".claude/skills/foo", e.p(".agents/skills/foo"))

	e.out.Reset()
	e.app("").Status(options{})
	if !strings.Contains(e.out.String(), "还有 1 个目录没换成整目录链接") {
		t.Fatalf("status 应提示还没整目录链接：\n%s", e.out)
	}
	e.apply("")
	e.mustLink(".claude/skills", ".agents/skills")
	e.mustFile(".claude/skills/foo/SKILL.md", "# foo")
}

func TestEmptyWholeDirIsLinked(t *testing.T) {
	e := newEnv(t)
	os.MkdirAll(e.p(".claude/skills"), 0o755) // 共享目录还不存在
	e.apply("")
	e.mustLink(".claude/skills", ".agents/skills")
	e.mustRealDir(".agents/skills")
	if err := e.app("").Undo(options{yes: true}); err != nil {
		t.Fatal(err)
	}
	e.mustRealDir(".claude/skills")
}

func TestWholeDirFallsBackToPerSkill(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env)
		why   string
	}{
		{"隐藏项", func(e *env) {
			e.skill(".claude/skills/.system/x", map[string]string{})
		}, "里面有隐藏项 .system"},
		{"外部链接和共享目录重名", func(e *env) {
			e.skill("dev/dup", map[string]string{})
			e.symlink(".claude/skills/dup", e.p("dev/dup"))
			e.skill(".agents/skills/dup", map[string]string{"SKILL.md": "other"})
		}, "外部链接 dup 和共享目录里的同名项冲突"},
		{"目录本身是软链接", func(e *env) {
			e.skill("dotfiles/claude-skills/foo", map[string]string{})
			e.symlink(".claude/skills", e.p("dotfiles/claude-skills"))
		}, "目录本身是指向"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			c.setup(e)
			if _, err := os.Lstat(e.p(".claude/skills/foo")); err != nil {
				e.skill(".claude/skills/foo", map[string]string{})
			}
			e.apply("")
			if !strings.Contains(e.out.String(), "claude：暂不整目录链接（"+c.why) {
				t.Fatalf("应提示原因 %q：\n%s", c.why, e.out)
			}
			e.mustLink(".claude/skills/foo", ".agents/skills/foo")
			if fi, _ := os.Lstat(e.p(".claude/skills")); fi.Mode()&fs.ModeSymlink != 0 {
				if t2, _ := os.Readlink(e.p(".claude/skills")); t2 == e.p(".agents/skills") {
					t.Fatal("不该整目录链接")
				}
			}
		})
	}
}

func TestPlanNotesPendingConflictForWholeDir(t *testing.T) {
	e := newEnv(t)
	e.skill(".claude/skills/foo", map[string]string{"SKILL.md": "a"})
	e.skill(".codex/skills/foo", map[string]string{"SKILL.md": "b"})
	if err := e.app("").Plan(options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "（前提是 apply 时冲突都选了版本）") {
		t.Fatalf("plan 应说明整目录链接的前提：\n%s", e.out)
	}
	e.out.Reset()
	e.apply("1\n")
	e.mustLink(".claude/skills", ".agents/skills")
	e.mustFile(".agents/skills/foo/SKILL.md", "a")
	e.mustGone(".codex/skills/foo")
}

func TestNativeAgentsGetNoLinks(t *testing.T) {
	e := newEnv(t)
	e.skill(".codex/skills/foo", map[string]string{"SKILL.md": "foo"})
	e.skill(".grok/skills/foo", map[string]string{"SKILL.md": "foo"}) // 内容相同
	e.skill(".cursor/skills/bar", map[string]string{})
	e.skill(".codex/skills/.system/imagegen", map[string]string{}) // Agent 自己管的，不动
	e.skill(".agents/skills/baz", map[string]string{})
	e.symlink(".cursor/skills/baz", e.p(".agents/skills/baz")) // 旧版 ssync 留下的逐个链接
	e.skill("dev/ext", map[string]string{})
	e.symlink(".cursor/skills/ext", e.p("dev/ext"))  // 指向自己仓库的，不动
	e.skill(".kiro/skills/qux", map[string]string{}) // 没核实的 Agent，仍逐个链接
	before := e.snapshot()

	e.out.Reset()
	e.app("").Status(options{})
	for _, want := range []string{"直接读共享目录", "旧链接   baz（apply 时移到备份）", "还有 4 个 skill 没迁移、1 个旧链接要清理"} {
		if !strings.Contains(e.out.String(), want) {
			t.Fatalf("status 输出缺少 %q：\n%s", want, e.out)
		}
	}

	e.out.Reset()
	e.apply("")
	if !strings.Contains(e.out.String(), "清理  ~/.cursor/skills/baz（指向共享目录的旧链接，cursor 会直接读共享目录）移到备份") {
		t.Fatalf("应清理旧链接：\n%s", e.out)
	}
	e.mustFile(".agents/skills/foo/SKILL.md", "foo")
	e.mustRealDir(".agents/skills/bar")
	e.mustGone(".codex/skills/foo")
	e.mustGone(".grok/skills/foo")
	e.mustGone(".cursor/skills/bar")
	e.mustGone(".cursor/skills/baz")
	e.mustLink(".cursor/skills/ext", "dev/ext")
	e.mustRealDir(".codex/skills/.system/imagegen")
	e.mustLink(".kiro/skills/qux", ".agents/skills/qux")
	if got := strings.Join(e.backups(), ","); got != "grok/foo" {
		t.Fatalf("备份应为 grok/foo，实际 %s", got)
	}

	e.out.Reset()
	e.app("").Status(options{})
	if strings.Contains(e.out.String(), "还有") {
		t.Fatalf("迁移后不该再有待办：\n%s", e.out)
	}

	if err := e.app("").Undo(options{yes: true}); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); !equalMap(got, before) {
		t.Fatalf("撤销后没有还原：\n前 %v\n后 %v", before, got)
	}
}

func TestConfigModes(t *testing.T) {
	e := newEnv(t)
	e.skill(".kiro/skills/a", map[string]string{})
	e.skill(".codex/skills/b", map[string]string{})
	os.MkdirAll(e.p(".config/ssync"), 0o755)
	os.WriteFile(e.p(".config/ssync/config.json"), []byte(`{"modes":{"kiro":"native","codex":"link"}}`), 0o644)
	e.apply("")
	e.mustGone(".kiro/skills/a")
	e.mustRealDir(".agents/skills/a")
	e.mustLink(".codex/skills/b", ".agents/skills/b")

	os.WriteFile(e.p(".config/ssync/config.json"), []byte(`{"modes":{"kiro":"symlink"}}`), 0o644)
	if err := e.app("").Apply(options{yes: true}); err == nil || !strings.Contains(err.Error(), "只能是 whole / native / link") {
		t.Fatalf("非法的 mode 应报错，实际 %v", err)
	}
	os.WriteFile(e.p(".config/ssync/config.json"), []byte(`{"modes":{"nobody":"native"}}`), 0o644)
	if err := e.app("").Apply(options{yes: true}); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("不存在的 Agent 应报错，实际 %v", err)
	}
}

func equalMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
