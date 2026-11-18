package git

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

func TestStash(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	if r := e.opFail(dir, sdk.VCSOpStashSave, map[string]any{"message": "nothing"}); r.Reason != "noChanges" {
		t.Fatalf("empty stash: %+v", r)
	}
	e.write(dir, "README.md", "changed\n")
	e.write(dir, "new.txt", "untracked\n")
	e.op(dir, sdk.VCSOpStashSave, map[string]any{"message": "my work", "includeUntracked": true})
	if st := e.status(dir); len(st.Files) != 0 || st.StashCount != 1 {
		t.Fatalf("after stash: %+v", st)
	}
	e.write(dir, "README.md", "second\n")
	e.op(dir, sdk.VCSOpStashSave, nil)
	var sl stashListResult
	e.must(dir, sdk.VCSOpStashList, nil, &sl)
	if len(sl.Stashes) != 2 || sl.Stashes[1].Message != "my work" || sl.Stashes[1].Branch != "main" || sl.Stashes[1].Index != 1 || sl.Stashes[0].Hash == "" {
		t.Fatalf("stash list: %+v", sl)
	}
	var ss stashShowResult
	e.must(dir, opStashShow, map[string]any{"index": 1}, &ss)
	if len(ss.Files) != 1 || len(ss.Untracked) != 1 || ss.Untracked[0] != "new.txt" {
		t.Fatalf("stash show: %+v", ss)
	}
	e.op(dir, sdk.VCSOpStashDrop, map[string]any{"index": 0})
	e.op(dir, sdk.VCSOpStashApply, map[string]any{"index": 0, "pop": true})
	if e.read(dir, "README.md") != "changed\n" || e.read(dir, "new.txt") != "untracked\n" {
		t.Fatalf("pop content")
	}
	e.must(dir, sdk.VCSOpStashList, nil, &sl)
	if len(sl.Stashes) != 0 {
		t.Fatalf("pop should drop")
	}
	// 冲突的恢复：储藏保留
	e.op(dir, sdk.VCSOpStashSave, nil)
	e.write(dir, "README.md", "conflicting\n")
	e.commitAll(dir, "conflict")
	r := e.opFail(dir, sdk.VCSOpStashApply, map[string]any{"index": 0, "pop": true})
	if r.Reason != "conflicts" {
		t.Fatalf("unstash conflict: %+v", r)
	}
	if e.status(dir).StashCount != 1 {
		t.Fatalf("stash should be kept after conflict")
	}
	e.git(dir, "reset", "-q", "--hard")
	// 恢复为新分支
	e.op(dir, sdk.VCSOpStashApply, map[string]any{"index": 0, "branch": "from-stash"})
	if cur := strings.TrimSpace(e.git(dir, "branch", "--show-current")); cur != "from-stash" {
		t.Fatalf("stash branch: %q", cur)
	}
}

