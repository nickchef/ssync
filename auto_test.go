package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLaunchd 代替真实的 launchctl，只记录调用和是否已加载。
type fakeLaunchd struct {
	loaded bool
	calls  []string
}

func (f *fakeLaunchd) install(t *testing.T, exe string) {
	oldCtl, oldExe := launchctl, executable
	launchctl = func(args ...string) (string, error) {
		f.calls = append(f.calls, args[0])
		switch args[0] {
		case "print":
			if !f.loaded {
				return "Could not find service", errors.New("exit status 113")
			}
		case "bootout":
			if !f.loaded {
				return "No such process", errors.New("exit status 3")
			}
			f.loaded = false
		case "bootstrap":
			if f.loaded {
				return "Bootstrap failed: 5: Input/output error", errors.New("exit status 5")
			}
			f.loaded = true
		}
		return "", nil
	}
	executable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { launchctl, executable = oldCtl, oldExe })
}

func (e *env) fakeExe() string {
	e.t.Helper()
	exe := e.p("bin/ssync & co")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		e.t.Fatal(err)
	}
	return exe
}

func TestAutoOnStatusOff(t *testing.T) {
	e := newEnv(t)
	exe := e.fakeExe()
	ld := &fakeLaunchd{}
	ld.install(t, exe)
	a := e.app("")
	plist := e.p("Library/LaunchAgents/" + autoLabel + ".plist")

	if err := a.AutoStatus(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "未开启") {
		t.Fatalf("还没开启时应显示未开启：\n%s", e.out)
	}

	if err := a.AutoOn("7:30"); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if !ld.loaded {
		t.Fatal("应该已经加载到 launchd")
	}
	job, err := readAutoJob(plist)
	if err != nil {
		t.Fatal(err)
	}
	if job.Exe != exe || job.Time != "07:30" {
		t.Fatalf("plist 读回来是 %+v", job)
	}
	if path, err := exec.LookPath("plutil"); err == nil {
		if out, err := exec.Command(path, "-lint", plist).CombinedOutput(); err != nil {
			t.Fatalf("plutil 认为 plist 不合法：%s", out)
		}
	}

	// 改时间：要先卸掉旧的再加载新的。
	ld.calls = nil
	if err := a.AutoOn("08:05"); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if got := strings.Join(ld.calls, ","); !strings.Contains(got, "bootout") || !ld.loaded {
		t.Fatalf("改时间时应先 bootout 再 bootstrap，实际调用：%s", got)
	}
	if job, _ := readAutoJob(plist); job.Time != "08:05" {
		t.Fatalf("时间没更新：%+v", job)
	}

	e.out.Reset()
	if err := a.AutoStatus(); err != nil {
		t.Fatal(err)
	}
	if s := e.out.String(); !strings.Contains(s, "已开启") || !strings.Contains(s, "08:05") || strings.Contains(s, "不在了") {
		t.Fatalf("状态不对：\n%s", s)
	}

	if err := a.AutoOff(); err != nil {
		t.Fatal(err)
	}
	if ld.loaded {
		t.Fatal("关闭后应从 launchd 卸载")
	}
	e.mustGone("Library/LaunchAgents/" + autoLabel + ".plist")
	e.out.Reset()
	if err := a.AutoOff(); err != nil || !strings.Contains(e.out.String(), "本来就没有开启") {
		t.Fatalf("重复关闭应无害：%v\n%s", err, e.out)
	}
}

func TestAutoOnRejectsBadTime(t *testing.T) {
	e := newEnv(t)
	ld := &fakeLaunchd{}
	ld.install(t, e.fakeExe())
	for _, at := range []string{"25:00", "9", "9:5", "abc"} {
		if err := e.app("").AutoOn(at); err == nil {
			t.Fatalf("--at %s 应该报错", at)
		}
	}
	if ld.loaded || len(ld.calls) > 0 {
		t.Fatal("时间不对时不应该调用 launchctl")
	}
}

func TestAutoOnBootstrapFailureLeavesNoPlist(t *testing.T) {
	e := newEnv(t)
	ld := &fakeLaunchd{}
	ld.install(t, e.fakeExe())
	launchctl = func(args ...string) (string, error) {
		if args[0] == "bootstrap" {
			return "Bootstrap failed: 125: Domain does not support specified action", errors.New("exit status 125")
		}
		return "", errors.New("exit status 113")
	}
	err := e.app("").AutoOn("09:00")
	if err == nil || !strings.Contains(err.Error(), "125") {
		t.Fatalf("应带上 launchctl 的报错：%v", err)
	}
	e.mustGone("Library/LaunchAgents/" + autoLabel + ".plist")
}

