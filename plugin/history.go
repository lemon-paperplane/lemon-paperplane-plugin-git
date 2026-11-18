package git

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// ---------------------------------------------------------------------------
// merge / rebase

type refArgs struct {
	sdk.VCSRefArgs
	AllowUnrelated bool   `json:"allowUnrelated,omitempty"` // merge --allow-unrelated-histories
	FFOnly         bool   `json:"ffOnly,omitempty"`         // merge --ff-only
	Onto           string `json:"onto,omitempty"`           // rebase --onto
	RecordOrigin   bool   `json:"recordOrigin,omitempty"`   // cherry-pick -x
	NoCommit       bool   `json:"noCommit,omitempty"`       // cherry-pick/revert -n
	Mainline       int    `json:"mainline,omitempty"`       // 合并提交用哪个父提交（-m），默认 1
	Remote         string `json:"remote,omitempty"`         // tagDelete：同时删除这个远程上的标签
}

func (a *refArgs) refs() []string {
	var out []string
	if a.Ref != "" {
		out = append(out, a.Ref)
	}
	for _, r := range a.Refs {
		if r != "" && r != a.Ref {
			out = append(out, r)
		}
	}
	return out
}

func (r *repo) merge(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[refArgs](raw)
	if err != nil {
		return nil, err
	}
	refs := a.refs()
	if len(refs) == 0 {
		return nil, invalidParams("ref is required")
	}
	for _, ref := range refs {
		if err := r.checkRef(ref); err != nil {
			return nil, err
		}
	}
	if op := r.operation(); op != nil {
		return nil, userError(r.ctx, "operationInProgress", "error.operationInProgress", op.Op)
	}
	args := []string{"merge", "--no-edit"}
	switch {
	case a.Squash:
		args = append(args, "--squash")
	case a.FFOnly:
		args = append(args, "--ff-only")
	case a.NoFF:
		args = append(args, "--no-ff")
	}
	if a.AllowUnrelated {
		args = append(args, "--allow-unrelated-histories")
	}
	var cleanup func()
	if msg := normalizeMessage(a.Message); msg != "" && !a.Squash {
		file, c, err := r.msgFile(msg)
		if err != nil {
			return nil, err
		}
		cleanup = c
		args = append(args, "-F", file)
	}
	if cleanup != nil {
		defer cleanup()
	}
	args = append(args, refs...)
	before := r.headHash()
	res, err := r.run(cmdOpts{console: true, title: "git merge"}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		if files := r.unmergedFiles(); len(files) > 0 {
			return r.conflictResult(res, files), nil
		}
		if classifyFailure(res.combined()) == "identityMissing" {
			return nil, r.identityError(nil)
		}
		return r.failResult(res), nil
	}
	text := res.combined()
	out := &opResult{VCSOpResult: sdk.VCSOpResult{OK: true, Output: truncateOutput(text)}}
	switch {
	case strings.Contains(text, "Already up to date") || strings.Contains(text, "Already up-to-date"):
		out.Message, out.Reason = tr(r.ctx, "result.upToDate"), "upToDate"
	case a.Squash:
		out.Message, out.Reason = tr(r.ctx, "result.squashed"), "squashed"
	case strings.Contains(text, "Fast-forward"):
		out.Message, out.Reason = tr(r.ctx, "result.fastForward", r.countRange(before, r.headHash())), "fastForward"
	default:
		out.Message = tr(r.ctx, "result.merged", strings.Join(refs, ", "))
	}
	return out, nil
}

func (r *repo) countRange(from, to string) int {
	if from == "" || to == "" || from == to {
		return 0
	}
	out, err := r.out("rev-list", "--count", from+".."+to)
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
	return n
}

