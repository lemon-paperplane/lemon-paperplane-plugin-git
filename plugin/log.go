package git

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// commitInfo 是 sdk.VCSCommit 的 JSON 超集。
type commitInfo struct {
	Hash    string           `json:"hash"`
	Short   string           `json:"short"`
	Parents []string         `json:"parents,omitempty"`
	Author  string           `json:"author"`
	Email   string           `json:"email,omitempty"`
	Date    string           `json:"date"`
	Subject string           `json:"subject"`
	Refs    []sdk.VCSRef     `json:"refs,omitempty"`
	Graph   *sdk.VCSGraphRow `json:"graph,omitempty"`

	Path       string `json:"path,omitempty"`       // fileHistory：该提交里文件的路径（跟踪重命名）
	PathStatus string `json:"pathStatus,omitempty"` // fileHistory：该提交对文件做了什么（A/M/D/R）
}

const logFormat = "%x1e%H%x1f%h%x1f%P%x1f%an%x1f%ae%x1f%aI%x1f%s%x1f%D"

// logQuery 是一次 git log 查询。
type logQuery struct {
	revs        []string
	skip, limit int
	authors     []string
	grep        string
	since       string
	until       string
	paths       []string
	order       string // topo（默认）| date | none
	rewrite     bool   // --parents：路径过滤时取改写后的父提交，提交图才连得上
	follow      bool
	nameStatus  bool
	firstParent bool
}

func (q *logQuery) args() []string {
	a := []string{"log", "--no-color", "--decorate=full", "--encoding=UTF-8", "--format=" + logFormat}
	switch q.order {
	case "date":
		a = append(a, "--date-order")
	case "none":
	default:
		a = append(a, "--topo-order")
	}
	if q.skip > 0 {
		a = append(a, "--skip="+strconv.Itoa(q.skip))
	}
	if q.limit > 0 {
		a = append(a, "--max-count="+strconv.Itoa(q.limit+1))
	}
	for _, au := range q.authors {
		a = append(a, "--author="+au)
	}
	if q.grep != "" {
		a = append(a, "--grep="+q.grep)
	}
	if len(q.authors) > 0 || q.grep != "" {
		a = append(a, "--fixed-strings", "--regexp-ignore-case")
	}
	if q.since != "" {
		a = append(a, "--since="+q.since)
	}
	if q.until != "" {
		a = append(a, "--until="+q.until)
	}
	if q.rewrite {
		a = append(a, "--parents")
	}
	if q.follow {
		a = append(a, "--follow")
	}
	if q.firstParent {
		a = append(a, "--first-parent")
	}
	if q.nameStatus {
		a = append(a, "--name-status", "-z", "-M")
	}
	a = append(a, q.revs...)
	a = append(a, "--")
	if q.follow {
		// --follow 只接受单个普通路径
		return append(a, q.paths...)
	}
	return append(a, lit(q.paths)...)
}

// logCommits 执行查询并解析；hasMore 表示还有更多。
func (r *repo) logCommits(q logQuery) ([]commitInfo, bool, error) {
	var commits []commitInfo
	res, err := r.stream(cmdOpts{read: true}, q.args(), func(rd io.Reader) error {
		return parseLog(rd, q.nameStatus, func(c commitInfo) {
			commits = append(commits, c)
		})
	})
	if err != nil {
		return nil, false, err
	}
	if !res.ok() {
		t := res.combined()
		// 没有提交的分支：返回空
		if strings.Contains(t, "does not have any commits") || strings.Contains(t, "unknown revision") && r.headHash() == "" {
			return []commitInfo{}, false, nil
		}
		return nil, false, r.gitFailure(res)
	}
	more := false
	if q.limit > 0 && len(commits) > q.limit {
		commits = commits[:q.limit]
		more = true
	}
	if commits == nil {
		commits = []commitInfo{}
	}
	return commits, more, nil
}

