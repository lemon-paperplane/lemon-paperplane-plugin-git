package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

func (e *tenv) head(dir string) string {
	return strings.TrimSpace(e.git(dir, "rev-parse", "HEAD"))
}

func TestUndoCommit(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	e.write(dir, "a.txt", "a\n")
	c := e.commitAll(dir, "add a")
	r := e.op(dir, sdk.VCSOpUndoCommit, nil)
	if r.Hash != c || e.logMsgs(dir)[0] != "init" {
		t.Fatalf("undo: %+v %v", r, e.logMsgs(dir))
	}
	if f := findFile(e.status(dir), "a.txt"); f == nil || f.Status != "A" || !f.Staged {
		t.Fatalf("changes should stay staged: %+v", f)
	}
	// 首个提交
	e.git(dir, "commit", "-q", "-m", "again")
	fresh := filepath.Join(t.TempDir(), "fresh")
	e.git("", "init", "-q", fresh)
	e.write(fresh, "x.txt", "x\n")
	e.commitAll(fresh, "root")
	e.op(fresh, sdk.VCSOpUndoCommit, nil)
	st := e.status(fresh)
	if !st.Unborn || findFile(st, "x.txt") == nil || findFile(st, "x.txt").Status != "A" {
		t.Fatalf("undo root commit: %+v", st)
	}
	// 已推送的提交默认不撤销
	remote := filepath.Join(t.TempDir(), "r.git")
	e.git("", "init", "-q", "--bare", remote)
	e.git(dir, "remote", "add", "origin", remote)
	e.git(dir, "push", "-q", "-u", "origin", "main")
	if err := e.call(dir, sdk.VCSOpUndoCommit, nil, nil); reasonOfErr(err) != "alreadyPushed" {
		t.Fatalf("pushed: %v", err)
	}
	e.op(dir, sdk.VCSOpUndoCommit, map[string]any{"force": true})
}

func TestReword(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	e.write(dir, "a.txt", "a\n")
	e.commitAll(dir, "second")
	e.write(dir, "b.txt", "b\n")
	e.commitAll(dir, "third")
	// HEAD：amend，不带上已暂存内容
	e.write(dir, "staged.txt", "s\n")
	e.git(dir, "add", "staged.txt")
	e.op(dir, sdk.VCSOpReword, map[string]any{"message": "third reworded"})
	if e.logMsgs(dir)[0] != "third reworded" || len(e.filesIn(dir, "HEAD")) != 1 {
		t.Fatalf("reword head: %v %v", e.logMsgs(dir), e.filesIn(dir, "HEAD"))
	}
	// 更早的提交：工作区有修改也能做，工作区和暂存区不动
	e.write(dir, "README.md", "dirty\n")
	second := strings.TrimSpace(e.git(dir, "rev-parse", "HEAD~1"))
	treeBefore := e.git(dir, "rev-parse", "HEAD^{tree}")
	authorBefore := e.git(dir, "log", "-1", "--format=%an %ae %ad", second)
	e.op(dir, sdk.VCSOpReword, map[string]any{"hash": second, "message": "second reworded\n\n#123 keep hash line"})
	msgs := e.logMsgs(dir)
	if msgs[0] != "third reworded" || msgs[1] != "second reworded" || msgs[2] != "init" {
		t.Fatalf("reword older: %v", msgs)
	}
	if e.git(dir, "rev-parse", "HEAD^{tree}") != treeBefore {
		t.Fatalf("tree changed")
	}
	if body := e.git(dir, "log", "-1", "--format=%B", "HEAD~1"); !strings.Contains(body, "#123 keep hash line") {
		t.Fatalf("comment-like line lost: %q", body)
	}
	if a := e.git(dir, "log", "-1", "--format=%an %ae %ad", "HEAD~1"); a != authorBefore {
		t.Fatalf("author changed: %q vs %q", a, authorBefore)
	}
	st := e.status(dir)
	if findFile(st, "README.md") == nil || findFile(st, "staged.txt") == nil || !findFile(st, "staged.txt").Staged {
		t.Fatalf("worktree/index changed: %+v", st.Files)
	}
	// 根提交
	root := strings.TrimSpace(e.git(dir, "rev-list", "--max-parents=0", "HEAD"))
	e.op(dir, sdk.VCSOpReword, map[string]any{"hash": root, "message": "root reworded"})
	if msgs := e.logMsgs(dir); msgs[2] != "root reworded" || len(msgs) != 3 {
		t.Fatalf("reword root: %v", msgs)
	}
}