func TestSubmodulesAndWorktrees(t *testing.T) {
	e := newEnv(t)
	sub := e.repo()
	dir := e.repo()
	e.git(dir, "submodule", "add", "-q", sub, "libs/sub mod")
	e.commitAll(dir, "add submodule")
	var sm submodulesResult
	e.must(dir, sdk.VCSOpSubmodules, nil, &sm)
	if len(sm.Submodules) != 1 || sm.Submodules[0].Path != "libs/sub mod" || sm.Submodules[0].Status != "ok" || sm.Submodules[0].URL != sub {
		t.Fatalf("submodules: %+v", sm)
	}
	// 克隆后未初始化 → submoduleUpdate
	clone := filepath.Join(t.TempDir(), "clone")
	e.git("", "clone", "-q", dir, clone)
	e.must(clone, sdk.VCSOpSubmodules, nil, &sm)
	if sm.Submodules[0].Status != "uninitialized" {
		t.Fatalf("uninitialized: %+v", sm)
	}
	e.op(clone, sdk.VCSOpSubmoduleUpdate, map[string]any{"init": true, "recursive": true})
	e.must(clone, sdk.VCSOpSubmodules, nil, &sm)
	if sm.Submodules[0].Status != "ok" || sm.Submodules[0].Head == "" {
		t.Fatalf("after update: %+v", sm)
	}
	// 子模块里提交 → modified
	inner := filepath.Join(clone, "libs", "sub mod")
	e.git(inner, "checkout", "-q", "-b", "x")
	e.write(inner, "y.txt", "y")
	e.commitAll(inner, "inner")
	e.must(clone, sdk.VCSOpSubmodules, nil, &sm)
	if sm.Submodules[0].Status != "modified" {
		t.Fatalf("modified: %+v", sm)
	}
	// 子模块自身也能当仓库用（.git 是文件）
	if st := e.status(inner); st.Branch != "x" {
		t.Fatalf("status inside submodule: %+v", st)
	}

	// 工作树
	wt := filepath.Join(t.TempDir(), "wt one")
	e.op(dir, sdk.VCSOpWorktreeAdd, map[string]any{"path": wt, "branch": "wt-branch", "newBranch": true})
	var wl worktreesResult
	e.must(dir, sdk.VCSOpWorktrees, nil, &wl)
	if len(wl.Worktrees) != 2 || !wl.Worktrees[0].Main || !wl.Worktrees[0].Current || wl.Worktrees[1].Branch != "wt-branch" {
		t.Fatalf("worktrees: %+v", wl)
	}
	var bs branchesResult
	e.must(dir, sdk.VCSOpBranches, nil, &bs)
	for _, b := range bs.Local {
		if b.Name == "wt-branch" && b.Worktree == "" {
			t.Fatalf("branch checked out in worktree not marked: %+v", b)
		}
	}
	e.write(wt, "dirty.txt", "x")
	if r := e.opFail(dir, sdk.VCSOpWorktreeRemove, map[string]any{"path": wt}); r.Reason != "localChanges" {
		t.Fatalf("remove dirty worktree: %+v", r)
	}
	e.op(dir, sdk.VCSOpWorktreeRemove, map[string]any{"path": wt, "force": true})
	e.must(dir, sdk.VCSOpWorktrees, nil, &wl)
	if len(wl.Worktrees) != 1 {
		t.Fatalf("worktree not removed")
	}
	// 已有分支 / 分离
	e.op(dir, sdk.VCSOpWorktreeAdd, map[string]any{"path": filepath.Join(t.TempDir(), "wt2"), "branch": "wt-branch"})
	e.op(dir, sdk.VCSOpWorktreeAdd, map[string]any{"path": filepath.Join(t.TempDir(), "wt3"), "start": "HEAD"})
}