func (r *repo) rebase(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[refArgs](raw)
	if err != nil {
		return nil, err
	}
	if err := r.checkRef(a.Ref); err != nil {
		return nil, err
	}
	if op := r.operation(); op != nil {
		return nil, userError(r.ctx, "operationInProgress", "error.operationInProgress", op.Op)
	}
	// 与 IDEA 一致：有本地修改时自动储藏，变基后恢复
	args := []string{"rebase", "--autostash"}
	if a.Onto != "" {
		if err := r.checkRef(a.Onto); err != nil {
			return nil, err
		}
		args = append(args, "--onto", a.Onto)
	}
	args = append(args, a.Ref)
	before := r.headHash()
	res, err := r.run(cmdOpts{console: true, title: "git rebase"}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		if files := r.unmergedFiles(); len(files) > 0 {
			return r.conflictResult(res, files), nil
		}
		if classifyFailure(res.combined()) == "identityMissing" {
			return nil, r.identityError(nil)
		}
		return r.failResult(res), nil
	}
	if before == r.headHash() {
		return &opResult{VCSOpResult: sdk.VCSOpResult{OK: true, Message: tr(r.ctx, "result.upToDate"), Output: truncateOutput(res.combined())}, Reason: "upToDate"}, nil
	}
	out := okResult(tr(r.ctx, "result.rebased", a.Ref))
	out.Output = truncateOutput(res.combined())
	return out, nil
}

// ---------------------------------------------------------------------------
// abort / continue / skip：按当前进行中的操作自动选

func (r *repo) abort() (*opResult, error) {
	op := r.operation()
	if op == nil {
		return nil, userError(r.ctx, "noOperation", "error.noOperation")
	}
	var args []string
	switch op.Op {
	case "rebase":
		args = []string{"rebase", "--abort"}
	case "am":
		args = []string{"am", "--abort"}
	case "merge":
		args = []string{"merge", "--abort"}
	case "cherry-pick":
		args = []string{"cherry-pick", "--abort"}
	case "revert":
		args = []string{"revert", "--abort"}
	}
	res, err := r.run(cmdOpts{console: true, title: "git " + args[0] + " --abort"}, args...)
	if err != nil {
		return nil, err
	}
	r.clearRebaseSession()
	if !res.ok() {
		return r.failResult(res), nil
	}
	return okResult(tr(r.ctx, "result.aborted", op.Op)), nil
}

func (r *repo) continueOp() (*opResult, error) {
	op := r.operation()
	if op == nil {
		return nil, userError(r.ctx, "noOperation", "error.noOperation")
	}
	if files := r.unmergedFiles(); len(files) > 0 {
		out := &opResult{VCSOpResult: sdk.VCSOpResult{OK: false, Message: tr(r.ctx, "result.unresolvedConflicts"), Conflicts: files}, Reason: "unresolvedConflicts"}
		return out, nil
	}
	o := cmdOpts{console: true}
	var args []string
	switch op.Op {
	case "rebase":
		args = []string{"rebase", "--continue"}
		if env := r.rebaseSessionEnv(); env != nil {
			o.env = env
			o.config = []string{"commit.cleanup=whitespace"}
		}
	case "am":
		args = []string{"am", "--continue"}
	case "merge":
		args = []string{"commit", "--no-edit"}
	case "cherry-pick":
		args = []string{"cherry-pick", "--continue"}
	case "revert":
		args = []string{"revert", "--continue"}
	}
	if err := r.checkIdentity(); err != nil {
		return nil, err
	}
	o.title = "git " + strings.Join(args, " ")
	res, err := r.run(o, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		if files := r.unmergedFiles(); len(files) > 0 {
			return r.conflictResult(res, files), nil
		}
		return r.failResult(res), nil
	}
	if r.operation() == nil {
		r.clearRebaseSession()
	}
	return okResult(tr(r.ctx, "result.continued", op.Op)), nil
}

func (r *repo) skip() (*opResult, error) {
	op := r.operation()
	if op == nil {
		return nil, userError(r.ctx, "noOperation", "error.noOperation")
	}
	o := cmdOpts{console: true}
	var args []string
	switch op.Op {
	case "rebase":
		args = []string{"rebase", "--skip"}
		if env := r.rebaseSessionEnv(); env != nil {
			o.env = env
			o.config = []string{"commit.cleanup=whitespace"}
		}
	case "am":
		args = []string{"am", "--skip"}
	case "cherry-pick":
		args = []string{"cherry-pick", "--skip"}
	case "revert":
		args = []string{"revert", "--skip"}
	default:
		return nil, userError(r.ctx, "notSupported", "error.skipNotSupported", op.Op)
	}
	o.title = "git " + strings.Join(args, " ")
	res, err := r.run(o, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		if files := r.unmergedFiles(); len(files) > 0 {
			return r.conflictResult(res, files), nil
		}
		return r.failResult(res), nil
	}
	if r.operation() == nil {
		r.clearRebaseSession()
	}
	return okResult(tr(r.ctx, "result.skipped")), nil
}

