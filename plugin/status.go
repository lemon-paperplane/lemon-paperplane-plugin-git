package git

import (
	"bufio"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
)

// fileChange 是 sdk.VCSFileChange 的 JSON 超集：额外给出暂存区/工作区各自的状态字母，
// 暂存区模式的界面要分开显示「已暂存」「未暂存」两栏。
type fileChange struct {
	Path       string `json:"path"`
	OldPath    string `json:"oldPath,omitempty"`
	Status     string `json:"status"`
	Staged     bool   `json:"staged,omitempty"`
	Unstaged   bool   `json:"unstaged,omitempty"`
	Conflicted bool   `json:"conflicted,omitempty"`
	Additions  int    `json:"additions,omitempty"`
	Deletions  int    `json:"deletions,omitempty"`
	Binary     bool   `json:"binary,omitempty"`

	IndexStatus    string `json:"indexStatus,omitempty"`    // 暂存区相对 HEAD：M A D R C T，空表示没变
	WorktreeStatus string `json:"worktreeStatus,omitempty"` // 工作区相对暂存区：M D T ?，空表示没变
	ConflictType   string `json:"conflictType,omitempty"`   // UU 双方修改 | AA 双方新增 | DU 我方删除 | UD 对方删除 | AU 我方新增 | UA 对方新增 | DD 双方删除
	Submodule      bool   `json:"submodule,omitempty"`
}

// statusResult 是 sdk.VCSStatus 的 JSON 超集。
type statusResult struct {
	Branch     string       `json:"branch"`
	Detached   bool         `json:"detached,omitempty"`
	Head       string       `json:"head,omitempty"`
	Upstream   string       `json:"upstream,omitempty"`
	Ahead      int          `json:"ahead"`
	Behind     int          `json:"behind"`
	Operation  string       `json:"operation,omitempty"`
	Files      []fileChange `json:"files"`
	Conflicts  int          `json:"conflicts"`
	StashCount int          `json:"stashCount"`

	HeadHash           string   `json:"headHash,omitempty"`           // 完整哈希
	Unborn             bool     `json:"unborn,omitempty"`             // 还没有任何提交
	UpstreamGone       bool     `json:"upstreamGone,omitempty"`       // 上游分支已在远端删除
	OperationDetail    *opState `json:"operationDetail,omitempty"`    // rebase 进度、目标等
	UntrackedCount     int      `json:"untrackedCount,omitempty"`     // 未跟踪文件数（截断时为已数到的数量）
	UntrackedTruncated bool     `json:"untrackedTruncated,omitempty"` // 未跟踪文件太多，列表只含前 untrackedLimit 个
}

type statusArgs struct {
	Numstat        *bool    `json:"numstat,omitempty"`        // 是否统计增删行数，默认 true
	UntrackedLimit int      `json:"untrackedLimit,omitempty"` // 列表里最多放多少个未跟踪文件
	Untracked      string   `json:"untracked,omitempty"`      // all（默认）| normal（未跟踪目录合并成一项）| no
	Paths          []string `json:"paths,omitempty"`          // 只看这些路径
}

const (
	defaultUntrackedLimit = 5000
	untrackedHardCap      = 100000 // 数到这么多就停，避免卡死
)

func (r *repo) status(raw json.RawMessage) (*statusResult, error) {
	a, err := decodeArgs[statusArgs](raw)
	if err != nil {
		return nil, err
	}
	paths, err := r.checkPaths(a.Paths)
	if err != nil {
		return nil, err
	}
	limit := a.UntrackedLimit
	if limit <= 0 {
		limit = settingInt(r.ctx, "untrackedLimit", defaultUntrackedLimit)
	}
	mode := a.Untracked
	if mode != "normal" && mode != "no" {
		mode = "all"
	}
	wantNumstat := a.Numstat == nil || *a.Numstat

	var (
		wg       sync.WaitGroup
		stats    map[string]numstat
		statsErr error
	)
	if wantNumstat {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats, statsErr = r.numstatHead(paths)
		}()
	}

	args := []string{"status", "--porcelain=v2", "-z", "--branch", "--untracked-files=" + mode}
	if len(paths) > 0 {
		args = r.pathspecArgs(args, paths)
	}
	st := &statusResult{Files: []fileChange{}}
	res, err := r.stream(cmdOpts{read: true}, args, func(rd io.Reader) error {
		return parseStatusV2(rd, st, limit, untrackedHardCap)
	})
	if err != nil {
		wg.Wait()
		return nil, err
	}
	if !res.ok() {
		wg.Wait()
		return nil, r.gitFailure(res)
	}
	wg.Wait()
	if wantNumstat && statsErr == nil {
		if stats == nil && st.Unborn {
			stats, _ = r.numstatCached(paths)
		}
		for i := range st.Files {
			f := &st.Files[i]
			if s, ok := stats[f.Path]; ok {
				f.Additions, f.Deletions, f.Binary = s.add, s.del, s.binary
			}
		}
	}
	st.StashCount = r.stashCount()
	if op := r.operation(); op != nil {
		st.Operation = op.Op
		st.OperationDetail = op
	}
	return st, nil
}

