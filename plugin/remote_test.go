package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// remoteSetup：一个 bare 远程 + 两个克隆（模拟两个人）。
func remoteSetup(t *testing.T) (e *tenv, bare, a, b string) {
	e = newEnv(t)
	tmp := t.TempDir()
	bare = filepath.Join(tmp, "remote.git")
	e.git("", "init", "-q", "--bare", bare)
	a = e.repo()
	e.git(a, "remote", "add", "origin", bare)
	e.git(a, "push", "-q", "-u", "origin", "main")
	b = filepath.Join(tmp, "b")
	e.git("", "clone", "-q", bare, b)
	return
}

func TestPushPullFetch(t *testing.T) {
	e, bare, a, b := remoteSetup(t)
	// 新分支：推送预览 + 推送并设置上游
	e.git(a, "checkout", "-q", "-b", "feature/x")
	e.write(a, "f.txt", "1\n")
	e.commitAll(a, "f1")
	e.write(a, "f.txt", "2\n")
	e.commitAll(a, "f2")
	var pv pushPreviewResult
	e.must(a, sdk.VCSOpPushPreview, nil, &pv)
	if !pv.NewBranch || pv.Remote != "origin" || pv.RemoteRef != "refs/heads/feature/x" || len(pv.Commits) != 2 || !pv.SetUpstream {
		t.Fatalf("preview new branch: %+v", pv)
	}
	e.op(a, sdk.VCSOpPush, nil)
	if up := strings.TrimSpace(e.git(a, "rev-parse", "--abbrev-ref", "@{u}")); up != "origin/feature/x" {
		t.Fatalf("upstream = %q", up)
	}
	if r := e.op(a, sdk.VCSOpPush, nil); r.Reason != "upToDate" {
		t.Fatalf("second push: %+v", r)
	}

	// b 推送到 main，a 的 main 落后
	e.write(b, "b.txt", "b\n")
	e.commitAll(b, "from b")
	e.git(b, "push", "-q")
	e.git(a, "checkout", "-q", "main")
	e.write(a, "a.txt", "a\n")
	e.commitAll(a, "from a")
	// 预览：远端信息要先 fetch 才知道
	e.op(a, sdk.VCSOpFetch, map[string]any{"prune": true})
	pv = pushPreviewResult{}
	e.must(a, sdk.VCSOpPushPreview, nil, &pv)
	if pv.NewBranch || pv.Behind != 1 || len(pv.Commits) != 1 {
		t.Fatalf("preview behind: %+v", pv)
	}
	r := e.opFail(a, sdk.VCSOpPush, nil)
	if !r.Rejected || r.Reason != "rejected" {
		t.Fatalf("push should be rejected: %+v", r)
	}
	// 拉取（变基）后再推
	r = e.op(a, sdk.VCSOpPull, map[string]any{"mode": "rebase"})
	if r.Message != "result.pulled" {
		t.Fatalf("pull: %+v", r)
	}
	e.op(a, sdk.VCSOpPush, nil)
	if e.git(bare, "log", "-1", "--format=%s", "main") != "from a\n" {
		t.Fatalf("remote main not updated")
	}
	// b 拉取（合并）
	r = e.op(b, sdk.VCSOpPull, nil)
	if e.read(b, "a.txt") != "a\n" {
		t.Fatalf("pull merge failed: %+v", r)
	}
	if r := e.op(b, sdk.VCSOpPull, nil); r.Reason != "upToDate" {
		t.Fatalf("up to date pull: %+v", r)
	}
	// 拉取冲突
	e.write(a, "c.txt", "a\n")
	e.commitAll(a, "c from a")
	e.git(a, "push", "-q")
	e.write(b, "c.txt", "b\n")
	e.commitAll(b, "c from b")
	r = e.opFail(b, sdk.VCSOpPull, map[string]any{"mode": "merge"})
	if r.Reason != "conflicts" || len(r.Conflicts) != 1 || r.Conflicts[0] != "c.txt" {
		t.Fatalf("pull conflict: %+v", r)
	}
	e.op(b, sdk.VCSOpAbort, nil)
	// 强推（--force-with-lease）
	e.git(b, "fetch", "-q")
	e.git(b, "reset", "-q", "--hard", "origin/main")
	e.write(b, "z.txt", "z\n")
	e.git(b, "add", "z.txt")
	e.git(b, "commit", "-q", "--amend", "-m", "rewritten")
	e.opFail(b, sdk.VCSOpPush, nil)
	e.op(b, sdk.VCSOpPush, map[string]any{"force": true})
	// 没有上游的分支拉取
	e.git(b, "checkout", "-q", "-b", "lonely")
	if r := e.opFail(b, sdk.VCSOpPull, nil); r.Reason != "noUpstream" {
		t.Fatalf("no upstream pull: %+v", r)
	}
	// 删除远程分支
	e.op(a, sdk.VCSOpBranchDelete, map[string]any{"name": "origin/feature/x", "remote": true})
	if _, err := e.gitErr(bare, "rev-parse", "--verify", "refs/heads/feature/x"); err == nil {
		t.Fatalf("remote branch not deleted")
	}
	// 推送到不存在的远程：网络/仓库错误识别
	e.git(a, "remote", "add", "broken", filepath.Join(t.TempDir(), "nope.git"))
	r = e.opFail(a, sdk.VCSOpPush, map[string]any{"remote": "broken"})
	if r.Reason != "repoNotFound" {
		t.Fatalf("broken remote: %+v", r)
	}
}

