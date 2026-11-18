package git

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

type resolveArgs struct {
	sdk.VCSResolveArgs
	Paths []string `json:"paths,omitempty"` // 一次解决多个文件
	Force bool     `json:"force,omitempty"` // resolved 时文件里还有冲突标记也标记解决
}

// resolve 解决冲突：ours/theirs 取一方（该方删除了文件就删除），resolved 表示用户已合并好、标记解决。
// 注意 rebase 时 ours 是变基目标（上游），theirs 是正在重放的提交，界面显示时要换成对应的名字。
func (r *repo) resolve(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[resolveArgs](raw)
	if err != nil {
		return nil, err
	}
	var in []string
	if a.Path != "" {
		in = append(in, a.Path)
	}
	in = append(in, a.Paths...)
	paths, err := r.checkPaths(in)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, invalidParams("path is required")
	}
	switch a.Side {
	case "ours", "theirs", "resolved":
	default:
		return nil, invalidParams("side must be ours, theirs or resolved")
	}
	stages, err := r.conflictStages(paths)
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		st := stages[p]
		if a.Side == "resolved" {
			if !a.Force && len(st) > 0 && hasConflictMarkers(filepath.Join(r.root, filepath.FromSlash(p))) {
				return &opResult{VCSOpResult: sdk.VCSOpResult{OK: false, Message: tr(r.ctx, "result.markersRemain", p)}, Reason: "markersRemain", Files: []string{p}}, nil
			}
			if _, err := r.do(cmdOpts{}, "add", "-A", "--", ":(literal)"+p); err != nil {
				return nil, err
			}
			continue
		}
		stage := 2
		flag := "--ours"
		if a.Side == "theirs" {
			stage, flag = 3, "--theirs"
		}
		if len(st) == 0 {
			return nil, userError(r.ctx, "notConflicted", "error.notConflicted", p)
		}
		if !st[stage] {
			// 这一方删除了文件
			if _, err := r.do(cmdOpts{}, "rm", "-q", "--force", "--", ":(literal)"+p); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := r.do(cmdOpts{}, "checkout", flag, "--", ":(literal)"+p); err != nil {
			return nil, err
		}
		if _, err := r.do(cmdOpts{}, "add", "--", ":(literal)"+p); err != nil {
			return nil, err
		}
	}
	return okResult(tr(r.ctx, "result.resolved", len(paths))), nil
}

// conflictStages 返回每个冲突文件有哪些阶段（1 共同祖先、2 我方、3 对方）。
func (r *repo) conflictStages(paths []string) (map[string]map[int]bool, error) {
	out, err := r.out(r.pathspecArgs([]string{"ls-files", "-u", "-z"}, paths)...)
	if err != nil {
		return nil, err
	}
	m := map[string]map[int]bool{}
	for _, rec := range strings.Split(out, "\x00") {
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 3 {
			continue
		}
		if m[p] == nil {
			m[p] = map[int]bool{}
		}
		switch f[2] {
		case "1":
			m[p][1] = true
		case "2":
			m[p][2] = true
		case "3":
			m[p][3] = true
		}
	}
	return m, nil
}

// hasConflictMarkers 检查文件里是否还有 <<<<<<< / >>>>>>> 冲突标记（只看 10 MB 以内的文本）。
func hasConflictMarkers(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() > 10<<20 {
		return false
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 10<<20)
	open := false
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "<<<<<<< ") || l == "<<<<<<<" {
			open = true
		} else if open && (strings.HasPrefix(l, ">>>>>>> ") || l == ">>>>>>>") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 外部合并工具

type mergeToolInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	Builtin bool   `json:"builtin,omitempty"` // git 自带支持（--tool=<id>），否则用自定义命令
	cmd     string // 自定义命令模板（git mergetool 的 $BASE $LOCAL $REMOTE $MERGED）
}

type mergeToolsResult struct {
	Configured string          `json:"configured,omitempty"` // 用户 git 配置里的 merge.tool
	Tools      []mergeToolInfo `json:"tools"`
}

func (r *repo) mergeTools() (*mergeToolsResult, error) {
	res := &mergeToolsResult{Tools: detectMergeTools()}
	if out, err := r.out("config", "--get", "merge.tool"); err == nil {
		res.Configured = strings.TrimSpace(out)
	}
	if res.Tools == nil {
		res.Tools = []mergeToolInfo{}
	}
	return res, nil
}

