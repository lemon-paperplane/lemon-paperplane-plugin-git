package git

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// ---------------------------------------------------------------------------
// stash

type stashInfo struct {
	Index   int    `json:"index"`
	Message string `json:"message"`
	Branch  string `json:"branch,omitempty"`
	Date    string `json:"date,omitempty"`
	Hash    string `json:"hash,omitempty"` // 储藏提交，可用 show / diff(commit) 查看内容
}

type stashListResult struct {
	Stashes []stashInfo `json:"stashes"`
}

var stashSubjectRe = regexp.MustCompile(`^(?:WIP on|On) ([^:]+): ?(.*)$`)

func (r *repo) stashList() (*stashListResult, error) {
	out := &stashListResult{Stashes: []stashInfo{}}
	if !r.refExists("refs/stash") {
		return out, nil
	}
	text, err := r.out("stash", "list", "--format=%gd%x1f%gs%x1f%cI%x1f%H%x1e")
	if err != nil {
		return nil, err
	}
	for _, rec := range strings.Split(text, "\x1e") {
		rec = strings.Trim(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 4)
		if len(f) < 4 {
			continue
		}
		s := stashInfo{Message: f[1], Date: f[2], Hash: f[3]}
		if i := strings.Index(f[0], "{"); i >= 0 {
			s.Index, _ = strconv.Atoi(strings.TrimSuffix(f[0][i+1:], "}"))
		}
		if m := stashSubjectRe.FindStringSubmatch(f[1]); m != nil {
			s.Branch = m[1]
			s.Message = m[2]
			if strings.HasPrefix(f[1], "WIP on") {
				// 默认信息是「哈希 主题」：保留原文
				s.Message = f[1]
			}
		}
		out.Stashes = append(out.Stashes, s)
	}
	return out, nil
}

type stashSaveArgs struct {
	sdk.VCSStashSaveArgs
	Paths     []string `json:"paths,omitempty"`     // 只储藏这些文件
	KeepIndex bool     `json:"keepIndex,omitempty"` // 保留暂存区
	Staged    bool     `json:"staged,omitempty"`    // 只储藏已暂存的（git 2.35+）
}

func (r *repo) stashSave(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[stashSaveArgs](raw)
	if err != nil {
		return nil, err
	}
	if err := r.checkIdentity(); err != nil {
		return nil, err
	}
	args := []string{"stash", "push"}
	if a.IncludeUntracked {
		args = append(args, "--include-untracked")
	}
	if a.KeepIndex {
		args = append(args, "--keep-index")
	}
	if a.Staged {
		args = append(args, "--staged")
	}
	if msg := strings.TrimSpace(a.Message); msg != "" {
		args = append(args, "-m", msg)
	}
	paths, err := r.checkPaths(a.Paths)
	if err != nil {
		return nil, err
	}
	if len(paths) > 0 {
		args = r.pathspecArgs(args, paths)
	}
	before := r.stashCount()
	res, err := r.run(cmdOpts{console: true, title: "git stash"}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.failResult(res), nil
	}
	if r.stashCount() == before || strings.Contains(res.combined(), "No local changes to save") {
		return &opResult{VCSOpResult: sdk.VCSOpResult{OK: false, Message: tr(r.ctx, "result.nothingToStash")}, Reason: "noChanges"}, nil
	}
	return okResult(tr(r.ctx, "result.stashed")), nil
}

type stashArgs struct {
	sdk.VCSStashArgs
	Index2 bool   `json:"restoreIndex,omitempty"` // 同时恢复暂存区（--index，IDEA 的 Reinstate index）
	Branch string `json:"branch,omitempty"`       // 恢复为新分支（git stash branch）
	All    bool   `json:"all,omitempty"`          // stashDrop：清空全部储藏
}

func stashRef(i int) string { return "stash@{" + strconv.Itoa(i) + "}" }

