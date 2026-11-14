# ssync

把各个 AI Agent 的 skill 集中到 `~/.agents/skills`，再用软链接放回原来的位置。之后不管在哪个 Agent 里改 skill，改的都是同一份。只支持 macOS。

## 安装

```sh
brew install nickchef/tap/ssync
```

## 用法

```sh
ssync status    # 看各 Agent 的 skills 目录现状
ssync plan      # 预演：列出会做哪些改动，不动任何文件
ssync apply     # 执行；同名但内容不同的 skill 会逐个问你保留哪个
ssync undo      # 撤销最近一次 apply（可重复执行，一次撤一层）
ssync agents    # 列出支持的 Agent 和目录
```

`status` / `plan` / `apply` 可以加 `--only claude,codex` 或 `--skip grok`。

## 它会做什么

对每个 Agent 的 skills 目录里含 `SKILL.md` 的子目录：

| 情况 | 处理 |
| --- | --- |
| 共享目录里还没有同名 skill | 移到 `~/.agents/skills/<name>`，原处换成软链接 |
| 共享目录或其他 Agent 里有内容完全相同的 | 这份副本移到备份，原处换成软链接 |
| 同名但内容不同 | 停下来列出各版本和差异文件，让你选保留哪个（`d` 看完整 diff，`s` 跳过，`q` 中止）；没选中的版本移到备份 |
| 已经是指向共享目录的软链接 | 不动 |
| 指向别处的软链接、断链、以 `.` 开头的目录（如 codex 的 `.system`） | 不动 |

- 执行前会列出全部改动并要求确认；`-y` 跳过确认（此时冲突项会被跳过）。
- 被替换下来的副本移到 `~/.agents/.ssync/backups/<运行时间>/`，**不会删除**。
- 每次运行的操作记录在 `~/.agents/.ssync/runs/`，`ssync undo` 按记录倒序还原；如果你之后手动改过某个位置，undo 会跳过那一项并提示，不会覆盖。
- 内容比较时忽略 `.DS_Store`、`__pycache__`、`.git`。

## 支持的 Agent

内置以下用户级目录，不存在的会被忽略：

```
claude       ~/.claude/skills
codex        ~/.codex/skills
cursor       ~/.cursor/skills
gemini       ~/.gemini/skills
antigravity  ~/.gemini/config/skills
grok         ~/.grok/skills
opencode     ~/.config/opencode/skills
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

自定义或忽略某些目录，写在 `~/.config/ssync/config.json`：

```json
{
  "agents": {"myagent": "~/.myagent/skills"},
  "ignore": ["grok"]
}
```

## 开发与发布

```sh
go test ./...
go build -o ssync .
scripts/release.sh 0.2.0   # 打 tag、建 GitHub Release、更新 nickchef/homebrew-tap 里的 formula
```
