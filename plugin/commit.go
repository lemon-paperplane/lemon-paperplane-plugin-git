package git

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// commitArgs 是 sdk.VCSCommitArgs 加扩展字段。
//
// 两种提交模式：
//   - Paths 非空（IDEA 默认模式，无暂存区）：只提交这些文件在工作区里的当前内容（含未跟踪的新文件、删除、重命名），
//     不带上别的已暂存内容，别的已暂存内容提交后仍保持暂存。实现：先 git add 选中的未跟踪文件，再 git commit --only -- 路径。
//   - Paths 为空（暂存区模式）：提交暂存区里的内容（等同 git commit）；暂存区为空且不是 amend 时报 nothingToCommit。
//     All=true 时先 git add -A 再提交全部变更。
type commitArgs struct {
	sdk.VCSCommitArgs
	All         bool             `json:"all,omitempty"`         // 提交全部变更（含未跟踪），Paths 为空时有效
	NoVerify    bool             `json:"noVerify,omitempty"`    // 跳过 pre-commit / commit-msg 钩子
	Hunks       map[string][]int `json:"hunks,omitempty"`       // 部分提交：文件 → 块下标（基于 mode=head 的 diff）
	HunkContext *int             `json:"hunkContext,omitempty"` // 取那份 diff 用的上下文行数，默认 3
}

type commitResult struct {
	Hash  string    `json:"hash"`
	Push  *opResult `json:"push,omitempty"`
	Short string    `json:"short,omitempty"`
}

func (r *repo) commit(raw json.RawMessage) (*commitResult, error) {
	a, err := decodeArgs[commitArgs](raw)
	if err != nil {
		return nil, err
	}
	msg := normalizeMessage(a.Message)
	op := r.operation()
	inMerge := op != nil && (op.Op == "merge" || op.Op == "cherry-pick" || op.Op == "revert")
	if msg == "" && !a.Amend && !(inMerge && op.Op == "merge") {
		return nil, userError(r.ctx, "emptyMessage", "error.emptyMessage")
	}
	if err := r.checkIdentity(); err != nil {
		return nil, err
	}
	paths, err := r.checkPaths(a.Paths)
	if err != nil {
		return nil, err
	}
	flags := []string{"commit"}
	if a.Amend {
		flags = append(flags, "--amend")
	}
	if a.SignOff {
		flags = append(flags, "--signoff")
	}
	if a.Author != "" {
		if !strings.Contains(a.Author, "<") {
			return nil, userError(r.ctx, "invalidAuthor", "error.invalidAuthor", a.Author)
		}
		flags = append(flags, "--author="+a.Author)
	}
	if a.NoVerify {
		flags = append(flags, "--no-verify")
	}
	if msg != "" {
		file, cleanup, err := r.msgFile(msg)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		flags = append(flags, "-F", file)
	} else {
		flags = append(flags, "--no-edit")
	}

	switch {
	case len(a.Hunks) > 0:
		if inMerge {
			return nil, userError(r.ctx, "partialDuringMerge", "error.partialDuringMerge")
		}
		ctxLines := 3
		if a.HunkContext != nil {
			ctxLines = *a.HunkContext
		}
		if err := r.commitPartial(flags, paths, a.Hunks, ctxLines); err != nil {
			return nil, err
		}
	case len(paths) > 0 && inMerge:
		// 合并/拣选进行中不能部分提交：把选中文件加入暂存区后提交整个暂存区
		if _, err := r.do(cmdOpts{}, r.pathspecArgs([]string{"add", "-A"}, paths)...); err != nil {
			return nil, err
		}
		if err := r.runCommit(flags); err != nil {
			return nil, err
		}
	case len(paths) > 0:
		if err := r.commitPaths(flags, paths, a.Amend); err != nil {
			return nil, err
		}
	default:
		if a.All {
			if _, err := r.do(cmdOpts{}, "add", "-A"); err != nil {
				return nil, err
			}
		} else if !a.Amend && !inMerge {
			res, err := r.run(cmdOpts{read: true}, "diff", "--cached", "--quiet", "--no-ext-diff")
			if err != nil {
				return nil, err
			}
			if res.code == 0 {
				return nil, userError(r.ctx, "nothingToCommit", "error.nothingStaged")
			}
		}
		if err := r.runCommit(flags); err != nil {
			return nil, err
		}
	}
	out := &commitResult{Hash: r.headHash()}
	out.Short = shortHash(out.Hash)
	if a.Push {
		pr, err := r.push(&pushArgs{})
		if err != nil {
			if e, ok := err.(*sdk.Error); ok {
				pr = &opResult{VCSOpResult: sdk.VCSOpResult{OK: false, Message: e.Message}, Reason: reasonOf(e)}
			} else {
				return out, err
			}
		}
		out.Push = pr
	}
	return out, nil
}