func (r *repo) stashApply(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[stashArgs](raw)
	if err != nil {
		return nil, err
	}
	if a.Index < 0 {
		return nil, invalidParams("index must be >= 0")
	}
	var args []string
	switch {
	case a.Branch != "":
		if err := r.checkBranchName(a.Branch); err != nil {
			return nil, err
		}
		args = []string{"stash", "branch", a.Branch, stashRef(a.Index)}
	case a.Pop:
		args = []string{"stash", "pop"}
	default:
		args = []string{"stash", "apply"}
	}
	if a.Branch == "" {
		if a.Index2 {
			args = append(args, "--index")
		}
		args = append(args, stashRef(a.Index))
	}
	res, err := r.run(cmdOpts{console: true, title: "git stash"}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		if files := r.unmergedFiles(); len(files) > 0 {
			out := r.conflictResult(res, files)
			out.Message = tr(r.ctx, "result.unstashConflicts", len(files))
			return out, nil
		}
		return r.failResult(res), nil
	}
	return okResult(tr(r.ctx, "result.unstashed")), nil
}

func (r *repo) stashDrop(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[stashArgs](raw)
	if err != nil {
		return nil, err
	}
	args := []string{"stash", "drop", stashRef(a.Index)}
	if a.All {
		args = []string{"stash", "clear"}
	}
	res, err := r.run(cmdOpts{}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.failResult(res), nil
	}
	return okResult(tr(r.ctx, "result.stashDropped")), nil
}

type stashShowResult struct {
	Hash      string       `json:"hash"`
	Files     []fileChange `json:"files"`
	Untracked []string     `json:"untracked,omitempty"` // 储藏里的未跟踪文件
}

