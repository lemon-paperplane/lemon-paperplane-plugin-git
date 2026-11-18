package git

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// dirs 返回本工作树的 git 目录与公共目录（worktree/子模块里 .git 是文件，要跟进去）。
func (r *repo) dirs() (gitDir, commonDir string) {
	if r.dirsResolved {
		return r.gitDir, r.commonDir
	}
	r.dirsResolved = true
	dotGit := filepath.Join(r.root, ".git")
	if st, err := os.Stat(dotGit); err == nil {
		if st.IsDir() {
			r.gitDir = dotGit
		} else if b, err := os.ReadFile(dotGit); err == nil {
			line := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
			if strings.HasPrefix(line, "gitdir:") {
				d := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
				if !filepath.IsAbs(d) {
					d = filepath.Join(r.root, d)
				}
				r.gitDir = filepath.Clean(d)
			}
		}
	}
	if r.gitDir == "" {
		// 根目录下没有 .git（例如 Root 是子目录）：问 git
		if out, err := r.out("rev-parse", "--absolute-git-dir", "--git-common-dir"); err == nil {
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if len(lines) >= 1 {
				r.gitDir = strings.TrimSpace(lines[0])
			}
			if len(lines) >= 2 {
				c := strings.TrimSpace(lines[1])
				if !filepath.IsAbs(c) {
					c = filepath.Join(r.root, c)
				}
				r.commonDir = filepath.Clean(c)
			}
		}
	}
	if r.commonDir == "" && r.gitDir != "" {
		r.commonDir = r.gitDir
		if b, err := os.ReadFile(filepath.Join(r.gitDir, "commondir")); err == nil {
			c := strings.TrimSpace(string(b))
			if !filepath.IsAbs(c) {
				c = filepath.Join(r.gitDir, c)
			}
			r.commonDir = filepath.Clean(c)
		}
	}
	return r.gitDir, r.commonDir
}

func (r *repo) gitPath(name string) string {
	g, _ := r.dirs()
	if g == "" {
		return ""
	}
	return filepath.Join(g, filepath.FromSlash(name))
}

func exists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// opState 是进行中的操作（看 git 目录下的标记文件，不跑命令）。
type opState struct {
	Op       string `json:"op"`                 // merge | rebase | cherry-pick | revert | am
	Step     int    `json:"step,omitempty"`     // rebase/am 当前第几个
	Total    int    `json:"total,omitempty"`    // 共几个
	HeadName string `json:"headName,omitempty"` // rebase 中的分支
	Onto     string `json:"onto,omitempty"`     // rebase 的目标提交
	Current  string `json:"current,omitempty"`  // 正在应用/合并的提交
	Message  string `json:"message,omitempty"`  // MERGE_MSG 第一行
}