// detectMergeTools 按偏好顺序检测本机可用的合并工具。
func detectMergeTools() []mergeToolInfo {
	var tools []mergeToolInfo
	find := func(names ...string) string {
		for _, n := range names {
			if n == "" {
				continue
			}
			if filepath.IsAbs(n) {
				if sdk.FileExists(n) {
					return n
				}
				continue
			}
			if p, err := exec.LookPath(n); err == nil {
				return p
			}
		}
		return ""
	}
	pf := func(rel string) string {
		for _, env := range []string{"ProgramW6432", "ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
			if d := os.Getenv(env); d != "" {
				if p := filepath.Join(d, rel); sdk.FileExists(p) {
					return p
				}
			}
		}
		return ""
	}
	home := sdk.HomeDir()
	add := func(t mergeToolInfo) {
		if t.Path != "" {
			tools = append(tools, t)
		}
	}
	switch runtime.GOOS {
	case "darwin":
		add(mergeToolInfo{ID: "ksdiff", Name: "Kaleidoscope", Path: find("ksdiff", "/usr/local/bin/ksdiff", "/opt/homebrew/bin/ksdiff", "/Applications/Kaleidoscope.app/Contents/MacOS/ksdiff"),
			cmd: `"$TOOL" --merge --output "$MERGED" --base "$BASE" -- "$LOCAL" --snapshot "$REMOTE" --snapshot`})
		add(mergeToolInfo{ID: "bc", Name: "Beyond Compare", Path: find("bcomp", "/usr/local/bin/bcomp", "/Applications/Beyond Compare.app/Contents/MacOS/bcomp"), Builtin: true})
		add(mergeToolInfo{ID: "vscode", Name: "Visual Studio Code", Path: find("code", "/usr/local/bin/code", "/opt/homebrew/bin/code", "/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code"),
			cmd: `"$TOOL" --wait --merge "$REMOTE" "$LOCAL" "$BASE" "$MERGED"`})
		add(mergeToolInfo{ID: "cursor", Name: "Cursor", Path: find("cursor", "/Applications/Cursor.app/Contents/Resources/app/bin/cursor"),
			cmd: `"$TOOL" --wait --merge "$REMOTE" "$LOCAL" "$BASE" "$MERGED"`})
		add(mergeToolInfo{ID: "idea", Name: "IntelliJ IDEA", Path: find("idea", "/Applications/IntelliJ IDEA.app/Contents/MacOS/idea", "/Applications/IntelliJ IDEA Ultimate.app/Contents/MacOS/idea", "/Applications/IntelliJ IDEA CE.app/Contents/MacOS/idea", filepath.Join(home, "Applications/IntelliJ IDEA Ultimate.app/Contents/MacOS/idea"), filepath.Join(home, "Applications/IntelliJ IDEA.app/Contents/MacOS/idea")),
			cmd: `"$TOOL" merge "$LOCAL" "$REMOTE" "$BASE" "$MERGED"`})
		add(mergeToolInfo{ID: "webstorm", Name: "WebStorm", Path: find("webstorm", "/Applications/WebStorm.app/Contents/MacOS/webstorm", filepath.Join(home, "Applications/WebStorm.app/Contents/MacOS/webstorm")),
			cmd: `"$TOOL" merge "$LOCAL" "$REMOTE" "$BASE" "$MERGED"`})
		// FileMerge 需要完整 Xcode；只有命令行工具时 opendiff 是个桩
		if p := find("/Applications/Xcode.app/Contents/Developer/usr/bin/opendiff"); p != "" {
			add(mergeToolInfo{ID: "opendiff", Name: "FileMerge", Path: p, Builtin: true})
		}
		add(mergeToolInfo{ID: "p4merge", Name: "P4Merge", Path: find("p4merge", "/Applications/p4merge.app/Contents/MacOS/p4merge"), Builtin: true})
		add(mergeToolInfo{ID: "meld", Name: "Meld", Path: find("meld"), Builtin: true})
	case "windows":
		add(mergeToolInfo{ID: "bc", Name: "Beyond Compare", Path: find("bcomp", pf(`Beyond Compare 5\BComp.exe`), pf(`Beyond Compare 4\BComp.exe`)), Builtin: true})
		add(mergeToolInfo{ID: "vscode", Name: "Visual Studio Code", Path: find("code.cmd", pf(`Programs\Microsoft VS Code\bin\code.cmd`), pf(`Microsoft VS Code\bin\code.cmd`)),
			cmd: `"$TOOL" --wait --merge "$REMOTE" "$LOCAL" "$BASE" "$MERGED"`})
		add(mergeToolInfo{ID: "idea", Name: "IntelliJ IDEA", Path: find("idea64.exe", "idea.cmd"),
			cmd: `"$TOOL" merge "$LOCAL" "$REMOTE" "$BASE" "$MERGED"`})
		add(mergeToolInfo{ID: "winmerge", Name: "WinMerge", Path: find("WinMergeU.exe", pf(`WinMerge\WinMergeU.exe`)), Builtin: true})
		add(mergeToolInfo{ID: "tortoisegitmerge", Name: "TortoiseGitMerge", Path: find("TortoiseGitMerge.exe", pf(`TortoiseGit\bin\TortoiseGitMerge.exe`)), Builtin: true})
		add(mergeToolInfo{ID: "kdiff3", Name: "KDiff3", Path: find("kdiff3.exe", pf(`KDiff3\kdiff3.exe`), pf(`KDiff3\bin\kdiff3.exe`)), Builtin: true})
		add(mergeToolInfo{ID: "p4merge", Name: "P4Merge", Path: find("p4merge.exe", pf(`Perforce\p4merge.exe`)), Builtin: true})
		add(mergeToolInfo{ID: "meld", Name: "Meld", Path: find("Meld.exe", pf(`Meld\Meld.exe`)), Builtin: true})
	default:
		add(mergeToolInfo{ID: "bc", Name: "Beyond Compare", Path: find("bcompare"), Builtin: true})
		add(mergeToolInfo{ID: "vscode", Name: "Visual Studio Code", Path: find("code"), cmd: `"$TOOL" --wait --merge "$REMOTE" "$LOCAL" "$BASE" "$MERGED"`})
		add(mergeToolInfo{ID: "idea", Name: "IntelliJ IDEA", Path: find("idea", "idea.sh", "intellij-idea-ultimate", "intellij-idea-community"), cmd: `"$TOOL" merge "$LOCAL" "$REMOTE" "$BASE" "$MERGED"`})
		add(mergeToolInfo{ID: "meld", Name: "Meld", Path: find("meld"), Builtin: true})
		add(mergeToolInfo{ID: "kdiff3", Name: "KDiff3", Path: find("kdiff3"), Builtin: true})
		add(mergeToolInfo{ID: "p4merge", Name: "P4Merge", Path: find("p4merge"), Builtin: true})
	}
	return tools
}