// stashShow 列出储藏的改动文件（相对储藏时的 HEAD）以及其中的未跟踪文件。
func (r *repo) stashShow(raw json.RawMessage) (*stashShowResult, error) {
	a, err := decodeArgs[sdk.VCSStashArgs](raw)
	if err != nil {
		return nil, err
	}
	ref := stashRef(a.Index)
	hash := r.revParse(ref)
	if hash == "" {
		return nil, userError(r.ctx, "invalidRef", "error.invalidRef", ref)
	}
	files, err := r.treeChanges(hash+"^1", hash)
	if err != nil {
		return nil, err
	}
	out := &stashShowResult{Hash: hash, Files: files}
	if u := r.revParse(hash + "^3"); u != "" {
		if t, err := r.out("ls-tree", "-r", "-z", "--name-only", u); err == nil {
			for _, p := range strings.Split(t, "\x00") {
				if p != "" {
					out.Untracked = append(out.Untracked, p)
				}
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// tags

type tagInfo struct {
	Name      string `json:"name"`
	Hash      string `json:"hash"` // 指向的提交
	Subject   string `json:"subject,omitempty"`
	Date      string `json:"date,omitempty"`
	Annotated bool   `json:"annotated,omitempty"`
	Message   string `json:"message,omitempty"` // 附注标签的说明
	Tagger    string `json:"tagger,omitempty"`
}

type tagsResult struct {
	Tags []tagInfo `json:"tags"`
}

func (r *repo) tags() (*tagsResult, error) {
	out, err := r.out("for-each-ref", "--sort=-creatordate",
		"--format=%(refname)%1f%(objecttype)%1f%(objectname)%1f%(*objectname)%1f%(creatordate:iso-strict)%1f%(contents:subject)%1f%(*subject)%1f%(subject)%1f%(taggername)%1e",
		"refs/tags")
	if err != nil {
		return nil, err
	}
	res := &tagsResult{Tags: []tagInfo{}}
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.Trim(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.Split(rec, "\x1f")
		if len(f) < 9 {
			continue
		}
		t := tagInfo{Name: strings.TrimPrefix(f[0], "refs/tags/"), Date: f[4]}
		if f[1] == "tag" {
			t.Annotated = true
			t.Hash = f[3]
			t.Message = f[5]
			t.Subject = f[6]
			t.Tagger = f[8]
		} else {
			t.Hash = f[2]
			t.Subject = f[7]
		}
		res.Tags = append(res.Tags, t)
	}
	return res, nil
}

type tagCreateArgs struct {
	sdk.VCSTagCreateArgs
	Force  bool   `json:"force,omitempty"`
	Push   bool   `json:"push,omitempty"`   // 创建后推送到远程
	Remote string `json:"remote,omitempty"` // 推送到哪个远程，默认 origin
}

func (r *repo) tagCreate(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[tagCreateArgs](raw)
	if err != nil {
		return nil, err
	}
	if err := r.checkRef(a.Name); err != nil {
		return nil, err
	}
	if res, err := r.run(cmdOpts{read: true}, "check-ref-format", "refs/tags/"+a.Name); err != nil || !res.ok() {
		return nil, userError(r.ctx, "invalidTagName", "error.invalidTagName", a.Name)
	}
	args := []string{"tag"}
	if a.Force {
		args = append(args, "--force")
	}
	if msg := normalizeMessage(a.Message); msg != "" {
		if err := r.checkIdentity(); err != nil {
			return nil, err
		}
		file, cleanup, err := r.msgFile(msg)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		args = append(args, "-a", "-F", file)
	}
	args = append(args, a.Name)
	if a.Ref != "" {
		if err := r.checkRef(a.Ref); err != nil {
			return nil, err
		}
		args = append(args, a.Ref)
	}
	res, err := r.run(cmdOpts{}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.failResult(res), nil
	}
	if a.Push {
		b, _ := json.Marshal(tagPushArgs{Ref: a.Name, Remote: a.Remote, Force: a.Force})
		return r.tagPush(b)
	}
	return okResult(tr(r.ctx, "result.tagCreated", a.Name)), nil
}

func (r *repo) tagDelete(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[refArgs](raw)
	if err != nil {
		return nil, err
	}
	name := strings.TrimPrefix(a.Ref, "refs/tags/")
	if err := r.checkRef(name); err != nil {
		return nil, err
	}
	res, err := r.run(cmdOpts{}, "tag", "-d", name)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.failResult(res), nil
	}
	if a.Remote != "" {
		b, _ := json.Marshal(tagPushArgs{Ref: name, Remote: a.Remote, Delete: true})
		return r.tagPush(b)
	}
	return okResult(tr(r.ctx, "result.tagDeleted", name)), nil
}

// ---------------------------------------------------------------------------
// remotes

type remoteInfo struct {
	Name     string   `json:"name"`
	FetchURL string   `json:"fetchUrl"`
	PushURL  string   `json:"pushUrl,omitempty"`
	Fetch    []string `json:"fetch,omitempty"`  // fetch 引用规格
	WebURL   string   `json:"webUrl,omitempty"` // 网页地址（GitHub/GitLab/Gitee 等）
}

type remotesResult struct {
	Remotes []remoteInfo `json:"remotes"`
}

func (r *repo) remotesOp() (*remotesResult, error) {
	list, err := r.remotes()
	if err != nil {
		return nil, err
	}
	return &remotesResult{Remotes: list}, nil
}

func (r *repo) remotes() ([]remoteInfo, error) {
	names := r.remoteNames()
	out := []remoteInfo{}
	if len(names) == 0 {
		return out, nil
	}
	res, err := r.run(cmdOpts{read: true}, "config", "-z", "--get-regexp", `^remote\..*\.(url|pushurl|fetch)$`)
	if err != nil {
		return nil, err
	}
	byName := map[string]*remoteInfo{}
	for _, n := range names {
		out = append(out, remoteInfo{Name: n})
	}
	for i := range out {
		byName[out[i].Name] = &out[i]
	}
	for _, rec := range strings.Split(res.out, "\x00") {
		k, v, ok := strings.Cut(rec, "\n")
		if !ok {
			continue
		}
		rest := strings.TrimPrefix(k, "remote.")
		dot := strings.LastIndex(rest, ".")
		if dot < 0 {
			continue
		}
		name, key := rest[:dot], rest[dot+1:]
		ri := byName[name]
		if ri == nil {
			continue
		}
		switch key {
		case "url":
			if ri.FetchURL == "" {
				ri.FetchURL = v
			}
		case "pushurl":
			if ri.PushURL == "" {
				ri.PushURL = v
			}
		case "fetch":
			ri.Fetch = append(ri.Fetch, v)
		}
	}
	for i := range out {
		out[i].WebURL = webBaseURL(out[i].FetchURL)
	}
	return out, nil
}

type remoteSetArgs struct {
	sdk.VCSRemote
	OldName string `json:"oldName,omitempty"` // 改名：原来的名字
}

var remoteNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

func (r *repo) remoteSet(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[remoteSetArgs](raw)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(a.Name)
	url := strings.TrimSpace(a.FetchURL)
	if !remoteNameRe.MatchString(name) {
		return nil, userError(r.ctx, "invalidRemoteName", "error.invalidRemoteName", name)
	}
	if url == "" || strings.HasPrefix(url, "-") {
		return nil, userError(r.ctx, "invalidUrl", "error.invalidUrl", url)
	}
	names := r.remoteNames()
	if a.OldName != "" && a.OldName != name {
		if !contains(names, a.OldName) {
			return nil, userError(r.ctx, "invalidRef", "error.remoteNotFound", a.OldName)
		}
		if contains(names, name) {
			return nil, userError(r.ctx, "remoteExists", "error.remoteExists", name)
		}
		if _, err := r.do(cmdOpts{}, "remote", "rename", a.OldName, name); err != nil {
			return nil, err
		}
		names = r.remoteNames()
	}
	created := false
	if contains(names, name) {
		if _, err := r.do(cmdOpts{}, "remote", "set-url", name, url); err != nil {
			return nil, err
		}
	} else {
		if _, err := r.do(cmdOpts{}, "remote", "add", name, url); err != nil {
			return nil, err
		}
		created = true
	}
	push := strings.TrimSpace(a.PushURL)
	if push != "" && push != url {
		if strings.HasPrefix(push, "-") {
			return nil, userError(r.ctx, "invalidUrl", "error.invalidUrl", push)
		}
		_, _ = r.run(cmdOpts{}, "config", "--unset-all", "remote."+name+".pushurl")
		if _, err := r.do(cmdOpts{}, "config", "--add", "remote."+name+".pushurl", push); err != nil {
			return nil, err
		}
	} else {
		_, _ = r.run(cmdOpts{}, "config", "--unset-all", "remote."+name+".pushurl")
	}
	if created {
		return okResult(tr(r.ctx, "result.remoteAdded", name)), nil
	}
	return okResult(tr(r.ctx, "result.remoteUpdated", name)), nil
}

func (r *repo) remoteRemove(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSRefArgs](raw)
	if err != nil {
		return nil, err
	}
	if err := r.checkRef(a.Ref); err != nil {
		return nil, err
	}
	res, err := r.run(cmdOpts{}, "remote", "remove", a.Ref)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.failResult(res), nil
	}
	return okResult(tr(r.ctx, "result.remoteRemoved", a.Ref)), nil
}

// ---------------------------------------------------------------------------
// submodules

type submoduleInfo struct {
	Path     string `json:"path"`
	URL      string `json:"url,omitempty"`
	Head     string `json:"head,omitempty"`
	Status   string `json:"status"` // ok | uninitialized | modified | conflict
	Name     string `json:"name,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Describe string `json:"describe,omitempty"`
}

type submodulesResult struct {
	Submodules []submoduleInfo `json:"submodules"`
}

func (r *repo) submodules() (*submodulesResult, error) {
	out := &submodulesResult{Submodules: []submoduleInfo{}}
	gm := filepath.Join(r.root, ".gitmodules")
	if !sdk.FileExists(gm) {
		return out, nil
	}
	res, err := r.run(cmdOpts{read: true}, "config", "-z", "--file", ".gitmodules", "--get-regexp", `^submodule\.`)
	if err != nil {
		return nil, err
	}
	type cfg struct{ name, path, url, branch string }
	var order []string
	byName := map[string]*cfg{}
	for _, rec := range strings.Split(res.out, "\x00") {
		k, v, ok := strings.Cut(rec, "\n")
		if !ok {
			continue
		}
		rest := strings.TrimPrefix(k, "submodule.")
		dot := strings.LastIndex(rest, ".")
		if dot < 0 {
			continue
		}
		name, key := rest[:dot], rest[dot+1:]
		c := byName[name]
		if c == nil {
			c = &cfg{name: name}
			byName[name] = c
			order = append(order, name)
		}
		switch key {
		case "path":
			c.path = v
		case "url":
			c.url = v
		case "branch":
			c.branch = v
		}
	}
	// 实际状态：git submodule status（- 未初始化，+ 检出的提交与记录的不同，U 冲突）
	st := map[string]submoduleInfo{}
	if s, err := r.run(cmdOpts{read: true}, "submodule", "status"); err == nil && s.ok() {
		for _, line := range strings.Split(s.out, "\n") {
			if len(line) < 42 {
				continue
			}
			flag := line[0]
			rest := line[1:]
			sp := strings.IndexByte(rest, ' ')
			if sp < 0 {
				continue
			}
			hash, p := rest[:sp], rest[sp+1:]
			desc := ""
			if i := strings.LastIndex(p, " ("); i >= 0 && strings.HasSuffix(p, ")") {
				desc = p[i+2 : len(p)-1]
				p = p[:i]
			}
			info := submoduleInfo{Path: p, Head: hash, Describe: desc, Status: "ok"}
			switch flag {
			case '-':
				info.Status = "uninitialized"
				info.Head = ""
			case '+':
				info.Status = "modified"
			case 'U':
				info.Status = "conflict"
			}
			st[p] = info
		}
	}
	for _, n := range order {
		c := byName[n]
		info, ok := st[c.path]
		if !ok {
			info = submoduleInfo{Path: c.path, Status: "uninitialized"}
		}
		info.Name, info.URL, info.Branch = c.name, c.url, c.branch
		out.Submodules = append(out.Submodules, info)
	}
	return out, nil
}

func (r *repo) submoduleUpdate(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSSubmoduleUpdateArgs](raw)
	if err != nil {
		return nil, err
	}
	paths, err := r.checkPaths(a.Paths)
	if err != nil {
		return nil, err
	}
	args := []string{"submodule", "update", "--progress"}
	if a.Init {
		args = append(args, "--init")
	}
	if a.Recursive {
		args = append(args, "--recursive")
	}
	if a.Remote {
		args = append(args, "--remote")
	}
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	r.progress(tr(r.ctx, "progress.submodule"))
	res, err := r.run(netOpts("git submodule update"), args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.netFailure(res)
	}
	return okResult(tr(r.ctx, "result.submodulesUpdated")), nil
}

// ---------------------------------------------------------------------------
// worktrees

type worktreeInfo struct {
	Path     string `json:"path"`
	Branch   string `json:"branch,omitempty"`
	Head     string `json:"head,omitempty"`
	Main     bool   `json:"main,omitempty"`
	Locked   bool   `json:"locked,omitempty"`
	Detached bool   `json:"detached,omitempty"`
	Bare     bool   `json:"bare,omitempty"`
	Prunable bool   `json:"prunable,omitempty"` // 目录已不存在
	Current  bool   `json:"current,omitempty"`  // 就是当前这个工作树
}

type worktreesResult struct {
	Worktrees []worktreeInfo `json:"worktrees"`
}

func (r *repo) worktrees() (*worktreesResult, error) {
	args := []string{"worktree", "list", "--porcelain"}
	sep := "\n"
	if r.ver.atLeast(2, 36) {
		args = append(args, "-z")
		sep = "\x00"
	}
	text, err := r.out(args...)
	if err != nil {
		return nil, err
	}
	out := &worktreesResult{Worktrees: []worktreeInfo{}}
	var cur *worktreeInfo
	self, _ := filepath.EvalSymlinks(r.root)
	flush := func() {
		if cur != nil {
			cur.Main = len(out.Worktrees) == 0
			if p, _ := filepath.EvalSymlinks(cur.Path); p != "" && p == self {
				cur.Current = true
			}
			out.Worktrees = append(out.Worktrees, *cur)
			cur = nil
		}
	}
	for _, l := range strings.Split(text, sep) {
		if l == "" {
			flush()
			continue
		}
		k, v, _ := strings.Cut(l, " ")
		switch k {
		case "worktree":
			flush()
			cur = &worktreeInfo{Path: filepath.FromSlash(v)}
		case "HEAD":
			if cur != nil {
				cur.Head = v
			}
		case "branch":
			if cur != nil {
				cur.Branch = strings.TrimPrefix(v, "refs/heads/")
			}
		case "detached":
			if cur != nil {
				cur.Detached = true
			}
		case "bare":
			if cur != nil {
				cur.Bare = true
			}
		case "locked":
			if cur != nil {
				cur.Locked = true
			}
		case "prunable":
			if cur != nil {
				cur.Prunable = true
			}
		}
	}
	flush()
	return out, nil
}

func (r *repo) worktreeAdd(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSWorktreeAddArgs](raw)
	if err != nil {
		return nil, err
	}
	p := sdk.Expand(strings.TrimSpace(a.Path))
	if p == "" || strings.HasPrefix(p, "-") {
		return nil, invalidParams("path is required")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.root, p)
	}
	args := []string{"worktree", "add"}
	switch {
	case a.NewBranch:
		if err := r.checkBranchName(a.Branch); err != nil {
			return nil, err
		}
		args = append(args, "-b", a.Branch, p)
		if a.Start != "" {
			if err := r.checkRef(a.Start); err != nil {
				return nil, err
			}
			args = append(args, a.Start)
		}
	case a.Branch != "":
		if err := r.checkRef(a.Branch); err != nil {
			return nil, err
		}
		if !r.refExists("refs/heads/"+a.Branch) && r.refExists("refs/remotes/"+a.Branch) {
			// 远程分支：建同名本地跟踪分支
			if _, local, ok := splitRemoteRef(a.Branch, r.remoteNames()); ok && !r.refExists("refs/heads/"+local) {
				args = append(args, "--track", "-b", local, p, "refs/remotes/"+a.Branch)
				break
			}
		}
		args = append(args, p, a.Branch)
	default:
		if a.Start != "" {
			if err := r.checkRef(a.Start); err != nil {
				return nil, err
			}
			args = append(args, "--detach", p, a.Start)
		} else {
			args = append(args, p)
		}
	}
	res, err := r.run(cmdOpts{console: true, title: "git worktree add"}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.failResult(res), nil
	}
	return okResult(tr(r.ctx, "result.worktreeAdded", p)), nil
}

func (r *repo) worktreeRemove(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSWorktreeRemoveArgs](raw)
	if err != nil {
		return nil, err
	}
	p := strings.TrimSpace(a.Path)
	if p == "" || strings.HasPrefix(p, "-") {
		return nil, invalidParams("path is required")
	}
	args := []string{"worktree", "remove"}
	if a.Force {
		args = append(args, "--force")
	}
	args = append(args, p)
	res, err := r.run(cmdOpts{}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		out := r.failResult(res)
		if strings.Contains(res.combined(), "contains modified or untracked files") {
			out.Reason = "localChanges"
			out.Message = tr(r.ctx, "result.worktreeDirty")
		}
		return out, nil
	}
	return okResult(tr(r.ctx, "result.worktreeRemoved", p)), nil
}

// ---------------------------------------------------------------------------
// config

type configGetArgs struct {
	sdk.VCSConfigGetArgs
	Global bool `json:"global,omitempty"` // 只读全局配置（~/.gitconfig）
}

func (r *repo) configGet(raw json.RawMessage) (*sdk.VCSConfig, error) {
	a, err := decodeArgs[configGetArgs](raw)
	if err != nil {
		return nil, err
	}
	out := &sdk.VCSConfig{Values: map[string]string{}, Scopes: map[string]string{}}
	args := []string{"config", "-z", "--list"}
	if a.Global {
		args = []string{"config", "-z", "--global", "--list"}
	}
	withScope := r.ver.atLeast(2, 26)
	if withScope {
		args = append(args, "--show-scope")
	}
	res, err := r.run(cmdOpts{read: true, dir: r.configDir()}, args...)
	if err != nil {
		return nil, err
	}
	want := map[string]string{}
	for _, k := range a.Keys {
		want[canonicalConfigKey(k)] = k
	}
	// -z 输出：[作用域\0]键\n值\0
	toks := strings.Split(res.out, "\x00")
	for i := 0; i < len(toks); i++ {
		rec := toks[i]
		if rec == "" {
			continue
		}
		scope := ""
		if withScope {
			scope = rec
			i++
			if i >= len(toks) {
				break
			}
			rec = toks[i]
		}
		k, v, _ := strings.Cut(rec, "\n")
		orig, ok := want[canonicalConfigKey(k)]
		if len(want) > 0 && !ok {
			continue
		}
		if !ok {
			orig = k
		}
		// 后出现的生效（local 覆盖 global 覆盖 system）
		out.Values[orig] = v
		if scope != "" {
			out.Scopes[orig] = scope
		}
	}
	return out, nil
}

// configDir：仓库根存在就在仓库里读（含 local），否则在主目录读（只有 global/system）。
func (r *repo) configDir() string {
	if r.root != "" && sdk.DirExists(r.root) {
		return r.root
	}
	return sdk.HomeDir()
}

// canonicalConfigKey 配置键的节名与变量名不区分大小写，子节区分。
func canonicalConfigKey(k string) string {
	first := strings.Index(k, ".")
	last := strings.LastIndex(k, ".")
	if first < 0 {
		return strings.ToLower(k)
	}
	if first == last {
		return strings.ToLower(k)
	}
	return strings.ToLower(k[:first]) + k[first:last] + strings.ToLower(k[last:])
}

var configKeyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*(\..+)?\.[A-Za-z][A-Za-z0-9-]*$`)

func (r *repo) configSet(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSConfigSetArgs](raw)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(a.Key)
	if !configKeyRe.MatchString(key) {
		return nil, userError(r.ctx, "invalidConfigKey", "error.invalidConfigKey", key)
	}
	args := []string{"config"}
	if a.Global {
		args = append(args, "--global")
	}
	if a.Unset {
		args = append(args, "--unset-all", key)
	} else {
		args = append(args, "--replace-all", key, a.Value)
	}
	res, err := r.run(cmdOpts{dir: r.configDir()}, args...)
	if err != nil {
		return nil, err
	}
	// --unset 时键本来就不存在：退出码 5，算成功
	if !res.ok() && !(a.Unset && res.code == 5) {
		return r.failResult(res), nil
	}
	return okResult(tr(r.ctx, "result.configSet", key)), nil
}