func TestConfigIgnoreCleanInitRepos(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	var cfg sdk.VCSConfig
	e.must(dir, sdk.VCSOpConfigGet, map[string]any{"keys": []string{"user.name", "User.Email", "pull.rebase"}}, &cfg)
	if cfg.Values["user.name"] != "Tester" || cfg.Values["User.Email"] != "tester@example.com" || cfg.Scopes["user.name"] != "global" {
		t.Fatalf("configGet: %+v", cfg)
	}
	if _, ok := cfg.Values["pull.rebase"]; ok {
		t.Fatalf("unset key should be absent")
	}
	e.op(dir, sdk.VCSOpConfigSet, map[string]any{"key": "user.name", "value": "Repo Name"})
	e.must(dir, sdk.VCSOpConfigGet, map[string]any{"keys": []string{"user.name"}}, &cfg)
	if cfg.Values["user.name"] != "Repo Name" || cfg.Scopes["user.name"] != "local" {
		t.Fatalf("local override: %+v", cfg)
	}
	e.must(dir, sdk.VCSOpConfigGet, map[string]any{"keys": []string{"user.name"}, "global": true}, &cfg)
	if cfg.Values["user.name"] != "Tester" {
		t.Fatalf("global only: %+v", cfg)
	}
	e.op(dir, sdk.VCSOpConfigSet, map[string]any{"key": "user.name", "unset": true})
	e.op(dir, sdk.VCSOpConfigSet, map[string]any{"key": "user.name", "unset": true}) // 不存在也算成功
	if err := e.call(dir, sdk.VCSOpConfigSet, map[string]any{"key": "bad key", "value": "x"}, nil); reasonOfErr(err) != "invalidConfigKey" {
		t.Fatalf("invalid key: %v", err)
	}

	// ignore：去重、补换行、exclude
	e.write(dir, ".gitignore", "node_modules")
	e.op(dir, sdk.VCSOpIgnore, map[string]any{"patterns": []string{"node_modules", "*.log", "dist/", "*.log"}})
	if got := e.read(dir, ".gitignore"); got != "node_modules\n*.log\ndist/\n" {
		t.Fatalf(".gitignore = %q", got)
	}
	e.op(dir, sdk.VCSOpIgnore, map[string]any{"patterns": []string{"secret.env"}, "file": "exclude"})
	if got := e.read(dir, ".git/info/exclude"); !strings.HasSuffix(got, "secret.env\n") {
		t.Fatalf("exclude = %q", got)
	}
	e.write(dir, "secret.env", "x")
	e.write(dir, "a.log", "x")
	e.write(dir, "trash/x.tmp", "x")
	e.write(dir, "keep.txt", "x")
	st := e.status(dir)
	if findFile(st, "secret.env") != nil || findFile(st, "a.log") != nil {
		t.Fatalf("ignored files listed: %+v", st.Files)
	}
	// clean 只删指定的未跟踪文件
	if err := e.call(dir, sdk.VCSOpClean, map[string]any{}, nil); err == nil {
		t.Fatalf("clean without paths must require all=true")
	}
	r := e.op(dir, sdk.VCSOpClean, map[string]any{"paths": []string{"trash"}})
	if len(r.Files) != 1 || exists(filepath.Join(dir, "trash")) || !exists(filepath.Join(dir, "keep.txt")) {
		t.Fatalf("clean: %+v", r)
	}

	// init / detect / repos
	root := t.TempDir()
	nr := filepath.Join(root, "new repo")
	e.op(nr, sdk.VCSOpInit, map[string]any{"initialBranch": "trunk"})
	if err := e.call(nr, sdk.VCSOpInit, nil, nil); reasonOfErr(err) != "alreadyRepo" {
		t.Fatalf("init twice: %v", err)
	}
	var info sdk.VCSRepoInfo
	e.must(filepath.Join(nr), sdk.VCSOpDetect, nil, &info)
	if !info.IsRepo || info.VCS != "git" {
		t.Fatalf("detect: %+v", info)
	}
	e.must(root, sdk.VCSOpDetect, nil, &info)
	if info.IsRepo {
		t.Fatalf("detect non-repo: %+v", info)
	}
	nested := filepath.Join(nr, "packages", "nested")
	e.git("", "init", "-q", nested)
	e.git("", "init", "-q", filepath.Join(nr, "node_modules", "dep"))
	e.git("", "init", "-q", filepath.Join(root, "other"))
	wt := filepath.Join(root, "wt")
	e.write(nr, "f.txt", "x")
	e.git(nr, "add", "f.txt")
	e.git(nr, "commit", "-q", "-m", "c")
	e.git(nr, "worktree", "add", "-q", wt)
	var rl repoListResult
	e.must(root, sdk.VCSOpRepos, nil, &rl)
	kinds := map[string]string{}
	for _, r := range rl.Repos {
		rel, _ := filepath.Rel(root, r.Root)
		kinds[filepath.ToSlash(rel)] = r.Kind + "|" + filepath.Base(r.Parent)
	}
	want := map[string]string{"new repo": "repo|.", "new repo/packages/nested": "repo|new repo", "other": "repo|.", "wt": "worktree|."}
	for k, v := range want {
		got := kinds[k]
		if !strings.HasPrefix(got, strings.Split(v, "|")[0]) {
			t.Errorf("repos[%s] = %q, want %q (all %v)", k, got, v, kinds)
		}
	}
	if _, ok := kinds["new repo/node_modules/dep"]; ok {
		t.Errorf("node_modules should be skipped")
	}
	if kinds["new repo/packages/nested"] != "repo|new repo" {
		t.Errorf("nested parent: %q", kinds["new repo/packages/nested"])
	}
}

