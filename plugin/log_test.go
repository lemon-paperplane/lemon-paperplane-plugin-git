package git

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

func (e *tenv) logOf(root string, args map[string]any) logResult {
	e.t.Helper()
	var l logResult
	e.must(root, sdk.VCSOpLog, args, &l)
	return l
}

func edgesOf(c commitInfo) map[[2]int]bool {
	m := map[[2]int]bool{}
	for _, e := range c.Graph.Edges {
		m[[2]int{e.From, e.To}] = true
	}
	return m
}

func TestGraphSimpleMerge(t *testing.T) {
	e := newEnv(t)
	e.fakeDates = true
	dir := e.repo() // A
	e.git(dir, "checkout", "-q", "-b", "feature")
	e.write(dir, "c.txt", "c\n")
	e.commitAll(dir, "C")
	e.git(dir, "checkout", "-q", "main")
	e.write(dir, "b.txt", "b\n")
	e.commitAll(dir, "B")
	e.git(dir, "merge", "-q", "--no-ff", "-m", "M", "feature")

	l := e.logOf(dir, map[string]any{})
	if !l.Graph || len(l.Commits) != 4 {
		t.Fatalf("log: graph=%v n=%d", l.Graph, len(l.Commits))
	}
	m := l.Commits[0]
	if m.Subject != "M" || m.Graph.Lane != 0 || m.Graph.Lanes != 1 || !edgesOf(m)[[2]int{0, 0}] || !edgesOf(m)[[2]int{0, 1}] || len(m.Graph.Edges) != 2 {
		t.Fatalf("merge row: %+v", m.Graph)
	}
	// 两条线在 A 之前汇合，A 在第 0 列且没有出边
	a := l.Commits[3]
	if a.Subject != "init" || a.Graph.Lane != 0 || len(a.Graph.Edges) != 0 {
		t.Fatalf("root row: %+v", a.Graph)
	}
	third := l.Commits[2]
	if third.Graph.Lanes != 2 {
		t.Fatalf("row before root should still have 2 lanes: %+v", third.Graph)
	}
	conv := false
	for _, ed := range third.Graph.Edges {
		if ed.From == 1 && ed.To == 0 {
			conv = true
		}
	}
	if !conv {
		t.Fatalf("lane 1 should converge into lane 0 before root: %+v", third.Graph.Edges)
	}
	checkGraph(t, l.Commits)
	// 颜色：第二条线颜色不同
	if l.Commits[0].Graph.Edges[0].Color == l.Commits[0].Graph.Edges[1].Color {
		t.Fatalf("merge edges should have different colors: %+v", m.Graph.Edges)
	}
	// refs 装饰
	if len(m.Refs) != 2 || m.Refs[0].Kind != "head" || m.Refs[1].Name != "main" || m.Refs[1].Kind != "local" {
		t.Fatalf("refs: %+v", m.Refs)
	}
}