// ---------------------------------------------------------------------------
// ignore / clean

func (r *repo) ignore(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSIgnoreArgs](raw)
	if err != nil {
		return nil, err
	}
	var file string
	switch {
	case a.File == "" || a.File == ".gitignore":
		file = filepath.Join(r.root, ".gitignore")
	case a.File == "exclude":
		_, common := r.dirs()
		if common == "" {
			return nil, userError(r.ctx, "notRepo", "error.notRepo", r.root)
		}
		file = filepath.Join(common, "info", "exclude")
	default:
		rel, err := r.relPath(a.File)
		if err != nil {
			return nil, err
		}
		if filepath.Base(rel) != ".gitignore" {
			return nil, invalidParams("file must be a .gitignore or \"exclude\"")
		}
		file = filepath.Join(r.root, filepath.FromSlash(rel))
	}
	existing := map[string]bool{}
	data, _ := os.ReadFile(file)
	for _, l := range strings.Split(string(data), "\n") {
		existing[strings.TrimSpace(strings.TrimSuffix(l, "\r"))] = true
	}
	var add []string
	for _, p := range a.Patterns {
		p = strings.TrimSpace(p)
		if p == "" || existing[p] {
			continue
		}
		existing[p] = true
		add = append(add, p)
	}
	if len(add) == 0 {
		return okResult(tr(r.ctx, "result.ignoreNothing")), nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return nil, err
	}
	nl := "\n"
	if strings.Contains(string(data), "\r\n") {
		nl = "\r\n"
	}
	var b strings.Builder
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		b.WriteString(nl)
	}
	for _, p := range add {
		b.WriteString(p)
		b.WriteString(nl)
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	_, werr := f.WriteString(b.String())
	cerr := f.Close()
	if werr != nil {
		return nil, werr
	}
	if cerr != nil {
		return nil, cerr
	}
	return okResult(tr(r.ctx, "result.ignored", len(add))), nil
}

