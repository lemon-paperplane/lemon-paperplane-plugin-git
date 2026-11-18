package git

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// progress 在任务上显示不确定进度（推送/拉取/克隆期间）。
func (r *repo) progress(msg string) {
	if r.ctx != nil && r.ctx.Host != nil && r.task != "" {
		r.ctx.Host.Progress(r.task, msg, -1)
	}
}

// netOpts 是网络操作的公共选项：输出进任务控制台，设置 askpass。
func netOpts(title string) cmdOpts {
	return cmdOpts{console: true, remote: true, title: title}
}

// pushTarget 是推送目标（与 IDEA 推送对话框一致：当前分支 → 上游，没有上游时推到同名分支并设置上游）。
type pushTarget struct {
	Remote       string
	Branch       string
	RemoteBranch string
	TrackingRef  string // refs/remotes/<remote>/<remoteBranch>
	HasUpstream  bool
	NewBranch    bool
}

func (r *repo) resolvePush(a *sdk.VCSPushArgs) (*pushTarget, error) {
	branch := strings.TrimPrefix(a.Branch, "refs/heads/")
	if branch == "" {
		b, detached := r.currentBranch()
		if detached {
			return nil, userError(r.ctx, "detachedHead", "error.detachedHead")
		}
		branch = b
	}
	if err := r.checkRef(branch); err != nil {
		return nil, err
	}
	if !r.refExists("refs/heads/" + branch) {
		return nil, userError(r.ctx, "noCommits", "error.branchNotFound", branch)
	}
	remotes := r.remoteNames()
	if len(remotes) == 0 && a.Remote == "" {
		return nil, userError(r.ctx, "noRemote", "error.noRemote")
	}
	out, err := r.out("for-each-ref", "--format=%(upstream)%00%(upstream:remotename)%00%(upstream:remoteref)%00%(push:remotename)", "refs/heads/"+branch)
	if err != nil {
		return nil, err
	}
	var upstream, upRemote, upRef, pushRemote string
	if f := strings.Split(strings.TrimRight(firstLineRaw(out), "\n"), "\x00"); len(f) >= 4 {
		upstream, upRemote, upRef, pushRemote = f[0], f[1], f[2], f[3]
	}
	remote := a.Remote
	if remote == "" {
		switch {
		case pushRemote != "" && pushRemote != ".":
			remote = pushRemote
		case upRemote != "" && upRemote != ".":
			remote = upRemote
		case contains(remotes, "origin"):
			remote = "origin"
		default:
			remote = remotes[0]
		}
	}
	t := &pushTarget{Remote: remote, Branch: branch, RemoteBranch: branch}
	if upstream != "" && upRemote == remote && strings.HasPrefix(upRef, "refs/heads/") {
		t.HasUpstream = true
		t.RemoteBranch = strings.TrimPrefix(upRef, "refs/heads/")
	}
	t.TrackingRef = "refs/remotes/" + remote + "/" + t.RemoteBranch
	t.NewBranch = !r.refExists(t.TrackingRef)
	return t, nil
}