func TestAutoStatusWarnsMissingExe(t *testing.T) {
	e := newEnv(t)
	exe := e.fakeExe()
	ld := &fakeLaunchd{}
	ld.install(t, exe)
	a := e.app("")
	if err := a.AutoOn("09:00"); err != nil {
		t.Fatal(err)
	}
	os.Remove(exe)
	ld.loaded = false // 例如在系统设置里被关掉了
	e.out.Reset()
	if err := a.AutoStatus(); err != nil {
		t.Fatal(err)
	}
	if s := e.out.String(); !strings.Contains(s, "不在了") || !strings.Contains(s, "没有加载") {
		t.Fatalf("应提示程序不在了、没有加载：\n%s", s)
	}
}

func TestAutoRunAppliesAndLogs(t *testing.T) {
	e := newEnv(t)
	ld := &fakeLaunchd{}
	ld.install(t, e.fakeExe())
	e.skill(".codex/skills/new", map[string]string{})
	e.skill(".claude/skills/foo", map[string]string{"SKILL.md": "a"})
	e.skill(".cursor/skills/foo", map[string]string{"SKILL.md": "b"})

	a := e.app("")
	a.Interactive = true // 即使在终端里手动运行，也不能停下来提问
	if err := a.AutoRun(); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	e.mustRealDir(".agents/skills/new")
	e.mustGone(".codex/skills/new")
	e.mustRealDir(".claude/skills/foo") // 冲突跳过，原样不动
	e.mustRealDir(".cursor/skills/foo")

	log, _ := os.ReadFile(e.p(".agents/.ssync/auto.log"))
	if !strings.Contains(string(log), "自动整理 =====") || !strings.Contains(string(log), "没有选择，保持原样") {
		t.Fatalf("日志内容不对：\n%s", log)
	}
	if !strings.Contains(e.out.String(), "迁移") {
		t.Fatalf("手动运行时终端也应看到输出：\n%s", e.out)
	}

	// 第二次没有改动；status 只显示最后一次。
	if err := a.AutoRun(); err != nil {
		t.Fatal(err)
	}
	e.out.Reset()
	if err := a.AutoStatus(); err != nil {
		t.Fatal(err)
	}
	s := e.out.String()
	last := s[strings.Index(s, "上次运行"):]
	if strings.Count(s, "自动整理 =====") != 1 || strings.Contains(last, "将执行") {
		t.Fatalf("status 应只显示最后一次运行：\n%s", s)
	}
}

func TestAutoLogRotates(t *testing.T) {
	e := newEnv(t)
	log := e.p(".agents/.ssync/auto.log")
	if err := os.MkdirAll(filepath.Dir(log), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, make([]byte, autoLogMax+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.app("").AutoRun(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(log + ".1"); err != nil || fi.Size() != autoLogMax+1 {
		t.Fatalf("旧日志应转成 auto.log.1：%v", err)
	}
	if fi, _ := os.Stat(log); fi.Size() > 4096 {
		t.Fatalf("新日志应该从头开始，现在 %d 字节", fi.Size())
	}
}

func TestStableExe(t *testing.T) {
	e := newEnv(t)
	cellar := e.p("homebrew/Cellar/ssync/0.3.0/bin/ssync")
	if got := stableExe(cellar); got != cellar {
		t.Fatalf("bin 下没有 ssync 时应保持原路径，得到 %s", got)
	}
	e.symlink("homebrew/bin/ssync", "../Cellar/ssync/0.3.0/bin/ssync")
	os.MkdirAll(filepath.Dir(cellar), 0o755)
	os.WriteFile(cellar, nil, 0o755)
	if got, want := stableExe(cellar), e.p("homebrew/bin/ssync"); got != want {
		t.Fatalf("Cellar 路径应换成 %s，得到 %s", want, got)
	}
	if got := stableExe("/usr/local/bin/ssync"); got != "/usr/local/bin/ssync" {
		t.Fatalf("普通路径不应改动，得到 %s", got)
	}
}