// mergeTool 在外部合并工具里打开冲突文件（后台运行，工具关闭后 git mergetool 自动标记解决）。
func (r *repo) mergeTool(raw json.RawMessage) (*opResult, error) {
	a, err := decodeArgs[sdk.VCSPathsArgs](raw)
	if err != nil {
		return nil, err
	}
	paths, err := r.checkPaths(a.Paths)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		paths = r.unmergedFiles()
	}
	if len(paths) == 0 {
		return nil, userError(r.ctx, "notConflicted", "error.noConflicts")
	}
	args := r.globalArgs(cmdOpts{})
	args = append(args, "-c", "mergetool.keepBackup=false", "-c", "mergetool.prompt=false")
	toolName := ""
	pref := settingString(r.ctx, "mergeTool", "auto")
	if a.Tool != "" {
		pref = a.Tool
	}
	configured, _ := r.out("config", "--get", "merge.tool")
	configured = strings.TrimSpace(configured)
	var chosen *mergeToolInfo
	if pref == "auto" || pref == "" {
		if configured != "" {
			toolName = configured
		}
	}
	if toolName == "" {
		tools := detectMergeTools()
		for i := range tools {
			if pref == "auto" || pref == "" || tools[i].ID == pref {
				chosen = &tools[i]
				break
			}
		}
		if chosen == nil && configured != "" {
			toolName = configured
		}
	}
	switch {
	case chosen != nil && chosen.Builtin:
		toolName = chosen.ID
		args = append(args, "-c", "mergetool."+chosen.ID+".path="+chosen.Path, "mergetool", "--no-prompt", "--tool="+chosen.ID)
	case chosen != nil:
		toolName = chosen.Name
		cmd := strings.ReplaceAll(chosen.cmd, "$TOOL", strings.ReplaceAll(chosen.Path, `\`, `/`))
		args = append(args, "-c", "mergetool.paperplane.cmd="+cmd, "-c", "mergetool.paperplane.trustExitCode=true", "mergetool", "--no-prompt", "--tool=paperplane")
	case toolName != "":
		args = append(args, "mergetool", "--no-prompt")
	default:
		return nil, userError(r.ctx, "noMergeTool", "error.noMergeTool")
	}
	args = append(args, "--")
	args = append(args, paths...)
	cmd := exec.Command(r.git, args...)
	cmd.Dir = r.root
	cmd.Env = buildEnv(r.envOverlay(cmdOpts{}))
	setDetached(cmd)
	if err := cmd.Start(); err != nil {
		return nil, userError(r.ctx, "noMergeTool", "error.mergeToolStart", err.Error())
	}
	go func() { _ = cmd.Wait() }()
	out := okResult(tr(r.ctx, "result.mergeToolOpened", toolName, len(paths)))
	out.Tool = toolName
	out.Files = paths
	return out, nil
}