func TestRebaseInteractive(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	var hs []string
	for _, n := range []string{"c1", "c2", "c3", "c4", "c5"} {
		e.write(dir, n+".txt", n+"\n")
		hs = append(hs, e.commitAll(dir, n))
	}
	base := strings.TrimSpace(e.git(dir, "rev-parse", "HEAD~5"))
	e.write(dir, "README.md", "dirty worktree\n") // 自动储藏
	plan := map[string]any{"base": base, "steps": []map[string]any{
		{"hash": hs[0], "action": "pick"},
		{"hash": hs[2], "action": "reword", "message": "c3 reworded"},
		{"hash": hs[1], "action": "squash"}, // 重排 + 压缩（默认合并信息）
		{"hash": hs[3], "action": "drop"},
		{"hash": hs[4], "action": "fixup"},
	}}
	e.op(dir, sdk.VCSOpRebaseInteractive, plan)
	msgs := e.logMsgs(dir)
	if len(msgs) != 3 || msgs[0] != "c3 reworded" || msgs[1] != "c1" || msgs[2] != "init" {
		t.Fatalf("log after rebase: %v", msgs)
	}
	body := e.git(dir, "log", "-1", "--format=%B")
	if !strings.Contains(body, "c3 reworded") || !strings.Contains(body, "c2") || strings.Contains(body, "c5") || strings.Contains(body, "#") {
		t.Fatalf("squashed message: %q", body)
	}
	files := e.filesIn(dir, "HEAD")
	if strings.Join(files, ",") != "c2.txt,c3.txt,c5.txt" {
		t.Fatalf("squashed files: %v", files)
	}
	if _, err := os.Stat(filepath.Join(dir, "c4.txt")); err == nil {
		t.Fatalf("dropped commit still present")
	}
	if e.read(dir, "README.md") != "dirty worktree\n" {
		t.Fatalf("autostash not restored")
	}
	if exists(filepath.Join(dir, ".git", "rebase-merge")) {
		t.Fatalf("rebase still in progress")
	}

	// 自定义 squash 信息
	h1 := e.head(dir)
	e.write(dir, "x.txt", "x\n")
	h2 := e.commitAll(dir, "x")
	e.op(dir, sdk.VCSOpRebaseInteractive, map[string]any{"base": h1 + "~1", "steps": []map[string]any{
		{"hash": h1, "action": "pick"},
		{"hash": h2, "action": "squash", "message": "combined custom"},
	}})
	if msgs := e.logMsgs(dir, "-1"); msgs[0] != "combined custom" {
		t.Fatalf("custom squash message: %v", msgs)
	}

	// 计划不完整被拒
	err := e.call(dir, sdk.VCSOpRebaseInteractive, map[string]any{"base": "HEAD~2", "steps": []map[string]any{{"hash": "HEAD", "action": "pick"}}}, nil)
	if reasonOfErr(err) != "planMismatch" {
		t.Fatalf("incomplete plan: %v", err)
	}
}