// ---------------------------------------------------------------------------
// cherry-pick / revert

// sortTopo 把提交按「祖先在前」排序（少量提交时两两判断祖先关系，多了按提交时间）。
func (r *repo) sortTopo(refs []string) ([]string, error) {
	var hashes []string
	for _, ref := range refs {
		if err := r.checkRef(ref); err != nil {
			return nil, err
		}
		h := r.revParse(ref)
		if h == "" {
			return nil, userError(r.ctx, "invalidRef", "error.invalidRef", ref)
		}
		hashes = append(hashes, h)
	}
	if len(hashes) <= 1 {
		return hashes, nil
	}
	out, err := r.out(append([]string{"rev-list", "--no-walk=sorted", "--reverse"}, hashes...)...)
	if err == nil {
		if sorted := strings.Fields(out); len(sorted) == len(hashes) {
			hashes = sorted
		}
	}
	if len(hashes) > 16 {
		return hashes, nil
	}
	// 时间相同（批量提交）时用祖先关系纠正
	var res []string
	for _, h := range hashes {
		pos := len(res)
		for i, x := range res {
			if r.isAncestor(h, x) {
				pos = i
				break
			}
		}
		res = append(res, "")
		copy(res[pos+1:], res[pos:])
		res[pos] = h
	}
	return res, nil
}

func (r *repo) isAncestor(a, b string) bool {
	res, err := r.run(cmdOpts{read: true}, "merge-base", "--is-ancestor", a, b)
	return err == nil && res.code == 0
}

func (r *repo) isMerge(hash string) bool {
	out, err := r.out("rev-list", "--parents", "-n", "1", hash)
	return err == nil && len(strings.Fields(out)) > 2
}

func (r *repo) cherryPick(raw json.RawMessage) (*opResult, error) {
	return r.pickOrRevert(raw, "cherry-pick")
}

func (r *repo) revert(raw json.RawMessage) (*opResult, error) {
	return r.pickOrRevert(raw, "revert")
}

func (r *repo) pickOrRevert(raw json.RawMessage, cmd string) (*opResult, error) {
	a, err := decodeArgs[refArgs](raw)
	if err != nil {
		return nil, err
	}
	refs := a.refs()
	if len(refs) == 0 {
		return nil, invalidParams("ref is required")
	}
	if op := r.operation(); op != nil {
		return nil, userError(r.ctx, "operationInProgress", "error.operationInProgress", op.Op)
	}
	if !a.NoCommit {
		if err := r.checkIdentity(); err != nil {
			return nil, err
		}
	}
	hashes, err := r.sortTopo(refs)
	if err != nil {
		return nil, err
	}
	if cmd == "revert" {
		// 撤销从新到旧
		for i, j := 0, len(hashes)-1; i < j; i, j = i+1, j-1 {
			hashes[i], hashes[j] = hashes[j], hashes[i]
		}
	}
	args := []string{cmd}
	if cmd == "revert" {
		args = append(args, "--no-edit")
	}
	if a.RecordOrigin && cmd == "cherry-pick" {
		args = append(args, "-x")
	}
	if a.NoCommit {
		args = append(args, "--no-commit")
	}
	mainline := a.Mainline
	for _, h := range hashes {
		if mainline == 0 && r.isMerge(h) {
			mainline = 1
		}
	}
	if mainline > 0 {
		args = append(args, "-m", fmt.Sprint(mainline))
	}
	args = append(args, hashes...)
	res, err := r.run(cmdOpts{console: true, title: "git " + cmd}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		if files := r.unmergedFiles(); len(files) > 0 {
			return r.conflictResult(res, files), nil
		}
		return r.failResult(res), nil
	}
	key := "result.cherryPicked"
	if cmd == "revert" {
		key = "result.reverted"
	}
	return okResult(tr(r.ctx, key, len(hashes))), nil
}

// ---------------------------------------------------------------------------
// reset / undoCommit

