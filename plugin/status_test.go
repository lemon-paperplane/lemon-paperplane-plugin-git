package git

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

func TestStatusParsing(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	e.write(dir, "a b.txt", "1\n2\n3\n")
	e.write(dir, "中 文/文件.txt", "甲\n乙\n")
	e.write(dir, "old.txt", "rename me please\nline2\nline3\n")
	e.write(dir, "bin.dat", "\x00\x01\x02")
	e.write(dir, "del.txt", "bye\n")
	e.commitAll(dir, "base")

	e.write(dir, "a b.txt", "1\n2 changed\n3\n4\n")                  // 未暂存修改
	e.write(dir, "中 文/文件.txt", "甲\n乙\n丙\n")                          // 暂存修改
	e.git(dir, "add", "中 文/文件.txt")                                  //
	e.git(dir, "mv", "old.txt", "new name.txt")                      // 暂存重命名
	e.write(dir, "bin.dat", "\x00\x01\x02\x03")                      // 二进制修改
	os.Remove(filepath.Join(dir, "del.txt"))                         // 未暂存删除
	e.write(dir, "untracked dir/新 文件.md", "x\n")                     // 未跟踪
	e.write(dir, "weird\nname.txt", "newline in name\n")             // 文件名带换行
	e.git(dir, "stash", "push", "-q", "-m", "s1", "--", "README.md") // 没东西可储藏也无妨
	e.write(dir, "README.md", "hello\nstash me\n")
	e.git(dir, "stash", "push", "-q", "-m", "s2", "--", "README.md")

	st := e.status(dir)
	if st.Branch != "main" || st.Detached || st.Head == "" || st.HeadHash == "" {
		t.Fatalf("branch info: %+v", st)
	}
	if st.StashCount != 1 {
		t.Errorf("stashCount = %d, want 1", st.StashCount)
	}
	check := func(path, status string, staged, unstaged bool) *fileChange {
		t.Helper()
		f := findFile(st, path)
		if f == nil {
			t.Fatalf("missing %q in %+v", path, st.Files)
		}
		if f.Status != status || f.Staged != staged || f.Unstaged != unstaged {
			t.Errorf("%q: got status=%s staged=%v unstaged=%v, want %s %v %v", path, f.Status, f.Staged, f.Unstaged, status, staged, unstaged)
		}
		return f
	}
	f := check("a b.txt", "M", false, true)
	if f.Additions != 2 || f.Deletions != 1 {
		t.Errorf("a b.txt numstat = +%d -%d", f.Additions, f.Deletions)
	}
	check("中 文/文件.txt", "M", true, false)
	r := check("new name.txt", "R", true, false)
	if r.OldPath != "old.txt" || r.IndexStatus != "R" {
		t.Errorf("rename: %+v", r)
	}
	b := check("bin.dat", "M", false, true)
	if !b.Binary {
		t.Errorf("bin.dat should be binary")
	}
	check("del.txt", "D", false, true)
	check("untracked dir/新 文件.md", "?", false, true)
	check("weird\nname.txt", "?", false, true)

	// 与 SDK 类型兼容
	raw, _ := json.Marshal(st)
	var sdkSt sdk.VCSStatus
	if err := json.Unmarshal(raw, &sdkSt); err != nil || len(sdkSt.Files) != len(st.Files) {
		t.Fatalf("not compatible with sdk.VCSStatus: %v", err)
	}
}

func TestStatusConflictAndOperation(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	e.write(dir, "f.txt", "base\n")
	e.write(dir, "gone.txt", "will be deleted by them\n")
	e.commitAll(dir, "base")
	e.git(dir, "checkout", "-q", "-b", "other")
	e.write(dir, "f.txt", "theirs\n")
	os.Remove(filepath.Join(dir, "gone.txt"))
	e.commitAll(dir, "theirs")
	e.git(dir, "checkout", "-q", "main")
	e.write(dir, "f.txt", "ours\n")
	e.write(dir, "gone.txt", "modified by us\n")
	e.commitAll(dir, "ours")
	e.gitErr(dir, "merge", "other")

	st := e.status(dir)
	if st.Operation != "merge" || st.OperationDetail == nil || st.OperationDetail.Current == "" {
		t.Fatalf("operation = %q %+v", st.Operation, st.OperationDetail)
	}
	if st.Conflicts != 2 {
		t.Fatalf("conflicts = %d, files %+v", st.Conflicts, st.Files)
	}
	if f := findFile(st, "f.txt"); f == nil || !f.Conflicted || f.ConflictType != "UU" || f.Status != "U" {
		t.Errorf("f.txt: %+v", f)
	}
	if f := findFile(st, "gone.txt"); f == nil || f.ConflictType != "UD" {
		t.Errorf("gone.txt: %+v", f)
	}
}

func TestStatusUntrackedTruncated(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	for i := 0; i < 30; i++ {
		e.write(dir, fmt.Sprintf("junk/f%02d.txt", i), "x\n")
	}
	e.write(dir, "README.md", "changed\n")
	var st statusResult
	e.must(dir, sdk.VCSOpStatus, map[string]any{"untrackedLimit": 10}, &st)
	if !st.UntrackedTruncated || st.UntrackedCount != 30 {
		t.Fatalf("truncated=%v count=%d", st.UntrackedTruncated, st.UntrackedCount)
	}
	untracked := 0
	for _, f := range st.Files {
		if f.Status == "?" {
			untracked++
		}
	}
	if untracked != 10 || findFile(st, "README.md") == nil {
		t.Fatalf("listed untracked=%d files=%d", untracked, len(st.Files))
	}
	// 硬上限：数到就停（git 被结束），已跟踪的修改仍在
	st2 := &statusResult{}
	if err := parseStatusV2(strings.NewReader("1 .M N... 100644 100644 100644 a a x\x00? a\x00? b\x00? c\x00? d\x00"), st2, 1, 3); err != errStop {
		t.Fatalf("want errStop, got %v", err)
	}
	if st2.UntrackedCount != 3 || len(st2.Files) != 2 {
		t.Fatalf("hard cap parse: %+v", st2)
	}
}