func TestRebaseInteractiveConflictContinue(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	e.write(dir, "f.txt", "1\n")
	a := e.commitAll(dir, "a")
	e.write(dir, "f.txt", "2\n")
	b := e.commitAll(dir, "b")
	e.write(dir, "g.txt", "g\n")
	c := e.commitAll(dir, "c")
	// 交换 a、b 的顺序会冲突；c 要改信息（冲突后继续时仍要用计划里的信息）
	r := e.opFail(dir, sdk.VCSOpRebaseInteractive, map[string]any{"base": a + "~1", "steps": []map[string]any{
		{"hash": b, "action": "pick"}, {"hash": a, "action": "pick"}, {"hash": c, "action": "reword", "message": "c after conflict"},
	}})
	if r.Reason != "conflicts" || len(r.Conflicts) != 1 {
		t.Fatalf("expected conflict: %+v", r)
	}
	if st := e.status(dir); st.Operation != "rebase" || st.OperationDetail.Total != 3 {
		t.Fatalf("operation: %+v", st.OperationDetail)
	}
	e.op(dir, sdk.VCSOpResolve, map[string]any{"path": "f.txt", "side": "theirs"})
	for i := 0; i < 3; i++ {
		var res opResult
		e.must(dir, sdk.VCSOpContinue, nil, &res)
		if res.OK {
			break
		}
		if len(res.Conflicts) > 0 {
			e.op(dir, sdk.VCSOpResolve, map[string]any{"path": res.Conflicts[0], "side": "theirs"})
		}
	}
	if st := e.status(dir); st.Operation != "" {
		t.Fatalf("still in operation %q", st.Operation)
	}
	if msgs := e.logMsgs(dir, "-1"); msgs[0] != "c after conflict" {
		t.Fatalf("reword after continue: %v", e.logMsgs(dir))
	}
}

func TestMergeConflictResolveContinue(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	e.write(dir, "f.txt", "base\n")
	e.write(dir, "g.txt", "base\n")
	e.write(dir, "d.txt", "base\n")
	e.commitAll(dir, "base")
	e.git(dir, "checkout", "-q", "-b", "feature")
	e.write(dir, "f.txt", "feature\n")
	e.write(dir, "g.txt", "feature\n")
	os.Remove(filepath.Join(dir, "d.txt"))
	e.commitAll(dir, "feature work")
	e.git(dir, "checkout", "-q", "main")
	e.write(dir, "f.txt", "main\n")
	e.write(dir, "g.txt", "main\n")
	e.write(dir, "d.txt", "main changed\n")
	e.commitAll(dir, "main work")

	r := e.opFail(dir, sdk.VCSOpMerge, map[string]any{"ref": "feature", "noFF": true})
	if r.Reason != "conflicts" || len(r.Conflicts) != 3 {
		t.Fatalf("merge conflicts: %+v", r)
	}
	// 冲突标记还在时标记解决会被拦下
	var mr opResult
	e.must(dir, sdk.VCSOpResolve, map[string]any{"path": "g.txt", "side": "resolved"}, &mr)
	if mr.OK || mr.Reason != "markersRemain" {
		t.Fatalf("markers check: %+v", mr)
	}
	e.op(dir, sdk.VCSOpResolve, map[string]any{"path": "f.txt", "side": "ours"})
	e.write(dir, "g.txt", "hand merged\n")
	e.op(dir, sdk.VCSOpResolve, map[string]any{"path": "g.txt", "side": "resolved"})
	// continue 时还有冲突
	var cr opResult
	e.must(dir, sdk.VCSOpContinue, nil, &cr)
	if cr.OK || cr.Reason != "unresolvedConflicts" {
		t.Fatalf("continue with conflicts: %+v", cr)
	}
	e.op(dir, sdk.VCSOpResolve, map[string]any{"path": "d.txt", "side": "theirs"}) // 对方删除了
	e.op(dir, sdk.VCSOpContinue, nil)
	if e.read(dir, "f.txt") != "main\n" || e.read(dir, "g.txt") != "hand merged\n" || exists(filepath.Join(dir, "d.txt")) {
		t.Fatalf("merge result wrong")
	}
	if p := strings.Fields(e.git(dir, "log", "-1", "--format=%P")); len(p) != 2 {
		t.Fatalf("not a merge commit")
	}
	// 已是最新
	if r := e.op(dir, sdk.VCSOpMerge, map[string]any{"ref": "feature"}); r.Reason != "upToDate" {
		t.Fatalf("up to date: %+v", r)
	}
	// squash
	e.git(dir, "checkout", "-q", "-b", "sq")
	e.write(dir, "s.txt", "s\n")
	e.commitAll(dir, "s")
	e.git(dir, "checkout", "-q", "main")
	if r := e.op(dir, sdk.VCSOpMerge, map[string]any{"ref": "sq", "squash": true}); r.Reason != "squashed" {
		t.Fatalf("squash: %+v", r)
	}
	if f := findFile(e.status(dir), "s.txt"); f == nil || !f.Staged {
		t.Fatalf("squash should stage changes")
	}
}