func TestCloneAndTags(t *testing.T) {
	e, bare, a, _ := remoteSetup(t)
	dest := filepath.Join(t.TempDir(), "clone dir")
	var cr cloneResult
	e.must("", sdk.VCSOpClone, map[string]any{"url": bare, "dest": dest, "depth": 1}, &cr)
	if !cr.OK || cr.Path != dest || !sdk.FileExists(filepath.Join(dest, "README.md")) {
		t.Fatalf("clone: %+v", cr)
	}
	if err := e.call("", sdk.VCSOpClone, map[string]any{"url": bare, "dest": dest}, nil); reasonOfErr(err) != "destNotEmpty" {
		t.Fatalf("clone into non-empty: %v", err)
	}
	e.must("", sdk.VCSOpClone, map[string]any{"url": filepath.Join(t.TempDir(), "missing.git"), "dest": filepath.Join(t.TempDir(), "x")}, &cr)
	if cr.OK {
		t.Fatalf("clone of missing repo should fail")
	}

	// 标签
	e.op(a, sdk.VCSOpTagCreate, map[string]any{"name": "v1.0", "message": "Release 1.0"})
	e.op(a, sdk.VCSOpTagCreate, map[string]any{"name": "light", "ref": "HEAD"})
	var tags tagsResult
	e.must(a, sdk.VCSOpTags, nil, &tags)
	if len(tags.Tags) != 2 {
		t.Fatalf("tags: %+v", tags)
	}
	var v1 tagInfo
	for _, tg := range tags.Tags {
		if tg.Name == "v1.0" {
			v1 = tg
		}
	}
	if !v1.Annotated || v1.Message != "Release 1.0" || v1.Hash != e.head(a) || v1.Subject != "init" {
		t.Fatalf("annotated tag: %+v", v1)
	}
	e.op(a, opTagPush, map[string]any{"ref": "v1.0"})
	if _, err := e.gitErr(bare, "rev-parse", "--verify", "refs/tags/v1.0"); err != nil {
		t.Fatalf("tag not pushed")
	}
	// 推送全部标签
	e.write(a, "n.txt", "n\n")
	e.commitAll(a, "n")
	e.op(a, sdk.VCSOpPush, map[string]any{"tags": true})
	if _, err := e.gitErr(bare, "rev-parse", "--verify", "refs/tags/light"); err != nil {
		t.Fatalf("tags not pushed with push.tags")
	}
	// 删除本地 + 远程标签
	e.op(a, sdk.VCSOpTagDelete, map[string]any{"ref": "v1.0", "remote": "origin"})
	if _, err := e.gitErr(bare, "rev-parse", "--verify", "refs/tags/v1.0"); err == nil {
		t.Fatalf("remote tag not deleted")
	}
	if err := e.call(a, sdk.VCSOpTagCreate, map[string]any{"name": "bad name"}, nil); reasonOfErr(err) != "invalidTagName" {
		t.Fatalf("invalid tag name: %v", err)
	}
}

