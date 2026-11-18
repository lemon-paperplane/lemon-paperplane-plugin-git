package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

func lines(n int, f func(i int) string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString(f(i))
		b.WriteByte('\n')
	}
	return b.String()
}

func (e *tenv) diff(root string, args map[string]any) diffResult {
	e.t.Helper()
	var d diffResult
	e.must(root, sdk.VCSOpDiff, args, &d)
	return d
}

func TestDiffModes(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	base := lines(30, func(i int) string { return fmt.Sprintf("line %d", i) })
	e.write(dir, "f.txt", base)
	c1 := e.commitAll(dir, "base")
	// 暂存：改第 2 行；工作区再改第 28 行
	staged := strings.Replace(base, "line 2\n", "line 2 staged\n", 1)
	e.write(dir, "f.txt", staged)
	e.git(dir, "add", "f.txt")
	work := strings.Replace(staged, "line 28\n", "line 28 work\n", 1)
	e.write(dir, "f.txt", work)

	d := e.diff(dir, map[string]any{"path": "f.txt"})
	if d.Mode != "head" || len(d.Hunks) != 2 || d.Additions != 2 || d.Deletions != 2 || d.Language != "text" {
		t.Fatalf("head diff: mode=%s hunks=%d +%d -%d lang=%s", d.Mode, len(d.Hunks), d.Additions, d.Deletions, d.Language)
	}
	h := d.Hunks[0]
	if h.OldStart != 1 || h.NewStart != 1 || !strings.HasPrefix(h.Header, "@@ -1,5 +1,5 @@") {
		t.Errorf("hunk0 header %q", h.Header)
	}
	// 行号
	var del, add diffLine
	for _, l := range h.Lines {
		if l.Kind == "-" {
			del = l
		}
		if l.Kind == "+" {
			add = l
		}
	}
	if del.Old != 2 || del.New != 0 || add.New != 2 || add.Old != 0 || add.Text != "line 2 staged" {
		t.Errorf("line numbers: del=%+v add=%+v", del, add)
	}
	if d := e.diff(dir, map[string]any{"path": "f.txt", "mode": "staged"}); len(d.Hunks) != 1 || d.Hunks[0].OldStart != 1 {
		t.Errorf("staged diff hunks=%d", len(d.Hunks))
	}
	if d := e.diff(dir, map[string]any{"path": "f.txt", "mode": "unstaged"}); len(d.Hunks) != 1 || d.Hunks[0].OldStart < 20 {
		t.Errorf("unstaged diff hunks=%d", len(d.Hunks))
	}
	if d := e.diff(dir, map[string]any{"path": "f.txt", "context": -1}); len(d.Hunks) != 1 || len(d.Hunks[0].Lines) != 32 {
		t.Errorf("whole-file diff: hunks=%d", len(d.Hunks))
	}
	if d := e.diff(dir, map[string]any{"path": "f.txt", "context": 0}); len(d.Hunks) != 2 || len(d.Hunks[0].Lines) != 2 {
		t.Errorf("context 0 diff: %+v", d.Hunks)
	}
	e.git(dir, "commit", "-q", "-am", "c2")
	c2 := strings.TrimSpace(e.git(dir, "rev-parse", "HEAD"))
	if d := e.diff(dir, map[string]any{"path": "f.txt", "commit": c2}); d.Mode != "commit" || len(d.Hunks) != 2 {
		t.Errorf("commit diff hunks=%d", len(d.Hunks))
	}
	if d := e.diff(dir, map[string]any{"path": "f.txt", "mode": "range", "base": c1, "head": c2}); len(d.Hunks) != 2 {
		t.Errorf("range diff hunks=%d", len(d.Hunks))
	}
	// 首个提交（没有父提交）
	first := strings.TrimSpace(e.git(dir, "rev-list", "--max-parents=0", "HEAD"))
	if d := e.diff(dir, map[string]any{"path": "README.md", "commit": first}); d.Status != "A" || len(d.Hunks) != 1 {
		t.Errorf("root commit diff: %+v", d)
	}
}