func TestRebaseConflictAbortAndSkip(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	e.write(dir, "f.txt", "base\n")
	e.commitAll(dir, "base")
	e.git(dir, "checkout", "-q", "-b", "topic")
	e.write(dir, "f.txt", "topic\n")
	e.commitAll(dir, "topic")
	before := e.head(dir)
	e.git(dir, "checkout", "-q", "main")
	e.write(dir, "f.txt", "main\n")
	e.commitAll(dir, "main")
	e.git(dir, "checkout", "-q", "topic")
	e.write(dir, "README.md", "local change\n")

	r := e.opFail(dir, sdk.VCSOpRebase, map[string]any{"ref": "main"})
	if r.Reason != "conflicts" {
		t.Fatalf("rebase conflict: %+v", r)
	}
	if e.status(dir).Operation != "rebase" {
		t.Fatalf("no rebase in progress")
	}
	e.op(dir, sdk.VCSOpAbort, nil)
	if e.head(dir) != before || e.status(dir).Operation != "" || e.read(dir, "README.md") != "local change\n" {
		t.Fatalf("abort did not restore state")
	}
	if err := e.call(dir, sdk.VCSOpAbort, nil, nil); reasonOfErr(err) != "noOperation" {
		t.Fatalf("abort without op: %v", err)
	}
	// 跳过冲突的提交
	e.opFail(dir, sdk.VCSOpRebase, map[string]any{"ref": "main"})
	e.op(dir, opSkip, nil)
	if e.status(dir).Operation != "" || e.read(dir, "f.txt") != "main\n" {
		t.Fatalf("skip failed")
	}
}

func TestCherryPickRevertReset(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	e.git(dir, "checkout", "-q", "-b", "src")
	var picks []string
	for _, n := range []string{"p1", "p2", "p3"} {
		e.write(dir, "log.txt", strings.Repeat(n+"\n", 1)+e.readOr(dir, "log.txt"))
		picks = append(picks, e.commitAll(dir, n))
	}
	e.git(dir, "checkout", "-q", "main")
	// 选择顺序打乱（界面从日志选的通常是新到旧），插件按祖先在前应用
	e.op(dir, sdk.VCSOpCherryPick, map[string]any{"refs": []string{picks[2], picks[0], picks[1]}})
	if msgs := e.logMsgs(dir); strings.Join(msgs, ",") != "p3,p2,p1,init" {
		t.Fatalf("cherry-pick order: %v", msgs)
	}
	e.op(dir, sdk.VCSOpRevert, map[string]any{"refs": []string{"HEAD~1", "HEAD"}})
	msgs := e.logMsgs(dir)
	if !strings.HasPrefix(msgs[0], "Revert \"p2\"") || !strings.HasPrefix(msgs[1], "Revert \"p3\"") {
		t.Fatalf("revert order: %v", msgs)
	}
	if e.read(dir, "log.txt") != "p1\n" {
		t.Fatalf("revert content: %q", e.read(dir, "log.txt"))
	}
	// 冲突的拣选
	e.write(dir, "log.txt", "conflict\n")
	e.commitAll(dir, "conflicting")
	r := e.opFail(dir, sdk.VCSOpCherryPick, map[string]any{"ref": picks[2]})
	if r.Reason != "conflicts" || e.status(dir).Operation != "cherry-pick" {
		t.Fatalf("cherry-pick conflict: %+v", r)
	}
	e.op(dir, sdk.VCSOpAbort, nil)
	// reset
	target := strings.TrimSpace(e.git(dir, "rev-parse", "HEAD~2"))
	e.op(dir, sdk.VCSOpReset, map[string]any{"ref": target, "mode": "soft"})
	if e.head(dir) != target || len(e.status(dir).Files) == 0 {
		t.Fatalf("soft reset")
	}
	e.op(dir, sdk.VCSOpReset, map[string]any{"ref": "HEAD", "mode": "hard"})
	if len(e.status(dir).Files) != 0 {
		t.Fatalf("hard reset left changes")
	}
	if err := e.call(dir, sdk.VCSOpReset, map[string]any{"ref": "--hard"}, nil); reasonOfErr(err) != "invalidRef" {
		t.Fatalf("option injection: %v", err)
	}
}

