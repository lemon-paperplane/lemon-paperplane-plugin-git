package git

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// 差异的展示上限：超过就标记 TooLarge 不返回行（界面可以带 noLimit 再要一次）。
const (
	diffMaxBytes    = 1 << 20
	diffMaxLines    = 5000
	diffHardMax     = 32 << 20 // noLimit 时的绝对上限
	wholeFileUnifed = 1 << 24  // context=-1 时用的上下文行数
)

// 差异模式。界面取 diff 用什么模式，按块操作就要传同样的模式，块下标才对得上。
const (
	diffModeHead     = "head"     // HEAD → 工作区（IDEA 默认模式看到的差异；Commit 为空时的默认）
	diffModeUnstaged = "unstaged" // 暂存区 → 工作区（git diff）
	diffModeStaged   = "staged"   // HEAD → 暂存区（git diff --cached）
	diffModeCommit   = "commit"   // 某提交 → 它的父提交（Commit 非空时的默认）
	diffModeRange    = "range"    // Base → Head（Head 为空表示工作区）
)

// diffLine / diffHunk / diffResult 是 sdk.VCSDiff 的 JSON 超集。
type diffLine struct {
	Kind  string `json:"kind"`
	Old   int    `json:"old,omitempty"`
	New   int    `json:"new,omitempty"`
	Text  string `json:"text"`
	NoEOL bool   `json:"noEol,omitempty"` // 这一行是文件最后一行且没有换行符
}

type diffHunk struct {
	Header   string     `json:"header"`
	OldStart int        `json:"oldStart"`
	OldLines int        `json:"oldLines"`
	NewStart int        `json:"newStart"`
	NewLines int        `json:"newLines"`
	Lines    []diffLine `json:"lines"`
}

type diffResult struct {
	Path     string     `json:"path"`
	OldPath  string     `json:"oldPath,omitempty"`
	Binary   bool       `json:"binary,omitempty"`
	TooLarge bool       `json:"tooLarge,omitempty"`
	Hunks    []diffHunk `json:"hunks"`
	Language string     `json:"language,omitempty"`

	Mode       string `json:"mode"`                 // 生成本 diff 的模式（按块操作要传回同样的 mode）
	Context    int    `json:"context"`              // 上下文行数
	Status     string `json:"status,omitempty"`     // A 新增 | D 删除 | M 修改 | R 重命名 | C 复制 | T 类型变化
	OldMode    string `json:"oldMode,omitempty"`    // 文件模式，如 100644
	NewMode    string `json:"newMode,omitempty"`    // 100755 可执行 | 120000 符号链接 | 160000 子模块
	Similarity int    `json:"similarity,omitempty"` // 重命名相似度
	Additions  int    `json:"additions"`
	Deletions  int    `json:"deletions"`
	Untracked  bool   `json:"untracked,omitempty"`
}

type diffArgs struct {
	sdk.VCSDiffArgs
	Mode             string `json:"mode,omitempty"`
	Base             string `json:"base,omitempty"`      // range 模式
	Head             string `json:"head,omitempty"`      // range 模式，空表示工作区
	MergeBase        bool   `json:"mergeBase,omitempty"` // range 模式用 base...head（与比较页的文件列表一致）
	Parent           int    `json:"parent,omitempty"`    // commit 模式对比第几个父提交（合并提交），默认 1
	IgnoreWhitespace bool   `json:"ignoreWhitespace,omitempty"`
	NoLimit          bool   `json:"noLimit,omitempty"` // 不做 1 MB / 5000 行限制
	Staged           bool   `json:"staged,omitempty"`  // 等同 mode=staged（内核 API 的写法）
}

// ---------------------------------------------------------------------------
// 统一 diff 解析

// rawHunk 保留原始行（含 \r、"\ No newline" 行），重新生成补丁时原样写回。
type rawHunk struct {
	header                                 string
	section                                string
	oldStart, oldLines, newStart, newLines int
	lines                                  []string
}

