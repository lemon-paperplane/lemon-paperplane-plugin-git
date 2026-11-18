// Package git 是 Lemon Paperplane 的 Git 版本控制插件：用系统 git 命令行实现 vcs/call 的全部操作，
// 并贡献远程、标签、子模块、工作树、Git 设置几个低频界面与若干动作。
package git

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// Plugin 是插件实例。没有包级可变状态；git 路径探测结果缓存在实例上。
type Plugin struct {
	mu          sync.Mutex
	gitResolved bool
	gitFor      string
	gitPath     string
	gitVer      gitVersion
	gitErr      error

	// extraEnv 覆盖在每条 git 命令的环境之上（测试用来隔离全局配置）。
	extraEnv map[string]string
}

// New 构造插件实例。
func New() any { return &Plugin{} }

// 扩展操作（不在 SDK 的 VCSOp* 里，界面/AI 可以直接用 op 名调用）。
const (
	opSkip               = "skip"               // 跳过当前提交（rebase/cherry-pick/revert/am）
	opFileContent        = "fileContent"        // 某版本的文件内容（图片对比、查看历史版本）
	opContainingBranches = "containingBranches" // 包含某提交的分支与标签
	opTagPush            = "tagPush"            // 推送单个标签
	opMergeTools         = "mergeTools"         // 检测到的外部合并工具
	opStashShow          = "stashShow"          // 某个储藏的文件列表
)

// repo 是一次请求里对某个仓库的操作上下文。
type repo struct {
	p    *Plugin
	ctx  *sdk.Context
	root string
	task string
	git  string
	ver  gitVersion

	dirsResolved bool
	gitDir       string
	commonDir    string
}