func (e *tenv) readOr(dir, rel string) string {
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return ""
	}
	return string(b)
}

func TestDiscardAndCheckoutFile(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	e.write(dir, "m.txt", "v1\n")
	e.write(dir, "old.txt", "rename\nme\nplease\n")
	v1 := e.commitAll(dir, "v1")
	e.write(dir, "m.txt", "v2\n")
	e.commitAll(dir, "v2")
	e.write(dir, "m.txt", "dirty\n")
	e.git(dir, "add", "m.txt")
	e.write(dir, "m.txt", "dirtier\n")
	e.write(dir, "added.txt", "a\n")
	e.git(dir, "add", "added.txt")
	e.git(dir, "mv", "old.txt", "new.txt")
	e.write(dir, "untracked.txt", "u\n")

	e.write(dir, "keep-untracked.txt", "k\n")
	// keepNew：新文件只取消暂存，未跟踪文件保留（IDEA Rollback 默认）
	e.op(dir, sdk.VCSOpDiscard, map[string]any{"paths": []string{"added.txt", "keep-untracked.txt"}, "keepNew": true})
	st := e.status(dir)
	if f := findFile(st, "added.txt"); f == nil || f.Status != "?" || findFile(st, "keep-untracked.txt") == nil {
		t.Fatalf("keepNew: %+v", st.Files)
	}
	e.git(dir, "add", "added.txt")
	// 默认：回到 HEAD 的样子，新文件和未跟踪文件删除
	e.op(dir, sdk.VCSOpDiscard, map[string]any{"paths": []string{"m.txt", "added.txt", "new.txt", "untracked.txt"}})
	st = e.status(dir)
	if e.read(dir, "m.txt") != "v2\n" || e.read(dir, "old.txt") != "rename\nme\nplease\n" {
		t.Fatalf("restore failed")
	}
	if exists(filepath.Join(dir, "added.txt")) || exists(filepath.Join(dir, "untracked.txt")) || exists(filepath.Join(dir, "new.txt")) {
		t.Fatalf("new files should be deleted: %+v", st.Files)
	}
	if len(st.Files) != 1 || findFile(st, "keep-untracked.txt") == nil {
		t.Fatalf("status after discard: %+v", st.Files)
	}
	// 只丢弃未暂存部分
	e.write(dir, "m.txt", "staged\n")
	e.git(dir, "add", "m.txt")
	e.write(dir, "m.txt", "unstaged\n")
	e.op(dir, sdk.VCSOpDiscard, map[string]any{"paths": []string{"m.txt"}, "mode": "unstaged"})
	if e.read(dir, "m.txt") != "staged\n" {
		t.Fatalf("unstaged discard: %q", e.read(dir, "m.txt"))
	}
	e.op(dir, sdk.VCSOpCheckoutFile, map[string]any{"path": "m.txt", "ref": v1})
	if e.read(dir, "m.txt") != "v1\n" || e.index(dir, "m.txt") != "v1\n" {
		t.Fatalf("checkoutFile")
	}
}