func TestGraphComplexHistory(t *testing.T) {
	e := newEnv(t)
	e.fakeDates = true
	dir := e.repo()
	commit := func(msg string) { e.write(dir, msg+".txt", msg); e.commitAll(dir, msg) }
	// 多个分支并行、交叉合并、章鱼合并、远程分支
	e.git(dir, "branch", "b1")
	e.git(dir, "branch", "b2")
	commit("m1")
	e.git(dir, "checkout", "-q", "b1")
	commit("b1-1")
	e.git(dir, "checkout", "-q", "b2")
	commit("b2-1")
	e.git(dir, "checkout", "-q", "main")
	commit("m2")
	e.git(dir, "checkout", "-q", "b1")
	commit("b1-2")
	e.git(dir, "merge", "-q", "--no-ff", "-m", "merge main into b1", "main")
	e.git(dir, "checkout", "-q", "b2")
	e.git(dir, "merge", "-q", "--no-ff", "-m", "merge b1 into b2", "b1")
	commit("b2-2")
	e.git(dir, "checkout", "-q", "main")
	e.git(dir, "merge", "-q", "--no-ff", "-m", "merge b2 into main (cross)", "b2~1")
	e.git(dir, "checkout", "-q", "-b", "o1", "main~1")
	commit("o1")
	e.git(dir, "checkout", "-q", "-b", "o2", "main~1")
	commit("o2")
	e.git(dir, "checkout", "-q", "main")
	e.git(dir, "merge", "-q", "--no-ff", "-m", "octopus", "o1", "o2", "b2")
	e.git(dir, "checkout", "-q", "-b", "dangling", "b1~1")
	commit("dangling-1")
	commit("dangling-2")
	e.git(dir, "checkout", "-q", "main")
	// 远程分支
	bare := filepath.Join(t.TempDir(), "r.git")
	e.git("", "init", "-q", "--bare", bare)
	e.git(dir, "remote", "add", "origin", bare)
	e.git(dir, "push", "-q", "origin", "dangling:remote-only")
	e.git(dir, "fetch", "-q", "origin")
	e.git(dir, "tag", "v1", "b1")

	for _, order := range []string{"topo", "date"} {
		l := e.logOf(dir, map[string]any{"ref": "--all", "order": order})
		if !l.Graph {
			t.Fatalf("no graph")
		}
		checkGraph(t, l.Commits)
		var oct *commitInfo
		for i := range l.Commits {
			if l.Commits[i].Subject == "octopus" {
				oct = &l.Commits[i]
			}
		}
		if oct == nil || len(oct.Parents) != 4 {
			t.Fatalf("octopus missing")
		}
		fromNode := 0
		for _, ed := range oct.Graph.Edges {
			if ed.From == oct.Graph.Lane {
				fromNode++
			}
		}
		if fromNode != 4 {
			t.Fatalf("octopus should have 4 edges from node, got %+v", oct.Graph)
		}
		// 分页：第二页的图与整页计算一致
		page := e.logOf(dir, map[string]any{"ref": "--all", "order": order, "skip": 5, "limit": 4})
		for i, c := range page.Commits {
			full := l.Commits[5+i]
			if c.Hash != full.Hash || fmt.Sprint(c.Graph) != fmt.Sprint(full.Graph) {
				t.Fatalf("page graph differs at %d: %+v vs %+v", i, c.Graph, full.Graph)
			}
		}
		if !page.HasMore {
			t.Fatalf("page should have more")
		}
	}
	// 装饰：远程、标签
	l := e.logOf(dir, map[string]any{"ref": "--all"})
	kinds := map[string]bool{}
	for _, c := range l.Commits {
		for _, r := range c.Refs {
			kinds[r.Kind+":"+r.Name] = true
		}
	}
	for _, want := range []string{"remote:origin/remote-only", "tag:v1", "local:dangling", "head:HEAD", "local:main"} {
		if !kinds[want] {
			t.Errorf("missing decoration %s in %v", want, kinds)
		}
	}
}