// parseLog 解析 logFormat 输出（记录以 \x1e 开头）；nameStatus 时每条记录后面跟 -z 的 name-status。
func parseLog(rd io.Reader, nameStatus bool, emit func(commitInfo)) error {
	br := bufio.NewReaderSize(rd, 256*1024)
	// 跳过第一个 \x1e 之前的内容
	if _, err := br.ReadString('\x1e'); err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	for {
		rec, err := br.ReadString('\x1e')
		last := err == io.EOF
		if err != nil && !last {
			return err
		}
		rec = strings.TrimSuffix(rec, "\x1e")
		if rec != "" {
			emit(parseLogRecord(rec, nameStatus))
		}
		if last {
			return nil
		}
	}
}

func parseLogRecord(rec string, nameStatus bool) commitInfo {
	header := rec
	var tail string
	if nameStatus {
		if i := strings.IndexByte(rec, 0); i >= 0 {
			header, tail = rec[:i], rec[i+1:]
		}
	}
	header = strings.TrimRight(header, "\n")
	f := strings.SplitN(header, "\x1f", 8)
	for len(f) < 8 {
		f = append(f, "")
	}
	c := commitInfo{Hash: f[0], Short: f[1], Author: f[3], Email: f[4], Date: f[5], Subject: f[6]}
	if p := strings.Fields(f[2]); len(p) > 0 {
		c.Parents = p
	}
	c.Refs = parseDecorations(f[7])
	if nameStatus {
		toks := strings.Split(strings.TrimLeft(tail, "\n"), "\x00")
		for i := 0; i < len(toks); i++ {
			st := strings.TrimSpace(toks[i])
			if st == "" {
				continue
			}
			if st[0] == 'R' || st[0] == 'C' {
				if i+2 < len(toks) {
					c.PathStatus = st[:1]
					c.Path = toks[i+2]
					i += 2
				}
			} else if i+1 < len(toks) {
				c.PathStatus = st[:1]
				c.Path = toks[i+1]
				i++
			}
		}
	}
	return c
}

// parseDecorations 解析 --decorate=full 的 %D：用完整引用名区分本地/远程/标签（分支名本身可能含斜杠）。
func parseDecorations(d string) []sdk.VCSRef {
	d = strings.TrimSpace(d)
	if d == "" {
		return nil
	}
	var refs []sdk.VCSRef
	for _, part := range strings.Split(d, ", ") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "HEAD -> ") {
			refs = append(refs, sdk.VCSRef{Name: "HEAD", Kind: "head"})
			part = strings.TrimPrefix(part, "HEAD -> ")
		}
		switch {
		case part == "HEAD":
			refs = append(refs, sdk.VCSRef{Name: "HEAD", Kind: "head"})
		case strings.HasPrefix(part, "tag: "):
			refs = append(refs, sdk.VCSRef{Name: strings.TrimPrefix(strings.TrimPrefix(part, "tag: "), "refs/tags/"), Kind: "tag"})
		case strings.HasPrefix(part, "refs/heads/"):
			refs = append(refs, sdk.VCSRef{Name: strings.TrimPrefix(part, "refs/heads/"), Kind: "local"})
		case strings.HasPrefix(part, "refs/remotes/"):
			n := strings.TrimPrefix(part, "refs/remotes/")
			if strings.HasSuffix(n, "/HEAD") {
				continue
			}
			refs = append(refs, sdk.VCSRef{Name: n, Kind: "remote"})
		}
	}
	return refs
}

// ---------------------------------------------------------------------------
// log 操作

type logArgs struct {
	sdk.VCSLogArgs
	Refs        []string `json:"refs,omitempty"`        // 多个分支
	Authors     []string `json:"authors,omitempty"`     // 多个作者（或）
	Since       string   `json:"since,omitempty"`       // 如 2026-01-01、"2 weeks ago"
	Until       string   `json:"until,omitempty"`       //
	Paths       []string `json:"paths,omitempty"`       // 多个路径
	Order       string   `json:"order,omitempty"`       // topo（默认）| date
	FirstParent bool     `json:"firstParent,omitempty"` // 只看第一父提交链
	NoGraph     bool     `json:"noGraph,omitempty"`     // 不计算提交图
}