func TestRemotesOps(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	e.op(dir, sdk.VCSOpRemoteSet, map[string]any{"name": "origin", "fetchUrl": "git@github.com:owner/repo.git"})
	e.op(dir, sdk.VCSOpRemoteSet, map[string]any{"name": "my.fork", "fetchUrl": "https://gitee.com/me/repo.git", "pushUrl": "git@gitee.com:me/repo.git"})
	var rs remotesResult
	e.must(dir, sdk.VCSOpRemotes, nil, &rs)
	byName := func() map[string]remoteInfo {
		m := map[string]remoteInfo{}
		for _, r := range rs.Remotes {
			m[r.Name] = r
		}
		return m
	}
	if m := byName(); len(m) != 2 || m["my.fork"].PushURL != "git@gitee.com:me/repo.git" || m["origin"].WebURL != "https://github.com/owner/repo" {
		t.Fatalf("remotes: %+v", rs)
	}
	e.op(dir, sdk.VCSOpRemoteSet, map[string]any{"name": "fork", "oldName": "my.fork", "fetchUrl": "https://gitee.com/me/repo2.git"})
	rs = remotesResult{}
	e.must(dir, sdk.VCSOpRemotes, nil, &rs)
	if f := byName()["fork"]; f.FetchURL != "https://gitee.com/me/repo2.git" || f.PushURL != "" {
		t.Fatalf("rename/update: %+v", rs.Remotes)
	}
	e.op(dir, sdk.VCSOpRemoteRemove, map[string]any{"ref": "fork"})
	e.must(dir, sdk.VCSOpRemotes, nil, &rs)
	if len(rs.Remotes) != 1 {
		t.Fatalf("remove: %+v", rs)
	}
	if err := e.call(dir, sdk.VCSOpRemoteSet, map[string]any{"name": "-x", "fetchUrl": "u"}, nil); reasonOfErr(err) != "invalidRemoteName" {
		t.Fatalf("invalid remote: %v", err)
	}
}