func TestDiffSpecialFiles(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	e.write(dir, "gone.txt", "a\nb\n")
	e.write(dir, "old name.txt", strings.Repeat("same content line\n", 20))
	e.write(dir, "img.png", "\x89PNG\x00\x00binary")
	e.write(dir, "noeol.txt", "a\nb")
	e.commitAll(dir, "base")
	os.Remove(filepath.Join(dir, "gone.txt"))
	e.git(dir, "mv", "old name.txt", "新 名字.txt")
	e.write(dir, "img.png", "\x89PNG\x00\x00binary2")
	e.write(dir, "noeol.txt", "a\nc")
	e.write(dir, "new file.txt", "n1\nn2\n")
	e.write(dir, "big.txt", lines(6000, func(i int) string { return fmt.Sprint(i) }))

	if d := e.diff(dir, map[string]any{"path": "gone.txt"}); d.Status != "D" || len(d.Hunks) != 1 || d.Deletions != 2 {
		t.Errorf("deleted: %+v", d)
	}
	if d := e.diff(dir, map[string]any{"path": "新 名字.txt", "oldPath": "old name.txt"}); d.Status != "R" || d.OldPath != "old name.txt" || d.Similarity != 100 || len(d.Hunks) != 0 {
		t.Errorf("renamed: %+v", d)
	}
	if d := e.diff(dir, map[string]any{"path": "img.png"}); !d.Binary || len(d.Hunks) != 0 {
		t.Errorf("binary: %+v", d)
	}
	d := e.diff(dir, map[string]any{"path": "noeol.txt"})
	if len(d.Hunks) != 1 {
		t.Fatalf("noeol hunks %d", len(d.Hunks))
	}
	noEOL := 0
	for _, l := range d.Hunks[0].Lines {
		if l.NoEOL {
			noEOL++
		}
	}
	if noEOL != 2 {
		t.Errorf("noEol markers = %d, want 2 (%+v)", noEOL, d.Hunks[0].Lines)
	}
	if d := e.diff(dir, map[string]any{"path": "new file.txt"}); !d.Untracked || d.Status != "A" || d.Additions != 2 {
		t.Errorf("untracked: %+v", d)
	}
	if d := e.diff(dir, map[string]any{"path": "big.txt"}); !d.TooLarge || len(d.Hunks) != 0 {
		t.Errorf("big file should be too large: lines=%d", len(d.Hunks))
	}
	if d := e.diff(dir, map[string]any{"path": "big.txt", "noLimit": true}); d.TooLarge || d.Additions != 6000 {
		t.Errorf("noLimit: tooLarge=%v +%d", d.TooLarge, d.Additions)
	}
}

func TestParseQuotedPaths(t *testing.T) {
	in := "diff --git \"a/tab\\there\" \"b/tab\\there\"\nnew file mode 100644\nindex 0000000..e69de29\n--- /dev/null\n+++ \"b/tab\\there\"\n@@ -0,0 +1 @@\n+x\n" +
		"diff --git a/plain b/plain\nindex 1..2 100644\n--- a/plain\n+++ b/plain\n@@ -1,2 +1,2 @@\n-a\n+b\n \n"
	files, err := parseUnifiedDiff(strings.NewReader(in))
	if err != nil || len(files) != 2 {
		t.Fatalf("parse: %v %d", err, len(files))
	}
	if files[0].newPath != "tab\there" || files[0].status != "A" {
		t.Errorf("quoted path = %q status=%s", files[0].newPath, files[0].status)
	}
	if files[1].newPath != "plain" || len(files[1].hunks) != 1 || len(files[1].hunks[0].lines) != 3 {
		t.Errorf("plain: %+v", files[1])
	}
	if got := unquoteCPath(`"\344\270\255.txt"`); got != "中.txt" {
		t.Errorf("octal unquote = %q", got)
	}
}

// ---------------------------------------------------------------------------
// 按块暂存 / 取消暂存 / 丢弃

func (e *tenv) index(dir, path string) string {
	e.t.Helper()
	return e.git(dir, "show", ":"+path)
}