type logResult struct {
	Commits []commitInfo `json:"commits"`
	HasMore bool         `json:"hasMore"`
	Graph   bool         `json:"graph"` // 是否带提交图（按作者/关键词/日期过滤时不画图）
}

const maxGraphWindow = 100000

func (r *repo) log(raw json.RawMessage) (*logResult, error) {
	a, err := decodeArgs[logArgs](raw)
	if err != nil {
		return nil, err
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 200
	}
	q := logQuery{order: a.Order, grep: strings.TrimSpace(a.Grep), since: a.Since, until: a.Until, firstParent: a.FirstParent}
	if a.Author != "" {
		q.authors = append(q.authors, a.Author)
	}
	q.authors = append(q.authors, a.Authors...)
	refs := a.Refs
	if a.Ref != "" {
		refs = append([]string{a.Ref}, refs...)
	}
	all := false
	for _, ref := range refs {
		if ref == "--all" {
			all = true
			continue
		}
		if err := r.checkRef(ref); err != nil {
			return nil, err
		}
		q.revs = append(q.revs, ref)
	}
	if all {
		q.revs = append([]string{"HEAD", "--branches", "--remotes", "--tags"}, q.revs...)
	}
	if len(q.revs) == 0 {
		if r.headHash() == "" {
			return &logResult{Commits: []commitInfo{}}, nil
		}
		q.revs = []string{"HEAD"}
	}
	var paths []string
	if a.Path != "" {
		paths = append(paths, a.Path)
	}
	paths = append(paths, a.Paths...)
	if q.paths, err = r.checkPaths(paths); err != nil {
		return nil, err
	}
	filtered := len(q.authors) > 0 || q.grep != "" || q.since != "" || q.until != ""
	withGraph := !a.NoGraph && !filtered
	if len(q.paths) > 0 {
		q.rewrite = true
	}
	if !withGraph || a.Skip+limit > maxGraphWindow {
		q.skip, q.limit = a.Skip, limit
		commits, more, err := r.logCommits(q)
		if err != nil {
			return nil, err
		}
		return &logResult{Commits: commits, HasMore: more}, nil
	}
	// 提交图要从第一行连续算下来：多取一行（最后一行的连线取决于下一行），算完再切出这一页
	q.limit = a.Skip + limit + 1
	commits, more, err := r.logCommits(q)
	if err != nil {
		return nil, err
	}
	computeGraph(commits)
	if a.Skip >= len(commits) {
		return &logResult{Commits: []commitInfo{}, HasMore: false, Graph: true}, nil
	}
	end := a.Skip + limit
	if end < len(commits) {
		more = true
	} else {
		end = len(commits)
	}
	return &logResult{Commits: commits[a.Skip:end], HasMore: more, Graph: true}, nil
}

// ---------------------------------------------------------------------------
// show

type showArgs struct {
	sdk.VCSShowArgs
	Parent int `json:"parent,omitempty"` // 合并提交对比第几个父提交，默认 1
}

type commitDetail struct {
	Commit commitInfo   `json:"commit"`
	Body   string       `json:"body,omitempty"` // 主题行之后的正文
	Files  []fileChange `json:"files"`

	Message        string `json:"message,omitempty"` // 完整提交信息
	Committer      string `json:"committer,omitempty"`
	CommitterEmail string `json:"committerEmail,omitempty"`
	CommitterDate  string `json:"committerDate,omitempty"`
	Parent         int    `json:"parent,omitempty"` // 文件列表对比的是第几个父提交
}