type fileDiff struct {
	oldPath, newPath string
	header           []string // diff --git 到第一个 @@ 之前的行
	hunks            []rawHunk
	binary           bool
	status           string
	oldMode, newMode string
	similarity       int
}

// parseUnifiedDiff 解析 git diff 输出（可含多个文件）。按块头里的行数读取块体，块体内容不会被误认成文件头。
func parseUnifiedDiff(rd io.Reader) ([]*fileDiff, error) {
	br := bufio.NewReaderSize(rd, 256*1024)
	readLine := func() (string, bool, error) {
		s, err := br.ReadString('\n')
		if err == io.EOF {
			if s == "" {
				return "", false, nil
			}
			return s, true, nil
		}
		if err != nil {
			return "", false, err
		}
		return s[:len(s)-1], true, nil
	}
	var files []*fileDiff
	var cur *fileDiff
	var pending string
	havePending := false
	for {
		var line string
		var ok bool
		var err error
		if havePending {
			line, ok, havePending = pending, true, false
		} else {
			line, ok, err = readLine()
			if err != nil {
				return nil, err
			}
		}
		if !ok {
			break
		}
		switch {
		case strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "diff --cc ") || strings.HasPrefix(line, "diff --combined "):
			cur = &fileDiff{status: "M"}
			cur.header = append(cur.header, line)
			cur.oldPath, cur.newPath = parseDiffGitLine(line)
			files = append(files, cur)
		case cur == nil:
			continue
		case strings.HasPrefix(line, "@@ "):
			h, ok := parseHunkHeader(line)
			if !ok {
				continue
			}
			// 按行数读块体
			oldLeft, newLeft := h.oldLines, h.newLines
			for oldLeft > 0 || newLeft > 0 {
				l, ok, err := readLine()
				if err != nil {
					return nil, err
				}
				if !ok {
					break
				}
				if l == "" {
					// 有的工具会把空的上下文行输出成真正的空行
					l = " "
				}
				switch l[0] {
				case ' ':
					oldLeft--
					newLeft--
				case '-':
					oldLeft--
				case '+':
					newLeft--
				case '\\':
				default:
					// 不该出现：当成块结束交给外层
					pending, havePending = l, true
					oldLeft, newLeft = 0, 0
					continue
				}
				h.lines = append(h.lines, l)
			}
			// 紧跟的 "\ No newline at end of file"
			if !havePending {
				if b, err := br.Peek(1); err == nil && len(b) == 1 && b[0] == '\\' {
					l, _, _ := readLine()
					h.lines = append(h.lines, l)
				}
			}
			cur.hunks = append(cur.hunks, h)
		default:
			if len(cur.hunks) > 0 {
				// 块之后的杂项行（理论上没有）
				continue
			}
			cur.header = append(cur.header, line)
			parseExtendedHeader(cur, line)
		}
	}
	return files, nil
}

func parseExtendedHeader(f *fileDiff, line string) {
	switch {
	case strings.HasPrefix(line, "new file mode "):
		f.status = "A"
		f.newMode = strings.TrimPrefix(line, "new file mode ")
	case strings.HasPrefix(line, "deleted file mode "):
		f.status = "D"
		f.oldMode = strings.TrimPrefix(line, "deleted file mode ")
	case strings.HasPrefix(line, "old mode "):
		f.oldMode = strings.TrimPrefix(line, "old mode ")
		if f.status == "M" {
			f.status = "T"
		}
	case strings.HasPrefix(line, "new mode "):
		f.newMode = strings.TrimPrefix(line, "new mode ")
	case strings.HasPrefix(line, "rename from "):
		f.status = "R"
		f.oldPath = unquoteCPath(strings.TrimPrefix(line, "rename from "))
	case strings.HasPrefix(line, "rename to "):
		f.newPath = unquoteCPath(strings.TrimPrefix(line, "rename to "))
	case strings.HasPrefix(line, "copy from "):
		f.status = "C"
		f.oldPath = unquoteCPath(strings.TrimPrefix(line, "copy from "))
	case strings.HasPrefix(line, "copy to "):
		f.newPath = unquoteCPath(strings.TrimPrefix(line, "copy to "))
	case strings.HasPrefix(line, "similarity index "):
		f.similarity, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(line, "similarity index "), "%"))
	case strings.HasPrefix(line, "index "):
		// index abc..def 100644
		if fs := strings.Fields(line); len(fs) == 3 && f.oldMode == "" && f.newMode == "" {
			f.oldMode, f.newMode = fs[2], fs[2]
		}
	case strings.HasPrefix(line, "--- "):
		if p := diffSidePath(strings.TrimPrefix(line, "--- ")); p != "" {
			f.oldPath = p
		}
	case strings.HasPrefix(line, "+++ "):
		if p := diffSidePath(strings.TrimPrefix(line, "+++ ")); p != "" {
			f.newPath = p
		}
	case strings.HasPrefix(line, "Binary files ") || line == "GIT binary patch":
		f.binary = true
	}
}