// VCS 实现 sdk.VCSProvider。
func (p *Plugin) VCS(ctx *sdk.Context, c *sdk.VCSCallParams) (any, error) {
	gitPath, ver, err := p.resolveGit(ctx)
	if err != nil {
		// 找仓库不需要 git
		if c.Op == sdk.VCSOpRepos {
			return p.repos(ctx, c)
		}
		return nil, err
	}
	root := c.Root
	if root != "" {
		root = filepath.Clean(sdk.Expand(root))
	}
	r := &repo{p: p, ctx: ctx, root: root, task: c.TaskID, git: gitPath, ver: ver}
	switch c.Op {
	case sdk.VCSOpRepos:
		return p.repos(ctx, c)
	case sdk.VCSOpClone:
		return r.clone(c.Args)
	case sdk.VCSOpInit:
		return r.initRepo(c.Args)
	case sdk.VCSOpDetect:
		return r.detect()
	}
	if root == "" {
		return nil, invalidParams("root is required")
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, userError(ctx, "notRepo", "error.rootMissing", root)
	}
	switch c.Op {
	case sdk.VCSOpStatus:
		return r.status(c.Args)
	case sdk.VCSOpDiff:
		return r.diffOp(c.Args)
	case sdk.VCSOpCommit:
		return r.commit(c.Args)
	case sdk.VCSOpPush:
		return r.pushOp(c.Args)
	case sdk.VCSOpPull:
		return r.pull(c.Args)
	case sdk.VCSOpFetch:
		return r.fetch(c.Args)
	case sdk.VCSOpBranches:
		return r.branches()
	case sdk.VCSOpCheckout:
		return r.checkout(c.Args)
	case sdk.VCSOpBranchCreate:
		return r.branchCreate(c.Args)
	case sdk.VCSOpBranchDelete:
		return r.branchDelete(c.Args)
	case sdk.VCSOpBranchRename:
		return r.branchRename(c.Args)
	case sdk.VCSOpMerge:
		return r.merge(c.Args)
	case sdk.VCSOpRebase:
		return r.rebase(c.Args)
	case sdk.VCSOpAbort:
		return r.abort()
	case sdk.VCSOpContinue:
		return r.continueOp()
	case opSkip:
		return r.skip()
	case sdk.VCSOpLog:
		return r.log(c.Args)
	case sdk.VCSOpShow:
		return r.show(c.Args)
	case sdk.VCSOpDiscard:
		return r.discard(c.Args)
	case sdk.VCSOpStashList:
		return r.stashList()
	case sdk.VCSOpStashSave:
		return r.stashSave(c.Args)
	case sdk.VCSOpStashApply:
		return r.stashApply(c.Args)
	case sdk.VCSOpStashDrop:
		return r.stashDrop(c.Args)
	case opStashShow:
		return r.stashShow(c.Args)
	case sdk.VCSOpResolve:
		return r.resolve(c.Args)
	case sdk.VCSOpMergeTool:
		return r.mergeTool(c.Args)
	case opMergeTools:
		return r.mergeTools()
	case sdk.VCSOpCherryPick:
		return r.cherryPick(c.Args)
	case sdk.VCSOpRevert:
		return r.revert(c.Args)
	case sdk.VCSOpReset:
		return r.reset(c.Args)
	case sdk.VCSOpTags:
		return r.tags()
	case sdk.VCSOpTagCreate:
		return r.tagCreate(c.Args)
	case sdk.VCSOpTagDelete:
		return r.tagDelete(c.Args)
	case opTagPush:
		return r.tagPush(c.Args)
	case sdk.VCSOpRemotes:
		return r.remotesOp()
	case sdk.VCSOpRemoteSet:
		return r.remoteSet(c.Args)
	case sdk.VCSOpRemoteRemove:
		return r.remoteRemove(c.Args)
	case sdk.VCSOpFileHistory:
		return r.fileHistory(c.Args)
	case sdk.VCSOpStage:
		return r.stage(c.Args)
	case sdk.VCSOpUnstage:
		return r.unstage(c.Args)
	case sdk.VCSOpStageHunks:
		return r.applyHunks(c.Args, hunkStage)
	case sdk.VCSOpUnstageHunks:
		return r.applyHunks(c.Args, hunkUnstage)
	case sdk.VCSOpDiscardHunks:
		return r.applyHunks(c.Args, hunkDiscard)
	case sdk.VCSOpUndoCommit:
		return r.undoCommit(c.Args)
	case sdk.VCSOpReword:
		return r.reword(c.Args)
	case sdk.VCSOpRebaseInteractive:
		return r.rebaseInteractive(c.Args)
	case sdk.VCSOpCheckoutFile:
		return r.checkoutFile(c.Args)
	case sdk.VCSOpCompare:
		return r.compare(c.Args)
	case sdk.VCSOpPushPreview:
		return r.pushPreview(c.Args)
	case sdk.VCSOpBlame:
		return r.blame(c.Args)
	case sdk.VCSOpSetUpstream:
		return r.setUpstream(c.Args)
	case sdk.VCSOpSubmodules:
		return r.submodules()
	case sdk.VCSOpSubmoduleUpdate:
		return r.submoduleUpdate(c.Args)
	case sdk.VCSOpWorktrees:
		return r.worktrees()
	case sdk.VCSOpWorktreeAdd:
		return r.worktreeAdd(c.Args)
	case sdk.VCSOpWorktreeRemove:
		return r.worktreeRemove(c.Args)
	case sdk.VCSOpConfigGet:
		return r.configGet(c.Args)
	case sdk.VCSOpConfigSet:
		return r.configSet(c.Args)
	case sdk.VCSOpIgnore:
		return r.ignore(c.Args)
	case sdk.VCSOpClean:
		return r.clean(c.Args)
	case opFileContent:
		return r.fileContent(c.Args)
	case opContainingBranches:
		return r.containingBranches(c.Args)
	}
	return nil, sdk.ErrNotSupported
}

// ---------------------------------------------------------------------------
// 参数与错误

func decodeArgs[T any](raw json.RawMessage) (*T, error) {
	var v T
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, invalidParams("%v", err)
		}
	}
	return &v, nil
}

func invalidParams(format string, args ...any) error {
	return sdk.NewError(sdk.ErrCodeInvalidParams, format, args...)
}

// userError 返回带原因码的用户错误：Message 已本地化，Data.reason 供界面判断要弹什么框。
func userError(ctx *sdk.Context, reason, key string, args ...any) *sdk.Error {
	return userErrorData(ctx, reason, nil, key, args...)
}

func userErrorData(ctx *sdk.Context, reason string, data map[string]any, key string, args ...any) *sdk.Error {
	if data == nil {
		data = map[string]any{}
	}
	data["reason"] = reason
	return &sdk.Error{Code: sdk.ErrCodeUserError, Message: tr(ctx, key, args...), Data: data}
}