func (r *repo) reset(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSResetArgs](raw)
	if err != nil {
		return nil, err
	}
	if err := r.checkRef(a.Ref); err != nil {
		return nil, err
	}
	mode := a.Mode
	if mode == "" {
		mode = "mixed"
	}
	switch mode {
	case "soft", "mixed", "hard", "keep", "merge":
	default:
		return nil, invalidParams("unknown reset mode %q", mode)
	}
	if r.revParse(a.Ref) == "" {
		return nil, userError(r.ctx, "invalidRef", "error.invalidRef", a.Ref)
	}
	res, err := r.run(cmdOpts{}, "reset", "--"+mode, a.Ref, "--")
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.failResult(res), nil
	}
	return okResult(tr(r.ctx, "result.reset", shortHash(r.headHash()))), nil
}

type undoArgs struct {
	Force bool `json:"force,omitempty"` // 提交已推送也撤销
}

func (r *repo) undoCommit(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[undoArgs](raw)
	if err != nil {
		return nil, err
	}
	if op := r.operation(); op != nil {
		return nil, userError(r.ctx, "operationInProgress", "error.operationInProgress", op.Op)
	}
	head := r.headHash()
	if head == "" {
		return nil, userError(r.ctx, "noCommits", "error.noCommits")
	}
	if !a.Force {
		// 已经在上游里的提交不默认撤销（撤销后再推送需要强推）
		if up := r.revParse("@{upstream}"); up != "" && r.isAncestor(head, up) {
			return nil, userError(r.ctx, "alreadyPushed", "error.alreadyPushed")
		}
	}
	subject, _ := r.out("log", "-1", "--format=%s", head)
	if r.revParse("HEAD~1") == "" {
		// 首个提交：删掉分支引用，改动全部留在暂存区
		if _, err := r.do(cmdOpts{}, "update-ref", "-d", "HEAD", head); err != nil {
			return nil, err
		}
	} else if _, err := r.do(cmdOpts{}, "reset", "--soft", "HEAD~1"); err != nil {
		return nil, err
	}
	out := okResult(tr(r.ctx, "result.undone", strings.TrimSpace(subject)))
	out.Hash = head
	return out, nil
}

// ---------------------------------------------------------------------------
// reword：HEAD 用 amend；更早的提交用 commit-tree 重写这一段历史（不动工作区与暂存区，有本地修改也能做）

func (r *repo) reword(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSRewordArgs](raw)
	if err != nil {
		return nil, err
	}
	msg := normalizeMessage(a.Message)
	if msg == "" {
		return nil, userError(r.ctx, "emptyMessage", "error.emptyMessage")
	}
	if err := r.checkIdentity(); err != nil {
		return nil, err
	}
	head := r.headHash()
	if head == "" {
		return nil, userError(r.ctx, "noCommits", "error.noCommits")
	}
	target := head
	if a.Hash != "" && a.Hash != "HEAD" {
		if err := r.checkRef(a.Hash); err != nil {
			return nil, err
		}
		if target = r.revParse(a.Hash); target == "" {
			return nil, userError(r.ctx, "invalidRef", "error.invalidRef", a.Hash)
		}
	}
	file, cleanup, err := r.msgFile(msg)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if target == head {
		if err := r.runCommit([]string{"commit", "--amend", "--only", "--allow-empty", "-F", file}); err != nil {
			return nil, err
		}
		return okResult(tr(r.ctx, "result.reworded")), nil
	}
	if op := r.operation(); op != nil {
		return nil, userError(r.ctx, "operationInProgress", "error.operationInProgress", op.Op)
	}
	if !r.isAncestor(target, head) {
		return nil, userError(r.ctx, "notAncestor", "error.notAncestor")
	}
	newHead, err := r.rewriteHistory(target, head, func(h string) (string, bool) {
		if h == target {
			return msg, true
		}
		return "", false
	})
	if err != nil {
		return nil, err
	}
	if _, err := r.do(cmdOpts{}, "update-ref", "-m", "reword: "+firstLine(msg), "HEAD", newHead, head); err != nil {
		return nil, err
	}
	return okResult(tr(r.ctx, "result.reworded")), nil
}