// diffSidePath 解析 ---/+++ 行的路径（去掉 a/ b/ 前缀与 C 风格引号）；/dev/null 返回空。
func diffSidePath(s string) string {
	s = strings.TrimSuffix(s, "\t")
	if s == "/dev/null" {
		return ""
	}
	s = unquoteCPath(s)
	if strings.HasPrefix(s, "a/") || strings.HasPrefix(s, "b/") {
		return s[2:]
	}
	return s
}

// parseDiffGitLine 尽力从 "diff --git a/x b/y" 取路径（有空格时有歧义，后面的 ---/+++、rename 行会覆盖）。
func parseDiffGitLine(line string) (string, string) {
	rest := strings.TrimPrefix(line, "diff --git ")
	if strings.HasPrefix(rest, "\"") {
		a, n := readCQuoted(rest)
		b := strings.TrimSpace(rest[n:])
		return trimAB(a), trimAB(unquoteCPath(b))
	}
	// 两边路径相同的常见情况：a/<p> b/<p>
	if strings.HasPrefix(rest, "a/") {
		n := len(rest)
		if (n-5)%2 == 0 {
			half := (n - 1) / 2
			a, b := rest[:half], rest[half+1:]
			if strings.HasPrefix(b, "b/") && a[2:] == b[2:] {
				return a[2:], b[2:]
			}
		}
		if i := strings.Index(rest, " b/"); i > 0 {
			return rest[2:i], rest[i+3:]
		}
	}
	return "", ""
}

func trimAB(s string) string {
	if strings.HasPrefix(s, "a/") || strings.HasPrefix(s, "b/") {
		return s[2:]
	}
	return s
}

// unquoteCPath 解 git 的 C 风格引号路径（core.quotepath=false 时只有控制字符、引号、反斜杠会被转义）。
func unquoteCPath(s string) string {
	if !strings.HasPrefix(s, "\"") {
		return s
	}
	v, _ := readCQuoted(s)
	return v
}

func readCQuoted(s string) (string, int) {
	if !strings.HasPrefix(s, "\"") {
		return s, len(s)
	}
	var b []byte
	i := 1
	for i < len(s) {
		c := s[i]
		if c == '"' {
			return string(b), i + 1
		}
		if c == '\\' && i+1 < len(s) {
			i++
			switch e := s[i]; e {
			case 'n':
				b = append(b, '\n')
			case 't':
				b = append(b, '\t')
			case 'r':
				b = append(b, '\r')
			case 'a':
				b = append(b, '\a')
			case 'b':
				b = append(b, '\b')
			case 'f':
				b = append(b, '\f')
			case 'v':
				b = append(b, '\v')
			case '0', '1', '2', '3':
				if i+2 < len(s) {
					if n, err := strconv.ParseUint(s[i:i+3], 8, 8); err == nil {
						b = append(b, byte(n))
						i += 2
						break
					}
				}
				b = append(b, e)
			default:
				b = append(b, e)
			}
			i++
			continue
		}
		b = append(b, c)
		i++
	}
	return string(b), len(s)
}

