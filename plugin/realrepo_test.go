package git

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// TestRealReposReadOnly 对真实仓库只读跑一遍常用读操作并计时。默认跳过；
// 设置 PAPERPLANE_GIT_REAL_REPOS=目录1:目录2（每个目录下的子目录各是一个仓库）才运行。
// 运行前后对比 .git 目录下 index/HEAD/FETCH_HEAD/ORIG_HEAD 的修改时间与大小，确认没有写入。
func TestRealReposReadOnly(t *testing.T) {
	roots := os.Getenv("PAPERPLANE_GIT_REAL_REPOS")
	if roots == "" {
		t.Skip("set PAPERPLANE_GIT_REAL_REPOS to run")
	}
	p := &Plugin{}
	ctx := &sdk.Context{Context: context.Background(), Init: &sdk.InitializeParams{CacheDir: t.TempDir(), Locale: "en", Settings: map[string]any{}}}
	var repos []string
	for _, root := range filepath.SplitList(roots) {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			d := filepath.Join(root, e.Name())
			if e.IsDir() && exists(filepath.Join(d, ".git")) {
				repos = append(repos, d)
			}
		}
	}
	sort.Strings(repos)
	call := func(root, op string, args any) (any, time.Duration, error) {
		raw, _ := json.Marshal(args)
		start := time.Now()
		res, err := p.VCS(ctx, &sdk.VCSCallParams{Op: op, Root: root, Args: raw})
		return res, time.Since(start), err
	}
	ms := func(d time.Duration) string { return fmt.Sprintf("%d", d.Milliseconds()) }
	var report []string
	report = append(report, "| 仓库 | 文件数 | status | log50 | branches | diff | remotes | stashList | 备注 |", "|---|---|---|---|---|---|---|---|---|")
	for _, root := range repos {
		before := snapshotGitDir(root)
		var cells []string
		var notes []string
		res, d, err := call(root, sdk.VCSOpStatus, nil)
		if err != nil {
			t.Errorf("%s status: %v", root, err)
			continue
		}
		st := res.(*statusResult)
		statusMs := ms(d)
		nFiles := len(st.Files)
		if st.UntrackedTruncated {
			notes = append(notes, fmt.Sprintf("未跟踪截断 %d", st.UntrackedCount))
		}
		if st.Operation != "" {
			notes = append(notes, "进行中: "+st.Operation)
		}
		res, d, err = call(root, sdk.VCSOpLog, map[string]any{"limit": 50})
		if err != nil {
			t.Errorf("%s log: %v", root, err)
			continue
		}
		lg := res.(*logResult)
		if len(lg.Commits) > 0 && lg.Commits[0].Graph == nil {
			t.Errorf("%s: log without graph", root)
		}
		logMs := ms(d)
		maxLanes := 0
		for _, c := range lg.Commits {
			if c.Graph != nil && c.Graph.Lanes > maxLanes {
				maxLanes = c.Graph.Lanes
			}
		}
		res, d, err = call(root, sdk.VCSOpBranches, nil)
		if err != nil {
			t.Errorf("%s branches: %v", root, err)
			continue
		}
		bs := res.(*branchesResult)
		brMs := ms(d)
		diffMs := "-"
		for _, f := range st.Files {
			if f.Status == "?" || f.Submodule || f.Conflicted {
				continue
			}
			res, d, err = call(root, sdk.VCSOpDiff, map[string]any{"path": f.Path, "oldPath": f.OldPath})
			if err != nil {
				t.Errorf("%s diff %s: %v", root, f.Path, err)
			} else {
				df := res.(*diffResult)
				diffMs = ms(d)
				if df.TooLarge {
					notes = append(notes, "diff 过大")
				}
				if df.Binary {
					notes = append(notes, "diff 二进制")
				}
				// 增删行数与 numstat 一致
				if !df.TooLarge && !df.Binary && (df.Additions != f.Additions || df.Deletions != f.Deletions) {
					notes = append(notes, fmt.Sprintf("diff +%d-%d vs numstat +%d-%d (%s)", df.Additions, df.Deletions, f.Additions, f.Deletions, f.Path))
				}
			}
			break
		}
		res, d, err = call(root, sdk.VCSOpRemotes, nil)
		if err != nil {
			t.Errorf("%s remotes: %v", root, err)
			continue
		}
		rmMs := ms(d)
		res, d, err = call(root, sdk.VCSOpStashList, nil)
		if err != nil {
			t.Errorf("%s stashList: %v", root, err)
			continue
		}
		stashes := len(res.(*stashListResult).Stashes)
		if stashes != st.StashCount {
			t.Errorf("%s: stashList %d != status.stashCount %d", root, stashes, st.StashCount)
		}
		cells = append(cells, filepath.Base(root), fmt.Sprint(nFiles), statusMs, logMs, brMs, diffMs, rmMs, ms(d))
		notes = append(notes, fmt.Sprintf("分支 %s 本地%d/远程%d，日志最宽 %d 列，储藏 %d", st.Branch, len(bs.Local), len(bs.Remote), maxLanes, stashes))
		after := snapshotGitDir(root)
		if before != after {
			t.Errorf("%s: .git changed!\nbefore %s\nafter  %s", root, before, after)
			notes = append(notes, "**.git 有变化**")
		}
		report = append(report, "| "+strings.Join(cells, " | ")+" | "+strings.Join(notes, "；")+" |")
	}
	t.Log("\n" + strings.Join(report, "\n"))
}

// snapshotGitDir 记录 .git 下会被写操作改动的文件状态（index、HEAD、FETCH_HEAD、ORIG_HEAD、index.lock）。
func snapshotGitDir(root string) string {
	r := &repo{root: root}
	g, _ := r.dirs()
	var parts []string
	for _, n := range []string{"index", "HEAD", "FETCH_HEAD", "ORIG_HEAD", "index.lock", "logs/HEAD"} {
		st, err := os.Stat(filepath.Join(g, n))
		if err != nil {
			parts = append(parts, n+":-")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:%d@%d", n, st.Size(), st.ModTime().UnixNano()))
	}
	return strings.Join(parts, " ")
}