// rewriteHistory 从 target 起沿祖先路径到 head 逐个用 commit-tree 重建提交（树不变，只换父提交/信息），返回新的 head。
// 保留作者与作者时间；提交者为当前用户（与 rebase 相同）。
func (r *repo) rewriteHistory(target, head string, newMsg func(hash string) (string, bool)) (string, error) {
	out, err := r.out("rev-list", "--topo-order", "--reverse", "--ancestry-path", "--parents", target+".."+head)
	if err != nil {
		return "", err
	}
	order := []string{target}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			order = append(order, f[0])
		}
	}
	mapped := map[string]string{}
	for _, h := range order {
		raw, err := r.out("cat-file", "commit", h)
		if err != nil {
			return "", err
		}
		c := parseRawCommit(raw)
		msg := c.message
		if m, ok := newMsg(h); ok {
			msg = m
		}
		args := []string{"commit-tree", c.tree}
		changed := h == target
		for _, p := range c.parents {
			if np, ok := mapped[p]; ok {
				p = np
				changed = true
			}
			args = append(args, "-p", p)
		}
		if !changed {
			mapped[h] = h
			continue
		}
		env := map[string]string{
			"GIT_AUTHOR_NAME":  c.authorName,
			"GIT_AUTHOR_EMAIL": c.authorEmail,
			"GIT_AUTHOR_DATE":  c.authorDate,
		}
		res, err := r.run(cmdOpts{env: env, stdin: msg}, args...)
		if err != nil {
			return "", err
		}
		if !res.ok() {
			return "", r.gitFailure(res)
		}
		mapped[h] = strings.TrimSpace(res.out)
	}
	return mapped[head], nil
}

type rawCommit struct {
	tree                                string
	parents                             []string
	authorName, authorEmail, authorDate string
	message                             string
}

// parseRawCommit 解析 git cat-file commit 的输出。
func parseRawCommit(raw string) rawCommit {
	var c rawCommit
	header, msg, _ := strings.Cut(raw, "\n\n")
	c.message = msg
	inSig := false
	for _, l := range strings.Split(header, "\n") {
		if strings.HasPrefix(l, " ") && inSig {
			continue
		}
		inSig = false
		key, val, _ := strings.Cut(l, " ")
		switch key {
		case "tree":
			c.tree = val
		case "parent":
			c.parents = append(c.parents, val)
		case "author":
			// Name <email> 1700000000 +0800
			lt := strings.LastIndex(val, "<")
			gt := strings.LastIndex(val, ">")
			if lt >= 0 && gt > lt {
				c.authorName = strings.TrimSpace(val[:lt])
				c.authorEmail = val[lt+1 : gt]
				c.authorDate = "@" + strings.TrimSpace(val[gt+1:])
			}
		case "gpgsig", "gpgsig-sha256", "mergetag":
			inSig = true
		}
	}
	return c
}

// ---------------------------------------------------------------------------
// 交互式变基（IDEA 的 Interactively Rebase from Here）

type rebasePlan struct {
	sdk.VCSRebasePlan
}

// rebaseSessionDir 是本仓库交互式变基的临时目录（在插件缓存目录里，不往用户仓库写东西）。
func (r *repo) rebaseSessionDir() string {
	g, _ := r.dirs()
	sum := sha1.Sum([]byte(g))
	return filepath.Join(r.cacheDir(), "rebase-"+hex.EncodeToString(sum[:])[:16])
}

// rebaseSessionEnv：有我们发起的交互式变基时，返回带编辑器脚本的环境（继续后的 reword/squash 仍用计划里的信息）。
func (r *repo) rebaseSessionEnv() map[string]string {
	dir := r.rebaseSessionDir()
	script := filepath.Join(dir, "editor.sh")
	if !exists(script) {
		return nil
	}
	return map[string]string{"GIT_EDITOR": shellCommand(script)}
}

func (r *repo) clearRebaseSession() {
	if r.operation() == nil {
		_ = os.RemoveAll(r.rebaseSessionDir())
	}
}