func firstLineRaw(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// countAhead 数将要推送的提交数。
func (r *repo) countAhead(t *pushTarget) int {
	var args []string
	if !t.NewBranch {
		args = []string{"rev-list", "--count", t.TrackingRef + "..refs/heads/" + t.Branch}
	} else {
		args = []string{"rev-list", "--count", "refs/heads/" + t.Branch, "--not", "--remotes=" + t.Remote}
	}
	res, err := r.run(cmdOpts{read: true}, args...)
	if err != nil || !res.ok() {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(res.out))
	return n
}

type pushArgs struct {
	sdk.VCSPushArgs
	TagsMode string `json:"tagsMode,omitempty"` // Tags=true 时：all（默认，--tags）| follow（--follow-tags，只推送要推送的提交上的附注标签）
	NoVerify bool   `json:"noVerify,omitempty"` // 跳过 pre-push 钩子
}

func (r *repo) pushOp(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[pushArgs](raw)
	if err != nil {
		return nil, err
	}
	return r.push(a)
}

func (r *repo) push(a *pushArgs) (*opResult, error) {
	t, err := r.resolvePush(&a.VCSPushArgs)
	if err != nil {
		return nil, err
	}
	n := r.countAhead(t)
	args := []string{"push", "--progress", "--porcelain"}
	if a.Force {
		args = append(args, "--force-with-lease")
		if r.ver.atLeast(2, 30) {
			args = append(args, "--force-if-includes")
		}
	}
	if a.SetUpstream || (!t.HasUpstream && settingBool(r.ctx, "setUpstreamOnPush", true)) {
		args = append(args, "--set-upstream")
	}
	if a.Tags {
		if a.TagsMode == "follow" {
			args = append(args, "--follow-tags")
		} else {
			args = append(args, "--tags")
		}
	}
	if a.NoVerify {
		args = append(args, "--no-verify")
	}
	args = append(args, t.Remote, "refs/heads/"+t.Branch+":refs/heads/"+t.RemoteBranch)
	r.progress(tr(r.ctx, "progress.push", t.Remote+"/"+t.RemoteBranch))
	res, err := r.run(netOpts("git push"), args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.netFailure(res)
	}
	if pushUpToDate(res.out) && !a.Tags {
		return &opResult{VCSOpResult: sdk.VCSOpResult{OK: true, Message: tr(r.ctx, "result.pushUpToDate")}, Reason: "upToDate"}, nil
	}
	msg := tr(r.ctx, "result.pushed", n, t.Remote+"/"+t.RemoteBranch)
	if t.NewBranch {
		msg = tr(r.ctx, "result.pushedNew", t.Remote+"/"+t.RemoteBranch)
	}
	return &opResult{VCSOpResult: sdk.VCSOpResult{OK: true, Message: msg, Output: truncateOutput(res.combined())}}, nil
}

// pushUpToDate 看 --porcelain 输出：所有引用都是 "="（已是最新）。
func pushUpToDate(out string) bool {
	seen := false
	for _, l := range strings.Split(out, "\n") {
		if len(l) > 1 && l[1] == '\t' {
			seen = true
			if l[0] != '=' {
				return false
			}
		}
	}
	return seen
}

// netFailure 处理网络操作失败：缺身份信息时报错（界面弹框），其余转成结果。
func (r *repo) netFailure(res *result) (*opResult, error) {
	if classifyFailure(res.combined()) == "identityMissing" {
		return nil, r.identityError(nil)
	}
	return r.failResult(res), nil
}

// ---------------------------------------------------------------------------
// pull

func (r *repo) pull(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSPullArgs](raw)
	if err != nil {
		return nil, err
	}
	if op := r.operation(); op != nil {
		return nil, userError(r.ctx, "operationInProgress", "error.operationInProgress", op.Op)
	}
	mode := a.Mode
	if mode == "" {
		mode = settingString(r.ctx, "pullMode", "merge")
	}
	branch, detached := r.currentBranch()
	if detached {
		return nil, userError(r.ctx, "detachedHead", "error.detachedHead")
	}
	remote, rbranch := a.Remote, a.Branch
	if remote == "" && rbranch == "" {
		out, err := r.out("for-each-ref", "--format=%(upstream)", "refs/heads/"+branch)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(out) == "" {
			return &opResult{VCSOpResult: sdk.VCSOpResult{OK: false, Message: tr(r.ctx, "result.noUpstream")}, Reason: "noUpstream"}, nil
		}
	}
	if remote == "" && rbranch != "" {
		remote = "origin"
		if rs := r.remoteNames(); len(rs) > 0 && !contains(rs, "origin") {
			remote = rs[0]
		}
	}
	args := []string{"pull", "--progress", "--no-edit"}
	switch mode {
	case "rebase":
		args = append(args, "--rebase")
	case "ff-only":
		args = append(args, "--ff-only")
	case "config":
		// 按用户 git 配置（pull.rebase / pull.ff）
	default:
		args = append(args, "--no-rebase")
	}
	if a.Autostash && (mode == "rebase" || r.ver.atLeast(2, 27)) {
		// 合并方式的 --autostash 需要 git 2.27+
		args = append(args, "--autostash")
	}
	if remote != "" {
		if err := r.checkRef(remote); err != nil {
			return nil, err
		}
		args = append(args, remote)
		if rbranch != "" {
			if err := r.checkRef(rbranch); err != nil {
				return nil, err
			}
			args = append(args, rbranch)
		}
	}
	before := r.headHash()
	r.progress(tr(r.ctx, "progress.pull"))
	res, err := r.run(netOpts("git pull"), args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		if files := r.unmergedFiles(); len(files) > 0 {
			return r.conflictResult(res, files), nil
		}
		return r.netFailure(res)
	}
	after := r.headHash()
	out := &opResult{VCSOpResult: sdk.VCSOpResult{OK: true, Output: truncateOutput(res.combined())}}
	if before == after {
		out.Message = tr(r.ctx, "result.upToDate")
		out.Reason = "upToDate"
	} else {
		// 拉到的提交数：上游里有、拉取前本地没有的（变基时本地提交会换哈希，不能用 before..after）
		n := 0
		if before != "" {
			target := after
			if rbranch == "" && r.revParse("@{upstream}") != "" {
				target = "@{upstream}"
			}
			if c, err := r.out("rev-list", "--count", before+".."+target); err == nil {
				n, _ = strconv.Atoi(strings.TrimSpace(c))
			}
		}
		out.Message = tr(r.ctx, "result.pulled", n)
	}
	if strings.Contains(res.combined(), "Applying autostash resulted in conflicts") {
		out.Message += tr(r.ctx, "result.autostashConflict")
		out.Reason = "autostashConflict"
	}
	return out, nil
}

