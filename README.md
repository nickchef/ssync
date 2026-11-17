# ssync

把各个 AI Agent 的 skill 集中到 `~/.agents/skills`，让每个 Agent 都从这一份读。之后不管在哪个 Agent 里改 skill，改的都是同一份。只支持 macOS。

skill 迁到共享目录后，各 Agent 怎么继续看到它们，分三种：

- **直接读取**（codex、cursor、gemini、grok、opencode）：这些 Agent 自己就会读 `~/.agents/skills`，所以不建任何链接，只把它们目录里的 skill 挪走，避免同一个 skill 出现两份。
- **整目录链接**（claude、antigravity）：它们不读 `~/.agents/skills`，所以把整个 skills 目录换成指向 `~/.agents/skills` 的软链接。在里面新建的 skill 也会直接落进共享目录。
- **逐个链接**（其余 Agent）：还没核实它们读不读 `~/.agents/skills`，保守起见每个 skill 目录各自换成软链接。确认后可以在配置文件里改成直接读取。

## 安装

```sh
brew install nickchef/tap/ssync
```

## 用法

```sh
ssync status    # 看各 Agent 的 skills 目录现状
ssync plan      # 预演：列出会做哪些改动，不动任何文件
ssync apply     # 执行；同名但内容不同的 skill 会逐个问你保留哪个
ssync apply -y  # 不确认、不提问；同名冲突直接保留最近修改的版本
ssync undo      # 撤销最近一次 apply（可重复执行，一次撤一层）
ssync agents    # 列出支持的 Agent 和目录
```

`status` / `plan` / `apply` 可以加 `--only claude,codex` 或 `--skip grok`。

## 每天自动整理

```sh
ssync auto on              # 每天 09:00 自动跑一次 ssync apply -y
ssync auto on --at 22:30   # 换个时间（再运行一次就会覆盖旧设置）
ssync auto                 # 查看是否开启、运行时间、上次运行的输出
ssync auto run             # 现在就按自动整理的方式跑一次
ssync auto off             # 关闭
```

- 用的是 macOS 自带的 launchd：在 `~/Library/LaunchAgents/io.github.nickchef.ssync.plist` 放一个定时任务，不需要常驻进程。第一次开启时系统可能弹出「已添加后台项目」的通知，属正常。
- 到点时 Mac 在睡眠，会在唤醒后补跑；关机错过的那次不补。
- 自动运行等同于 `apply -y`：同名但内容不同的 skill 保留最近修改的版本，其余版本移到备份；`ssync auto` 能看到上次自动选了哪些。
- 每次运行的输出追加到 `~/.agents/.ssync/auto.log`（超过 1 MiB 转成 `auto.log.1`）。自动运行产生的改动同样可以 `ssync undo`。
- 定时任务里记的是开启时 ssync 的路径（Homebrew 安装的会记成 `$(brew --prefix)/bin/ssync`，升级后仍然有效）。卸载 ssync 前先 `ssync auto off`。

## 它会做什么

对每个 Agent 的 skills 目录里含 `SKILL.md` 的子目录：

| 情况 | 处理 |
| --- | --- |
| 共享目录里还没有同名 skill | 移到 `~/.agents/skills/<name>`；逐个链接的 Agent 原处换成软链接 |
| 共享目录或其他 Agent 里有内容完全相同的 | 这份副本移到备份；逐个链接的 Agent 原处换成软链接 |
| 同名但内容不同 | 停下来列出各版本和差异文件，让你选保留哪个（`d` 看完整 diff，`s` 跳过，`q` 中止）；没选中的版本移到备份。加 `-y`（以及每天自动整理）时不提问，直接保留最近修改的版本；几个版本最近修改时间完全相同时跳过 |
| 已经是指向共享目录的软链接 | 逐个链接的不动；直接读取的移到备份（多余了） |
| 指向别处的软链接、断链、以 `.` 开头的目录（如 codex 的 `.system`） | 不动 |

整目录链接的 Agent 还会：

- 里面的 skill 照上表迁走，但不再逐个建软链接；
- 指向别处的外部链接在共享目录里重建（相对路径会改成绝对路径）；
- 最后把原目录（剩下的断链、非 skill 文件、旧的逐个链接等）整个移到备份，原处换成指向 `~/.agents/skills` 的软链接。

以下情况这次先退回逐个链接，并说明原因，处理好后再 apply 即可换成整目录：有冲突跳过没选（或 `-y` 时几个版本修改时间相同、分不出新旧）、目录里有以 `.` 开头的隐藏项、目录本身就是指向别处的软链接、外部链接和共享目录里的同名项冲突。

- 执行前会列出全部改动并要求确认；`-y` 跳过确认，冲突也不再提问。`ssync plan` 会提前写出 `-y` 时每个冲突会保留哪个版本。
- "最近修改"看的是 skill 目录里最新那个文件的修改时间（忽略 `.DS_Store`、`__pycache__`、`.git`）。注意用不保留时间的方式复制过来的旧副本，修改时间会变成复制那一刻，可能被当成较新的。
- 被替换下来的副本移到 `~/.agents/.ssync/backups/<运行时间>/`，**不会删除**。
- 每次运行的操作记录在 `~/.agents/.ssync/runs/`，`ssync undo` 按记录倒序还原；如果你之后手动改过某个位置，undo 会跳过那一项并提示，不会覆盖。
- 内容比较时忽略 `.DS_Store`、`__pycache__`、`.git`。

## 支持的 Agent

内置以下用户级目录，不存在的会被忽略（`ssync agents` 也会列出每个的链接方式）：

```
claude       ~/.claude/skills            整目录链接
codex        ~/.codex/skills             直接读取
cursor       ~/.cursor/skills            直接读取
gemini       ~/.gemini/skills            直接读取
antigravity  ~/.gemini/config/skills     整目录链接
grok         ~/.grok/skills              直接读取
opencode     ~/.config/opencode/skills   直接读取
copilot      ~/.copilot/skills
windsurf     ~/.codeium/windsurf/skills
kiro         ~/.kiro/skills
qwen         ~/.qwen/skills
factory      ~/.factory/skills
goose        ~/.config/goose/skills
amp          ~/.config/amp/skills
roo          ~/.roo/skills
cline        ~/.cline/skills
augment      ~/.augment/skills
trae         ~/.trae/skills
openclaw     ~/.openclaw/skills
```

其余 Agent 都是逐个链接。

自定义目录、忽略某些 Agent、改链接方式（`whole` / `native` / `link`），写在 `~/.config/ssync/config.json`：

```json
{
  "agents": {"myagent": "~/.myagent/skills"},
  "ignore": ["grok"],
  "modes": {"kiro": "native"}
}
```

## 开发与发布

```sh
go test ./...
go build -o ssync .
scripts/release.sh 0.2.0   # 打 tag、建 GitHub Release、更新 nickchef/homebrew-tap 里的 formula
```