func (r *repo) operation() *opState {
	g, _ := r.dirs()
	if g == "" {
		return nil
	}
	if d := filepath.Join(g, "rebase-merge"); exists(d) {
		s := &opState{Op: "rebase"}
		s.Step, _ = strconv.Atoi(readTrim(filepath.Join(d, "msgnum")))
		s.Total, _ = strconv.Atoi(readTrim(filepath.Join(d, "end")))
		s.HeadName = strings.TrimPrefix(readTrim(filepath.Join(d, "head-name")), "refs/heads/")
		s.Onto = readTrim(filepath.Join(d, "onto"))
		s.Current = readTrim(filepath.Join(d, "stopped-sha"))
		return s
	}
	if d := filepath.Join(g, "rebase-apply"); exists(d) {
		s := &opState{Op: "rebase"}
		if exists(filepath.Join(d, "applying")) {
			s.Op = "am"
		}
		s.Step, _ = strconv.Atoi(readTrim(filepath.Join(d, "next")))
		s.Total, _ = strconv.Atoi(readTrim(filepath.Join(d, "last")))
		s.HeadName = strings.TrimPrefix(readTrim(filepath.Join(d, "head-name")), "refs/heads/")
		s.Onto = readTrim(filepath.Join(d, "onto"))
		return s
	}
	if h := readTrim(filepath.Join(g, "MERGE_HEAD")); h != "" {
		return &opState{Op: "merge", Current: firstLine(h), Message: firstLine(readTrim(filepath.Join(g, "MERGE_MSG")))}
	}
	if h := readTrim(filepath.Join(g, "CHERRY_PICK_HEAD")); h != "" {
		return &opState{Op: "cherry-pick", Current: h}
	}
	if h := readTrim(filepath.Join(g, "REVERT_HEAD")); h != "" {
		return &opState{Op: "revert", Current: h}
	}
	// 多个提交的 cherry-pick/revert：冲突解决并提交后 *_HEAD 没了，但 sequencer 还在
	if todo := readTrim(filepath.Join(g, "sequencer", "todo")); todo != "" {
		f := strings.Fields(firstLine(todo))
		if len(f) > 0 {
			switch f[0] {
			case "revert", "r":
				return &opState{Op: "revert"}
			default:
				return &opState{Op: "cherry-pick"}
			}
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// identityError 生成「没有设置 user.name/email」错误，界面据此弹框（配合 configSet）。
func (r *repo) identityError(missing []string) error {
	if len(missing) == 0 {
		missing = []string{"user.name", "user.email"}
	}
	return userErrorData(r.ctx, "identityMissing", map[string]any{"missing": missing}, "error.identityMissing", strings.Join(missing, ", "))
}

// checkIdentity 确认提交身份已显式配置（git 会自动拼 user@host，IDEA 也要求显式设置）。
func (r *repo) checkIdentity() error {
	res, err := r.run(cmdOpts{read: true}, "config", "-z", "--get-regexp", `^user\.(name|email)$`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, rec := range strings.Split(res.out, "\x00") {
		k, v, _ := strings.Cut(rec, "\n")
		if strings.TrimSpace(v) != "" {
			have[strings.ToLower(k)] = true
		}
	}
	env := func(keys ...string) bool {
		for _, k := range keys {
			if r.p.extraEnv[k] != "" || os.Getenv(k) != "" {
				return true
			}
		}
		return false
	}
	var missing []string
	if !have["user.name"] && !env("GIT_COMMITTER_NAME") {
		missing = append(missing, "user.name")
	}
	if !have["user.email"] && !env("GIT_COMMITTER_EMAIL", "EMAIL") {
		missing = append(missing, "user.email")
	}
	if len(missing) > 0 {
		return r.identityError(missing)
	}
	return nil
}

// currentBranch 返回当前分支名；分离头时 detached=true。未出生的分支（还没有提交）也能拿到名字。
func (r *repo) currentBranch() (name string, detached bool) {
	res, err := r.run(cmdOpts{read: true}, "symbolic-ref", "-q", "HEAD")
	if err != nil || !res.ok() {
		return "", true
	}
	return strings.TrimPrefix(strings.TrimSpace(res.out), "refs/heads/"), false
}

// headHash 返回 HEAD 的完整哈希；没有提交时为空。
func (r *repo) headHash() string {
	return r.revParse("HEAD")
}

// revParse 解析一个引用为完整哈希，失败返回空。
func (r *repo) revParse(ref string) string {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return ""
	}
	res, err := r.run(cmdOpts{read: true}, "rev-parse", "-q", "--verify", ref+"^{commit}")
	if err != nil || !res.ok() {
		return ""
	}
	return strings.TrimSpace(res.out)
}

// refExists 判断完整引用名（如 refs/heads/main）是否存在。
func (r *repo) refExists(full string) bool {
	res, err := r.run(cmdOpts{read: true}, "show-ref", "--verify", "-q", full)
	return err == nil && res.ok()
}

// remoteNames 返回远程名（按配置顺序）。
func (r *repo) remoteNames() []string {
	res, err := r.run(cmdOpts{read: true}, "remote")
	if err != nil || !res.ok() {
		return nil
	}
	var out []string
	for _, l := range strings.Split(res.out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// splitRemoteRef 把 "origin/feature/x" 拆成远程名和分支名（远程名可以带斜杠，取最长匹配）。
func splitRemoteRef(ref string, remotes []string) (remote, branch string, ok bool) {
	sorted := append([]string(nil), remotes...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, rm := range sorted {
		if strings.HasPrefix(ref, rm+"/") && len(ref) > len(rm)+1 {
			return rm, ref[len(rm)+1:], true
		}
	}
	return "", "", false
}

// emptyTree 返回空树的对象名（sha1/sha256 仓库都对）。
func (r *repo) emptyTree() string {
	res, err := r.run(cmdOpts{read: true, stdin: ""}, "hash-object", "-t", "tree", "--stdin")
	if err == nil && res.ok() {
		return strings.TrimSpace(res.out)
	}
	return "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
}

// stashCount 数储藏条目：refs/stash 的引用日志每行一条，直接读文件不跑命令。
func (r *repo) stashCount() int {
	_, common := r.dirs()
	if common == "" {
		return 0
	}
	if exists(filepath.Join(common, "reftable")) {
		res, err := r.run(cmdOpts{read: true}, "rev-list", "--walk-reflogs", "--count", "refs/stash")
		if err != nil || !res.ok() {
			return 0
		}
		n, _ := strconv.Atoi(strings.TrimSpace(res.out))
		return n
	}
	f, err := os.Open(filepath.Join(common, "logs", "refs", "stash"))
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n
}

// detect 实现 detect 操作。
func (r *repo) detect() (*sdk.VCSRepoInfo, error) {
	if r.root == "" {
		return &sdk.VCSRepoInfo{IsRepo: false}, nil
	}
	if !sdk.DirExists(r.root) {
		return &sdk.VCSRepoInfo{IsRepo: false}, nil
	}
	res, err := r.run(cmdOpts{read: true}, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return &sdk.VCSRepoInfo{IsRepo: false}, nil
	}
	top := filepath.FromSlash(strings.TrimSpace(res.out))
	return &sdk.VCSRepoInfo{IsRepo: true, Root: top, VCS: "git"}, nil
}

// msgFile 把提交信息写进缓存目录里的临时文件（-F 读取，避免命令行转义与长度问题）。
func (r *repo) msgFile(msg string) (string, func(), error) {
	f, err := os.CreateTemp(r.cacheDir(), "msg-*.txt")
	if err != nil {
		return "", func() {}, err
	}
	_, werr := f.WriteString(msg)
	cerr := f.Close()
	if werr != nil {
		_ = os.Remove(f.Name())
		return "", func() {}, werr
	}
	if cerr != nil {
		_ = os.Remove(f.Name())
		return "", func() {}, cerr
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

// cacheDir 返回插件缓存目录（内核给的 CacheDir，测试时为系统临时目录）。
func (r *repo) cacheDir() string {
	if r.ctx != nil && r.ctx.Init != nil && r.ctx.Init.CacheDir != "" {
		_ = os.MkdirAll(r.ctx.Init.CacheDir, 0o755)
		return r.ctx.Init.CacheDir
	}
	return os.TempDir()
}

// normalizeMessage 去掉提交信息首尾空白，保证以换行结尾。
func normalizeMessage(msg string) string {
	msg = strings.ReplaceAll(msg, "\r\n", "\n")
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	return msg + "\n"
}