func TestStageUnstageDiscardHunks(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	base := lines(40, func(i int) string { return fmt.Sprintf("L%d", i) })
	e.write(dir, "f.txt", base)
	e.commitAll(dir, "base")
	// 三处修改：首行、中间（多加一行改变行数）、末行
	work := strings.Replace(base, "L1\n", "L1 changed\n", 1)
	work = strings.Replace(work, "L20\n", "L20 changed\nL20b inserted\n", 1)
	work = strings.Replace(work, "L40\n", "L40 changed\n", 1)
	e.write(dir, "f.txt", work)

	d := e.diff(dir, map[string]any{"path": "f.txt", "mode": "unstaged"})
	if len(d.Hunks) != 3 {
		t.Fatalf("want 3 hunks, got %d", len(d.Hunks))
	}
	// 只暂存第 3 块（前面的块没选，补丁位置要自洽）
	e.op(dir, sdk.VCSOpStageHunks, map[string]any{"path": "f.txt", "hunks": []int{2}, "headers": []string{d.Hunks[2].Header}})
	idx := e.index(dir, "f.txt")
	if !strings.Contains(idx, "L40 changed") || strings.Contains(idx, "L1 changed") || strings.Contains(idx, "L20b") {
		t.Fatalf("index after staging hunk 2:\n%s", idx)
	}
	// 再暂存第 1 块：基于新的未暂存 diff（剩 2 块）
	d = e.diff(dir, map[string]any{"path": "f.txt", "mode": "unstaged"})
	if len(d.Hunks) != 2 {
		t.Fatalf("remaining unstaged hunks %d", len(d.Hunks))
	}
	e.op(dir, sdk.VCSOpStageHunks, map[string]any{"path": "f.txt", "hunks": []int{0}})
	idx = e.index(dir, "f.txt")
	if !strings.Contains(idx, "L1 changed") || strings.Contains(idx, "L20b") {
		t.Fatalf("index after staging hunk 0:\n%s", idx)
	}
	// 取消暂存：暂存区 diff 现在有 2 块，取消第 2 块（末行）
	sd := e.diff(dir, map[string]any{"path": "f.txt", "mode": "staged"})
	if len(sd.Hunks) != 2 {
		t.Fatalf("staged hunks %d", len(sd.Hunks))
	}
	e.op(dir, sdk.VCSOpUnstageHunks, map[string]any{"path": "f.txt", "hunks": []int{1}})
	idx = e.index(dir, "f.txt")
	if strings.Contains(idx, "L40 changed") || !strings.Contains(idx, "L1 changed") {
		t.Fatalf("index after unstaging:\n%s", idx)
	}
	// 丢弃：未暂存 diff 中间那块（L20）
	d = e.diff(dir, map[string]any{"path": "f.txt", "mode": "unstaged"})
	mid := -1
	for i, h := range d.Hunks {
		for _, l := range h.Lines {
			if l.Text == "L20b inserted" {
				mid = i
			}
		}
	}
	if mid < 0 {
		t.Fatalf("middle hunk not found")
	}
	e.op(dir, sdk.VCSOpDiscardHunks, map[string]any{"path": "f.txt", "hunks": []int{mid}})
	got := e.read(dir, "f.txt")
	if strings.Contains(got, "L20b") || !strings.Contains(got, "L40 changed") || !strings.Contains(got, "L1 changed") {
		t.Fatalf("worktree after discard:\n%s", got)
	}
	// 过期的块头被拒绝
	var r opResult
	err := e.call(dir, sdk.VCSOpStageHunks, map[string]any{"path": "f.txt", "hunks": []int{0}, "headers": []string{"@@ -9,9 +9,9 @@"}}, &r)
	if reasonOfErr(err) != "staleDiff" {
		t.Fatalf("stale headers: err=%v", err)
	}
}

