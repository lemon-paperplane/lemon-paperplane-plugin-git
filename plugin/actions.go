package git

import (
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// 动作（manifest contributes.actions）。只放需要插件逻辑的：浏览器打开、补丁、清理分支、优化仓库。
// 上下文参数约定（由界面放进 Args）：root 仓库根；vcs.commitMenu 给 hash / hashes；vcs.fileMenu 给 path / paths。
const (
	actOpenRepo      = "openRepoInBrowser"
	actOpenCommit    = "openCommitInBrowser"
	actOpenFile      = "openFileInBrowser"
	actPatchCommits  = "createPatchFromCommits"
	actPatchChanges  = "createPatchFromChanges"
	actCleanBranches = "cleanupBranches"
	actOptimize      = "optimizeRepository"
)

// InvokeAction 实现 sdk.ActionInvoker。
func (p *Plugin) InvokeAction(ctx *sdk.Context, a *sdk.InvokeActionParams) (*sdk.InvokeActionResult, error) {
	state := map[string]any{}
	if root := argString(a.Args, "root"); root != "" {
		state["root"] = root
	}
	proj := a.Project
	mod := a.Module
	r, _, err := p.viewRepo(ctx, &proj, &mod, state, a.TaskID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, userError(ctx, "notRepo", "error.notRepo", proj.Root)
	}
	switch a.Action {
	case actOpenRepo:
		base, err := r.webBase()
		if err != nil {
			return nil, err
		}
		return &sdk.InvokeActionResult{OpenURL: base}, nil
	case actOpenCommit:
		hashes := argStrings(a.Args, "hash", "hashes")
		if len(hashes) == 0 {
			return nil, invalidParams("hash is required")
		}
		base, err := r.webBase()
		if err != nil {
			return nil, err
		}
		h := r.revParse(hashes[0])
		if h == "" {
			return nil, userError(ctx, "invalidRef", "error.invalidRef", hashes[0])
		}
		return &sdk.InvokeActionResult{OpenURL: webCommitURL(base, h)}, nil
	case actOpenFile:
		paths := argStrings(a.Args, "path", "paths")
		if len(paths) == 0 {
			return nil, invalidParams("path is required")
		}
		rel, err := r.relPath(paths[0])
		if err != nil {
			return nil, err
		}
		base, err := r.webBase()
		if err != nil {
			return nil, err
		}
		// 已推送就用上游分支名，否则用当前提交哈希
		ref := r.headHash()
		if up, err := r.out("rev-parse", "--abbrev-ref", "@{upstream}"); err == nil {
			if _, b, ok := splitRemoteRef(strings.TrimSpace(up), r.remoteNames()); ok {
				ref = b
			}
		}
		return &sdk.InvokeActionResult{OpenURL: webFileURL(base, ref, rel)}, nil
	case actPatchCommits:
		return r.patchFromCommits(a)
	case actPatchChanges:
		return r.patchFromChanges(a)
	case actCleanBranches:
		return r.cleanupBranches(a)
	case actOptimize:
		r.progress(tr(ctx, "progress.optimize"))
		if res, err := r.run(cmdOpts{console: true, title: "git gc"}, "gc"); err != nil {
			return nil, err
		} else if !res.ok() {
			return nil, r.gitFailure(res)
		}
		// 带路径布隆过滤器的提交图：日志、文件历史按路径过滤时快很多
		if res, err := r.run(cmdOpts{console: true, title: "git commit-graph write"}, "commit-graph", "write", "--reachable", "--changed-paths"); err != nil {
			return nil, err
		} else if !res.ok() {
			return nil, r.gitFailure(res)
		}
		return &sdk.InvokeActionResult{Message: tr(ctx, "result.optimized")}, nil
	}
	return nil, sdk.ErrNotSupported
}

func argString(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	return str(m, k)
}

// argStrings 取单个或数组形式的参数。
func argStrings(m map[string]any, single, multi string) []string {
	var out []string
	if s := argString(m, single); s != "" {
		out = append(out, s)
	}
	if arr, ok := m[multi].([]any); ok {
		for _, x := range arr {
			if s, ok := x.(string); ok && s != "" && !contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// webBase 取 origin（或第一个远程）的网页地址。
func (r *repo) webBase() (string, error) {
	list, err := r.remotes()
	if err != nil {
		return "", err
	}
	var pick *remoteInfo
	for i := range list {
		if list[i].Name == "origin" {
			pick = &list[i]
		}
	}
	if pick == nil && len(list) > 0 {
		pick = &list[0]
	}
	if pick == nil {
		return "", userError(r.ctx, "noRemote", "error.noRemote")
	}
	if pick.WebURL == "" {
		return "", userError(r.ctx, "noWebUrl", "error.noWebUrl", pick.FetchURL)
	}
	return pick.WebURL, nil
}

// patchOutput 解析输出文件路径：表单给的路径；相对路径相对仓库根。
func (r *repo) patchOutput(a *sdk.InvokeActionParams, def string) (string, error) {
	out := sdk.Expand(argString(a.Args, "output"))
	if out == "" {
		out = filepath.Join(r.root, def)
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(r.root, out)
	}
	if st, err := os.Stat(out); err == nil && st.IsDir() {
		out = filepath.Join(out, def)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	return out, nil
}

// patchFromCommits 把选中的提交导出成补丁（git format-patch，按提交顺序拼在一个文件里）。
func (r *repo) patchFromCommits(a *sdk.InvokeActionParams) (*sdk.InvokeActionResult, error) {
	hashes := argStrings(a.Args, "hash", "hashes")
	if len(hashes) == 0 {
		return nil, invalidParams("hash is required")
	}
	sorted, err := r.sortTopo(hashes)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, h := range sorted {
		out, err := r.out("format-patch", "-1", "--stdout", "--binary", "--no-color", h)
		if err != nil {
			return nil, err
		}
		b.WriteString(out)
	}
	file, err := r.patchOutput(a, shortHash(sorted[len(sorted)-1])+".patch")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(file, []byte(b.String()), 0o644); err != nil {
		return nil, err
	}
	return &sdk.InvokeActionResult{Message: tr(r.ctx, "result.patchCreated", file)}, nil
}

// patchFromChanges 把选中文件的本地修改（相对 HEAD，含未跟踪的新文件）导出成补丁。
func (r *repo) patchFromChanges(a *sdk.InvokeActionParams) (*sdk.InvokeActionResult, error) {
	paths, err := r.checkPaths(argStrings(a.Args, "path", "paths"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, invalidParams("path is required")
	}
	base := "HEAD"
	if r.headHash() == "" {
		base = r.emptyTree()
	}
	var b strings.Builder
	out, err := r.out(r.pathspecArgs([]string{"diff", "--binary", "--no-color", "--no-ext-diff", "--no-textconv", "-M", base}, paths)...)
	if err != nil {
		return nil, err
	}
	b.WriteString(out)
	st, err := r.pathStatus(paths)
	if err != nil {
		return nil, err
	}
	for _, f := range st.Files {
		if f.Status != "?" {
			continue
		}
		res, err := r.run(cmdOpts{read: true}, "diff", "--no-index", "--binary", "--no-color", "--no-ext-diff", "--", "/dev/null", f.Path)
		if err != nil {
			return nil, err
		}
		b.WriteString(res.out)
	}
	if b.Len() == 0 {
		return nil, userError(r.ctx, "nothingToCommit", "error.noChanges")
	}
	file, err := r.patchOutput(a, "changes.patch")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(file, []byte(b.String()), 0o644); err != nil {
		return nil, err
	}
	return &sdk.InvokeActionResult{Message: tr(r.ctx, "result.patchCreated", file)}, nil
}

// cleanupBranches 删除已合并进当前分支的本地分支、上游已删除的本地分支（不动当前分支、受保护名字、别的工作树检出的分支）。
func (r *repo) cleanupBranches(a *sdk.InvokeActionParams) (*sdk.InvokeActionResult, error) {
	merged := true
	gone := true
	if v, ok := a.Args["merged"].(bool); ok {
		merged = v
	}
	if v, ok := a.Args["gone"].(bool); ok {
		gone = v
	}
	bs, err := r.branches()
	if err != nil {
		return nil, err
	}
	protected := map[string]bool{"main": true, "master": true, "develop": true, "dev": true, "release": true, bs.Current: true}
	mergedSet := map[string]bool{}
	if merged && bs.Head != "" {
		if out, err := r.out("for-each-ref", "--merged", "HEAD", "--format=%(refname)", "refs/heads"); err == nil {
			for _, l := range strings.Split(out, "\n") {
				if n := strings.TrimPrefix(l, "refs/heads/"); n != l {
					mergedSet[n] = true
				}
			}
		}
	}
	var deleted, skipped []string
	for _, b := range bs.Local {
		if protected[b.Name] || b.Worktree != "" {
			continue
		}
		if !(mergedSet[b.Name] || (gone && b.Gone)) {
			continue
		}
		// 上游已删但没合并的分支用 -d 会被拒绝：跳过并报告，不强删
		res, err := r.run(cmdOpts{}, "branch", "-d", b.Name)
		if err != nil {
			return nil, err
		}
		if res.ok() {
			deleted = append(deleted, b.Name)
		} else {
			skipped = append(skipped, b.Name)
		}
	}
	msg := tr(r.ctx, "result.branchesCleaned", len(deleted))
	if len(skipped) > 0 {
		msg += tr(r.ctx, "result.branchesSkipped", strings.Join(skipped, ", "))
	}
	if r.task != "" && r.ctx.Host != nil && len(deleted) > 0 {
		r.ctx.Host.Console(r.task, strings.Join(deleted, "\n"))
	}
	return &sdk.InvokeActionResult{Message: msg}, nil
}