// shellCommand 把脚本路径包成 git 编辑器命令。git（含 Git for Windows）通过自带的 sh 执行编辑器命令，
// 所以 Windows 上也用 sh 脚本，只需把路径换成正斜杠。
func shellCommand(script string) string {
	p := script
	if runtime.GOOS == "windows" {
		p = filepath.ToSlash(p)
	}
	return "sh " + shQuote(p)
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (r *repo) rebaseInteractive(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[rebasePlan](raw)
	if err != nil {
		return nil, err
	}
	if len(a.Steps) == 0 {
		return nil, invalidParams("steps is required")
	}
	if op := r.operation(); op != nil {
		return nil, userError(r.ctx, "operationInProgress", "error.operationInProgress", op.Op)
	}
	head := r.headHash()
	if head == "" {
		return nil, userError(r.ctx, "noCommits", "error.noCommits")
	}
	if err := r.checkIdentity(); err != nil {
		return nil, err
	}
	root := a.Base == ""
	base := ""
	if !root {
		if err := r.checkRef(a.Base); err != nil {
			return nil, err
		}
		if base = r.revParse(a.Base); base == "" {
			return nil, userError(r.ctx, "invalidRef", "error.invalidRef", a.Base)
		}
		if !r.isAncestor(base, head) {
			return nil, userError(r.ctx, "notAncestor", "error.notAncestor")
		}
	}
	// 范围内的提交（旧到新）：必须和计划一一对应，且没有合并提交
	rng := head
	if !root {
		rng = base + ".." + head
	}
	out, err := r.out("rev-list", "--reverse", "--parents", rng)
	if err != nil {
		return nil, err
	}
	inRange := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		if len(f) > 2 {
			return nil, userError(r.ctx, "mergesInRange", "error.mergesInRange")
		}
		inRange[f[0]] = true
	}
	type step struct {
		hash, action, msg string
	}
	var steps []step
	planned := map[string]bool{}
	for i, s := range a.Steps {
		h := r.revParse(s.Hash)
		if h == "" || !inRange[h] {
			return nil, userError(r.ctx, "planMismatch", "error.planMismatch", s.Hash)
		}
		if planned[h] {
			return nil, userError(r.ctx, "planMismatch", "error.planMismatch", s.Hash)
		}
		planned[h] = true
		act := s.Action
		switch act {
		case "pick", "reword", "squash", "fixup", "drop":
		case "":
			act = "pick"
		default:
			return nil, invalidParams("unknown action %q", s.Action)
		}
		if act == "reword" && normalizeMessage(s.Message) == "" {
			return nil, userError(r.ctx, "emptyMessage", "error.emptyMessage")
		}
		if (act == "squash" || act == "fixup") && (i == 0 || allDropped(a.Steps[:i])) {
			return nil, userError(r.ctx, "invalidPlan", "error.squashFirst")
		}
		steps = append(steps, step{hash: h, action: act, msg: s.Message})
	}
	if len(planned) != len(inRange) {
		return nil, userError(r.ctx, "planMismatch", "error.planIncomplete")
	}

	// 编辑器要填的信息：reword 按本提交；squash 链按链上最后一个提交（git 在链尾打开编辑器）
	messages := map[string]string{}
	raws := map[string]string{}
	rawMsg := func(h string) string {
		if m, ok := raws[h]; ok {
			return m
		}
		o, _ := r.out("cat-file", "commit", h)
		m := parseRawCommit(o).message
		raws[h] = m
		return m
	}
	for i := 0; i < len(steps); i++ {
		s := steps[i]
		if s.action == "reword" {
			messages[s.hash] = normalizeMessage(s.msg)
		}
		if s.action == "drop" || s.action == "squash" || s.action == "fixup" {
			continue
		}
		// 链：本提交后面连续的 squash/fixup（中间的 drop 不打断）
		j := i + 1
		hasSquash := false
		lastInChain := ""
		custom := ""
		parts := []string{}
		if s.action == "reword" {
			parts = append(parts, strings.TrimSpace(messages[s.hash]))
		} else {
			parts = append(parts, strings.TrimSpace(rawMsg(s.hash)))
		}
		if s.action == "pick" && normalizeMessage(s.msg) != "" {
			custom = s.msg
		}
		for ; j < len(steps); j++ {
			t := steps[j]
			if t.action == "drop" {
				continue
			}
			if t.action != "squash" && t.action != "fixup" {
				break
			}
			lastInChain = t.hash
			if t.action == "squash" {
				hasSquash = true
				parts = append(parts, strings.TrimSpace(rawMsg(t.hash)))
			}
			if normalizeMessage(t.msg) != "" {
				custom = t.msg
			}
		}
		if hasSquash && lastInChain != "" {
			if custom != "" {
				messages[lastInChain] = normalizeMessage(custom)
			} else {
				messages[lastInChain] = normalizeMessage(strings.Join(parts, "\n\n"))
			}
		}
	}

	// 写计划、信息文件与编辑器脚本
	dir := r.rebaseSessionDir()
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var todo strings.Builder
	for _, s := range steps {
		fmt.Fprintf(&todo, "%s %s\n", s.action, s.hash)
	}
	planFile := filepath.Join(dir, "todo")
	if err := os.WriteFile(planFile, []byte(todo.String()), 0o644); err != nil {
		return nil, err
	}
	for h, m := range messages {
		if err := os.WriteFile(filepath.Join(dir, "msg-"+h), []byte(m), 0o644); err != nil {
			return nil, err
		}
	}
	seqScript := filepath.Join(dir, "sequence.sh")
	seq := "#!/bin/sh\ncat " + shQuote(slashPath(planFile)) + " > \"$1\"\n"
	if err := os.WriteFile(seqScript, []byte(seq), 0o755); err != nil {
		return nil, err
	}
	// 编辑器按 rebase-merge/done 最后一行的提交找对应信息；找不到就保留 git 给的默认信息
	ed := "#!/bin/sh\n" +
		"dir=" + shQuote(slashPath(dir)) + "\n" +
		"done_file=$(git rev-parse --git-path rebase-merge/done)\n" +
		"h=$(tail -n 1 \"$done_file\" 2>/dev/null | awk '{print $2}')\n" +
		"[ -n \"$h\" ] || exit 0\n" +
		"for f in \"$dir\"/msg-*; do\n" +
		"  [ -f \"$f\" ] || continue\n" +
		"  n=${f##*/msg-}\n" +
		"  case \"$n\" in \"$h\"*) cat \"$f\" > \"$1\"; exit 0;; esac\n" +
		"done\n" +
		"exit 0\n"
	edScript := filepath.Join(dir, "editor.sh")
	if err := os.WriteFile(edScript, []byte(ed), 0o755); err != nil {
		return nil, err
	}
	env := map[string]string{
		"GIT_SEQUENCE_EDITOR": shellCommand(seqScript),
		"GIT_EDITOR":          shellCommand(edScript),
	}
	args := []string{"rebase", "-i", "--autostash", "--no-autosquash"}
	if r.ver.atLeast(2, 38) {
		args = append(args, "--no-update-refs")
	}
	if root {
		args = append(args, "--root")
	} else {
		args = append(args, base)
	}
	res, err := r.run(cmdOpts{console: true, title: "git rebase -i", env: env,
		config: []string{"commit.cleanup=whitespace", "rebase.missingCommitsCheck=ignore", "rebase.abbreviateCommands=false"}}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		if files := r.unmergedFiles(); len(files) > 0 {
			return r.conflictResult(res, files), nil
		}
		if r.operation() == nil {
			r.clearRebaseSession()
		}
		return r.failResult(res), nil
	}
	if r.operation() != nil {
		// 停在某处（例如空提交）：交给 continue/skip/abort
		out := r.failResult(res)
		out.Reason = "stopped"
		out.Message = tr(r.ctx, "result.rebaseStopped")
		return out, nil
	}
	r.clearRebaseSession()
	return okResult(tr(r.ctx, "result.rebaseInteractiveDone")), nil
}

