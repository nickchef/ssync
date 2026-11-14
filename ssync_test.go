package main

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func (e *env) apply(input string) {
	e.t.Helper()
	if err := e.app(input).Apply(options{yes: true}); err != nil {
		e.t.Fatalf("apply 出错：%v\n%s", err, e.out)
	}
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
	e.skill(".claude/skills/foo", map[string]string{"SKILL.md": "foo v1", "ref/a.md": "a"})
	before := e.snapshot()

	e.apply("")

	e.mustRealDir(".agents/skills/foo")
	e.mustFile(".agents/skills/foo/ref/a.md", "a")
	e.mustLink(".claude/skills/foo", ".agents/skills/foo")
	e.mustFile(".claude/skills/foo/SKILL.md", "foo v1") // 透过软链接仍可读

	if err := e.app("").Undo(options{yes: true}); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); !equalMap(got, before) {
		t.Fatalf("撤销后没有还原：\n前 %v\n后 %v", before, got)
	}
}

func TestIdenticalCopiesAreMerged(t *testing.T) {
	e := newEnv(t)
	e.skill(".claude/skills/foo", map[string]string{"SKILL.md": "same"})
	e.skill(".cursor/skills/foo", map[string]string{"SKILL.md": "same", ".DS_Store": "junk"})
	e.skill(".agents/skills/bar", map[string]string{"SKILL.md": "bar"})
	e.skill(".codex/skills/bar", map[string]string{"SKILL.md": "bar"})
	before := e.snapshot()

	e.apply("")

	e.mustLink(".claude/skills/foo", ".agents/skills/foo")
	e.mustLink(".cursor/skills/foo", ".agents/skills/foo")
	e.mustLink(".codex/skills/bar", ".agents/skills/bar")
	e.mustRealDir(".agents/skills/foo")
	e.mustRealDir(".agents/skills/bar")
	got := strings.Join(e.backups(), ",")
	if got != "codex/bar,cursor/foo" {
		t.Fatalf("备份应为 codex/bar,cursor/foo，实际 %s", got)
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
	e.skill(".claude/skills/foo", map[string]string{"SKILL.md": "old"})
	e.skill(".codex/skills/foo", map[string]string{"SKILL.md": "new", "extra.md": "x"})
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
	e.mustLink(".codex/skills/foo", ".agents/skills/foo")
	e.mustLink(".claude/skills/foo", ".agents/skills/foo")
	if got := strings.Join(e.backups(), ","); got != "_central/foo,claude/foo" {
		t.Fatalf("备份应为 _central/foo,claude/foo，实际 %s", got)
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
	e.skill(".codex/skills/foo", map[string]string{"SKILL.md": "codex"})
	e.apply("1\n")
	e.mustFile(".agents/skills/foo/SKILL.md", "central")
	e.mustLink(".codex/skills/foo", ".agents/skills/foo")
	if got := strings.Join(e.backups(), ","); got != "codex/foo" {
		t.Fatalf("备份应为 codex/foo，实际 %s", got)
	}
}

func TestConflictSkipLeavesEverything(t *testing.T) {
	e := newEnv(t)
	e.skill(".claude/skills/foo", map[string]string{"SKILL.md": "a"})
	e.skill(".codex/skills/foo", map[string]string{"SKILL.md": "b"})
	e.skill(".claude/skills/ok", map[string]string{"SKILL.md": "ok"})
	before := e.snapshot()

	e.apply("s\n")

	e.mustLink(".claude/skills/ok", ".agents/skills/ok")
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
	err := e.app("q\n").Apply(options{yes: true})
	if err != errAbort {
		t.Fatalf("期望中止，实际 %v", err)
	}
	if got := e.snapshot(); !equalMap(got, before) {
		t.Fatal("中止后不应有任何改动")
	}
}

func TestNonInteractiveSkipsConflicts(t *testing.T) {
	e := newEnv(t)
	e.skill(".claude/skills/foo", map[string]string{"SKILL.md": "a"})
	e.skill(".codex/skills/foo", map[string]string{"SKILL.md": "b"})
	a := e.app("")
	a.Interactive = false
	if err := a.Apply(options{}); err == nil {
		t.Fatal("非交互且没有 -y 时应拒绝执行")
	}
	if err := a.Apply(options{yes: true}); err != nil {
		t.Fatal(err)
	}
	e.mustRealDir(".claude/skills/foo")
	e.mustRealDir(".codex/skills/foo")
	if !strings.Contains(e.out.String(), "没有选择，保持原样") {
		t.Fatalf("应提示冲突被跳过：\n%s", e.out)
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
	e.symlink(".claude/skills/my-skill", e.p("dev/my-skill"))        // 指向自己的仓库
	e.symlink(".claude/skills/gone", e.p("nowhere"))                 // 断链
	e.skill(".agents/skills/shared", map[string]string{})            //
	e.symlink(".cursor/skills/shared", e.p(".agents/skills/shared")) // 已链接
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
	e.symlink(".cursor/skills/foo", e.p(".claude/skills/foo")) // cursor 借用 claude 的
	e.apply("")
	e.mustFile(".cursor/skills/foo/SKILL.md", "foo")
	e.out.Reset()
	e.app("").Status(options{})
	if strings.Contains(e.out.String(), "外部链接") {
		t.Fatalf("迁移后 cursor/foo 最终指向共享目录，应算已链接：\n%s", e.out)
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
	if !strings.Contains(out, "跳过 cursor：这个目录本身就是共享目录") ||
		!strings.Contains(out, "跳过 grok：和 claude 是同一个目录") {
		t.Fatalf("应跳过 cursor 和 grok：\n%s", out)
	}
	e.mustLink(".claude/skills/bar", ".agents/skills/bar")
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
	e.skill(".claude/skills/foo", map[string]string{})
	e.apply("")
	// 用户自己把软链接换成了别的东西
	os.Remove(e.p(".claude/skills/foo"))
	e.skill(".claude/skills/foo", map[string]string{"SKILL.md": "mine"})
	if err := e.app("").Undo(options{yes: true}); err != nil {
		t.Fatal(err)
	}
	e.mustFile(".claude/skills/foo/SKILL.md", "mine")
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
	e.mustLink(".claude/skills/a", ".agents/skills/a")
	e.mustLink("custom/agent-skills/c", ".agents/skills/c")
	e.mustRealDir(".codex/skills/b")
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