func reasonOf(e *sdk.Error) string {
	if m, ok := e.Data.(map[string]any); ok {
		if s, ok := m["reason"].(string); ok {
			return s
		}
	}
	return "failed"
}

// runCommit 执行 git commit（钩子输出进任务控制台），失败时给出明确原因。
func (r *repo) runCommit(args []string, extraEnv ...map[string]string) error {
	o := cmdOpts{console: true, title: "git commit"}
	if len(extraEnv) > 0 {
		o.env = extraEnv[0]
	}
	res, err := r.run(o, args...)
	if err != nil {
		return err
	}
	if res.ok() {
		return nil
	}
	return r.commitFailure(res)
}

func (r *repo) commitFailure(res *result) error {
	text := res.combined()
	switch classifyFailure(text) {
	case "identityMissing":
		return r.identityError(nil)
	case "emptyCommit":
		return userError(r.ctx, "nothingToCommit", "error.nothingToCommit")
	case "unresolvedConflicts":
		return userError(r.ctx, "unresolvedConflicts", "result.unresolvedConflicts")
	case "locked":
		return userError(r.ctx, "locked", "result.locked")
	}
	return userErrorData(r.ctx, "commitFailed", map[string]any{"output": truncateOutput(text)}, "error.commitFailed", firstErrorLine(text))
}

// commitPaths 实现默认模式：只提交选中路径，不动其它已暂存内容。
func (r *repo) commitPaths(flags, paths []string, amend bool) error {
	st, err := r.pathStatus(paths)
	if err != nil {
		return err
	}
	var untracked []string
	expanded := append([]string{}, paths...)
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p] = true
	}
	changed := 0
	renames := r.stagedRenames()
	for _, f := range st.Files {
		changed++
		if f.Status == "?" {
			untracked = append(untracked, f.Path)
		}
		// 已暂存的重命名：旧路径必须一起提交，否则旧文件会留在提交里。
		// 带路径过滤的 status 只看到新路径（显示为新增），所以从暂存区的重命名检测里找旧路径。
		old := f.OldPath
		if old == "" {
			old = renames[f.Path]
		}
		if old != "" && !seen[old] {
			seen[old] = true
			expanded = append(expanded, old)
		}
	}
	if changed == 0 && !amend {
		return userError(r.ctx, "nothingToCommit", "error.nothingToCommit")
	}
	if len(untracked) > 0 {
		if _, err := r.do(cmdOpts{}, r.pathspecArgs([]string{"add"}, untracked)...); err != nil {
			return err
		}
	}
	args := append([]string{}, flags...)
	args = append(args, "--only")
	cerr := r.runCommit(r.pathspecArgs(args, expanded))
	if cerr != nil && len(untracked) > 0 {
		// 提交失败：把刚 add 的新文件退回未跟踪状态
		_, _ = r.run(cmdOpts{}, r.pathspecArgs([]string{"rm", "--cached", "-q", "--ignore-unmatch"}, untracked)...)
	}
	return cerr
}