func TestWebURLs(t *testing.T) {
	cases := map[string]string{
		"git@github.com:owner/repo.git":                   "https://github.com/owner/repo",
		"https://user:tok@github.com/owner/repo.git":      "https://github.com/owner/repo",
		"ssh://git@gitlab.example.com:2222/grp/sub/r.git": "https://gitlab.example.com/grp/sub/r",
		"https://gitee.com/a/b":                           "https://gitee.com/a/b",
		"git@ssh.dev.azure.com:v3/org/proj/repo":          "https://dev.azure.com/org/proj/_git/repo",
		"/local/path/repo.git":                            "",
		"C:/repos/x.git":                                  "",
		"http://192.168.1.10:3000/team/app.git":           "https://192.168.1.10:3000/team/app",
	}
	for in, want := range cases {
		if got := webBaseURL(in); got != want {
			t.Errorf("webBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
	if u := webCommitURL("https://gitlab.com/a/b", "abc"); u != "https://gitlab.com/a/b/-/commit/abc" {
		t.Errorf("gitlab commit url %q", u)
	}
	if u := webFileURL("https://github.com/a/b", "main", "src/中 文.go"); u != "https://github.com/a/b/blob/main/src/%E4%B8%AD%20%E6%96%87.go" {
		t.Errorf("file url %q", u)
	}
}

func TestClassifyFailure(t *testing.T) {
	cases := map[string]string{
		"fatal: Authentication failed for 'https://x'":                                            "needsAuth",
		"git@github.com: Permission denied (publickey).\nfatal: Could not read from remote":       "needsAuth",
		"fatal: could not read Username for 'https://github.com': terminal prompts disabled":      "needsAuth",
		" ! [rejected]        main -> main (fetch first)":                                         "rejected",
		" ! [remote rejected] main -> main (pre-receive hook declined)":                           "remoteRejected",
		"fatal: unable to access 'https://x/': Could not resolve host: x":                         "network",
		"fatal: unable to access 'https://x/': The requested URL returned error: 403":             "needsAuth",
		"error: Your local changes to the following files would be overwritten by checkout:\n\ta": "localChanges",
		"*** Please tell me who you are.":                                                         "identityMissing",
		"fatal: Unable to create '/x/.git/index.lock': File exists.":                              "locked",
		"remote: Repository not found.\nfatal: repository 'https://x' not found":                  "repoNotFound",
	}
	for in, want := range cases {
		if got := classifyFailure(in); got != want {
			t.Errorf("classify(%q) = %s, want %s", in, got, want)
		}
	}
	files := parseIndentedFiles("error: Your local changes to the following files would be overwritten by merge:\n\ta b.txt\n\tc.txt\nPlease commit", "would be overwritten")
	if len(files) != 2 || files[0] != "a b.txt" {
		t.Errorf("parseIndentedFiles = %v", files)
	}
}

func TestGitNotFound(t *testing.T) {
	e := newEnv(t)
	e.ctx.Init.Settings["gitPath"] = filepath.Join(t.TempDir(), "no-git")
	err := e.call(t.TempDir(), sdk.VCSOpStatus, nil, nil)
	if reasonOfErr(err) != "gitNotFound" {
		t.Fatalf("want gitNotFound, got %v", err)
	}
	if e, ok := err.(*sdk.Error); !ok || e.Code != sdk.ErrCodeUserError {
		t.Fatalf("not a user error: %v", err)
	}
}

func TestViewsAndActions(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	e.git(dir, "remote", "add", "origin", "git@github.com:owner/repo.git")
	e.git(dir, "tag", "v0.1")
	proj := &sdk.ProjectInfo{ID: "p", Name: "p", Root: dir}
	for _, v := range []string{viewRemotes, viewTags, viewSubmodules, viewWorktrees, viewSettings} {
		res, err := e.p.RenderView(e.ctx, &sdk.ViewRenderParams{View: v, Project: proj})
		if err != nil {
			t.Fatalf("render %s: %v", v, err)
		}
		b, _ := json.Marshal(res)
		if strings.Contains(string(b), `"alert","tone":"danger"`) || res.State["root"] != dir {
			t.Fatalf("render %s: %s", v, b)
		}
	}
	// 事件：添加远程、编辑、新建标签、设置身份
	ev := func(view, event string, values map[string]any, state map[string]any) *sdk.ViewEventResult {
		t.Helper()
		r, err := e.p.HandleViewEvent(e.ctx, &sdk.ViewEventParams{View: view, Event: event, Values: values, Project: proj, State: state})
		if err != nil {
			t.Fatalf("event %s: %v", event, err)
		}
		return r
	}
	ev(viewRemotes, "remote.save", map[string]any{"name": "up", "url": "https://github.com/up/repo.git"}, nil)
	r := ev(viewRemotes, "remote.edit", map[string]any{"name": "up"}, nil)
	if r.State["editing"] != "up" {
		t.Fatalf("edit state: %+v", r.State)
	}
	ev(viewRemotes, "remote.save", map[string]any{"name": "upstream", "url": "https://github.com/up/repo.git"}, r.State)
	if !strings.Contains(e.git(dir, "remote"), "upstream") {
		t.Fatalf("remote rename via view failed")
	}
	ev(viewTags, "tag.create", map[string]any{"name": "v0.2", "message": "second"}, nil)
	if !strings.Contains(e.git(dir, "tag"), "v0.2") {
		t.Fatalf("tag create via view")
	}
	r = ev(viewTags, "tag.delete", map[string]any{"name": "v0.1"}, nil)
	if strings.Contains(e.git(dir, "tag"), "v0.1") {
		t.Fatalf("tag delete via view: %+v", r)
	}
	ev(viewSettings, "settings.localIdentity", map[string]any{"name": "Repo Person", "email": ""}, nil)
	if got := strings.TrimSpace(e.git(dir, "config", "--local", "user.name")); got != "Repo Person" {
		t.Fatalf("local identity: %q", got)
	}
	ev(viewSettings, "settings.behavior", map[string]any{"pullRebase": "true", "fetchPrune": true, "autoSetupRemote": false, "autocrlf": "unset"}, nil)
	if got := strings.TrimSpace(e.git(dir, "config", "--local", "pull.rebase")); got != "true" {
		t.Fatalf("pull.rebase: %q", got)
	}
	// 非仓库
	res, err := e.p.RenderView(e.ctx, &sdk.ViewRenderParams{View: viewTags, Project: &sdk.ProjectInfo{Root: t.TempDir()}})
	if err != nil || res.Root.Type != "empty" {
		t.Fatalf("non-repo view: %+v %v", res, err)
	}

	// 动作
	act := func(id string, args map[string]any) (*sdk.InvokeActionResult, error) {
		return e.p.InvokeAction(e.ctx, &sdk.InvokeActionParams{Action: id, Project: *proj, Args: args})
	}
	ar, err := act(actOpenCommit, map[string]any{"hash": "HEAD"})
	if err != nil || !strings.HasPrefix(ar.OpenURL, "https://github.com/owner/repo/commit/") {
		t.Fatalf("open commit: %+v %v", ar, err)
	}
	ar, err = act(actOpenFile, map[string]any{"path": "README.md"})
	if err != nil || !strings.Contains(ar.OpenURL, "/blob/") {
		t.Fatalf("open file: %+v %v", ar, err)
	}
	out := filepath.Join(t.TempDir(), "x.patch")
	if _, err := act(actPatchCommits, map[string]any{"hash": "HEAD", "output": out}); err != nil || !strings.Contains(e.read(filepath.Dir(out), "x.patch"), "Subject: [PATCH] init") {
		t.Fatalf("patch from commit: %v", err)
	}
	e.write(dir, "README.md", "changed\n")
	e.write(dir, "brand.txt", "new\n")
	out2 := filepath.Join(t.TempDir(), "changes.patch")
	if _, err := act(actPatchChanges, map[string]any{"paths": []any{"README.md", "brand.txt"}, "output": out2}); err != nil {
		t.Fatalf("patch from changes: %v", err)
	}
	patch := e.read(filepath.Dir(out2), "changes.patch")
	if !strings.Contains(patch, "+changed") || !strings.Contains(patch, "+new") {
		t.Fatalf("changes patch: %s", patch)
	}
	// 补丁能应用回去
	e.git(dir, "stash", "-u", "-q")
	e.git(dir, "apply", out2)
	if e.read(dir, "brand.txt") != "new\n" {
		t.Fatalf("patch does not apply")
	}
	// 清理分支：已合并的删除，未合并的保留
	e.git(dir, "checkout", "-q", "-f", "-b", "merged-one")
	e.git(dir, "checkout", "-q", "main")
	e.git(dir, "checkout", "-q", "-b", "unmerged-one")
	e.write(dir, "u.txt", "u")
	e.commitAll(dir, "u")
	e.git(dir, "checkout", "-q", "main")
	ar, err = act(actCleanBranches, nil)
	if err != nil || strings.Contains(e.git(dir, "branch"), "merged-one\n") && !strings.Contains(e.git(dir, "branch"), "unmerged-one") {
		t.Fatalf("cleanup: %+v %v", ar, err)
	}
	if b := e.git(dir, "branch"); strings.Contains(b, " merged-one") || !strings.Contains(b, "unmerged-one") {
		t.Fatalf("cleanup result: %s", b)
	}
	if _, err := act(actOptimize, nil); err != nil {
		t.Fatalf("optimize: %v", err)
	}
	_ = os.Remove(out)
}
