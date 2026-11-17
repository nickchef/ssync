package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 每天自动整理：在 ~/Library/LaunchAgents 放一个 launchd 任务，每天定时运行
// `ssync auto run`，相当于 `ssync apply -y`，输出追加到 ~/.agents/.ssync/auto.log。

const (
	autoLabel       = "io.github.nickchef.ssync"
	autoDefaultTime = "09:00"
	autoLogMax      = 1 << 20 // 日志超过 1 MiB 就转成 auto.log.1，只留一份旧的
	autoLogHeader   = "===== "
)

func (p Paths) launchAgent() string {
	return filepath.Join(p.Home, "Library", "LaunchAgents", autoLabel+".plist")
}
func (p Paths) autoLog() string { return filepath.Join(p.State, "auto.log") }

// launchctl 和 executable 在测试里会被换掉。
var (
	launchctl = func(args ...string) (string, error) {
		out, err := exec.Command("launchctl", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	executable = os.Executable
)

func autoDomain() string  { return fmt.Sprintf("gui/%d", os.Getuid()) }
func autoService() string { return autoDomain() + "/" + autoLabel }

func autoLoaded() bool {
	_, err := launchctl("print", autoService())
	return err == nil
}

// stableExe：Homebrew 的 Cellar 路径带版本号，升级后就失效了，换成 <prefix>/bin/ssync。
func stableExe(exe string) string {
	if i := strings.Index(exe, "/Cellar/ssync/"); i > 0 {
		bin := filepath.Join(exe[:i], "bin", "ssync")
		if _, err := os.Stat(bin); err == nil {
			return bin
		}
	}
	return exe
}

// ---------- on / off / status ----------

func (a *App) AutoOn(at string) error {
	t, err := time.Parse("15:04", at)
	if err != nil {
		return fmt.Errorf("时间 %q 不对，请写成 24 小时制，例如 --at 09:00", at)
	}
	exe, err := executable()
	if err != nil {
		return fmt.Errorf("找不到 ssync 自身的路径：%w", err)
	}
	exe = stableExe(filepath.Clean(exe))
	if strings.Contains(exe, "/go-build") {
		return fmt.Errorf("当前的 ssync 是 go run 生成的临时文件（%s），请先 go build 或安装后再开启", exe)
	}

	plist := a.Paths.launchAgent()
	for _, dir := range []string{filepath.Dir(plist), a.Paths.State} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := writeAtomic(plist, []byte(autoPlist(exe, t.Hour(), t.Minute()))); err != nil {
		return err
	}
	// 已加载的要先卸掉，新时间才会生效。
	if autoLoaded() {
		launchctl("bootout", autoService())
	}
	launchctl("enable", autoService()) // 清掉以前 launchctl disable 留下的标记
	var out string
	for i := 0; i < 3; i++ { // 刚 bootout 完马上 bootstrap 偶尔会报 5: Input/output error
		if out, err = launchctl("bootstrap", autoDomain(), plist); err == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if err != nil {
		os.Remove(plist)
		return fmt.Errorf("launchctl 加载失败：%s\n如果在「系统设置 › 通用 › 登录项」里关掉过 ssync，请先打开再重试", out)
	}

	a.printf("%s 已开启每天自动整理：每天 %s 运行一次 ssync apply -y。\n", a.green("✓"), t.Format("15:04"))
	a.printf("  同名但内容不同的 skill 会跳过，留给你手动 ssync apply 处理。\n")
	a.printf("  到点时如果 Mac 在睡眠，会在唤醒后补跑；关机错过的那次不补。\n")
	a.printf("  日志：%s\n", disp(a.Paths, a.Paths.autoLog()))
	a.printf("\n第一次运行会做什么，可以先用 ssync plan 看。查看状态：ssync auto status；关闭：ssync auto off\n")
	return nil
}

func (a *App) AutoOff() error {
	plist := a.Paths.launchAgent()
	_, statErr := os.Lstat(plist)
	loaded := autoLoaded()
	if statErr != nil && !loaded {
		a.printf("每天自动整理本来就没有开启。\n")
		return nil
	}
	if loaded {
		if out, err := launchctl("bootout", autoService()); err != nil && autoLoaded() {
			return fmt.Errorf("launchctl 卸载失败：%s", out)
		}
	}
	if err := os.Remove(plist); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	a.printf("%s 已关闭每天自动整理。以前的日志留在 %s\n", a.green("✓"), disp(a.Paths, a.Paths.autoLog()))
	return nil
}

func (a *App) AutoStatus() error {
	plist := a.Paths.launchAgent()
	job, err := readAutoJob(plist)
	loaded := autoLoaded()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if loaded {
			a.printf("每天自动整理：%s（launchd 里还挂着，但 %s 不见了；运行 ssync auto off 清理）\n",
				a.yellow("状态异常"), disp(a.Paths, plist))
		} else {
			a.printf("每天自动整理：%s。开启：ssync auto on [--at %s]\n", a.dim("未开启"), autoDefaultTime)
		}
	case err != nil:
		return err
	default:
		state := a.green("已开启")
		if !loaded {
			state = a.yellow("没有加载") + "（运行 ssync auto on 重新开启）"
		}
		when := "时间未知"
		if job.Time != "" {
			when = "每天 " + job.Time + " 运行"
		}
		a.printf("每天自动整理：%s，%s\n", state, when)
		a.printf("  程序：%s", disp(a.Paths, job.Exe))
		if _, err := os.Stat(job.Exe); err != nil {
			a.printf("  %s", a.red("这个文件不在了，运行 ssync auto on 更新"))
		}
		a.printf("\n  配置：%s\n", disp(a.Paths, plist))
	}
	a.printf("  日志：%s\n", disp(a.Paths, a.Paths.autoLog()))
	if last := lastAutoRun(a.Paths.autoLog()); last != "" {
		a.printf("\n上次运行：\n")
		for _, line := range strings.Split(strings.TrimRight(last, "\n"), "\n") {
			a.printf("  %s\n", line)
		}
	}
	return nil
}

// ---------- run ----------

// AutoRun 是定时任务实际调用的入口：执行 apply -y，输出同时写进日志和 a.Out。
// launchd 下 a.Out 是 /dev/null，手动运行时能直接在终端看到。
func (a *App) AutoRun() error {
	logf, err := openAutoLog(a.Paths)
	if err != nil {
		return err
	}
	defer logf.Close()
	w := io.MultiWriter(logf, a.Out)
	fmt.Fprintf(w, "\n%s%s 自动整理 =====\n", autoLogHeader, time.Now().Format("2006-01-02 15:04:05"))
	sub := &App{Paths: a.Paths, Out: w, In: a.In}
	err = sub.Apply(options{yes: true})
	if err != nil {
		fmt.Fprintf(logf, "错误：%v\n", err) // 终端那边由 main 打印
	}
	return err
}

func openAutoLog(p Paths) (*os.File, error) {
	if err := os.MkdirAll(p.State, 0o755); err != nil {
		return nil, err
	}
	log := p.autoLog()
	if fi, err := os.Stat(log); err == nil && fi.Size() > autoLogMax {
		os.Rename(log, log+".1")
	}
	return os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// lastAutoRun 返回日志里最后一次运行的那一段。
func lastAutoRun(log string) string {
	data, err := os.ReadFile(log)
	if err != nil {
		return ""
	}
	s := string(data)
	i := strings.LastIndex(s, "\n"+autoLogHeader)
	if i < 0 {
		if !strings.HasPrefix(s, autoLogHeader) {
			return ""
		}
		return s
	}
	return s[i+1:]
}

// ---------- plist ----------

func autoPlist(exe string, hour, minute int) string {
	var esc strings.Builder
	xml.EscapeText(&esc, []byte(exe))
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>auto</string>
		<string>run</string>
	</array>
	<key>StartCalendarInterval</key>
	<dict>
		<key>Hour</key>
		<integer>%d</integer>
		<key>Minute</key>
		<integer>%d</integer>
	</dict>
	<key>ProcessType</key>
	<string>Background</string>
</dict>
</plist>
`, autoLabel, esc.String(), hour, minute)
}

type autoJob struct {
	Exe  string
	Time string // "09:00"；plist 被手动改得看不懂时为空
}

func readAutoJob(path string) (*autoJob, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	v, err := decodePlist(f)
	if err != nil {
		return nil, fmt.Errorf("%s 格式不对：%w", path, err)
	}
	top, _ := v.(map[string]any)
	j := &autoJob{}
	if args, ok := top["ProgramArguments"].([]any); ok && len(args) > 0 {
		j.Exe, _ = args[0].(string)
	}
	if cal, ok := top["StartCalendarInterval"].(map[string]any); ok {
		h, ok1 := cal["Hour"].(int)
		m, ok2 := cal["Minute"].(int)
		if ok1 && ok2 {
			j.Time = fmt.Sprintf("%02d:%02d", h, m)
		}
	}
	return j, nil
}

// decodePlist 解析 XML plist，只支持 dict / array / string / integer / true / false。
func decodePlist(r io.Reader) (any, error) {
	d := xml.NewDecoder(r)
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local != "plist" {
			return plistValue(d, se)
		}
	}
}

func plistValue(d *xml.Decoder, se xml.StartElement) (any, error) {
	switch se.Name.Local {
	case "dict", "array":
		m := map[string]any{}
		var arr []any
		key := ""
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.EndElement:
				if se.Name.Local == "dict" {
					return m, nil
				}
				return arr, nil
			case xml.StartElement:
				if se.Name.Local == "dict" && t.Name.Local == "key" {
					if err := d.DecodeElement(&key, &t); err != nil {
						return nil, err
					}
					continue
				}
				v, err := plistValue(d, t)
				if err != nil {
					return nil, err
				}
				if se.Name.Local == "dict" {
					m[key] = v
				} else {
					arr = append(arr, v)
				}
			}
		}
	case "true", "false":
		return se.Name.Local == "true", d.Skip()
	}
	var s string
	if err := d.DecodeElement(&s, &se); err != nil {
		return nil, err
	}
	if se.Name.Local == "integer" {
		return strconv.Atoi(strings.TrimSpace(s))
	}
	return s, nil
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