// parseStatusV2 解析 git status --porcelain=v2 -z --branch 的输出。
// 未跟踪文件超过 limit 后只计数不入列表，超过 hardCap 返回 errStop 让 git 结束。
func parseStatusV2(rd io.Reader, st *statusResult, limit, hardCap int) error {
	br := bufio.NewReaderSize(rd, 256*1024)
	next := func() (string, error) {
		s, err := br.ReadString(0)
		if err != nil {
			if err == io.EOF && s != "" {
				return s, nil
			}
			return "", err
		}
		return s[:len(s)-1], nil
	}
	sawAB := false
	for {
		tok, err := next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if tok == "" {
			continue
		}
		switch tok[0] {
		case '#':
			parseBranchHeader(tok, st, &sawAB)
		case '1':
			// 1 XY sub mH mI mW hH hI path
			f := strings.SplitN(tok, " ", 9)
			if len(f) < 9 {
				continue
			}
			st.Files = append(st.Files, makeChange(f[1], f[2], f[8], ""))
		case '2':
			// 2 XY sub mH mI mW hH hI Xscore path \0 origPath
			f := strings.SplitN(tok, " ", 10)
			orig, err := next()
			if err != nil && err != io.EOF {
				return err
			}
			if len(f) < 10 {
				continue
			}
			st.Files = append(st.Files, makeChange(f[1], f[2], f[9], orig))
		case 'u':
			// u XY sub m1 m2 m3 mW h1 h2 h3 path
			f := strings.SplitN(tok, " ", 11)
			if len(f) < 11 {
				continue
			}
			st.Files = append(st.Files, fileChange{
				Path: f[10], Status: "U", Conflicted: true, Staged: true, Unstaged: true,
				ConflictType: f[1], Submodule: strings.HasPrefix(f[2], "S"),
			})
			st.Conflicts++
		case '?':
			st.UntrackedCount++
			if st.UntrackedCount <= limit {
				st.Files = append(st.Files, fileChange{Path: tok[2:], Status: "?", Unstaged: true, WorktreeStatus: "?"})
			} else {
				st.UntrackedTruncated = true
			}
			if hardCap > 0 && st.UntrackedCount >= hardCap {
				return errStop
			}
		}
	}
	if st.Upstream != "" && !sawAB {
		st.UpstreamGone = true
	}
	return nil
}

func parseBranchHeader(tok string, st *statusResult, sawAB *bool) {
	switch {
	case strings.HasPrefix(tok, "# branch.oid "):
		oid := strings.TrimPrefix(tok, "# branch.oid ")
		if oid == "(initial)" {
			st.Unborn = true
		} else {
			st.HeadHash = oid
			st.Head = shortHash(oid)
		}
	case strings.HasPrefix(tok, "# branch.head "):
		h := strings.TrimPrefix(tok, "# branch.head ")
		if h == "(detached)" {
			st.Detached = true
		} else {
			st.Branch = h
		}
	case strings.HasPrefix(tok, "# branch.upstream "):
		st.Upstream = strings.TrimPrefix(tok, "# branch.upstream ")
	case strings.HasPrefix(tok, "# branch.ab "):
		*sawAB = true
		for _, f := range strings.Fields(strings.TrimPrefix(tok, "# branch.ab ")) {
			n, _ := strconv.Atoi(f[1:])
			if strings.HasPrefix(f, "+") {
				st.Ahead = n
			} else if strings.HasPrefix(f, "-") {
				st.Behind = n
			}
		}
	}
}

// makeChange 由 XY 两个状态字母算出一个合并状态（相对 HEAD 的整体变化，IDEA 默认模式显示这个）。
func makeChange(xy, sub, path, orig string) fileChange {
	x, y := xy[0], xy[1]
	fc := fileChange{Path: path, OldPath: orig, Staged: x != '.', Unstaged: y != '.', Submodule: strings.HasPrefix(sub, "S")}
	if x != '.' {
		fc.IndexStatus = string(x)
	}
	if y != '.' {
		fc.WorktreeStatus = string(y)
	}
	switch {
	case y == 'D':
		fc.Status = "D"
	case x == 'A' || x == 'R' || x == 'C' || x == 'D':
		fc.Status = string(x)
	case y != '.':
		fc.Status = string(y)
	default:
		fc.Status = string(x)
	}
	return fc
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

type numstat struct {
	add, del int
	binary   bool
}

// numstatHead 统计 HEAD 到工作区的增删行数（IDEA 默认模式看到的就是这个差异）。
func (r *repo) numstatHead(paths []string) (map[string]numstat, error) {
	args := []string{"diff", "HEAD", "--numstat", "-z", "-M", "--no-ext-diff", "--no-textconv", "--no-color"}
	if len(paths) > 0 {
		args = r.pathspecArgs(args, paths)
	}
	res, err := r.run(cmdOpts{read: true, maxOut: 16 << 20}, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		// 没有提交时 HEAD 不存在，由调用方改用 --cached
		return nil, nil
	}
	return parseNumstatZ(res.out), nil
}

func (r *repo) numstatCached(paths []string) (map[string]numstat, error) {
	args := []string{"diff", "--cached", "--numstat", "-z", "-M", "--no-ext-diff", "--no-textconv", "--no-color"}
	if len(paths) > 0 {
		args = r.pathspecArgs(args, paths)
	}
	res, err := r.run(cmdOpts{read: true, maxOut: 16 << 20}, args...)
	if err != nil || !res.ok() {
		return nil, err
	}
	return parseNumstatZ(res.out), nil
}

// parseNumstatZ 解析 --numstat -z：普通项 "add\tdel\tpath\0"，重命名 "add\tdel\t\0old\0new\0"；二进制的增删是 "-"。
func parseNumstatZ(out string) map[string]numstat {
	m := map[string]numstat{}
	toks := strings.Split(out, "\x00")
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t == "" {
			continue
		}
		parts := strings.SplitN(t, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		ns := numstat{}
		if parts[0] == "-" && parts[1] == "-" {
			ns.binary = true
		} else {
			ns.add, _ = strconv.Atoi(parts[0])
			ns.del, _ = strconv.Atoi(parts[1])
		}
		path := parts[2]
		if path == "" && i+2 < len(toks) {
			// 重命名：后面两个是旧路径、新路径
			path = toks[i+2]
			i += 2
		}
		m[path] = ns
	}
	return m
}