// parseHunkHeader 解析 "@@ -a,b +c,d @@ section"。
func parseHunkHeader(line string) (rawHunk, bool) {
	h := rawHunk{header: line}
	end := strings.Index(line[3:], " @@")
	if end < 0 {
		return h, false
	}
	ranges := strings.Fields(line[3 : 3+end])
	if len(ranges) != 2 || !strings.HasPrefix(ranges[0], "-") || !strings.HasPrefix(ranges[1], "+") {
		return h, false
	}
	var ok1, ok2 bool
	h.oldStart, h.oldLines, ok1 = parseRange(ranges[0][1:])
	h.newStart, h.newLines, ok2 = parseRange(ranges[1][1:])
	h.section = strings.TrimPrefix(line[3+end+3:], " ")
	return h, ok1 && ok2
}

func parseRange(s string) (int, int, bool) {
	a, b, has := strings.Cut(s, ",")
	start, err := strconv.Atoi(a)
	if err != nil {
		return 0, 0, false
	}
	n := 1
	if has {
		if n, err = strconv.Atoi(b); err != nil {
			return 0, 0, false
		}
	}
	return start, n, true
}

// toResult 把原始块转成带行号的展示结构。
func (f *fileDiff) toResult() *diffResult {
	d := &diffResult{Path: f.newPath, OldPath: f.oldPath, Binary: f.binary, Status: f.status,
		OldMode: f.oldMode, NewMode: f.newMode, Similarity: f.similarity, Hunks: []diffHunk{}}
	if d.Path == "" {
		d.Path = f.oldPath
	}
	if d.OldPath == d.Path {
		d.OldPath = ""
	}
	for _, h := range f.hunks {
		dh := diffHunk{Header: h.header, OldStart: h.oldStart, OldLines: h.oldLines, NewStart: h.newStart, NewLines: h.newLines, Lines: []diffLine{}}
		o, n := h.oldStart, h.newStart
		for _, l := range h.lines {
			if l == "" {
				continue
			}
			text := strings.TrimSuffix(l[1:], "\r")
			switch l[0] {
			case ' ':
				dh.Lines = append(dh.Lines, diffLine{Kind: " ", Old: o, New: n, Text: text})
				o++
				n++
			case '-':
				dh.Lines = append(dh.Lines, diffLine{Kind: "-", Old: o, Text: text})
				o++
				d.Deletions++
			case '+':
				dh.Lines = append(dh.Lines, diffLine{Kind: "+", New: n, Text: text})
				n++
				d.Additions++
			case '\\':
				if len(dh.Lines) > 0 {
					dh.Lines[len(dh.Lines)-1].NoEOL = true
				}
			}
		}
		d.Hunks = append(d.Hunks, dh)
	}
	return d
}

func (f *fileDiff) lineCount() int {
	n := 0
	for _, h := range f.hunks {
		n += len(h.lines)
	}
	return n
}

// ---------------------------------------------------------------------------
// diff 操作