type cleanArgs struct {
	sdk.VCSPathsArgs
	All     bool `json:"all,omitempty"`     // Paths 为空时必须显式 all=true 才清理整个仓库
	Ignored bool `json:"ignored,omitempty"` // 同时删除被忽略的文件（-x）
}

func (r *repo) clean(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[cleanArgs](raw)
	if err != nil {
		return nil, err
	}
	paths, err := r.checkPaths(a.Paths)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 && !a.All {
		return nil, invalidParams("paths is required (or all=true)")
	}
	args := []string{"clean", "-f", "-d"}
	if a.Ignored {
		args = append(args, "-x")
	}
	args = r.pathspecArgs(args, paths)
	res, err := r.run(cmdOpts{}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.failResult(res), nil
	}
	var removed []string
	for _, l := range strings.Split(res.out, "\n") {
		if strings.HasPrefix(l, "Removing ") {
			removed = append(removed, strings.TrimPrefix(l, "Removing "))
		}
	}
	out := okResult(tr(r.ctx, "result.cleaned", len(removed)))
	out.Files = removed
	return out, nil
}

// ---------------------------------------------------------------------------
// init / repos

type initArgs struct {
	InitialBranch string `json:"initialBranch,omitempty"`
}

func (r *repo) initRepo(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[initArgs](raw)
	if err != nil {
		return nil, err
	}
	if r.root == "" {
		return nil, invalidParams("root is required")
	}
	if err := os.MkdirAll(r.root, 0o755); err != nil {
		return nil, err
	}
	if exists(filepath.Join(r.root, ".git")) {
		return nil, userError(r.ctx, "alreadyRepo", "error.alreadyRepo", r.root)
	}
	args := []string{"init"}
	if a.InitialBranch != "" {
		if err := r.checkBranchName(a.InitialBranch); err != nil {
			return nil, err
		}
		args = append(args, "--initial-branch="+a.InitialBranch)
	}
	args = append(args, r.root)
	if _, err := r.do(cmdOpts{}, args...); err != nil {
		return nil, err
	}
	return okResult(tr(r.ctx, "result.initialized", r.root)), nil
}

