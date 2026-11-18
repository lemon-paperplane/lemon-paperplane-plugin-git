package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

func (e *tenv) commit(root string, args map[string]any) commitResult {
	e.t.Helper()
	var r commitResult
	e.must(root, sdk.VCSOpCommit, args, &r)
	if r.Hash == "" {
		e.t.Fatalf("commit returned no hash")
	}
	return r
}

// filesIn 返回某提交改动的文件。
func (e *tenv) filesIn(dir, rev string) []string {
	out := strings.TrimSpace(e.git(dir, "-c", "core.quotepath=false", "show", "--name-only", "--no-renames", "--format=", rev))
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func TestCommitPathsMode(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	e.write(dir, "keep-staged.txt", "1\n")
	e.write(dir, "mod.txt", "1\n")
	e.write(dir, "del.txt", "1\n")
	e.write(dir, "old.txt", "rename content\nmore\nmore\n")
	e.commitAll(dir, "base")

	e.write(dir, "keep-staged.txt", "2\n")
	e.git(dir, "add", "keep-staged.txt") // 用户另外暂存的内容，不能被带进提交
	e.write(dir, "mod.txt", "2\n")
	os.Remove(filepath.Join(dir, "del.txt"))
	e.write(dir, "新 文件.txt", "new\n") // 未跟踪的新文件
	e.git(dir, "mv", "old.txt", "renamed.txt")
	e.write(dir, "untouched-new.txt", "not selected\n")

	r := e.commit(dir, map[string]any{"message": "pick some\n\nbody line", "paths": []string{"mod.txt", "del.txt", "新 文件.txt", "renamed.txt"}})
	files := e.filesIn(dir, r.Hash)
	want := map[string]bool{"mod.txt": true, "del.txt": true, "新 文件.txt": true, "renamed.txt": true, "old.txt": true}
	if len(files) != len(want) {
		t.Fatalf("committed files = %v", files)
	}
	for _, f := range files {
		if !want[f] {
			t.Fatalf("unexpected file %q in commit (%v)", f, files)
		}
	}
	if msg := e.git(dir, "log", "-1", "--format=%B"); !strings.HasPrefix(msg, "pick some\n\nbody line") {
		t.Errorf("message = %q", msg)
	}
	st := e.status(dir)
	if f := findFile(st, "keep-staged.txt"); f == nil || !f.Staged {
		t.Fatalf("other staged content must stay staged: %+v", st.Files)
	}
	if f := findFile(st, "untouched-new.txt"); f == nil || f.Status != "?" {
		t.Fatalf("unselected untracked file must stay untracked: %+v", f)
	}
	if len(st.Files) != 2 {
		t.Fatalf("unexpected leftovers: %+v", st.Files)
	}
}

func TestCommitStagingModeAndErrors(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	// 暂存区为空
	err := e.call(dir, sdk.VCSOpCommit, map[string]any{"message": "x"}, nil)
	if reasonOfErr(err) != "nothingToCommit" {
		t.Fatalf("empty index: %v", err)
	}
	// 空信息
	if err := e.call(dir, sdk.VCSOpCommit, map[string]any{"message": "  "}, nil); reasonOfErr(err) != "emptyMessage" {
		t.Fatalf("empty message: %v", err)
	}
	e.write(dir, "a.txt", "a\n")
	e.write(dir, "b.txt", "b\n")
	e.git(dir, "add", "a.txt")
	r := e.commit(dir, map[string]any{"message": "staged only", "signOff": true, "author": "Someone Else <else@example.com>"})
	if files := e.filesIn(dir, r.Hash); len(files) != 1 || files[0] != "a.txt" {
		t.Fatalf("staging mode committed %v", files)
	}
	info := e.git(dir, "log", "-1", "--format=%an|%ae|%cn|%B")
	if !strings.HasPrefix(info, "Someone Else|else@example.com|Tester|staged only") || !strings.Contains(info, "Signed-off-by: Tester <tester@example.com>") {
		t.Errorf("author/signoff: %q", info)
	}
	// amend：只改信息
	e.commit(dir, map[string]any{"message": "amended", "amend": true})
	if msgs := e.logMsgs(dir, "-2"); msgs[0] != "amended" || msgs[1] != "init" {
		t.Fatalf("amend: %v", msgs)
	}
	// amend 带上新文件
	r = e.commit(dir, map[string]any{"amend": true, "paths": []string{"b.txt"}})
	if files := e.filesIn(dir, r.Hash); len(files) != 2 {
		t.Fatalf("amend with paths: %v", files)
	}
	// all：全部变更
	e.write(dir, "c.txt", "c\n")
	e.write(dir, "a.txt", "a2\n")
	r = e.commit(dir, map[string]any{"message": "all", "all": true})
	if files := e.filesIn(dir, r.Hash); len(files) != 2 {
		t.Fatalf("all: %v", files)
	}
	// 未选中的路径没有修改
	if err := e.call(dir, sdk.VCSOpCommit, map[string]any{"message": "x", "paths": []string{"a.txt"}}, nil); reasonOfErr(err) != "nothingToCommit" {
		t.Fatalf("no changes in paths: %v", err)
	}
}

func TestCommitIdentityMissing(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	// 清掉全局身份
	cfg := e.p.extraEnv["GIT_CONFIG_GLOBAL"]
	if err := os.WriteFile(cfg, []byte("[init]\n\tdefaultBranch = main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.write(dir, "a.txt", "a\n")
	err := e.call(dir, sdk.VCSOpCommit, map[string]any{"message": "x", "paths": []string{"a.txt"}}, nil)
	se, ok := err.(*sdk.Error)
	if !ok || se.Code != sdk.ErrCodeUserError || reasonOf(se) != "identityMissing" {
		t.Fatalf("want identityMissing, got %v", err)
	}
	if missing := se.Data.(map[string]any)["missing"].([]string); len(missing) != 2 {
		t.Fatalf("missing = %v", missing)
	}
	// 用 configSet 设置后可以提交
	e.op(dir, sdk.VCSOpConfigSet, map[string]any{"key": "user.name", "value": "Local User"})
	e.op(dir, sdk.VCSOpConfigSet, map[string]any{"key": "user.email", "value": "local@example.com", "global": true})
	r := e.commit(dir, map[string]any{"message": "x", "paths": []string{"a.txt"}})
	if got := e.git(dir, "log", "-1", "--format=%an <%ae>", r.Hash); strings.TrimSpace(got) != "Local User <local@example.com>" {
		t.Fatalf("identity used: %q", got)
	}
	if strings.Contains(e.read(e.home, "gitconfig"), "Local User") || !strings.Contains(e.read(e.home, "gitconfig"), "local@example.com") {
		t.Fatalf("global/local scopes wrong")
	}
}

func TestCommitPartialHunks(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	base := lines(30, func(i int) string { return "L" + string(rune('0'+i%10)) })
	e.write(dir, "f.txt", base)
	e.write(dir, "g.txt", "g\n")
	e.commitAll(dir, "base")
	work := "TOP\n" + base + "BOTTOM\n"
	e.write(dir, "f.txt", work)
	e.write(dir, "g.txt", "g2\n")
	e.write(dir, "other.txt", "staged elsewhere\n")
	e.git(dir, "add", "other.txt")

	r := e.commit(dir, map[string]any{"message": "partial", "paths": []string{"f.txt", "g.txt"}, "hunks": map[string][]int{"f.txt": {1}}})
	committed := e.git(dir, "show", r.Hash+":f.txt")
	if !strings.HasSuffix(committed, "BOTTOM\n") || strings.HasPrefix(committed, "TOP") {
		t.Fatalf("partial content = %q", committed)
	}
	if g := e.git(dir, "show", r.Hash+":g.txt"); g != "g2\n" {
		t.Fatalf("whole file g.txt = %q", g)
	}
	if files := e.filesIn(dir, r.Hash); len(files) != 2 {
		t.Fatalf("partial commit files %v", files)
	}
	st := e.status(dir)
	if f := findFile(st, "f.txt"); f == nil || f.Staged || !f.Unstaged || f.Additions != 1 {
		t.Fatalf("remaining f.txt change: %+v", f)
	}
	if f := findFile(st, "other.txt"); f == nil || !f.Staged {
		t.Fatalf("other staged file lost: %+v", st.Files)
	}
	if e.read(dir, "f.txt") != work {
		t.Fatalf("worktree changed")
	}
}

func TestCommitDuringMergeAndInitialCommit(t *testing.T) {
	e := newEnv(t)
	// 首个提交（还没有 HEAD）用路径模式
	dir := filepath.Join(t.TempDir(), "fresh")
	e.git("", "init", "-q", dir)
	e.write(dir, "a.txt", "a\n")
	e.write(dir, "b.txt", "b\n")
	r := e.commit(dir, map[string]any{"message": "first", "paths": []string{"a.txt"}})
	if files := e.filesIn(dir, r.Hash); len(files) != 1 {
		t.Fatalf("first commit files %v", files)
	}

	// 合并冲突解决后提交
	dir = e.repo()
	e.write(dir, "f.txt", "base\n")
	e.commitAll(dir, "base")
	e.git(dir, "checkout", "-q", "-b", "x")
	e.write(dir, "f.txt", "x\n")
	e.commitAll(dir, "x")
	e.git(dir, "checkout", "-q", "main")
	e.write(dir, "f.txt", "main\n")
	e.commitAll(dir, "main")
	e.gitErr(dir, "merge", "x")
	e.write(dir, "f.txt", "merged\n")
	e.commit(dir, map[string]any{"message": "merge x", "paths": []string{"f.txt"}})
	if parents := strings.Fields(e.git(dir, "log", "-1", "--format=%P")); len(parents) != 2 {
		t.Fatalf("merge commit parents %v", parents)
	}
}

// 文件名里的通配字符按字面量处理（不会把 xy.txt 当成 x*.txt 一起提交）。
func TestCommitGlobCharsInPath(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	e.write(dir, "x*.txt", "a\n")
	e.write(dir, "xy.txt", "b\n")
	e.write(dir, "[ab].txt", "c\n")
	e.write(dir, "a.txt", "d\n")
	r := e.commit(dir, map[string]any{"message": "glob", "paths": []string{"x*.txt", "[ab].txt"}})
	files := e.filesIn(dir, r.Hash)
	if strings.Join(files, ",") != "[ab].txt,x*.txt" {
		t.Fatalf("committed %v", files)
	}
	var d diffResult
	e.write(dir, "x*.txt", "a2\n")
	e.must(dir, sdk.VCSOpDiff, map[string]any{"path": "x*.txt"}, &d)
	if len(d.Hunks) != 1 || d.Path != "x*.txt" {
		t.Fatalf("diff of glob name: %+v", d)
	}
}

// 没指定模式、文件又有已暂存内容时，界面若拿的是默认 HEAD→工作区 diff，靠块头自动对上。
func TestStageHunksHeaderFallback(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	base := lines(30, func(i int) string { return "L" + strings.Repeat("x", i) })
	e.write(dir, "f.txt", base)
	e.commitAll(dir, "base")
	staged := strings.Replace(base, "Lx\n", "first staged\n", 1)
	e.write(dir, "f.txt", staged)
	e.git(dir, "add", "f.txt")
	e.write(dir, "f.txt", strings.Replace(staged, "L"+strings.Repeat("x", 29)+"\n", "tail work\n", 1))
	d := e.diff(dir, map[string]any{"path": "f.txt"}) // 默认 head 模式：2 块
	if len(d.Hunks) != 2 {
		t.Fatalf("head hunks %d", len(d.Hunks))
	}
	e.op(dir, sdk.VCSOpStageHunks, map[string]any{"path": "f.txt", "hunks": []int{1}, "headers": []string{d.Hunks[1].Header}})
	if idx := e.index(dir, "f.txt"); !strings.Contains(idx, "tail work") || !strings.Contains(idx, "first staged") {
		t.Fatalf("index: %q", idx)
	}
}