func (r *repo) show(raw json.RawMessage) (*commitDetail, error) {
	a, err := decodeArgs[showArgs](raw)
	if err != nil {
		return nil, err
	}
	if err := r.checkRef(a.Hash); err != nil {
		return nil, err
	}
	out, err := r.out("log", "-1", "--no-walk", "--no-color", "--decorate=full", "--encoding=UTF-8",
		"--format="+logFormat+"%x1f%cn%x1f%ce%x1f%cI%x1f%B", a.Hash, "--")
	if err != nil {
		return nil, err
	}
	rec := strings.TrimPrefix(out, "\x1e")
	f := strings.SplitN(rec, "\x1f", 12)
	for len(f) < 12 {
		f = append(f, "")
	}
	d := &commitDetail{Commit: parseLogRecord(strings.Join(f[:8], "\x1f"), false)}
	d.Committer, d.CommitterEmail, d.CommitterDate = f[8], f[9], f[10]
	d.Message = strings.TrimRight(f[11], "\n")
	if _, body, ok := strings.Cut(d.Message, "\n"); ok {
		d.Body = strings.TrimSpace(body)
	}
	parent := a.Parent
	if parent <= 0 {
		parent = 1
	}
	if parent > len(d.Commit.Parents) {
		parent = 0
	}
	d.Parent = parent
	from := r.emptyTree()
	if parent > 0 {
		from = d.Commit.Parents[parent-1]
	}
	files, err := r.treeChanges(from, d.Commit.Hash)
	if err != nil {
		return nil, err
	}
	d.Files = files
	return d, nil
}

// treeChanges 两个树之间的文件变化（名字状态 + 增删行数，并行取）。
func (r *repo) treeChanges(from, to string) ([]fileChange, error) {
	var (
		wg    sync.WaitGroup
		stats map[string]numstat
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := r.run(cmdOpts{read: true, maxOut: 16 << 20}, "diff", "--numstat", "-z", "-M", "--no-ext-diff", "--no-textconv", "--no-color", from, to, "--")
		if err == nil && res.ok() {
			stats = parseNumstatZ(res.out)
		}
	}()
	res, err := r.run(cmdOpts{read: true, maxOut: 16 << 20}, "diff", "--name-status", "-z", "-M", "--no-ext-diff", "--no-color", from, to, "--")
	wg.Wait()
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return nil, r.gitFailure(res)
	}
	files := parseNameStatusZ(res.out)
	for i := range files {
		if s, ok := stats[files[i].Path]; ok {
			files[i].Additions, files[i].Deletions, files[i].Binary = s.add, s.del, s.binary
		}
	}
	return files, nil
}

// parseNameStatusZ 解析 --name-status -z。
func parseNameStatusZ(out string) []fileChange {
	files := []fileChange{}
	toks := strings.Split(out, "\x00")
	for i := 0; i < len(toks); i++ {
		st := strings.TrimSpace(toks[i])
		if st == "" {
			continue
		}
		switch st[0] {
		case 'R', 'C':
			if i+2 < len(toks) {
				files = append(files, fileChange{Status: st[:1], OldPath: toks[i+1], Path: toks[i+2]})
				i += 2
			}
		default:
			if i+1 < len(toks) {
				files = append(files, fileChange{Status: st[:1], Path: toks[i+1]})
				i++
			}
		}
	}
	return files
}

// ---------------------------------------------------------------------------
// fileHistory

type fileHistoryArgs struct {
	sdk.VCSFileHistoryArgs
	Skip   int    `json:"skip,omitempty"`
	Ref    string `json:"ref,omitempty"`    // 默认 HEAD；"--all" 表示全部分支
	Follow *bool  `json:"follow,omitempty"` // 跟踪重命名，默认 true
}

func (r *repo) fileHistory(raw json.RawMessage) (*logResult, error) {
	a, err := decodeArgs[fileHistoryArgs](raw)
	if err != nil {
		return nil, err
	}
	p, err := r.relPath(a.Path)
	if err != nil {
		return nil, err
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 200
	}
	q := logQuery{skip: a.Skip, limit: limit, paths: []string{p}, nameStatus: true, follow: a.Follow == nil || *a.Follow, order: "none"}
	switch a.Ref {
	case "":
		if r.headHash() == "" {
			return &logResult{Commits: []commitInfo{}}, nil
		}
		q.revs = []string{"HEAD"}
	case "--all":
		q.revs = []string{"HEAD", "--branches", "--remotes", "--tags"}
	default:
		if err := r.checkRef(a.Ref); err != nil {
			return nil, err
		}
		q.revs = []string{a.Ref}
	}
	commits, more, err := r.logCommits(q)
	if err != nil {
		return nil, err
	}
	// 合并提交没有 name-status：沿用较新一条的路径
	cur := p
	for i := range commits {
		if commits[i].Path == "" {
			commits[i].Path = cur
		} else {
			cur = commits[i].Path
		}
	}
	return &logResult{Commits: commits, HasMore: more}, nil
}