// repoEntry 是 sdk.VCSRepoInfo 的 JSON 超集。
type repoEntry struct {
	IsRepo bool   `json:"isRepo"`
	Root   string `json:"root,omitempty"`
	VCS    string `json:"vcs,omitempty"`
	Kind   string `json:"kind,omitempty"`   // repo | submodule | worktree
	Parent string `json:"parent,omitempty"` // 嵌套在哪个仓库里
}

type repoListResult struct {
	Repos []repoEntry `json:"repos"`
}

// repos 在目录下找所有 git 仓库（含嵌套仓库、子模块、工作树），跳过 node_modules 等目录；只看文件系统，不跑 git。
func (p *Plugin) repos(ctx *sdk.Context, c *sdk.VCSCallParams) (*repoListResult, error) {
	a, err := decodeArgs[sdk.VCSReposArgs](c.Args)
	if err != nil {
		return nil, err
	}
	root := filepath.Clean(sdk.Expand(c.Root))
	if c.Root == "" || !sdk.DirExists(root) {
		return nil, invalidParams("root must be an existing directory")
	}
	maxDepth := a.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 6
	}
	out := &repoListResult{Repos: []repoEntry{}}
	baseDepth := strings.Count(root, string(filepath.Separator))
	var stack []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != root && (sdk.DefaultSkipDirs[name] || strings.HasPrefix(name, ".")) {
			return fs.SkipDir
		}
		depth := strings.Count(path, string(filepath.Separator)) - baseDepth
		for len(stack) > 0 && !strings.HasPrefix(path+string(filepath.Separator), stack[len(stack)-1]+string(filepath.Separator)) {
			stack = stack[:len(stack)-1]
		}
		dotGit := filepath.Join(path, ".git")
		if st, err := os.Lstat(dotGit); err == nil {
			e := repoEntry{IsRepo: true, Root: path, VCS: "git", Kind: "repo"}
			if !st.IsDir() {
				if b, err := os.ReadFile(dotGit); err == nil {
					g := filepath.ToSlash(string(b))
					switch {
					case strings.Contains(g, "/modules/"):
						e.Kind = "submodule"
					case strings.Contains(g, "/worktrees/"):
						e.Kind = "worktree"
					}
				}
			}
			if len(stack) > 0 {
				e.Parent = stack[len(stack)-1]
			}
			out.Repos = append(out.Repos, e)
			stack = append(stack, path)
		}
		if depth >= maxDepth {
			return fs.SkipDir
		}
		if len(out.Repos) >= 2000 {
			return fs.SkipAll
		}
		return nil
	})
	return out, nil
}