// allDropped 判断 steps 里是否全是 drop（这时 squash/fixup 没有可合并的前一个提交）。
func allDropped(steps []sdk.VCSRebaseStep) bool {
	for _, s := range steps {
		if s.Action != "drop" {
			return false
		}
	}
	return true
}

func slashPath(p string) string {
	if runtime.GOOS == "windows" {
		return filepath.ToSlash(p)
	}
	return p
}

// ---------------------------------------------------------------------------
// checkoutFile / discard

func (r *repo) checkoutFile(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSCheckoutFileArgs](raw)
	if err != nil {
		return nil, err
	}
	p, err := r.relPath(a.Path)
	if err != nil {
		return nil, err
	}
	ref := a.Ref
	if ref == "" {
		ref = "HEAD"
	}
	if err := r.checkRef(ref); err != nil {
		return nil, err
	}
	// restore：源版本里没有该文件时会删除它（与「从版本获取」一致）
	res, err := r.run(cmdOpts{}, "restore", "--source="+ref, "--staged", "--worktree", "--", ":(literal)"+p)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return r.failResult(res), nil
	}
	return okResult(tr(r.ctx, "result.fileRestored", p, ref)), nil
}

type discardArgs struct {
	sdk.VCSPathsArgs
	Mode    string `json:"mode,omitempty"`    // head（默认，恢复到 HEAD，暂存区一起恢复）| unstaged（只丢弃未暂存的修改，恢复到暂存区版本）
	KeepNew bool   `json:"keepNew,omitempty"` // 保留新文件：已暂存的新文件只取消暂存、未跟踪文件不删（IDEA Rollback 的默认做法）
}