// checkGraph 校验提交图的一致性：
// 1. 每行进入的线（上一行 Edges 的 To）只落在本行节点或本行穿过的线上；本行穿过的线必须有来处；
// 2. 有子提交的行节点一定有线进入，分支末端没有；
// 3. 每个父提交（在列表里的）都能沿连线从子提交走到；
// 4. 列号都在范围内。
func checkGraph(t *testing.T, cs []commitInfo) {
	t.Helper()
	row := map[string]int{}
	for i, c := range cs {
		row[c.Hash] = i
		if c.Graph == nil {
			t.Fatalf("row %d has no graph", i)
		}
	}
	hasChild := map[string]bool{}
	for _, c := range cs {
		for _, p := range c.Parents {
			hasChild[p] = true
		}
	}
	for i, c := range cs {
		g := c.Graph
		if g.Lane < 0 || g.Lane >= g.Lanes {
			t.Fatalf("row %d lane %d out of %d", i, g.Lane, g.Lanes)
		}
		in := map[int]bool{}
		if i > 0 {
			for _, ed := range cs[i-1].Graph.Edges {
				in[ed.To] = true
			}
		}
		pass := map[int]bool{}
		for _, ed := range g.Edges {
			if ed.From < 0 || ed.From >= g.Lanes {
				t.Fatalf("row %d edge from %d out of %d", i, ed.From, g.Lanes)
			}
			if i+1 < len(cs) && (ed.To < 0 || ed.To >= cs[i+1].Graph.Lanes) {
				t.Fatalf("row %d edge to %d out of next lanes %d", i, ed.To, cs[i+1].Graph.Lanes)
			}
			if ed.From != g.Lane {
				pass[ed.From] = true
				if !in[ed.From] {
					t.Fatalf("row %d (%s): line at col %d comes from nowhere; in=%v", i, c.Subject, ed.From, in)
				}
			}
		}
		for col := range in {
			if col != g.Lane && !pass[col] {
				t.Fatalf("row %d (%s): incoming line at col %d ends without node; node=%d edges=%+v", i, c.Subject, col, g.Lane, g.Edges)
			}
		}
		if hasChild[c.Hash] != in[g.Lane] && i > 0 {
			t.Fatalf("row %d (%s): hasChild=%v but incoming at node=%v", i, c.Subject, hasChild[c.Hash], in[g.Lane])
		}
	}
	// 可达性
	for i, c := range cs {
		for _, p := range c.Parents {
			j, ok := row[p]
			if !ok {
				continue
			}
			// 从 (i, lane) 出发沿本行节点的边走
			cur := map[int]bool{}
			for _, ed := range c.Graph.Edges {
				if ed.From == c.Graph.Lane {
					cur[ed.To] = true
				}
			}
			for k := i + 1; k < j; k++ {
				next := map[int]bool{}
				for _, ed := range cs[k].Graph.Edges {
					if cur[ed.From] && ed.From != cs[k].Graph.Lane {
						next[ed.To] = true
					}
				}
				cur = next
			}
			if !cur[cs[j].Graph.Lane] {
				t.Fatalf("parent of %q (%s) not reachable via edges", c.Subject, cs[j].Subject)
			}
		}
	}
}