func TestHunksEdgeCases(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	// 无换行结尾
	e.write(dir, "noeol.txt", "a\nb\nc")
	e.commitAll(dir, "base")
	e.write(dir, "noeol.txt", "a\nb\nc changed")
	e.op(dir, sdk.VCSOpStageHunks, map[string]any{"path": "noeol.txt", "hunks": []int{0}})
	if idx := e.index(dir, "noeol.txt"); idx != "a\nb\nc changed" {
		t.Fatalf("noeol staged = %q", idx)
	}
	e.op(dir, sdk.VCSOpUnstageHunks, map[string]any{"path": "noeol.txt", "hunks": []int{0}})
	if idx := e.index(dir, "noeol.txt"); idx != "a\nb\nc" {
		t.Fatalf("noeol unstaged = %q", idx)
	}
	e.op(dir, sdk.VCSOpDiscardHunks, map[string]any{"path": "noeol.txt", "hunks": []int{0}})
	if got := e.read(dir, "noeol.txt"); got != "a\nb\nc" {
		t.Fatalf("noeol discarded = %q", got)
	}

	// 新文件（未跟踪）按块暂存
	e.write(dir, "brand new.txt", "x\ny\n")
	e.op(dir, sdk.VCSOpStageHunks, map[string]any{"path": "brand new.txt", "hunks": []int{0}})
	if idx := e.index(dir, "brand new.txt"); idx != "x\ny\n" {
		t.Fatalf("new file staged = %q", idx)
	}

	// -U0：两块，只暂存第二块
	e.write(dir, "z.txt", lines(10, func(i int) string { return fmt.Sprint(i) }))
	e.commitAll(dir, "z")
	e.write(dir, "z.txt", strings.Replace(strings.Replace(lines(10, func(i int) string { return fmt.Sprint(i) }), "2\n", "2\n2.5\n", 1), "9\n", "", 1))
	d := e.diff(dir, map[string]any{"path": "z.txt", "mode": "unstaged", "context": 0})
	if len(d.Hunks) != 2 {
		t.Fatalf("U0 hunks %d", len(d.Hunks))
	}
	e.op(dir, sdk.VCSOpStageHunks, map[string]any{"path": "z.txt", "hunks": []int{1}, "context": 0})
	if idx := e.index(dir, "z.txt"); strings.Contains(idx, "9\n") || strings.Contains(idx, "2.5") {
		t.Fatalf("U0 staged = %q", idx)
	}
	// 首行插入（-U0 时 oldStart=0）
	e.git(dir, "checkout", "--", "z.txt")
	e.git(dir, "reset", "-q", "--hard")
	e.write(dir, "z.txt", "0\n"+lines(10, func(i int) string { return fmt.Sprint(i) })+"11\n")
	e.op(dir, sdk.VCSOpStageHunks, map[string]any{"path": "z.txt", "hunks": []int{1}, "context": 0})
	if idx := e.index(dir, "z.txt"); !strings.HasSuffix(idx, "10\n11\n") || strings.HasPrefix(idx, "0\n") {
		t.Fatalf("U0 append staged = %q", idx)
	}
	e.op(dir, sdk.VCSOpStageHunks, map[string]any{"path": "z.txt", "hunks": []int{0}, "context": 0})
	if idx := e.index(dir, "z.txt"); !strings.HasPrefix(idx, "0\n1\n") {
		t.Fatalf("U0 prepend staged = %q", idx)
	}

	// 删除的文件按块暂存 = 暂存删除
	os.Remove(filepath.Join(dir, "README.md"))
	e.op(dir, sdk.VCSOpStageHunks, map[string]any{"path": "README.md", "hunks": []int{0}})
	if _, err := e.gitErr(dir, "cat-file", "-e", ":README.md"); err == nil {
		t.Fatalf("README.md should be removed from index")
	}

	// CRLF 文件
	e.write(dir, "crlf.txt", "a\r\nb\r\nc\r\n")
	e.commitAll(dir, "crlf")
	e.write(dir, "crlf.txt", "a\r\nB\r\nc\r\n")
	dd := e.diff(dir, map[string]any{"path": "crlf.txt"})
	if len(dd.Hunks) != 1 || strings.ContainsRune(dd.Hunks[0].Lines[1].Text, '\r') {
		t.Fatalf("crlf diff: %+v", dd.Hunks)
	}
	e.op(dir, sdk.VCSOpStageHunks, map[string]any{"path": "crlf.txt", "hunks": []int{0}})
	if idx := e.index(dir, "crlf.txt"); idx != "a\r\nB\r\nc\r\n" {
		t.Fatalf("crlf staged = %q", idx)
	}
}

func TestStageUnstageFiles(t *testing.T) {
	e := newEnv(t)
	dir := e.repo()
	e.write(dir, "a.txt", "a\n")
	e.write(dir, "README.md", "changed\n")
	e.op(dir, sdk.VCSOpStage, map[string]any{"paths": []string{"a.txt", "README.md"}})
	st := e.status(dir)
	if f := findFile(st, "a.txt"); f == nil || f.Status != "A" || !f.Staged {
		t.Fatalf("a.txt: %+v", f)
	}
	e.op(dir, sdk.VCSOpUnstage, map[string]any{"paths": []string{"a.txt", "README.md"}})
	st = e.status(dir)
	if f := findFile(st, "a.txt"); f == nil || f.Status != "?" {
		t.Fatalf("a.txt after unstage: %+v", f)
	}
	if f := findFile(st, "README.md"); f == nil || f.Staged || !f.Unstaged {
		t.Fatalf("README after unstage: %+v", f)
	}
}