// ---------------------------------------------------------------------------
// compare

type compareResult struct {
	Ahead     []commitInfo `json:"ahead"`
	Behind    []commitInfo `json:"behind"`
	Files     []fileChange `json:"files"`
	MergeBase string       `json:"mergeBase,omitempty"`
	Truncated bool         `json:"truncated,omitempty"` // 提交太多，列表只含前 1000 个
}

func (r *repo) compare(raw json.RawMessage) (*compareResult, error) {
	a, err := decodeArgs[sdk.VCSCompareArgs](raw)
	if err != nil {
		return nil, err
	}
	head := a.Head
	if head == "" {
		head = "HEAD"
	}
	if err := r.checkRef(a.Base); err != nil {
		return nil, err
	}
	if err := r.checkRef(head); err != nil {
		return nil, err
	}
	if r.revParse(a.Base) == "" {
		return nil, userError(r.ctx, "invalidRef", "error.invalidRef", a.Base)
	}
	if r.revParse(head) == "" {
		return nil, userError(r.ctx, "invalidRef", "error.invalidRef", head)
	}
	const limit = 1000
	out := &compareResult{}
	var wg sync.WaitGroup
	var errA, errB error
	var moreA, moreB bool
	wg.Add(2)
	go func() {
		defer wg.Done()
		out.Ahead, moreA, errA = r.logCommits(logQuery{revs: []string{a.Base + ".." + head}, limit: limit})
	}()
	go func() {
		defer wg.Done()
		out.Behind, moreB, errB = r.logCommits(logQuery{revs: []string{head + ".." + a.Base}, limit: limit})
	}()
	mb, _ := r.out("merge-base", a.Base, head)
	out.MergeBase = strings.TrimSpace(mb)
	var files []fileChange
	var errF error
	if out.MergeBase != "" {
		files, errF = r.treeChanges(out.MergeBase, head)
	} else {
		files, errF = r.treeChanges(a.Base, head)
	}
	wg.Wait()
	for _, e := range []error{errA, errB, errF} {
		if e != nil {
			return nil, e
		}
	}
	out.Files = files
	out.Truncated = moreA || moreB
	return out, nil
}

// ---------------------------------------------------------------------------
// blame

type blameArgs struct {
	sdk.VCSBlameArgs
	IgnoreWhitespace bool `json:"ignoreWhitespace,omitempty"`
	DetectMoves      bool `json:"detectMoves,omitempty"` // -M -C：追溯文件内/跨文件移动的行
}

type blameLine struct {
	Line        int    `json:"line"`
	Hash        string `json:"hash"`
	Author      string `json:"author"`
	Date        string `json:"date"`
	Summary     string `json:"summary,omitempty"`
	Email       string `json:"email,omitempty"`
	Text        string `json:"text"`                  // 行内容（界面没有编辑器，靠它显示）
	OrigLine    int    `json:"origLine,omitempty"`    // 该行在那个提交里的行号
	Filename    string `json:"filename,omitempty"`    // 该行在那个提交里的文件名（重命名过时与当前不同）
	Uncommitted bool   `json:"uncommitted,omitempty"` // 尚未提交的修改
	Boundary    bool   `json:"boundary,omitempty"`
}

type blameResult struct {
	Lines     []blameLine `json:"lines"`
	Truncated bool        `json:"truncated,omitempty"`
}

const blameMaxLines = 50000