func tr(ctx *sdk.Context, key string, args ...any) string {
	if ctx == nil {
		return fmt.Sprint(append([]any{key}, args...)...)
	}
	return ctx.T(key, args...)
}

func settingString(ctx *sdk.Context, key, def string) string {
	if ctx == nil {
		return def
	}
	if s, ok := ctx.Setting(key, def).(string); ok {
		return s
	}
	return def
}

func settingInt(ctx *sdk.Context, key string, def int) int {
	if ctx == nil {
		return def
	}
	switch v := ctx.Setting(key, def).(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func settingBool(ctx *sdk.Context, key string, def bool) bool {
	if ctx == nil {
		return def
	}
	if b, ok := ctx.Setting(key, def).(bool); ok {
		return b
	}
	return def
}

// checkRef 拒绝以 - 开头的引用（防止被 git 当成选项）和空引用。
func (r *repo) checkRef(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, "\x00\n") {
		return userError(r.ctx, "invalidRef", "error.invalidRef", ref)
	}
	return nil
}

// checkPaths 校验路径参数：必须在仓库内、不能为空。返回去重后的 "/" 风格相对路径。
func (r *repo) checkPaths(paths []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		rel, err := r.relPath(p)
		if err != nil {
			return nil, err
		}
		if !seen[rel] {
			seen[rel] = true
			out = append(out, rel)
		}
	}
	return out, nil
}

// relPath 把绝对路径或相对路径转成仓库根下的 "/" 风格相对路径。
func (r *repo) relPath(p string) (string, error) {
	if strings.TrimSpace(p) == "" || strings.ContainsRune(p, 0) {
		return "", userError(r.ctx, "invalidPath", "error.invalidPath", p)
	}
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(r.root, p)
		if err != nil {
			return "", userError(r.ctx, "invalidPath", "error.invalidPath", p)
		}
		p = rel
	}
	p = filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", userError(r.ctx, "invalidPath", "error.pathOutside", p)
	}
	return p, nil
}

// opResult 是写操作结果：SDK 的 VCSOpResult 加原因码等扩展字段（JSON 兼容）。
type opResult struct {
	sdk.VCSOpResult
	Reason string   `json:"reason,omitempty"` // 机器可读原因：conflicts | localChanges | rejected | needsAuth | upToDate ...
	Files  []string `json:"files,omitempty"`  // 相关文件（如会被覆盖的本地修改）
	Hash   string   `json:"hash,omitempty"`   // 相关提交（如被删分支原来指向的提交，可用于恢复）
	Tool   string   `json:"tool,omitempty"`   // mergeTool 用到的工具
}

func okResult(msg string) *opResult {
	return &opResult{VCSOpResult: sdk.VCSOpResult{OK: true, Message: msg}}
}

// failResult 根据 git 输出生成失败结果。
func (r *repo) failResult(res *result) *opResult {
	text := res.combined()
	reason := classifyFailure(text)
	out := &opResult{VCSOpResult: sdk.VCSOpResult{OK: false, Output: truncateOutput(text)}, Reason: reason}
	switch reason {
	case "needsAuth":
		out.NeedsAuth = true
		out.Message = tr(r.ctx, "result.needsAuth")
	case "hostKey":
		out.NeedsAuth = true
		out.Message = tr(r.ctx, "result.hostKey")
	case "rejected":
		out.Rejected = true
		out.Message = tr(r.ctx, "result.rejected")
	case "remoteRejected":
		out.Rejected = true
		out.Message = tr(r.ctx, "result.remoteRejected", firstRemoteRejectReason(text))
	case "network":
		out.Message = tr(r.ctx, "result.network", firstErrorLine(text))
	case "repoNotFound":
		out.Message = tr(r.ctx, "result.repoNotFound")
	case "localChanges":
		out.Files = parseIndentedFiles(text, "would be overwritten")
		out.Message = tr(r.ctx, "result.localChanges")
	case "untrackedOverwritten":
		out.Files = parseIndentedFiles(text, "untracked working tree files would be")
		out.Message = tr(r.ctx, "result.untrackedOverwritten")
	case "unresolvedConflicts":
		out.Message = tr(r.ctx, "result.unresolvedConflicts")
	case "emptyCommit":
		out.Message = tr(r.ctx, "result.emptyCommit")
	case "notFullyMerged":
		out.Message = tr(r.ctx, "result.notFullyMerged")
	case "noUpstream":
		out.Message = tr(r.ctx, "result.noUpstream")
	case "unrelatedHistories":
		out.Message = tr(r.ctx, "result.unrelatedHistories")
	case "checkedOutElsewhere":
		out.Message = tr(r.ctx, "result.checkedOutElsewhere")
	case "divergent":
		out.Message = tr(r.ctx, "result.divergent")
	case "locked":
		out.Message = tr(r.ctx, "result.locked")
	default:
		out.Message = tr(r.ctx, "result.failed", firstErrorLine(text))
	}
	return out
}