func (r *repo) diffOp(raw json.RawMessage) (*diffResult, error) {
	a, err := decodeArgs[diffArgs](raw)
	if err != nil {
		return nil, err
	}
	if a.Path == "" {
		return nil, invalidParams("path is required")
	}
	ctxLines := a.Context
	if ctxLines == 0 && !strings.Contains(string(raw), `"context"`) {
		ctxLines = 3
	}
	d, err := r.diffFile(a, ctxLines, !a.NoLimit)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// diffFile 按模式取一个文件的差异。limited 时超过 1 MB / 5000 行标记 TooLarge。
func (r *repo) diffFile(a *diffArgs, ctxLines int, limited bool) (*diffResult, error) {
	fd, mode, untracked, tooLarge, err := r.rawFileDiff(a, ctxLines, limited)
	if err != nil {
		return nil, err
	}
	p, _ := r.relPath(a.Path)
	op := ""
	if a.OldPath != "" {
		op, _ = r.relPath(a.OldPath)
	}
	var d *diffResult
	if fd == nil {
		d = &diffResult{Path: p, OldPath: op, Hunks: []diffHunk{}}
	} else {
		d = fd.toResult()
		if limited && fd.lineCount() > diffMaxLines {
			tooLarge = true
		}
	}
	if tooLarge {
		d.TooLarge = true
		d.Hunks = []diffHunk{}
	}
	if d.Path == "" {
		d.Path = p
	}
	d.Mode = mode
	d.Context = ctxLines
	d.Untracked = untracked
	d.Language = guessLanguage(d.Path)
	return d, nil
}

// rawFileDiff 运行 git diff 并挑出目标文件的那一段。
func (r *repo) rawFileDiff(a *diffArgs, ctxLines int, limited bool) (fd *fileDiff, mode string, untracked, tooLarge bool, err error) {
	p, err := r.relPath(a.Path)
	if err != nil {
		return nil, "", false, false, err
	}
	var oldp string
	if a.OldPath != "" {
		if oldp, err = r.relPath(a.OldPath); err != nil {
			return nil, "", false, false, err
		}
	}
	mode = a.Mode
	if mode == "" {
		switch {
		case a.Staged:
			mode = diffModeStaged
		case a.Commit != "":
			mode = diffModeCommit
		case a.Base != "":
			mode = diffModeRange
		default:
			mode = diffModeHead
		}
	}
	unified := ctxLines
	if unified < 0 {
		unified = wholeFileUnifed
	}
	base := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "-M", "--unified=" + strconv.Itoa(unified)}
	if a.IgnoreWhitespace {
		base = append(base, "-w")
	}
	pathspec := []string{"--", ":(literal)" + p}
	if oldp != "" && oldp != p {
		pathspec = append(pathspec, ":(literal)"+oldp)
	}
	var args []string
	checkUntracked := false
	switch mode {
	case diffModeHead:
		// 工作区对 HEAD；还没有提交时对空树
		head := "HEAD"
		if r.headHash() == "" {
			head = r.emptyTree()
		}
		args = append(base, head)
		checkUntracked = true
	case diffModeUnstaged:
		args = base
		checkUntracked = true
	case diffModeStaged:
		args = append(base, "--cached")
	case diffModeCommit:
		c := a.Commit
		if err := r.checkRef(c); err != nil {
			return nil, "", false, false, err
		}
		parent := a.Parent
		if parent <= 0 {
			parent = 1
		}
		pc := r.revParse(c + "^" + strconv.Itoa(parent))
		if pc == "" {
			pc = r.emptyTree()
		}
		args = append(base, pc, c)
	case diffModeRange:
		if err := r.checkRef(a.Base); err != nil {
			return nil, "", false, false, err
		}
		switch {
		case a.Head == "":
			args = append(base, a.Base)
		case a.MergeBase:
			if err := r.checkRef(a.Head); err != nil {
				return nil, "", false, false, err
			}
			args = append(base, a.Base+"..."+a.Head)
		default:
			if err := r.checkRef(a.Head); err != nil {
				return nil, "", false, false, err
			}
			args = append(base, a.Base, a.Head)
		}
	default:
		return nil, "", false, false, invalidParams("unknown diff mode %q", mode)
	}
	args = append(args, pathspec...)
	files, tl, err := r.runDiff(args, limited)
	if err != nil {
		return nil, mode, false, false, err
	}
	if tl {
		return nil, mode, false, true, nil
	}
	fd = pickFileDiff(files, p, oldp)
	if fd == nil && checkUntracked && r.isUntracked(p) {
		// 未跟踪文件：和 /dev/null 比
		nargs := []string{"diff", "--no-index", "--no-ext-diff", "--no-textconv", "--no-color", "--unified=" + strconv.Itoa(unified), "--", "/dev/null", p}
		files, tl, err = r.runDiff(nargs, limited)
		if err != nil {
			return nil, mode, true, false, err
		}
		if tl {
			return nil, mode, true, true, nil
		}
		fd = pickFileDiff(files, p, "")
		if fd != nil {
			fd.status = "A"
			fd.oldPath = ""
		}
		return fd, mode, true, false, nil
	}
	return fd, mode, false, false, nil
}