func TestLogFiltersShowHistoryCompareBlame(t *testing.T) {
	e := newEnv(t)
	e.fakeDates = true
	dir := e.repo()
	e.write(dir, "src/a.go", "package a\n")
	e.commitAll(dir, "add a.go")
	e.git(dir, "mv", "src/a.go", "src/b 文件.go")
	e.commitAll(dir, "rename to b")
	e.write(dir, "src/b 文件.go", "package a\n\nfunc F() {}\n")
	e.git(dir, "-c", "user.name=Other", "-c", "user.email=o@x", "commit", "-q", "-am", "修复 bug(1)")
	for i := 0; i < 5; i++ {
		e.write(dir, "n.txt", fmt.Sprint(i))
		e.commitAll(dir, fmt.Sprintf("n%d", i))
	}
	// 分页
	l := e.logOf(dir, map[string]any{"limit": 3})
	if len(l.Commits) != 3 || !l.HasMore || l.Commits[0].Subject != "n4" {
		t.Fatalf("page1: %+v", l)
	}
	l = e.logOf(dir, map[string]any{"limit": 3, "skip": 6})
	if len(l.Commits) != 3 || l.HasMore || l.Commits[2].Subject != "init" {
		t.Fatalf("last page: %d %v", len(l.Commits), l.HasMore)
	}
	// 作者 / 关键词（按字面量，不当正则）/ 路径过滤
	if l := e.logOf(dir, map[string]any{"author": "other"}); len(l.Commits) != 1 || l.Graph {
		t.Fatalf("author filter: %+v", l)
	}
	if l := e.logOf(dir, map[string]any{"grep": "bug(1"}); len(l.Commits) != 1 {
		t.Fatalf("grep literal: %d", len(l.Commits))
	}
	if l := e.logOf(dir, map[string]any{"path": "n.txt"}); len(l.Commits) != 5 || !l.Graph {
		t.Fatalf("path filter: %d", len(l.Commits))
	} else {
		checkGraph(t, l.Commits)
	}
	// 文件历史：跟踪重命名
	var fh logResult
	e.must(dir, sdk.VCSOpFileHistory, map[string]any{"path": "src/b 文件.go"}, &fh)
	if len(fh.Commits) != 3 || fh.Commits[0].Path != "src/b 文件.go" || fh.Commits[2].Path != "src/a.go" || fh.Commits[1].PathStatus != "R" {
		t.Fatalf("file history: %+v", fh.Commits)
	}
	// show
	var d commitDetail
	h := strings.TrimSpace(e.git(dir, "rev-parse", "HEAD~5"))
	e.must(dir, sdk.VCSOpShow, map[string]any{"hash": h}, &d)
	if d.Commit.Subject != "修复 bug(1)" || d.Commit.Author != "Other" || len(d.Files) != 1 || d.Files[0].Additions != 2 || d.Committer == "" {
		t.Fatalf("show: %+v", d)
	}
	e.must(dir, sdk.VCSOpShow, map[string]any{"hash": "HEAD~6"}, &d)
	if len(d.Files) != 1 || d.Files[0].Status != "R" || d.Files[0].OldPath != "src/a.go" {
		t.Fatalf("show rename: %+v", d.Files)
	}
	// compare
	e.git(dir, "checkout", "-q", "-b", "side", "HEAD~2")
	e.write(dir, "side.txt", "s")
	e.commitAll(dir, "side work")
	var cmp compareResult
	e.must(dir, sdk.VCSOpCompare, map[string]any{"base": "main"}, &cmp)
	if len(cmp.Ahead) != 1 || len(cmp.Behind) != 2 || len(cmp.Files) != 1 || cmp.Files[0].Path != "side.txt" || cmp.MergeBase == "" {
		t.Fatalf("compare: ahead=%d behind=%d files=%+v", len(cmp.Ahead), len(cmp.Behind), cmp.Files)
	}
	// blame（含未提交的行）
	e.write(dir, "src/b 文件.go", "package a\n\nfunc F() {}\nfunc G() {}\n")
	var bl blameResult
	e.must(dir, sdk.VCSOpBlame, map[string]any{"path": "src/b 文件.go"}, &bl)
	if len(bl.Lines) != 4 || bl.Lines[0].Summary != "add a.go" || bl.Lines[2].Author != "Other" || !bl.Lines[3].Uncommitted || bl.Lines[3].Text != "func G() {}" {
		t.Fatalf("blame: %+v", bl.Lines)
	}
	if bl.Lines[0].Date == "" || bl.Lines[0].Filename != "src/a.go" {
		t.Fatalf("blame meta: %+v", bl.Lines[0])
	}
	// 包含某提交的分支
	var cb containingResult
	e.must(dir, opContainingBranches, map[string]any{"hash": "HEAD~1"}, &cb)
	if len(cb.Refs) != 2 {
		t.Fatalf("containing: %+v", cb.Refs)
	}
	// 文件内容：历史版本、暂存区、工作区
	var fc fileContentResult
	e.must(dir, opFileContent, map[string]any{"path": "src/b 文件.go", "ref": "HEAD"}, &fc)
	if !fc.Exists || fc.Text != "package a\n\nfunc F() {}\n" {
		t.Fatalf("fileContent: %+v", fc)
	}
	e.must(dir, opFileContent, map[string]any{"path": "src/b 文件.go"}, &fc)
	if !strings.Contains(fc.Text, "func G") {
		t.Fatalf("worktree content: %+v", fc)
	}
	e.must(dir, opFileContent, map[string]any{"path": "missing.txt", "ref": "HEAD"}, &fc)
	if fc.Exists {
		t.Fatalf("missing file exists")
	}
	// 空仓库的日志
	empty := filepath.Join(t.TempDir(), "empty")
	e.git("", "init", "-q", empty)
	if l := e.logOf(empty, nil); len(l.Commits) != 0 {
		t.Fatalf("empty log")
	}
}