// stagedRenames 返回暂存区里的重命名：新路径 → 旧路径。
func (r *repo) stagedRenames() map[string]string {
	m := map[string]string{}
	res, err := r.run(cmdOpts{read: true, maxOut: 16 << 20}, "diff", "--cached", "--name-status", "-z", "-M", "--no-ext-diff", "--diff-filter=R")
	if err != nil || !res.ok() {
		return m
	}
	for _, f := range parseNameStatusZ(res.out) {
		if f.Status == "R" {
			m[f.Path] = f.OldPath
		}
	}
	return m
}

// pathStatus 取若干路径的状态（含未跟踪文件）。
func (r *repo) pathStatus(paths []string) (*statusResult, error) {
	args := r.pathspecArgs([]string{"status", "--porcelain=v2", "-z", "--untracked-files=all"}, paths)
	st := &statusResult{}
	res, err := r.stream(cmdOpts{read: true}, args, func(rd io.Reader) error {
		return parseStatusV2(rd, st, 1<<30, 0)
	})
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return nil, r.gitFailure(res)
	}
	return st, nil
}

// commitPartial 部分提交（选中文件的部分块）：在临时索引里从 HEAD 出发，加入整文件与选中的块后提交，
// 再把这些路径的真实索引项重置为新提交的内容。用户其它已暂存内容不受影响。
func (r *repo) commitPartial(flags, paths []string, hunks map[string][]int, ctxLines int) error {
	tmp := filepath.Join(r.cacheDir(), "index-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	defer func() {
		_ = os.Remove(tmp)
		_ = os.Remove(tmp + ".lock")
	}()
	env := map[string]string{"GIT_INDEX_FILE": tmp}
	if r.headHash() != "" {
		if _, err := r.do(cmdOpts{env: env}, "read-tree", "HEAD"); err != nil {
			return err
		}
	} else if _, err := r.do(cmdOpts{env: env}, "read-tree", "--empty"); err != nil {
		return err
	}
	partial := map[string]bool{}
	var all []string
	for p := range hunks {
		rel, err := r.relPath(p)
		if err != nil {
			return err
		}
		partial[rel] = true
		all = append(all, rel)
	}
	sort.Strings(all)
	var whole []string
	for _, p := range paths {
		if !partial[p] {
			whole = append(whole, p)
			all = append(all, p)
		}
	}
	if len(whole) > 0 {
		if _, err := r.do(cmdOpts{env: env}, r.pathspecArgs([]string{"add", "-A"}, whole)...); err != nil {
			return err
		}
	}
	for p, idx := range hunks {
		rel, _ := r.relPath(p)
		fd, _, _, _, err := r.rawFileDiff(&diffArgs{VCSDiffArgs: sdk.VCSDiffArgs{Path: rel}, Mode: diffModeHead}, ctxLines, false)
		if err != nil {
			return err
		}
		if fd == nil || len(fd.hunks) == 0 {
			return userError(r.ctx, "staleDiff", "error.staleDiff")
		}
		sel, err := selectHunks(fd, idx, nil)
		if err != nil {
			return userError(r.ctx, "staleDiff", "error.staleDiff")
		}
		args := []string{"apply", "--cached", "--recount", "--whitespace=nowarn"}
		if ctxLines == 0 {
			args = append(args, "--unidiff-zero")
		}
		res, err := r.run(cmdOpts{env: env, stdin: buildPatch(fd, sel, false)}, append(args, "-")...)
		if err != nil {
			return err
		}
		if !res.ok() {
			return userErrorData(r.ctx, "applyFailed", map[string]any{"output": truncateOutput(res.combined())}, "result.applyFailed", firstErrorLine(res.combined()))
		}
	}
	if err := r.runCommit(flags, env); err != nil {
		return err
	}
	// 真实索引：提交过的路径对齐到新 HEAD，剩下的改动留在工作区
	_, _ = r.run(cmdOpts{}, r.pathspecArgs([]string{"reset", "-q", "HEAD"}, all)...)
	return nil
}