func (r *repo) blame(raw json.RawMessage) (*blameResult, error) {
	a, err := decodeArgs[blameArgs](raw)
	if err != nil {
		return nil, err
	}
	p, err := r.relPath(a.Path)
	if err != nil {
		return nil, err
	}
	args := []string{"blame", "--porcelain", "--encoding=UTF-8"}
	if a.IgnoreWhitespace {
		args = append(args, "-w")
	}
	if a.DetectMoves {
		args = append(args, "-M", "-C")
	}
	if a.Ref != "" {
		if err := r.checkRef(a.Ref); err != nil {
			return nil, err
		}
		args = append(args, a.Ref)
	}
	args = append(args, "--", p)
	out := &blameResult{Lines: []blameLine{}}
	res, err := r.stream(cmdOpts{read: true}, args, func(rd io.Reader) error {
		return parseBlamePorcelain(rd, func(l blameLine) error {
			if len(out.Lines) >= blameMaxLines {
				out.Truncated = true
				return errStop
			}
			out.Lines = append(out.Lines, l)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return nil, r.gitFailure(res)
	}
	return out, nil
}

type blameCommit struct {
	author, mail, summary, filename string
	time                            int64
	tz                              string
	boundary                        bool
}

func parseBlamePorcelain(rd io.Reader, emit func(blameLine) error) error {
	br := bufio.NewReaderSize(rd, 256*1024)
	commits := map[string]*blameCommit{}
	var cur *blameCommit
	var line blameLine
	for {
		s, err := br.ReadString('\n')
		if err != nil && (err != io.EOF || s == "") {
			if err == io.EOF {
				return nil
			}
			return err
		}
		s = strings.TrimSuffix(s, "\n")
		if strings.HasPrefix(s, "\t") {
			if cur != nil {
				line.Author = cur.author
				line.Email = strings.Trim(cur.mail, "<>")
				line.Summary = cur.summary
				line.Date = formatUnix(cur.time, cur.tz)
				line.Filename = cur.filename
				line.Boundary = cur.boundary
			}
			line.Text = strings.TrimSuffix(s[1:], "\r")
			if e := emit(line); e != nil {
				return e
			}
			line = blameLine{}
			continue
		}
		f := strings.Fields(s)
		if len(f) >= 3 && len(f[0]) >= 40 && isHex(f[0]) {
			h := f[0]
			cur = commits[h]
			if cur == nil {
				cur = &blameCommit{}
				commits[h] = cur
			}
			line = blameLine{Hash: h}
			line.OrigLine, _ = strconv.Atoi(f[1])
			line.Line, _ = strconv.Atoi(f[2])
			line.Uncommitted = strings.Trim(h, "0") == ""
			continue
		}
		if cur == nil {
			continue
		}
		key, val, _ := strings.Cut(s, " ")
		switch key {
		case "author":
			cur.author = val
		case "author-mail":
			cur.mail = val
		case "author-time":
			cur.time, _ = strconv.ParseInt(val, 10, 64)
		case "author-tz":
			cur.tz = val
		case "summary":
			cur.summary = val
		case "filename":
			cur.filename = val
		case "boundary":
			cur.boundary = true
		}
		if err == io.EOF {
			return nil
		}
	}
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// formatUnix 把 unix 时间 + git 时区（+0800）格式化为 RFC3339。
func formatUnix(sec int64, tz string) string {
	if sec == 0 {
		return ""
	}
	loc := time.UTC
	if len(tz) == 5 && (tz[0] == '+' || tz[0] == '-') {
		h, _ := strconv.Atoi(tz[1:3])
		m, _ := strconv.Atoi(tz[3:5])
		off := h*3600 + m*60
		if tz[0] == '-' {
			off = -off
		}
		loc = time.FixedZone(tz, off)
	}
	return time.Unix(sec, 0).In(loc).Format(time.RFC3339)
}

// ---------------------------------------------------------------------------
// containingBranches：包含某提交的分支与标签（提交详情里「所在分支」）

type containingResult struct {
	Refs      []sdk.VCSRef `json:"refs"`
	Truncated bool         `json:"truncated,omitempty"`
}

func (r *repo) containingBranches(raw json.RawMessage) (*containingResult, error) {
	a, err := decodeArgs[sdk.VCSShowArgs](raw)
	if err != nil {
		return nil, err
	}
	if err := r.checkRef(a.Hash); err != nil {
		return nil, err
	}
	out, err := r.out("for-each-ref", "--contains", a.Hash, "--format=%(refname)", "refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		return nil, err
	}
	res := &containingResult{Refs: []sdk.VCSRef{}}
	for _, l := range strings.Split(out, "\n") {
		if rs := parseDecorations(l); len(rs) > 0 {
			res.Refs = append(res.Refs, rs...)
		} else if strings.HasPrefix(l, "refs/tags/") {
			res.Refs = append(res.Refs, sdk.VCSRef{Name: strings.TrimPrefix(l, "refs/tags/"), Kind: "tag"})
		}
		if len(res.Refs) >= 500 {
			res.Truncated = true
			break
		}
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// fileContent：某版本的文件内容

type fileContentArgs struct {
	Path     string `json:"path"`
	Ref      string `json:"ref,omitempty"` // 空：工作区文件；":" 或 "INDEX"：暂存区版本；其它：提交/分支
	MaxBytes int    `json:"maxBytes,omitempty"`
}

type fileContentResult struct {
	Path     string `json:"path"`
	Ref      string `json:"ref,omitempty"`
	Exists   bool   `json:"exists"`
	Size     int64  `json:"size"`
	Binary   bool   `json:"binary,omitempty"`
	TooLarge bool   `json:"tooLarge,omitempty"`
	Text     string `json:"text,omitempty"`   // 文本文件（UTF-8）
	Base64   string `json:"base64,omitempty"` // 二进制文件（如图片对比）或非 UTF-8 文本
	NotUTF8  bool   `json:"notUtf8,omitempty"`
}

func (r *repo) fileContent(raw json.RawMessage) (*fileContentResult, error) {
	a, err := decodeArgs[fileContentArgs](raw)
	if err != nil {
		return nil, err
	}
	p, err := r.relPath(a.Path)
	if err != nil {
		return nil, err
	}
	max := a.MaxBytes
	if max <= 0 {
		max = 10 << 20
	}
	out := &fileContentResult{Path: p, Ref: a.Ref}
	var data []byte
	switch a.Ref {
	case "":
		full := filepath.Join(r.root, filepath.FromSlash(p))
		st, err := os.Stat(full)
		if err != nil || st.IsDir() {
			return out, nil
		}
		out.Exists, out.Size = true, st.Size()
		if st.Size() > int64(max) {
			out.TooLarge = true
			return out, nil
		}
		if data, err = os.ReadFile(full); err != nil {
			return nil, err
		}
	default:
		spec := a.Ref + ":" + p
		if a.Ref == ":" || a.Ref == "INDEX" {
			spec = ":" + p
		} else if err := r.checkRef(a.Ref); err != nil {
			return nil, err
		}
		res, err := r.run(cmdOpts{read: true}, "cat-file", "-s", spec)
		if err != nil {
			return nil, err
		}
		if !res.ok() {
			return out, nil
		}
		out.Exists = true
		out.Size, _ = strconv.ParseInt(strings.TrimSpace(res.out), 10, 64)
		if out.Size > int64(max) {
			out.TooLarge = true
			return out, nil
		}
		res, err = r.run(cmdOpts{read: true, maxOut: max + 1}, "cat-file", "blob", spec)
		if err != nil {
			return nil, err
		}
		if !res.ok() {
			return nil, r.gitFailure(res)
		}
		data = []byte(res.out)
	}
	switch {
	case isBinary(data):
		out.Binary = true
		out.Base64 = base64.StdEncoding.EncodeToString(data)
	case !utf8.Valid(data):
		// 不是 UTF-8 的文本（如 GBK）：原样给 base64，界面按需解码
		out.NotUTF8 = true
		out.Base64 = base64.StdEncoding.EncodeToString(data)
	default:
		out.Text = string(data)
	}
	return out, nil
}

// isBinary 与 git 的判断一致：前 8000 字节里有 NUL 即二进制。
func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	for _, c := range b[:n] {
		if c == 0 {
			return true
		}
	}
	return false
}