// classifyFailure 识别 git 失败输出的原因（LC_ALL=C 下的英文输出）。
func classifyFailure(text string) string {
	t := strings.ToLower(text)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(t, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("please tell me who you are", "author identity unknown", "committer identity unknown", "unable to auto-detect email address", "empty ident name"):
		return "identityMissing"
	case has("not a git repository"):
		return "notRepo"
	case has("host key verification failed", "remote host identification has changed"):
		return "hostKey"
	case has("authentication failed", "could not read username", "could not read password", "terminal prompts disabled",
		"permission denied (publickey", "permission denied, please try again", "http basic: access denied",
		"invalid username or password", "returned error: 401", "returned error: 403", "access denied", "incorrect username or password",
		"support for password authentication was removed", "bad credentials"):
		return "needsAuth"
	case has("[remote rejected]", "pre-receive hook declined", "protected branch", "you are not allowed to push"):
		return "remoteRejected"
	case has("[rejected]", "non-fast-forward", "fetch first", "updates were rejected", "stale info"):
		return "rejected"
	case has("repository not found", "does not appear to be a git repository", "returned error: 404", "project you were looking for could not be found"):
		return "repoNotFound"
	case has("could not resolve host", "connection timed out", "connection refused", "network is unreachable", "operation timed out",
		"failed to connect", "unable to access", "connection reset", "ssl_error", "gnutls", "early eof", "the remote end hung up unexpectedly", "could not resolve proxy"):
		return "network"
	case has("untracked working tree files would be overwritten", "untracked working tree files would be removed"):
		return "untrackedOverwritten"
	case has("your local changes to the following files would be overwritten", "would be overwritten by merge", "would be overwritten by checkout",
		"cannot pull with rebase: you have unstaged changes", "cannot rebase: you have unstaged changes", "please commit your changes or stash them"):
		return "localChanges"
	case has("you must edit all merge conflicts", "unmerged files", "fix conflicts and then commit", "is unmerged", "you have not concluded your merge", "needs merge"):
		return "unresolvedConflicts"
	case has("is now empty", "nothing added to commit", "no changes - did you forget to use 'git add'", "nothing to commit"):
		return "emptyCommit"
	case has("is not fully merged"):
		return "notFullyMerged"
	case has("there is no tracking information", "no upstream configured", "has no upstream branch"):
		return "noUpstream"
	case has("refusing to merge unrelated histories"):
		return "unrelatedHistories"
	case has("is already checked out at", "is already used by worktree", "already checked out at"):
		return "checkedOutElsewhere"
	case has("divergent branches", "not possible to fast-forward"):
		return "divergent"
	case has("index.lock", "unable to create", "another git process seems to be running"):
		return "locked"
	}
	return "failed"
}

// parseIndentedFiles 取 git 在某句提示之后用 Tab 缩进列出的文件。
func parseIndentedFiles(text, marker string) []string {
	var files []string
	in := false
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(strings.ToLower(l), marker) {
			in = true
			continue
		}
		if in {
			if strings.HasPrefix(l, "\t") {
				files = append(files, strings.TrimSpace(l))
				continue
			}
			if len(files) > 0 {
				in = false
			}
		}
	}
	return files
}

func firstRemoteRejectReason(text string) string {
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "remote:") {
			t = strings.TrimSpace(strings.TrimPrefix(t, "remote:"))
			if t != "" && !strings.HasPrefix(t, "---") {
				return t
			}
		}
	}
	for _, l := range strings.Split(text, "\n") {
		if i := strings.Index(l, "[remote rejected]"); i >= 0 {
			return strings.TrimSpace(l[i+len("[remote rejected]"):])
		}
	}
	return ""
}