// runDiff 执行 diff 并解析；limited 时输出超过 1 MB 立即停止。
func (r *repo) runDiff(args []string, limited bool) ([]*fileDiff, bool, error) {
	max := diffHardMax
	if limited {
		max = diffMaxBytes
	}
	var files []*fileDiff
	var perr error
	tooLarge := false
	res, err := r.stream(cmdOpts{read: true}, args, func(rd io.Reader) error {
		lr := &limitReader{r: rd, n: int64(max)}
		files, perr = parseUnifiedDiff(lr)
		if lr.hit {
			tooLarge = true
			return errStop
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if tooLarge {
		return nil, true, nil
	}
	// --no-index 有差异时退出码是 1
	if res.code != 0 && res.code != 1 {
		return nil, false, r.gitFailure(res)
	}
	if res.code == 1 && !strings.Contains(strings.Join(args, " "), "--no-index") {
		return nil, false, r.gitFailure(res)
	}
	return files, false, perr
}

type limitReader struct {
	r   io.Reader
	n   int64
	hit bool
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		l.hit = true
		return 0, io.EOF
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

func pickFileDiff(files []*fileDiff, p, oldp string) *fileDiff {
	if len(files) == 0 {
		return nil
	}
	for _, f := range files {
		if f.newPath == p {
			return f
		}
	}
	for _, f := range files {
		if f.oldPath == p || (oldp != "" && f.oldPath == oldp) {
			return f
		}
	}
	if len(files) == 1 {
		return files[0]
	}
	return nil
}

// isUntracked 判断路径不在暂存区（也就不在 HEAD 的跟踪范围里）且工作区存在。
func (r *repo) isUntracked(p string) bool {
	res, err := r.run(cmdOpts{read: true}, "ls-files", "-z", "--", ":(literal)"+p)
	if err != nil || !res.ok() || res.out != "" {
		return false
	}
	return exists(r.abs(p))
}

func (r *repo) abs(rel string) string {
	return path.Join(strings.ReplaceAll(r.root, "\\", "/"), rel)
}

// ---------------------------------------------------------------------------
// 按块暂存 / 取消暂存 / 丢弃

type hunkKind int

const (
	hunkStage hunkKind = iota
	hunkUnstage
	hunkDiscard
)

type hunksArgs struct {
	sdk.VCSHunksArgs
	OldPath string   `json:"oldPath,omitempty"`
	Mode    string   `json:"mode,omitempty"`    // 取 diff 时用的模式：stage 默认 unstaged，unstage 默认 staged，discard 默认 unstaged
	Headers []string `json:"headers,omitempty"` // 可选：所选块的 Header，用来确认界面拿的 diff 没过期
	Staged  bool     `json:"staged,omitempty"`  // 等同 mode=staged
}

func (r *repo) applyHunks(raw json.RawMessage, kind hunkKind) (*opResult, error) {
	a, err := decodeArgs[hunksArgs](raw)
	if err != nil {
		return nil, err
	}
	if a.Path == "" || len(a.Hunks) == 0 {
		return nil, invalidParams("path and hunks are required")
	}
	ctxLines := a.Context
	if ctxLines == 0 && !strings.Contains(string(raw), `"context"`) {
		ctxLines = 3
	}
	if ctxLines < 0 {
		ctxLines = wholeFileUnifed
	}
	mode := a.Mode
	auto := mode == "" && !a.Staged
	if mode == "" {
		switch {
		case kind == hunkUnstage || a.Staged:
			mode = diffModeStaged
		default:
			mode = diffModeUnstaged
		}
	}
	switch {
	case kind == hunkStage && mode != diffModeUnstaged && mode != diffModeHead,
		kind == hunkUnstage && mode != diffModeStaged,
		kind == hunkDiscard && mode != diffModeUnstaged && mode != diffModeHead:
		return nil, invalidParams("mode %q is not valid for this operation", mode)
	}
	da := &diffArgs{VCSDiffArgs: sdk.VCSDiffArgs{Path: a.Path, OldPath: a.OldPath}, Mode: mode}
	fd, _, _, _, err := r.rawFileDiff(da, ctxLines, false)
	if err != nil {
		return nil, err
	}
	if fd == nil || len(fd.hunks) == 0 {
		return nil, userError(r.ctx, "staleDiff", "error.staleDiff")
	}
	sel, err := selectHunks(fd, a.Hunks, a.Headers)
	if err != nil && auto && kind != hunkUnstage && len(a.Headers) > 0 {
		// 没指定模式：界面可能拿的是默认的 HEAD→工作区 diff（文件有已暂存内容时两者不同），按块头再对一次
		da.Mode = diffModeHead
		if fd2, _, _, _, err2 := r.rawFileDiff(da, ctxLines, false); err2 == nil && fd2 != nil {
			if sel2, err3 := selectHunks(fd2, a.Hunks, a.Headers); err3 == nil {
				fd, sel, err = fd2, sel2, nil
			}
		}
	}
	if err != nil {
		return nil, userError(r.ctx, "staleDiff", "error.staleDiff")
	}
	reverse := kind != hunkStage
	patch := buildPatch(fd, sel, reverse)
	args := []string{"apply", "--recount", "--whitespace=nowarn"}
	switch kind {
	case hunkStage:
		args = append(args, "--cached")
	case hunkUnstage:
		args = append(args, "--cached", "-R")
	case hunkDiscard:
		args = append(args, "-R")
	}
	if ctxLines == 0 {
		args = append(args, "--unidiff-zero")
	}
	args = append(args, "-")
	res, err := r.run(cmdOpts{stdin: patch}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		out := r.failResult(res)
		out.Reason = "applyFailed"
		out.Message = tr(r.ctx, "result.applyFailed", firstErrorLine(res.combined()))
		return out, nil
	}
	return okResult(tr(r.ctx, map[hunkKind]string{hunkStage: "result.hunksStaged", hunkUnstage: "result.hunksUnstaged", hunkDiscard: "result.hunksDiscarded"}[kind], len(sel))), nil
}

func selectHunks(fd *fileDiff, idx []int, headers []string) ([]int, error) {
	seen := map[int]bool{}
	var sel []int
	for i, k := range idx {
		if k < 0 || k >= len(fd.hunks) {
			return nil, fmt.Errorf("hunk %d out of range", k)
		}
		if i < len(headers) && headers[i] != "" && headers[i] != fd.hunks[k].header {
			return nil, fmt.Errorf("hunk %d header mismatch", k)
		}
		if !seen[k] {
			seen[k] = true
			sel = append(sel, k)
		}
	}
	sort.Ints(sel)
	return sel, nil
}

// buildPatch 用选中的块生成补丁。正向应用时旧侧位置是准的，按已选块的累计行差重算新侧起点；
// 反向应用（-R）时新侧位置是准的，重算旧侧起点。这样补丁自洽，-U0 也能精确应用。
func buildPatch(fd *fileDiff, sel []int, reverse bool) string {
	var b strings.Builder
	for _, h := range fd.header {
		b.WriteString(h)
		b.WriteByte('\n')
	}
	delta := 0
	for _, i := range sel {
		h := fd.hunks[i]
		os, ol, ns, nl := h.oldStart, h.oldLines, h.newStart, h.newLines
		if !reverse {
			anchor := os
			if ol > 0 {
				anchor = os - 1
			}
			na := anchor + delta
			ns = na
			if nl > 0 {
				ns = na + 1
			}
		} else {
			anchor := ns
			if nl > 0 {
				anchor = ns - 1
			}
			oa := anchor - delta
			os = oa
			if ol > 0 {
				os = oa + 1
			}
		}
		delta += nl - ol
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@", os, ol, ns, nl)
		if h.section != "" {
			b.WriteString(" ")
			b.WriteString(h.section)
		}
		b.WriteByte('\n')
		for _, l := range h.lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// 暂存 / 取消暂存整个文件

func (r *repo) stage(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSPathsArgs](raw)
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
	// -A：同时暂存删除；冲突文件 add 即标记已解决
	if _, err := r.do(cmdOpts{}, r.pathspecArgs([]string{"add", "-A"}, paths)...); err != nil {
		return nil, err
	}
	return okResult(tr(r.ctx, "result.staged", len(paths))), nil
}

func (r *repo) unstage(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSPathsArgs](raw)
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
	if r.headHash() == "" {
		// 没有提交：取消暂存就是从暂存区移除
		if _, err := r.do(cmdOpts{}, r.pathspecArgs([]string{"rm", "--cached", "-r", "-q", "--ignore-unmatch"}, paths)...); err != nil {
			return nil, err
		}
	} else if _, err := r.do(cmdOpts{}, r.pathspecArgs([]string{"reset", "-q", "HEAD"}, paths)...); err != nil {
		return nil, err
	}
	return okResult(tr(r.ctx, "result.unstaged", len(paths))), nil
}

// pathspecArgs 拼「命令 -- 路径…」，每个路径加 :(literal) 前缀，文件名里的 * ? [ 不会被当成通配。
// 不用全局 --literal-pathspecs：它会通过环境变量传给钩子里的 git（lint-staged、git-lfs 等），改变它们的行为。
func (r *repo) pathspecArgs(cmd []string, paths []string) []string {
	out := append([]string{}, cmd...)
	out = append(out, "--")
	return append(out, lit(paths)...)
}

// lit 给路径加 :(literal) 魔法前缀。
func lit(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = ":(literal)" + p
	}
	return out
}

// guessLanguage 按扩展名猜语言名（界面可选用于等宽着色）。
func guessLanguage(p string) string {
	base := strings.ToLower(path.Base(p))
	switch base {
	case "dockerfile":
		return "dockerfile"
	case "makefile", "gnumakefile":
		return "makefile"
	case "cmakelists.txt":
		return "cmake"
	case "go.mod", "go.sum":
		return "go-mod"
	case ".gitignore", ".gitattributes", ".gitmodules":
		return "git"
	}
	ext := strings.TrimPrefix(path.Ext(base), ".")
	if l, ok := langByExt[ext]; ok {
		return l
	}
	return ""
}

var langByExt = map[string]string{
	"go": "go", "java": "java", "kt": "kotlin", "kts": "kotlin", "groovy": "groovy", "gradle": "groovy", "scala": "scala",
	"js": "javascript", "mjs": "javascript", "cjs": "javascript", "jsx": "javascript", "ts": "typescript", "mts": "typescript", "cts": "typescript", "tsx": "typescript",
	"vue": "vue", "svelte": "svelte", "html": "html", "htm": "html", "css": "css", "scss": "scss", "sass": "sass", "less": "less",
	"json": "json", "jsonc": "json", "json5": "json", "yaml": "yaml", "yml": "yaml", "toml": "toml", "xml": "xml", "plist": "xml", "svg": "xml",
	"md": "markdown", "markdown": "markdown", "py": "python", "pyi": "python", "rb": "ruby", "php": "php", "rs": "rust",
	"c": "c", "h": "c", "cc": "cpp", "cpp": "cpp", "cxx": "cpp", "hpp": "cpp", "hh": "cpp", "m": "objective-c", "mm": "objective-cpp",
	"swift": "swift", "cs": "csharp", "fs": "fsharp", "vb": "vb", "sql": "sql", "sh": "shell", "bash": "shell", "zsh": "shell",
	"ps1": "powershell", "bat": "batch", "cmd": "batch", "lua": "lua", "dart": "dart", "r": "r", "pl": "perl",
	"properties": "properties", "ini": "ini", "cfg": "ini", "conf": "ini", "proto": "protobuf", "graphql": "graphql", "gql": "graphql",
	"wxml": "xml", "wxss": "css", "nvue": "vue", "uvue": "vue", "glsl": "glsl", "hlsl": "hlsl", "shader": "hlsl", "txt": "text",
}