// unmergedFiles 列出冲突文件。
func (r *repo) unmergedFiles() []string {
	res, err := r.run(cmdOpts{read: true}, "diff", "--name-only", "--diff-filter=U", "-z", "--no-ext-diff")
	if err != nil || !res.ok() {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(res.out, "\x00") {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func (r *repo) conflictResult(res *result, files []string) *opResult {
	return &opResult{
		VCSOpResult: sdk.VCSOpResult{OK: false, Message: tr(r.ctx, "result.conflicts", len(files)), Output: truncateOutput(res.combined()), Conflicts: files},
		Reason:      "conflicts",
	}
}

// ---------------------------------------------------------------------------
// fetch

type fetchArgs struct {
	sdk.VCSFetchArgs
	Tags bool `json:"tags,omitempty"` // 同时取全部标签
}

func (r *repo) fetch(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[fetchArgs](raw)
	if err != nil {
		return nil, err
	}
	args := []string{"fetch", "--progress"}
	if a.Prune || settingBool(r.ctx, "fetchPrune", false) {
		args = append(args, "--prune")
	}
	if a.Tags {
		args = append(args, "--tags")
	}
	if a.Remote == "" {
		if len(r.remoteNames()) == 0 {
			return nil, userError(r.ctx, "noRemote", "error.noRemote")
		}
		args = append(args, "--all")
	} else {
		if err := r.checkRef(a.Remote); err != nil {
			return nil, err
		}
		args = append(args, a.Remote)
	}
	r.progress(tr(r.ctx, "progress.fetch"))
	res, err := r.run(netOpts("git fetch"), args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.netFailure(res)
	}
	n := 0
	for _, l := range splitLines(res.errOut) {
		if strings.Contains(l, " -> ") || strings.Contains(l, "[deleted]") {
			n++
		}
	}
	out := &opResult{VCSOpResult: sdk.VCSOpResult{OK: true, Output: truncateOutput(res.combined())}}
	if n == 0 {
		out.Message = tr(r.ctx, "result.fetchNothing")
		out.Reason = "upToDate"
	} else {
		out.Message = tr(r.ctx, "result.fetched", n)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// clone

type cloneArgs struct {
	sdk.VCSCloneArgs
	RecurseSubmodules bool `json:"recurseSubmodules,omitempty"`
}

type cloneResult struct {
	opResult
	Path string `json:"path,omitempty"` // 克隆出的仓库目录
}

func (r *repo) clone(raw json.RawMessage) (*cloneResult, error) {
	a, err := decodeArgs[cloneArgs](raw)
	if err != nil {
		return nil, err
	}
	url := strings.TrimSpace(a.URL)
	if url == "" || strings.HasPrefix(url, "-") {
		return nil, userError(r.ctx, "invalidUrl", "error.invalidUrl", url)
	}
	dest := sdk.Expand(strings.TrimSpace(a.Dest))
	if dest == "" {
		return nil, invalidParams("dest is required")
	}
	if !filepath.IsAbs(dest) {
		if r.root == "" {
			return nil, invalidParams("dest must be an absolute path")
		}
		dest = filepath.Join(r.root, dest)
	}
	dest = filepath.Clean(dest)
	if entries, err := os.ReadDir(dest); err == nil && len(entries) > 0 {
		return nil, userError(r.ctx, "destNotEmpty", "error.destNotEmpty", dest)
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, userError(r.ctx, "failed", "error.gitFailed", err.Error())
	}
	args := []string{"clone", "--progress"}
	if a.Branch != "" {
		if err := r.checkRef(a.Branch); err != nil {
			return nil, err
		}
		args = append(args, "--branch", a.Branch)
	}
	if a.Depth > 0 {
		args = append(args, "--depth", strconv.Itoa(a.Depth))
	}
	if a.RecurseSubmodules {
		args = append(args, "--recurse-submodules")
	}
	args = append(args, "--", url, dest)
	o := netOpts("git clone")
	o.dir = parent
	r.progress(tr(r.ctx, "progress.clone", url))
	res, err := r.run(o, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		fr, err := r.netFailure(res)
		if err != nil {
			return nil, err
		}
		return &cloneResult{opResult: *fr}, nil
	}
	return &cloneResult{opResult: opResult{VCSOpResult: sdk.VCSOpResult{OK: true, Message: tr(r.ctx, "result.cloned", dest), Output: truncateOutput(res.combined())}}, Path: dest}, nil
}

// ---------------------------------------------------------------------------
// pushPreview

type pushPreviewResult struct {
	Remote    string       `json:"remote"`
	Branch    string       `json:"branch"`
	RemoteRef string       `json:"remoteRef"`
	NewBranch bool         `json:"newBranch,omitempty"`
	Commits   []commitInfo `json:"commits"`
	Behind    int          `json:"behind"`

	SetUpstream bool `json:"setUpstream,omitempty"` // 推送时会设置上游
	Truncated   bool `json:"truncated,omitempty"`   // 提交太多，只列出前 500 个
}

func (r *repo) pushPreview(raw json.RawMessage) (*pushPreviewResult, error) {
	a, err := decodeArgs[pushArgs](raw)
	if err != nil {
		return nil, err
	}
	t, err := r.resolvePush(&a.VCSPushArgs)
	if err != nil {
		return nil, err
	}
	const limit = 500
	var revs []string
	if !t.NewBranch {
		revs = []string{t.TrackingRef + "..refs/heads/" + t.Branch}
	} else {
		revs = []string{"refs/heads/" + t.Branch, "--not", "--remotes=" + t.Remote}
	}
	commits, more, err := r.logCommits(logQuery{revs: revs, limit: limit})
	if err != nil {
		return nil, err
	}
	out := &pushPreviewResult{
		Remote: t.Remote, Branch: t.Branch, RemoteRef: "refs/heads/" + t.RemoteBranch, NewBranch: t.NewBranch,
		Commits: commits, Truncated: more,
		SetUpstream: a.SetUpstream || (!t.HasUpstream && settingBool(r.ctx, "setUpstreamOnPush", true)),
	}
	if !t.NewBranch {
		if c, err := r.out("rev-list", "--count", "refs/heads/"+t.Branch+".."+t.TrackingRef); err == nil {
			out.Behind, _ = strconv.Atoi(strings.TrimSpace(c))
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 单个标签推送 / 删除远程标签

type tagPushArgs struct {
	Ref    string `json:"ref"`              // 标签名
	Remote string `json:"remote,omitempty"` // 默认 origin
	Delete bool   `json:"delete,omitempty"` // 删除远程标签
	Force  bool   `json:"force,omitempty"`
}

func (r *repo) tagPush(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[tagPushArgs](raw)
	if err != nil {
		return nil, err
	}
	name := strings.TrimPrefix(a.Ref, "refs/tags/")
	if err := r.checkRef(name); err != nil {
		return nil, err
	}
	remote, err := r.defaultRemote(a.Remote)
	if err != nil {
		return nil, err
	}
	args := []string{"push", "--progress", "--porcelain"}
	if a.Force {
		args = append(args, "--force")
	}
	if a.Delete {
		args = append(args, remote, "--delete", "refs/tags/"+name)
	} else {
		args = append(args, remote, "refs/tags/"+name+":refs/tags/"+name)
	}
	r.progress(tr(r.ctx, "progress.push", remote))
	res, err := r.run(netOpts("git push"), args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.netFailure(res)
	}
	if a.Delete {
		return okResult(tr(r.ctx, "result.remoteTagDeleted", name, remote)), nil
	}
	return okResult(tr(r.ctx, "result.tagPushed", name, remote)), nil
}

// defaultRemote：指定了就用指定的，否则 origin，否则第一个远程。
func (r *repo) defaultRemote(remote string) (string, error) {
	if remote != "" {
		if err := r.checkRef(remote); err != nil {
			return "", err
		}
		return remote, nil
	}
	rs := r.remoteNames()
	if len(rs) == 0 {
		return "", userError(r.ctx, "noRemote", "error.noRemote")
	}
	if contains(rs, "origin") {
		return "origin", nil
	}
	return rs[0], nil
}