func TestBranchesAndCheckout(t *testing.T) {
	e, bare, a, b := remoteSetup(t)
	e.git(b, "checkout", "-q", "-b", "remote-only")
	e.write(b, "r.txt", "r\n")
	e.commitAll(b, "r")
	e.git(b, "push", "-q", "-u", "origin", "remote-only")
	e.git(b, "checkout", "-q", "-b", "gone-branch")
	e.git(b, "push", "-q", "-u", "origin", "gone-branch")
	e.git(a, "fetch", "-q")
	_ = bare

	// 远程分支检出：自动建同名跟踪分支
	r := e.op(a, sdk.VCSOpCheckout, map[string]any{"ref": "origin/remote-only"})
	if !strings.Contains(r.Message, "checkedOutTracking") {
		t.Fatalf("checkout remote: %+v", r)
	}
	var bs branchesResult
	e.must(a, sdk.VCSOpBranches, nil, &bs)
	if bs.Current != "remote-only" {
		t.Fatalf("current = %q", bs.Current)
	}
	var local branchInfo
	for _, br := range bs.Local {
		if br.Name == "remote-only" {
			local = br
		}
	}
	if local.Upstream != "origin/remote-only" || !local.Current || local.Hash == "" || local.Date == "" || local.Subject != "r" {
		t.Fatalf("local branch info: %+v", local)
	}
	foundRemote := false
	for _, br := range bs.Remote {
		if br.Name == "origin/HEAD" {
			t.Fatalf("symref listed")
		}
		if br.Name == "origin/main" && br.Remote == "origin" {
			foundRemote = true
		}
	}
	if !foundRemote || len(bs.Recent) == 0 || bs.Recent[0] != "remote-only" {
		t.Fatalf("remote/recent: %+v %v", bs.Remote, bs.Recent)
	}
	// 上游已删除 + ahead
	e.op(a, sdk.VCSOpCheckout, map[string]any{"ref": "origin/gone-branch"})
	e.write(a, "ahead.txt", "x\n")
	e.commitAll(a, "ahead")
	e.git(b, "push", "-q", "origin", "--delete", "gone-branch")
	e.git(a, "fetch", "-q", "--prune")
	e.must(a, sdk.VCSOpBranches, nil, &bs)
	for _, br := range bs.Local {
		if br.Name == "gone-branch" && !br.Gone {
			t.Fatalf("gone not detected: %+v", br)
		}
		if br.Name == "remote-only" && (br.Ahead != 0 || br.Behind != 0) {
			t.Fatalf("remote-only ahead/behind: %+v", br)
		}
	}
	// 本地修改冲突的检出：报原因与文件；smart 储藏后检出再恢复
	e.git(a, "checkout", "-q", "main")
	e.write(a, "README.md", "main side\n")
	e.commitAll(a, "readme main")
	e.git(a, "checkout", "-q", "remote-only")
	e.write(a, "README.md", "local edit\n")
	fr := e.opFail(a, sdk.VCSOpCheckout, map[string]any{"ref": "main"})
	if fr.Reason != "localChanges" || len(fr.Files) != 1 || fr.Files[0] != "README.md" {
		t.Fatalf("local changes: %+v", fr)
	}
	sr := e.opFail(a, sdk.VCSOpCheckout, map[string]any{"ref": "main", "smart": true})
	if sr.Reason != "conflicts" {
		t.Fatalf("smart checkout with conflict: %+v", sr)
	}
	e.git(a, "checkout", "-q", "-f", "remote-only")
	e.git(a, "stash", "drop", "-q")
	// 不重叠的本地修改：smart 检出后两边的修改都在
	e.git(a, "checkout", "-q", "main")
	multi := lines(10, func(i int) string { return "m" + string(rune('0'+i%10)) })
	e.write(a, "multi.txt", multi)
	e.commitAll(a, "multi")
	e.git(a, "checkout", "-q", "-b", "s1")
	e.write(a, "multi.txt", strings.Replace(multi, "m1\n", "m1 changed on s1\n", 1))
	e.commitAll(a, "s1 change")
	e.git(a, "checkout", "-q", "main")
	e.write(a, "multi.txt", strings.Replace(multi, "m9\n", "m9 local\n", 1))
	e.opFail(a, sdk.VCSOpCheckout, map[string]any{"ref": "s1"})
	e.op(a, sdk.VCSOpCheckout, map[string]any{"ref": "s1", "smart": true})
	if got := e.read(a, "multi.txt"); !strings.Contains(got, "m1 changed on s1") || !strings.Contains(got, "m9 local") {
		t.Fatalf("smart checkout content: %q", got)
	}
	e.git(a, "checkout", "-q", "-f", "remote-only")
	// 强制检出丢弃修改
	e.write(a, "README.md", "throw away\n")
	e.op(a, sdk.VCSOpCheckout, map[string]any{"ref": "remote-only", "force": true})
	if e.read(a, "README.md") != "hello\n" {
		t.Fatalf("force checkout kept changes")
	}
	// 新建、改名、删除、设置上游、分离头
	e.op(a, sdk.VCSOpBranchCreate, map[string]any{"name": "topic", "start": "main", "checkout": true})
	if cur, _ := (&repo{p: e.p, ctx: e.ctx, root: a, git: "git"}).currentBranch(); cur != "topic" {
		t.Fatalf("branchCreate checkout: %q", cur)
	}
	if err := e.call(a, sdk.VCSOpBranchCreate, map[string]any{"name": "bad..name"}, nil); reasonOfErr(err) != "invalidBranchName" {
		t.Fatalf("invalid name: %v", err)
	}
	e.op(a, sdk.VCSOpBranchRename, map[string]any{"old": "topic", "new": "topic2"})
	e.op(a, sdk.VCSOpSetUpstream, map[string]any{"upstream": "origin/topic2"}) // 远端还没有：直接写配置
	if up := e.git(a, "config", "branch.topic2.merge"); strings.TrimSpace(up) != "refs/heads/topic2" {
		t.Fatalf("setUpstream config: %q", up)
	}
	e.op(a, sdk.VCSOpSetUpstream, map[string]any{"upstream": ""})
	e.write(a, "t.txt", "t\n")
	e.commitAll(a, "t")
	e.git(a, "checkout", "-q", "main")
	dr := e.opFail(a, sdk.VCSOpBranchDelete, map[string]any{"name": "topic2"})
	if dr.Reason != "notFullyMerged" || dr.Hash == "" {
		t.Fatalf("delete unmerged: %+v", dr)
	}
	e.op(a, sdk.VCSOpBranchDelete, map[string]any{"name": "topic2", "force": true})
	if err := e.call(a, sdk.VCSOpBranchDelete, map[string]any{"name": "main"}, nil); reasonOfErr(err) != "currentBranch" {
		t.Fatalf("delete current: %v", err)
	}
	r = e.op(a, sdk.VCSOpCheckout, map[string]any{"ref": "HEAD~1"})
	if !strings.Contains(r.Message, "Detached") {
		t.Fatalf("detached: %+v", r)
	}
	e.must(a, sdk.VCSOpBranches, nil, &bs)
	if !bs.Detached || bs.Current != "" {
		t.Fatalf("detached branches: %+v", bs.Current)
	}
	os.Remove(filepath.Join(a, "nothing"))
}
