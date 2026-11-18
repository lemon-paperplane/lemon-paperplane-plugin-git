package git

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// branchInfo 是 sdk.VCSBranch 的 JSON 超集。
type branchInfo struct {
	Name     string `json:"name"`
	Upstream string `json:"upstream,omitempty"`
	Ahead    int    `json:"ahead,omitempty"`
	Behind   int    `json:"behind,omitempty"`
	Head     string `json:"head,omitempty"` // 短哈希
	Subject  string `json:"subject,omitempty"`
	Date     string `json:"date,omitempty"`
	Current  bool   `json:"current,omitempty"`
	Gone     bool   `json:"gone,omitempty"`

	Hash     string `json:"hash,omitempty"`     // 完整哈希
	Remote   string `json:"remote,omitempty"`   // 远程分支所属远程
	Worktree string `json:"worktree,omitempty"` // 在别的工作树里检出着（不能在这里检出）
	Author   string `json:"author,omitempty"`
}

type branchesResult struct {
	Current  string       `json:"current"`
	Local    []branchInfo `json:"local"`
	Remote   []branchInfo `json:"remote"`
	Detached bool         `json:"detached,omitempty"`
	Head     string       `json:"head,omitempty"`   // HEAD 完整哈希
	Recent   []string     `json:"recent,omitempty"` // 最近检出过的本地分支（新的在前，IDEA 的 Recent）
}

func (r *repo) branches() (*branchesResult, error) {
	var (
		wg      sync.WaitGroup
		remotes []string
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		remotes = r.remoteNames()
	}()
	format := "--format=%(refname)%1f%(objectname)%1f%(objectname:short)%1f%(upstream)%1f%(upstream:track,nobracket)%1f%(subject)%1f%(committerdate:iso-strict)%1f%(HEAD)%1f%(worktreepath)%1f%(symref)%1f%(authorname)"
	out, err := r.out("for-each-ref", format, "refs/heads", "refs/remotes")
	wg.Wait()
	if err != nil {
		return nil, err
	}
	res := &branchesResult{Local: []branchInfo{}, Remote: []branchInfo{}}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\x1f")
		if len(f) < 11 {
			continue
		}
		if f[9] != "" {
			continue // origin/HEAD 这类符号引用
		}
		b := branchInfo{Hash: f[1], Head: f[2], Subject: f[5], Date: f[6], Current: f[7] == "*", Author: f[10]}
		switch {
		case strings.HasPrefix(f[0], "refs/heads/"):
			b.Name = strings.TrimPrefix(f[0], "refs/heads/")
			b.Upstream = shortRefName(f[3])
			b.Ahead, b.Behind, b.Gone = parseTrack(f[4])
			if f[8] != "" && !b.Current {
				b.Worktree = f[8]
			}
			if b.Current {
				res.Current = b.Name
			}
			res.Local = append(res.Local, b)
		case strings.HasPrefix(f[0], "refs/remotes/"):
			b.Name = strings.TrimPrefix(f[0], "refs/remotes/")
			if rm, _, ok := splitRemoteRef(b.Name, remotes); ok {
				b.Remote = rm
			}
			res.Remote = append(res.Remote, b)
		}
	}
	if res.Current == "" {
		if name, detached := r.currentBranch(); detached {
			res.Detached = true
		} else {
			res.Current = name // 还没有提交的分支
		}
	}
	res.Head = r.headHash()
	res.Recent = r.recentBranches(res.Local, 5)
	return res, nil
}

// shortRefName 去掉 refs/heads/ 或 refs/remotes/ 前缀。
func shortRefName(full string) string {
	switch {
	case strings.HasPrefix(full, "refs/heads/"):
		return strings.TrimPrefix(full, "refs/heads/")
	case strings.HasPrefix(full, "refs/remotes/"):
		return strings.TrimPrefix(full, "refs/remotes/")
	case strings.HasPrefix(full, "refs/tags/"):
		return strings.TrimPrefix(full, "refs/tags/")
	}
	return full
}

// parseTrack 解析 %(upstream:track,nobracket)："ahead 1, behind 2" | "gone" | ""。
func parseTrack(s string) (ahead, behind int, gone bool) {
	if s == "gone" {
		return 0, 0, true
	}
	for _, part := range strings.Split(s, ",") {
		f := strings.Fields(part)
		if len(f) != 2 {
			continue
		}
		n, _ := strconv.Atoi(f[1])
		switch f[0] {
		case "ahead":
			ahead = n
		case "behind":
			behind = n
		}
	}
	return
}