// discard 丢弃文件的本地修改，让这些路径回到 HEAD 的样子（与内核 vcs.discard 的说明一致）：
// 已跟踪文件恢复到 HEAD（暂存区一起恢复）；HEAD 里没有的新文件（已暂存的新增、未跟踪）删除。
// keepNew=true 时新文件只取消暂存、不删除。不可撤销，界面要先确认。
func (r *repo) discard(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[discardArgs](raw)
	if err != nil {
		return nil, err
	}
	paths, err := r.checkPaths(a.Paths)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, invalidParams("paths is required")
	}
	if a.Mode == "unstaged" {
		res, err := r.run(cmdOpts{}, r.pathspecArgs([]string{"restore", "--worktree"}, paths)...)
		if err != nil {
			return nil, err
		}
		if !res.ok() {
			return r.failResult(res), nil
		}
		return okResult(tr(r.ctx, "result.discarded", len(paths))), nil
	}
	st, err := r.pathStatus(paths)
	if err != nil {
		return nil, err
	}
	// HEAD 里有的路径恢复到 HEAD；HEAD 里没有的（新增、重命名后的新路径）从暂存区移除
	var candidates, untracked []string
	renames := r.stagedRenames()
	for _, f := range st.Files {
		if f.Status == "?" {
			untracked = append(untracked, f.Path)
			continue
		}
		candidates = append(candidates, f.Path)
		if f.OldPath != "" {
			candidates = append(candidates, f.OldPath)
		} else if old := renames[f.Path]; old != "" {
			candidates = append(candidates, old)
		}
	}
	inHead := map[string]bool{}
	if r.headHash() != "" && len(candidates) > 0 {
		res, err := r.run(cmdOpts{read: true}, r.pathspecArgs([]string{"ls-tree", "-r", "-z", "--name-only", "HEAD"}, candidates)...)
		if err != nil {
			return nil, err
		}
		for _, p := range strings.Split(res.out, "\x00") {
			if p != "" {
				inHead[p] = true
			}
		}
	}
	var restore, added []string
	seen := map[string]bool{}
	for _, p := range candidates {
		if seen[p] {
			continue
		}
		seen[p] = true
		if inHead[p] {
			restore = append(restore, p)
		} else {
			added = append(added, p)
		}
	}
	if len(restore) > 0 {
		res, err := r.run(cmdOpts{}, r.pathspecArgs([]string{"restore", "--source=HEAD", "--staged", "--worktree"}, restore)...)
		if err != nil {
			return nil, err
		}
		if !res.ok() {
			return r.failResult(res), nil
		}
	}
	if len(added) > 0 {
		res, err := r.run(cmdOpts{}, r.pathspecArgs([]string{"rm", "--cached", "-q", "-r", "--ignore-unmatch", "--force"}, added)...)
		if err != nil {
			return nil, err
		}
		if !res.ok() {
			return r.failResult(res), nil
		}
		if !a.KeepNew {
			for _, p := range added {
				_ = os.Remove(filepath.Join(r.root, filepath.FromSlash(p)))
			}
		}
	}
	if len(untracked) > 0 && !a.KeepNew {
		// 只删列出来的未跟踪文件（status 已按路径展开到文件），不用 clean 的目录递归
		for _, p := range untracked {
			_ = os.Remove(filepath.Join(r.root, filepath.FromSlash(p)))
		}
	}
	n := len(restore) + len(added)
	if !a.KeepNew {
		n += len(untracked)
	}
	return okResult(tr(r.ctx, "result.discarded", n)), nil
}