func TestStatusUnbornAndUpstream(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(t.TempDir(), "empty")
	e.git("", "init", "-q", dir)
	e.write(dir, "a.txt", "1\n2\n")
	e.git(dir, "add", "a.txt")
	st := e.status(dir)
	if !st.Unborn || st.Branch != "main" {
		t.Fatalf("unborn: %+v", st)
	}
	if f := findFile(st, "a.txt"); f == nil || f.Status != "A" || f.Additions != 2 {
		t.Fatalf("a.txt: %+v", f)
	}

	// 上游已删除
	remote := filepath.Join(t.TempDir(), "remote.git")
	e.git("", "init", "-q", "--bare", remote)
	work := e.repo()
	e.git(work, "remote", "add", "origin", remote)
	e.git(work, "push", "-q", "-u", "origin", "main")
	e.git(work, "checkout", "-q", "-b", "feat")
	e.git(work, "push", "-q", "-u", "origin", "feat")
	e.git(work, "push", "-q", "origin", "--delete", "feat")
	st = e.status(work)
	if st.Upstream != "origin/feat" || !st.UpstreamGone {
		t.Fatalf("gone: %+v", st)
	}
	e.git(work, "checkout", "-q", "main")
	e.write(work, "x.txt", "x\n")
	e.commitAll(work, "local")
	st = e.status(work)
	if st.Upstream != "origin/main" || st.Ahead != 1 || st.Behind != 0 || st.UpstreamGone {
		t.Fatalf("ahead: %+v", st)
	}
}

func TestStatusInWorktreeAndSubmoduleGitFile(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	wt := filepath.Join(t.TempDir(), "wt")
	e.git(dir, "worktree", "add", "-q", "-b", "wtb", wt)
	e.write(wt, "README.md", "x\n")
	e.write(wt, "f.txt", "1\n")
	e.commitAll(wt, "wt commit")
	e.git(wt, "checkout", "-q", "-b", "side")
	e.git(wt, "checkout", "-q", "wtb")
	e.write(wt, "f.txt", "2\n")
	e.git(wt, "commit", "-q", "-am", "c2")
	e.git(wt, "checkout", "-q", "side")
	e.write(wt, "f.txt", "3\n")
	e.git(wt, "commit", "-q", "-am", "c3")
	e.gitErr(wt, "merge", "wtb")
	// .git 是文件：操作状态要在 worktrees/<名字> 下找
	st := e.status(wt)
	if st.Operation != "merge" || st.Branch != "side" {
		t.Fatalf("worktree status: op=%q branch=%q", st.Operation, st.Branch)
	}
	if main := e.status(dir); main.Operation != "" {
		t.Fatalf("main worktree should not see merge: %q", main.Operation)
	}
}

// 读操作不能写仓库：文件 stat 变了但内容没变时，git status/diff 默认会刷新 .git/index。
func TestReadOpsDoNotWriteIndex(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	for i := 0; i < 5; i++ {
		e.write(dir, fmt.Sprintf("f%d.txt", i), "same\n")
	}
	e.commitAll(dir, "files")
	e.git(dir, "submodule", "add", "-q", e.repo(), "sub")
	e.commitAll(dir, "sub")
	idx := filepath.Join(dir, ".git", "index")
	// 让文件 stat 变脏（内容不变，修改时间改到索引之后），再改一个文件
	future := time.Now().Add(10 * time.Second)
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatal(err)
		}
	}
	e.write(dir, "f0.txt", "changed\n")
	before, _ := os.Stat(idx)
	for _, c := range []struct {
		op   string
		args any
	}{
		{sdk.VCSOpStatus, nil}, {sdk.VCSOpDiff, map[string]any{"path": "f0.txt"}}, {sdk.VCSOpDiff, map[string]any{"path": "f1.txt", "mode": "unstaged"}},
		{sdk.VCSOpLog, nil}, {sdk.VCSOpBranches, nil}, {sdk.VCSOpRemotes, nil}, {sdk.VCSOpStashList, nil}, {sdk.VCSOpTags, nil},
		{sdk.VCSOpSubmodules, nil}, {sdk.VCSOpWorktrees, nil}, {sdk.VCSOpBlame, map[string]any{"path": "f0.txt"}},
		{sdk.VCSOpFileHistory, map[string]any{"path": "f0.txt"}}, {sdk.VCSOpShow, map[string]any{"hash": "HEAD"}},
		{sdk.VCSOpCompare, map[string]any{"base": "HEAD~1"}}, {sdk.VCSOpConfigGet, map[string]any{"keys": []string{"user.name"}}},
	} {
		e.must(dir, c.op, c.args, nil)
		after, _ := os.Stat(idx)
		if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
			t.Fatalf("%s wrote .git/index", c.op)
		}
	}
	if st := e.status(dir); len(st.Files) != 1 || st.Files[0].Path != "f0.txt" {
		t.Fatalf("stat-dirty files must not show as changed: %+v", st.Files)
	}
}
