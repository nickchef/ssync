package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
)

var version = "dev"

const usage = `ssync：把各个 Agent 的 skill 集中到 ~/.agents/skills，让它们都从这一份读。

用法：
  ssync status          查看各 Agent 的 skills 目录现状
  ssync plan            预演：列出 apply 会做的改动，不动任何文件
  ssync apply [-y]      执行迁移；同名但内容不同的 skill 会逐个问你保留哪个
  ssync undo [-y]       撤销最近一次 apply（可以重复执行，一次撤一层）
  ssync agents          列出支持的 Agent 和对应目录
  ssync auto on [--at 09:00]
                        每天定时自动执行一次 apply -y（冲突会跳过，留给你手动处理）
  ssync auto off        关闭每天自动整理
  ssync auto [status]   查看自动整理的设置和上次运行结果
  ssync auto run        立即按自动整理的方式跑一次，输出也会写进日志
  ssync version

status / plan / apply 可加：
  --only claude,codex   只处理这些 Agent
  --skip grok           跳过这些 Agent

说明：
  - codex、cursor、gemini、grok、opencode 自己会读 ~/.agents/skills，不建链接；
    claude、antigravity 整个 skills 目录换成指向共享目录的软链接；
    其余 Agent 逐个 skill 链接。用 ssync agents 查看每个的方式。
  - 只处理含 SKILL.md 的目录；以 . 开头的（如 codex 的 .system）不碰。
  - 内容相同的副本合并成一份；被替换下来的副本移到 ~/.agents/.ssync/backups，不会删除。
  - 自定义目录写在 ~/.config/ssync/config.json：
      {"agents": {"myagent": "~/.myagent/skills"}, "ignore": ["grok"],
       "modes": {"kiro": "native"}}
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin, stdout *os.File, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "-v", "--version":
		v := version
		if bi, ok := debug.ReadBuildInfo(); ok && v == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version // go install ...@vX.Y.Z 时没有 ldflags
		}
		fmt.Fprintln(stdout, "ssync", v)
		return 0
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, "错误：找不到用户主目录：", err)
		return 1
	}
	app := &App{
		Paths:       pathsFor(home),
		Out:         stdout,
		In:          bufio.NewReader(stdin),
		Interactive: isTerminal(stdin),
		Color:       isTerminal(stdout) && os.Getenv("NO_COLOR") == "",
	}

	if cmd == "auto" {
		sub := "status"
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			sub, rest = rest[0], rest[1:]
		}
		cmd = "auto " + sub
	}

	o, err := parseFlags(cmd, rest, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	switch cmd {
	case "status":
		err = app.Status(o)
	case "plan":
		err = app.Plan(o)
	case "apply":
		err = app.Apply(o)
	case "undo":
		err = app.Undo(o)
	case "agents":
		err = app.Agents()
	case "auto on":
		err = app.AutoOn(o.at)
	case "auto off":
		err = app.AutoOff()
	case "auto status":
		err = app.AutoStatus()
	case "auto run":
		err = app.AutoRun()
	default:
		fmt.Fprintf(stderr, "未知命令：%s\n\n%s", cmd, usage)
		return 2
	}
	if errors.Is(err, errAbort) {
		fmt.Fprintln(stderr, "已中止，没有改动任何文件。")
		return 1
	}
	if err != nil {
		fmt.Fprintln(stderr, "错误：", err)
		return 1
	}
	return 0
}

type options struct {
	only, skip []string
	yes        bool
	at         string // auto on 的运行时间
}

func parseFlags(cmd string, args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("ssync "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var only, skip string
	switch cmd {
	case "status", "plan", "apply":
		fs.StringVar(&only, "only", "", "只处理这些 Agent，逗号分隔")
		fs.StringVar(&skip, "skip", "", "跳过这些 Agent，逗号分隔")
	}
	switch cmd {
	case "apply", "undo":
		fs.BoolVar(&o.yes, "y", false, "不再确认，直接执行")
		fs.BoolVar(&o.yes, "yes", false, "同 -y")
	case "auto on":
		fs.StringVar(&o.at, "at", autoDefaultTime, "每天几点运行，24 小时制")
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "多余的参数：%s\n", strings.Join(fs.Args(), " "))
		return o, errors.New("bad args")
	}
	o.only = splitList(only)
	o.skip = splitList(skip)
	return o, nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