// recentBranches 从 HEAD 的引用日志里找最近检出的分支（只读文件尾部）。
func (r *repo) recentBranches(local []branchInfo, n int) []string {
	g, _ := r.dirs()
	if g == "" {
		return nil
	}
	f, err := os.Open(filepath.Join(g, "logs", "HEAD"))
	if err != nil {
		return nil
	}
	defer f.Close()
	const tail = 128 << 10
	if st, err := f.Stat(); err == nil && st.Size() > tail {
		_, _ = f.Seek(st.Size()-tail, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	exist := map[string]bool{}
	for _, l := range local {
		exist[l.Name] = true
	}
	lines := strings.Split(string(b), "\n")
	var out []string
	seen := map[string]bool{}
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		_, msg, ok := strings.Cut(lines[i], "\t")
		if !ok || !strings.HasPrefix(msg, "checkout: moving from ") {
			continue
		}
		rest := strings.TrimPrefix(msg, "checkout: moving from ")
		from, to, ok := strings.Cut(rest, " to ")
		if !ok {
			continue
		}
		for _, name := range []string{to, from} {
			if exist[name] && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// checkout

type checkoutArgs struct {
	sdk.VCSCheckoutArgs
	Smart bool `json:"smart,omitempty"` // 有冲突的本地修改时：储藏 → 检出 → 恢复（IDEA 的 Smart Checkout）
}

func (r *repo) checkout(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[checkoutArgs](raw)
	if err != nil {
		return nil, err
	}
	ref := strings.TrimSpace(a.Ref)
	if err := r.checkRef(ref); err != nil {
		return nil, err
	}
	var args []string
	msgKey, msgArg := "result.checkedOut", ref
	switch {
	case a.Create:
		if err := r.checkBranchName(ref); err != nil {
			return nil, err
		}
		args = []string{"checkout", "-b", ref}
		if a.Start != "" {
			if err := r.checkRef(a.Start); err != nil {
				return nil, err
			}
			args = append(args, a.Start)
		}
	case r.refExists("refs/heads/" + strings.TrimPrefix(ref, "refs/heads/")):
		args = []string{"checkout", strings.TrimPrefix(ref, "refs/heads/")}
	default:
		remoteRef := strings.TrimPrefix(ref, "refs/remotes/")
		if r.refExists("refs/remotes/" + remoteRef) {
			if _, local, ok := splitRemoteRef(remoteRef, r.remoteNames()); ok {
				if r.refExists("refs/heads/" + local) {
					// 本地已有同名分支：检出本地分支
					args = []string{"checkout", local}
					msgArg = local
				} else {
					args = []string{"checkout", "-b", local, "--track", "refs/remotes/" + remoteRef}
					msgKey, msgArg = "result.checkedOutTracking", local
				}
				break
			}
		}
		if r.revParse(ref) == "" {
			return nil, userError(r.ctx, "invalidRef", "error.invalidRef", ref)
		}
		args = []string{"checkout", "--detach", ref}
		msgKey = "result.checkedOutDetached"
	}
	if a.Force {
		args = append(args[:1], append([]string{"--force"}, args[1:]...)...)
	}
	args = append(args, "--")
	res, err := r.run(cmdOpts{console: true, title: "git checkout"}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		fr := r.failResult(res)
		if a.Smart && (fr.Reason == "localChanges") {
			return r.smartCheckout(args)
		}
		return fr, nil
	}
	return okResult(tr(r.ctx, msgKey, msgArg)), nil
}

// smartCheckout：储藏本地修改 → 检出 → 恢复储藏；恢复冲突时储藏保留。
func (r *repo) smartCheckout(args []string) (*opResult, error) {
	res, err := r.run(cmdOpts{}, "stash", "push", "-m", "paperplane: smart checkout")
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.failResult(res), nil
	}
	res, err = r.run(cmdOpts{console: true, title: "git checkout"}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		_, _ = r.run(cmdOpts{}, "stash", "pop")
		return r.failResult(res), nil
	}
	res, err = r.run(cmdOpts{}, "stash", "pop")
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		if files := r.unmergedFiles(); len(files) > 0 {
			out := r.conflictResult(res, files)
			out.Message = tr(r.ctx, "result.smartCheckoutConflicts", len(files))
			return out, nil
		}
		return r.failResult(res), nil
	}
	return okResult(tr(r.ctx, "result.smartCheckedOut")), nil
}

func (r *repo) checkBranchName(name string) error {
	if err := r.checkRef(name); err != nil {
		return err
	}
	res, err := r.run(cmdOpts{read: true}, "check-ref-format", "--branch", name)
	if err != nil {
		return err
	}
	if !res.ok() {
		return userError(r.ctx, "invalidBranchName", "error.invalidBranchName", name)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 分支增删改

func (r *repo) branchCreate(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSBranchCreateArgs](raw)
	if err != nil {
		return nil, err
	}
	if err := r.checkBranchName(a.Name); err != nil {
		return nil, err
	}
	if r.refExists("refs/heads/" + a.Name) {
		return nil, userError(r.ctx, "branchExists", "error.branchExists", a.Name)
	}
	if a.Start != "" {
		if err := r.checkRef(a.Start); err != nil {
			return nil, err
		}
	}
	if a.Checkout {
		b, _ := json.Marshal(sdk.VCSCheckoutArgs{Ref: a.Name, Create: true, Start: a.Start})
		return r.checkout(b)
	}
	args := []string{"branch", a.Name}
	if a.Start != "" {
		args = append(args, a.Start)
	}
	res, err := r.run(cmdOpts{}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.failResult(res), nil
	}
	return okResult(tr(r.ctx, "result.branchCreated", a.Name)), nil
}

func (r *repo) branchDelete(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSBranchDeleteArgs](raw)
	if err != nil {
		return nil, err
	}
	if err := r.checkRef(a.Name); err != nil {
		return nil, err
	}
	if a.Remote {
		name := strings.TrimPrefix(a.Name, "refs/remotes/")
		remote, branch, ok := splitRemoteRef(name, r.remoteNames())
		if !ok {
			return nil, userError(r.ctx, "invalidRef", "error.invalidRef", a.Name)
		}
		hash := r.revParse("refs/remotes/" + name)
		r.progress(tr(r.ctx, "progress.push", remote))
		res, err := r.run(netOpts("git push"), "push", "--porcelain", remote, "--delete", "refs/heads/"+branch)
		if err != nil {
			return nil, err
		}
		if !res.ok() {
			return r.netFailure(res)
		}
		out := okResult(tr(r.ctx, "result.remoteBranchDeleted", name))
		out.Hash = hash
		return out, nil
	}
	name := strings.TrimPrefix(a.Name, "refs/heads/")
	if cur, _ := r.currentBranch(); cur == name {
		return nil, userError(r.ctx, "currentBranch", "error.deleteCurrentBranch", name)
	}
	hash := r.revParse("refs/heads/" + name)
	flag := "-d"
	if a.Force {
		flag = "-D"
	}
	res, err := r.run(cmdOpts{}, "branch", flag, name)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		out := r.failResult(res)
		out.Hash = hash
		return out, nil
	}
	out := okResult(tr(r.ctx, "result.branchDeleted", name))
	out.Hash = hash
	return out, nil
}

func (r *repo) branchRename(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSBranchRenameArgs](raw)
	if err != nil {
		return nil, err
	}
	old := a.Old
	if old == "" {
		var detached bool
		if old, detached = r.currentBranch(); detached {
			return nil, userError(r.ctx, "detachedHead", "error.detachedHead")
		}
	}
	if err := r.checkRef(old); err != nil {
		return nil, err
	}
	if err := r.checkBranchName(a.New); err != nil {
		return nil, err
	}
	res, err := r.run(cmdOpts{}, "branch", "-m", old, a.New)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.failResult(res), nil
	}
	return okResult(tr(r.ctx, "result.branchRenamed", old, a.New)), nil
}

