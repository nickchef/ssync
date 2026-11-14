package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Run 是一次 apply 的运行记录，保存在 ~/.agents/.ssync/runs/<id>.json。
type Run struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Ops     []Op      `json:"ops"`
	Undone  bool      `json:"undone"`
}

func (r *Run) doneCount() int {
	n := 0
	for _, op := range r.Ops {
		if op.Done {
			n++
		}
	}
	return n
}

func newRunID(p Paths) string {
	base := time.Now().Format("20060102-150405")
	id := base
	for i := 2; ; i++ {
		_, e1 := os.Lstat(filepath.Join(p.runsDir(), id+".json"))
		_, e2 := os.Lstat(p.backupRoot(id))
		if errors.Is(e1, fs.ErrNotExist) && errors.Is(e2, fs.ErrNotExist) {
			return id
		}
		id = fmt.Sprintf("%s-%d", base, i)
	}
}

func saveRun(p Paths, r *Run) error {
	if err := os.MkdirAll(p.runsDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	file := filepath.Join(p.runsDir(), r.ID+".json")
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

func loadRuns(p Paths) ([]*Run, error) {
	entries, err := os.ReadDir(p.runsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []*Run
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(p.runsDir(), e.Name()))
		if err != nil {
			return nil, err
		}
		var r Run
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("运行记录 %s 损坏：%w", e.Name(), err)
		}
		runs = append(runs, &r)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Created.Before(runs[j].Created) })
	return runs, nil
}

// latestUndoable 找最近一次还没撤销、且确实改动过文件的运行。
// 只允许撤销最近一次，保证按倒序一层层撤回。
func latestUndoable(p Paths) (*Run, error) {
	runs, err := loadRuns(p)
	if err != nil {
		return nil, err
	}
	for i := len(runs) - 1; i >= 0; i-- {
		if !runs[i].Undone && runs[i].doneCount() > 0 {
			return runs[i], nil
		}
	}
	return nil, nil
}

func executePlan(p Paths, pl *Plan) (*Run, error) {
	r := &Run{ID: pl.RunID, Created: time.Now()}
	for _, s := range pl.Steps {
		r.Ops = append(r.Ops, s.Ops...)
	}
	if err := saveRun(p, r); err != nil {
		return r, fmt.Errorf("写运行记录失败：%w", err)
	}
	for i := range r.Ops {
		if err := applyOp(r.Ops[i]); err != nil {
			return r, fmt.Errorf("%s 失败：%w", describeOp(p, r.Ops[i]), err)
		}
		r.Ops[i].Done = true
		if err := saveRun(p, r); err != nil {
			return r, fmt.Errorf("写运行记录失败：%w", err)
		}
	}
	return r, nil
}

func describeOp(p Paths, op Op) string {
	if op.Kind == "link" {
		return fmt.Sprintf("创建软链接 %s → %s", disp(p, op.Src), disp(p, op.Dst))
	}
	return fmt.Sprintf("移动 %s → %s", disp(p, op.Src), disp(p, op.Dst))
}

func applyOp(op Op) error {
	switch op.Kind {
	case "move":
		return moveDir(op.Src, op.Dst)
	case "link":
		if _, err := os.Lstat(op.Src); err == nil {
			return fmt.Errorf("%s 已存在", op.Src)
		}
		return os.Symlink(op.Dst, op.Src)
	}
	return fmt.Errorf("未知操作 %q", op.Kind)
}

// undoRun 倒序撤销；现场已被改动的项跳过并给出提示，不强行覆盖。
func undoRun(p Paths, r *Run) ([]string, error) {
	var warns []string
	for i := len(r.Ops) - 1; i >= 0; i-- {
		op := r.Ops[i]
		if !op.Done {
			continue
		}
		switch op.Kind {
		case "link":
			t, err := os.Readlink(op.Src)
			if err != nil || t != op.Dst {
				warns = append(warns, fmt.Sprintf("%s 已经不是 ssync 创建的软链接，没动它", disp(p, op.Src)))
				continue
			}
			if err := os.Remove(op.Src); err != nil {
				return warns, err
			}
		case "move":
			if _, err := os.Lstat(op.Src); err == nil {
				warns = append(warns, fmt.Sprintf("%s 已存在，没有把 %s 移回去", disp(p, op.Src), disp(p, op.Dst)))
				continue
			}
			if _, err := os.Lstat(op.Dst); err != nil {
				warns = append(warns, fmt.Sprintf("%s 不见了，无法移回 %s", disp(p, op.Dst), disp(p, op.Src)))
				continue
			}
			if err := moveDir(op.Dst, op.Src); err != nil {
				return warns, err
			}
		}
		r.Ops[i].Done = false
	}
	r.Undone = true
	pruneEmptyDirs(p.backupRoot(r.ID))
	return warns, saveRun(p, r)
}

func moveDir(src, dst string) error {
	if _, err := os.Lstat(src); err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%s 已存在", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	err := os.Rename(src, dst)
	var le *os.LinkError
	if err != nil && errors.As(err, &le) && errors.Is(le.Err, syscall.EXDEV) {
		// 跨磁盘：先完整复制再删源。
		if err := copyTree(src, dst); err != nil {
			os.RemoveAll(dst)
			return err
		}
		return os.RemoveAll(src)
	}
	return err
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(t, target)
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := os.WriteFile(target, data, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chtimes(target, info.ModTime(), info.ModTime())
		}
		return nil
	})
}

// pruneEmptyDirs 删除 root 下（含 root）的空目录，有文件的目录不动。
func pruneEmptyDirs(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			pruneEmptyDirs(filepath.Join(root, e.Name()))
		}
	}
	os.Remove(root) // 非空时会失败，正好
}

func acquireLock(p Paths) (func(), error) {
	if err := os.MkdirAll(p.State, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(p.State, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("另一个 ssync 正在运行")
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