func (r *repo) setUpstream(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSSetUpstreamArgs](raw)
	if err != nil {
		return nil, err
	}
	branch := a.Branch
	if branch == "" {
		var detached bool
		if branch, detached = r.currentBranch(); detached {
			return nil, userError(r.ctx, "detachedHead", "error.detachedHead")
		}
	}
	if err := r.checkRef(branch); err != nil {
		return nil, err
	}
	if a.Upstream == "" {
		res, err := r.run(cmdOpts{}, "branch", "--unset-upstream", branch)
		if err != nil {
			return nil, err
		}
		if !res.ok() && !strings.Contains(res.combined(), "has no upstream") {
			return r.failResult(res), nil
		}
		return okResult(tr(r.ctx, "result.upstreamUnset", branch)), nil
	}
	up := strings.TrimPrefix(a.Upstream, "refs/remotes/")
	if err := r.checkRef(up); err != nil {
		return nil, err
	}
	if r.refExists("refs/remotes/"+up) || r.refExists("refs/heads/"+up) {
		res, err := r.run(cmdOpts{}, "branch", "--set-upstream-to="+up, branch)
		if err != nil {
			return nil, err
		}
		if !res.ok() {
			return r.failResult(res), nil
		}
		return okResult(tr(r.ctx, "result.upstreamSet", branch, up)), nil
	}
	// 远程分支还没取回（比如还没推送）：直接写配置
	remote, rb, ok := splitRemoteRef(up, r.remoteNames())
	if !ok {
		return nil, userError(r.ctx, "invalidRef", "error.invalidRef", a.Upstream)
	}
	if _, err := r.do(cmdOpts{}, "config", "branch."+branch+".remote", remote); err != nil {
		return nil, err
	}
	if _, err := r.do(cmdOpts{}, "config", "branch."+branch+".merge", "refs/heads/"+rb); err != nil {
		return nil, err
	}
	return okResult(tr(r.ctx, "result.upstreamSet", branch, up)), nil
}
