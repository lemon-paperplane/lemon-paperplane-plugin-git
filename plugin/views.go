package git

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// 插件贡献的低频页面（挂在版本控制页的标签里）：远程、标签、子模块、工作树、Git 设置。
// 高频界面（变更/提交、差异、日志图、分支）由原生界面直接调 vcs/call。
const (
	viewRemotes    = "remotes"
	viewTags       = "tags"
	viewSubmodules = "submodules"
	viewWorktrees  = "worktrees"
	viewSettings   = "settings"
)

const maxTagRows = 500

// viewRepo 确定视图操作哪个仓库：State.root（用户在下拉框里选的）> 模块目录所在仓库 > 工程目录所在仓库 > 工程目录下找到的第一个仓库。
func (p *Plugin) viewRepo(ctx *sdk.Context, project *sdk.ProjectInfo, module *sdk.ModuleInfo, state map[string]any, task string) (*repo, []string, error) {
	gitPath, ver, err := p.resolveGit(ctx)
	if err != nil {
		return nil, nil, err
	}
	mk := func(root string) *repo {
		return &repo{p: p, ctx: ctx, root: root, task: task, git: gitPath, ver: ver}
	}
	base := ""
	if module != nil && module.Dir != "" {
		base = module.Dir
	} else if project != nil {
		base = project.Root
	}
	var candidates []string
	add := func(p string) {
		for _, c := range candidates {
			if samePath(c, p) {
				return
			}
		}
		candidates = append(candidates, p)
	}
	if base != "" {
		if info, err := mk(base).detect(); err == nil && info.IsRepo {
			top := info.Root
			if samePath(top, base) {
				top = base // 保留调用方给的写法（/var 与 /private/var 这类符号链接）
			}
			add(top)
		}
		if project != nil && project.Root != "" {
			if list, err := p.repos(ctx, &sdk.VCSCallParams{Root: project.Root, Args: json.RawMessage(`{"maxDepth":3}`)}); err == nil {
				for _, e := range list.Repos {
					add(e.Root)
				}
			}
		}
	}
	if s, ok := state["root"].(string); ok && s != "" && sdk.DirExists(s) {
		found := false
		for _, c := range candidates {
			if samePath(c, s) {
				s, found = c, true
				break
			}
		}
		if !found {
			candidates = append([]string{s}, candidates...)
		}
		return mk(s), candidates, nil
	}
	if len(candidates) == 0 {
		return nil, nil, nil
	}
	return mk(candidates[0]), candidates, nil
}

// RenderView 实现 sdk.ViewProvider。
func (p *Plugin) RenderView(ctx *sdk.Context, v *sdk.ViewRenderParams) (*sdk.ViewRenderResult, error) {
	r, candidates, err := p.viewRepo(ctx, v.Project, v.Module, v.State, "")
	if err != nil {
		return &sdk.ViewRenderResult{Root: sdk.Alert("danger", tr(ctx, "view.error"), err.Error())}, nil
	}
	if r == nil {
		return &sdk.ViewRenderResult{Root: emptyNode("builtin:folder-git-2", tr(ctx, "view.notRepo"), "")}, nil
	}
	state := v.State
	if state == nil {
		state = map[string]any{}
	}
	var body []sdk.ViewNode
	if len(candidates) > 1 {
		body = append(body, repoSelector(ctx, r.root, candidates))
	}
	var content sdk.ViewNode
	switch v.View {
	case viewRemotes:
		content, err = r.renderRemotes(state)
	case viewTags:
		content, err = r.renderTags(state)
	case viewSubmodules:
		content, err = r.renderSubmodules()
	case viewWorktrees:
		content, err = r.renderWorktrees()
	case viewSettings:
		content, err = r.renderSettings()
	default:
		return nil, sdk.ErrNotSupported
	}
	if err != nil {
		content = sdk.Alert("danger", tr(ctx, "view.error"), errText(err))
	}
	body = append(body, content)
	state["root"] = r.root
	return &sdk.ViewRenderResult{Root: sdk.Column(body...), State: state}, nil
}

// samePath 判断两个路径是否指向同一目录（解析符号链接）。
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

func errText(err error) string {
	if e, ok := err.(*sdk.Error); ok {
		return e.Message
	}
	return err.Error()
}

func emptyNode(icon, title, text string) sdk.ViewNode {
	n := sdk.Empty(title, text)
	n.Icon = icon
	return n
}

func repoSelector(ctx *sdk.Context, cur string, candidates []string) sdk.ViewNode {
	var opts []sdk.Option
	for _, c := range candidates {
		opts = append(opts, sdk.Option{Value: c, Label: filepath.Base(c) + "  " + c})
	}
	return sdk.Form("selectRoot", tr(ctx, "view.switch"), []sdk.FormField{
		{Key: "root", Title: tr(ctx, "view.repository"), Type: "select", Options: opts, Default: cur},
	}, map[string]any{"root": cur})
}

func button(text, action, style, icon string, data map[string]string) sdk.ViewNode {
	return sdk.ViewNode{Type: "button", Text: text, Action: action, Style: style, Icon: icon, Data: data}
}

// ---------------------------------------------------------------------------
// 远程

func (r *repo) renderRemotes(state map[string]any) (sdk.ViewNode, error) {
	list, err := r.remotes()
	if err != nil {
		return sdk.ViewNode{}, err
	}
	ctx := r.ctx
	var nodes []sdk.ViewNode
	nodes = append(nodes, sdk.ViewNode{Type: "toolbar", Children: []sdk.ViewNode{
		button(tr(ctx, "view.fetchAll"), "remote.fetchAll", "secondary", "builtin:cloud-download", nil),
	}})
	if len(list) == 0 {
		nodes = append(nodes, emptyNode("builtin:server", tr(ctx, "view.remotes.empty"), ""))
	} else {
		cols := []sdk.TableColumn{
			{Key: "name", Title: tr(ctx, "view.col.name"), Width: 120},
			{Key: "url", Title: tr(ctx, "view.col.url"), Mono: true},
			{Key: "push", Title: tr(ctx, "view.col.pushUrl"), Mono: true},
		}
		var rows []sdk.TableRow
		for _, rm := range list {
			d := map[string]string{"name": rm.Name}
			acts := []sdk.ViewNode{
				button(tr(ctx, "view.fetch"), "remote.fetch", "link", "builtin:cloud-download", d),
				button(tr(ctx, "view.edit"), "remote.edit", "link", "builtin:pencil", d),
				{Type: "button", Text: tr(ctx, "view.remove"), Action: "remote.remove", Style: "danger", Icon: "builtin:trash-2", Data: d,
					Confirm: tr(ctx, "view.remotes.confirmRemove", rm.Name)},
			}
			if rm.WebURL != "" {
				acts = append([]sdk.ViewNode{{Type: "link", Text: tr(ctx, "view.openWeb"), Href: rm.WebURL}}, acts...)
			}
			rows = append(rows, sdk.TableRow{ID: rm.Name, Icon: "builtin:server",
				Cells: map[string]string{"name": rm.Name, "url": rm.FetchURL, "push": rm.PushURL}, Actions: acts})
		}
		nodes = append(nodes, sdk.Table(cols, rows))
	}
	editing, _ := state["editing"].(string)
	title := tr(ctx, "view.remotes.add")
	values := map[string]any{}
	if editing != "" {
		title = tr(ctx, "view.remotes.edit", editing)
		for _, rm := range list {
			if rm.Name == editing {
				values = map[string]any{"name": rm.Name, "url": rm.FetchURL, "pushUrl": rm.PushURL}
			}
		}
	}
	form := sdk.Form("remote.save", tr(ctx, "view.save"), []sdk.FormField{
		{Key: "name", Title: tr(ctx, "view.col.name"), Type: "string", Required: true, Placeholder: "origin"},
		{Key: "url", Title: tr(ctx, "view.col.url"), Type: "string", Required: true, Placeholder: "git@github.com:owner/repo.git"},
		{Key: "pushUrl", Title: tr(ctx, "view.col.pushUrl"), Type: "string", Advanced: true, Info: tr(ctx, "view.remotes.pushUrlInfo")},
	}, values)
	sec := sdk.Section(title, form)
	if editing != "" {
		sec.Children = append(sec.Children, button(tr(ctx, "view.cancel"), "remote.cancel", "link", "", nil))
	}
	nodes = append(nodes, sec)
	return sdk.Column(nodes...), nil
}

// ---------------------------------------------------------------------------
// 标签

func (r *repo) renderTags(state map[string]any) (sdk.ViewNode, error) {
	ctx := r.ctx
	res, err := r.tags()
	if err != nil {
		return sdk.ViewNode{}, err
	}
	filter, _ := state["filter"].(string)
	var nodes []sdk.ViewNode
	create := sdk.Form("tag.create", tr(ctx, "view.create"), []sdk.FormField{
		{Key: "name", Title: tr(ctx, "view.col.name"), Type: "string", Required: true, Placeholder: "v1.0.0"},
		{Key: "ref", Title: tr(ctx, "view.tags.ref"), Type: "string", Placeholder: "HEAD"},
		{Key: "message", Title: tr(ctx, "view.tags.message"), Type: "text", Info: tr(ctx, "view.tags.messageInfo")},
		{Key: "push", Title: tr(ctx, "view.tags.pushAfter"), Type: "boolean"},
	}, nil)
	sec := sdk.Section(tr(ctx, "view.tags.new"), create)
	sec.Collapsed = true
	nodes = append(nodes, sec)
	nodes = append(nodes, sdk.Form("tag.filter", tr(ctx, "view.search"), []sdk.FormField{
		{Key: "filter", Title: tr(ctx, "view.filter"), Type: "string", Placeholder: tr(ctx, "view.tags.filterPlaceholder")},
	}, map[string]any{"filter": filter}))
	if len(res.Tags) == 0 {
		nodes = append(nodes, emptyNode("builtin:tag", tr(ctx, "view.tags.empty"), ""))
		return sdk.Column(nodes...), nil
	}
	cols := []sdk.TableColumn{
		{Key: "name", Title: tr(ctx, "view.col.name"), Width: 160},
		{Key: "commit", Title: tr(ctx, "view.col.commit"), Width: 90, Mono: true},
		{Key: "date", Title: tr(ctx, "view.col.date"), Width: 150},
		{Key: "subject", Title: tr(ctx, "view.col.subject")},
	}
	var rows []sdk.TableRow
	shown := 0
	for _, t := range res.Tags {
		if filter != "" && !strings.Contains(strings.ToLower(t.Name), strings.ToLower(filter)) {
			continue
		}
		shown++
		if shown > maxTagRows {
			continue
		}
		d := map[string]string{"name": t.Name, "hash": t.Hash}
		subject := t.Subject
		if t.Annotated && t.Message != "" {
			subject = t.Message
		}
		rows = append(rows, sdk.TableRow{ID: t.Name, Icon: "builtin:tag",
			Cells: map[string]string{"name": t.Name, "commit": shortHash(t.Hash), "date": shortDate(t.Date), "subject": subject},
			Actions: []sdk.ViewNode{
				button(tr(ctx, "view.push"), "tag.push", "link", "builtin:cloud-upload", d),
				button(tr(ctx, "view.checkout"), "tag.checkout", "link", "builtin:git-branch", d),
				{Type: "button", Text: tr(ctx, "view.remove"), Action: "tag.delete", Style: "danger", Icon: "builtin:trash-2", Data: d, Confirm: tr(ctx, "view.tags.confirmDelete", t.Name)},
				{Type: "button", Text: tr(ctx, "view.tags.deleteRemote"), Action: "tag.deleteRemote", Style: "danger", Data: d, Confirm: tr(ctx, "view.tags.confirmDeleteRemote", t.Name)},
			}})
	}
	nodes = append(nodes, sdk.Table(cols, rows))
	if shown > maxTagRows {
		nodes = append(nodes, sdk.ViewNode{Type: "text", Tone: "neutral", Text: tr(ctx, "view.tags.truncated", maxTagRows, shown)})
	}
	return sdk.Column(nodes...), nil
}

func shortDate(iso string) string {
	if len(iso) >= 16 {
		return strings.Replace(iso[:16], "T", " ", 1)
	}
	return iso
}

// ---------------------------------------------------------------------------
// 子模块

func (r *repo) renderSubmodules() (sdk.ViewNode, error) {
	ctx := r.ctx
	res, err := r.submodules()
	if err != nil {
		return sdk.ViewNode{}, err
	}
	if len(res.Submodules) == 0 {
		return emptyNode("builtin:boxes", tr(ctx, "view.submodules.empty"), ""), nil
	}
	nodes := []sdk.ViewNode{{Type: "toolbar", Children: []sdk.ViewNode{
		button(tr(ctx, "view.submodules.updateAll"), "submodule.updateAll", "primary", "builtin:refresh-cw", nil),
		button(tr(ctx, "view.submodules.updateRemote"), "submodule.updateRemote", "secondary", "builtin:cloud-download", nil),
	}}}
	cols := []sdk.TableColumn{
		{Key: "path", Title: tr(ctx, "view.col.path"), Mono: true},
		{Key: "status", Title: tr(ctx, "view.col.status"), Width: 100},
		{Key: "head", Title: tr(ctx, "view.col.commit"), Width: 90, Mono: true},
		{Key: "url", Title: tr(ctx, "view.col.url"), Mono: true},
	}
	var rows []sdk.TableRow
	for _, s := range res.Submodules {
		tone := "success"
		switch s.Status {
		case "uninitialized":
			tone = "neutral"
		case "modified":
			tone = "warning"
		case "conflict":
			tone = "danger"
		}
		d := map[string]string{"path": s.Path}
		rows = append(rows, sdk.TableRow{ID: s.Path, Icon: "builtin:package", Tone: tone,
			Cells:   map[string]string{"path": s.Path, "status": tr(ctx, "view.submodules.status."+s.Status), "head": shortHash(s.Head), "url": s.URL},
			Actions: []sdk.ViewNode{button(tr(ctx, "view.update"), "submodule.update", "link", "builtin:refresh-cw", d)}})
	}
	nodes = append(nodes, sdk.Table(cols, rows))
	return sdk.Column(nodes...), nil
}

// ---------------------------------------------------------------------------
// 工作树

func (r *repo) renderWorktrees() (sdk.ViewNode, error) {
	ctx := r.ctx
	res, err := r.worktrees()
	if err != nil {
		return sdk.ViewNode{}, err
	}
	cols := []sdk.TableColumn{
		{Key: "path", Title: tr(ctx, "view.col.path"), Mono: true},
		{Key: "branch", Title: tr(ctx, "view.col.branch"), Width: 180},
		{Key: "head", Title: tr(ctx, "view.col.commit"), Width: 90, Mono: true},
		{Key: "flags", Title: "", Width: 140},
	}
	var rows []sdk.TableRow
	for _, w := range res.Worktrees {
		var flags []string
		if w.Main {
			flags = append(flags, tr(ctx, "view.worktrees.main"))
		}
		if w.Current {
			flags = append(flags, tr(ctx, "view.worktrees.current"))
		}
		if w.Locked {
			flags = append(flags, tr(ctx, "view.worktrees.locked"))
		}
		if w.Prunable {
			flags = append(flags, tr(ctx, "view.worktrees.prunable"))
		}
		branch := w.Branch
		if w.Detached {
			branch = tr(ctx, "view.worktrees.detached")
		}
		row := sdk.TableRow{ID: w.Path, Icon: "builtin:folder",
			Cells: map[string]string{"path": w.Path, "branch": branch, "head": shortHash(w.Head), "flags": strings.Join(flags, " · ")}}
		if !w.Main {
			d := map[string]string{"path": w.Path}
			row.Actions = []sdk.ViewNode{
				{Type: "button", Text: tr(ctx, "view.remove"), Action: "worktree.remove", Style: "danger", Icon: "builtin:trash-2", Data: d, Confirm: tr(ctx, "view.worktrees.confirmRemove", w.Path)},
				{Type: "button", Text: tr(ctx, "view.worktrees.forceRemove"), Action: "worktree.forceRemove", Style: "danger", Data: d, Confirm: tr(ctx, "view.worktrees.confirmForceRemove", w.Path)},
			}
		}
		rows = append(rows, row)
	}
	add := sdk.Form("worktree.add", tr(ctx, "view.create"), []sdk.FormField{
		{Key: "path", Title: tr(ctx, "view.col.path"), Type: "path", Required: true, Info: tr(ctx, "view.worktrees.pathInfo")},
		{Key: "branch", Title: tr(ctx, "view.col.branch"), Type: "string", Placeholder: "feature/x"},
		{Key: "newBranch", Title: tr(ctx, "view.worktrees.newBranch"), Type: "boolean"},
		{Key: "start", Title: tr(ctx, "view.worktrees.start"), Type: "string", Placeholder: "HEAD", Advanced: true},
	}, nil)
	sec := sdk.Section(tr(ctx, "view.worktrees.add"), add)
	return sdk.Column(sdk.Table(cols, rows), sec), nil
}

// ---------------------------------------------------------------------------
// Git 设置

func (r *repo) renderSettings() (sdk.ViewNode, error) {
	ctx := r.ctx
	keys := []string{"user.name", "user.email", "pull.rebase", "fetch.prune", "push.autoSetupRemote", "core.autocrlf", "credential.helper", "merge.tool", "init.defaultBranch"}
	b, _ := json.Marshal(configGetArgs{VCSConfigGetArgs: sdk.VCSConfigGetArgs{Keys: keys}})
	eff, err := r.configGet(b)
	if err != nil {
		return sdk.ViewNode{}, err
	}
	gb, _ := json.Marshal(configGetArgs{VCSConfigGetArgs: sdk.VCSConfigGetArgs{Keys: keys}, Global: true})
	glob, _ := r.configGet(gb)
	if glob == nil {
		glob = &sdk.VCSConfig{Values: map[string]string{}}
	}
	local := map[string]string{}
	for k, v := range eff.Values {
		if s := eff.Scopes[k]; s == "local" || s == "worktree" {
			local[k] = v
		}
	}
	var nodes []sdk.ViewNode
	if eff.Values["user.name"] == "" || eff.Values["user.email"] == "" {
		nodes = append(nodes, sdk.Alert("warning", tr(ctx, "view.settings.identityMissing"), ""))
	}
	nodes = append(nodes, sdk.Section(tr(ctx, "view.settings.globalIdentity"),
		sdk.Form("settings.globalIdentity", tr(ctx, "view.save"), []sdk.FormField{
			{Key: "name", Title: tr(ctx, "view.settings.userName"), Type: "string", Required: true},
			{Key: "email", Title: tr(ctx, "view.settings.userEmail"), Type: "string", Required: true},
		}, map[string]any{"name": glob.Values["user.name"], "email": glob.Values["user.email"]})))
	repoID := sdk.Form("settings.localIdentity", tr(ctx, "view.save"), []sdk.FormField{
		{Key: "name", Title: tr(ctx, "view.settings.userName"), Type: "string", Placeholder: glob.Values["user.name"]},
		{Key: "email", Title: tr(ctx, "view.settings.userEmail"), Type: "string", Placeholder: glob.Values["user.email"]},
	}, map[string]any{"name": local["user.name"], "email": local["user.email"]})
	repoSec := sdk.Section(tr(ctx, "view.settings.repoIdentity"), repoID)
	repoSec.Info = tr(ctx, "view.settings.repoIdentityInfo")
	nodes = append(nodes, repoSec)

	pullRebase := eff.Values["pull.rebase"]
	if pullRebase == "" {
		pullRebase = "unset"
	}
	autocrlf := glob.Values["core.autocrlf"]
	if autocrlf == "" {
		autocrlf = "unset"
	}
	nodes = append(nodes, sdk.Section(tr(ctx, "view.settings.behavior"),
		sdk.Form("settings.behavior", tr(ctx, "view.save"), []sdk.FormField{
			{Key: "pullRebase", Title: tr(ctx, "view.settings.pullRebase"), Type: "select", Options: []sdk.Option{
				{Value: "unset", Label: tr(ctx, "view.settings.unset")}, {Value: "false", Label: tr(ctx, "view.settings.pullMerge")},
				{Value: "true", Label: tr(ctx, "view.settings.pullRebaseOpt")}, {Value: "merges", Label: tr(ctx, "view.settings.pullRebaseMerges")},
			}, Info: tr(ctx, "view.settings.pullRebaseInfo")},
			{Key: "fetchPrune", Title: tr(ctx, "view.settings.fetchPrune"), Type: "boolean"},
			{Key: "autoSetupRemote", Title: tr(ctx, "view.settings.autoSetupRemote"), Type: "boolean"},
			{Key: "autocrlf", Title: tr(ctx, "view.settings.autocrlf"), Type: "select", Options: []sdk.Option{
				{Value: "unset", Label: tr(ctx, "view.settings.unset")}, {Value: "true", Label: "true"}, {Value: "input", Label: "input"}, {Value: "false", Label: "false"},
			}, Info: tr(ctx, "view.settings.autocrlfInfo")},
		}, map[string]any{
			"pullRebase": pullRebase, "fetchPrune": eff.Values["fetch.prune"] == "true",
			"autoSetupRemote": eff.Values["push.autoSetupRemote"] == "true", "autocrlf": autocrlf,
		})))

	tools := detectMergeTools()
	var toolNames []string
	for _, t := range tools {
		toolNames = append(toolNames, t.Name)
	}
	mt := eff.Values["merge.tool"]
	if mt == "" && len(toolNames) > 0 {
		mt = tr(ctx, "view.settings.autoTool", toolNames[0])
	}
	if mt == "" {
		mt = tr(ctx, "view.settings.none")
	}
	nodes = append(nodes, sdk.Section(tr(ctx, "view.settings.about"), sdk.KVList(
		sdk.KV{Key: tr(ctx, "view.settings.gitVersion"), Value: r.ver.raw},
		sdk.KV{Key: tr(ctx, "view.settings.gitPath"), Value: r.git, Mono: true},
		sdk.KV{Key: tr(ctx, "view.settings.repoRoot"), Value: r.root, Mono: true},
		sdk.KV{Key: tr(ctx, "view.settings.credentialHelper"), Value: orDash(eff.Values["credential.helper"])},
		sdk.KV{Key: tr(ctx, "view.settings.mergeTool"), Value: mt},
		sdk.KV{Key: tr(ctx, "view.settings.detectedTools"), Value: orDash(strings.Join(toolNames, ", "))},
	)))
	return sdk.Column(nodes...), nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// ---------------------------------------------------------------------------
// 事件

// HandleViewEvent 实现 sdk.ViewProvider。行按钮的 Data 由界面合并进 Values 传回。
func (p *Plugin) HandleViewEvent(ctx *sdk.Context, e *sdk.ViewEventParams) (*sdk.ViewEventResult, error) {
	state := e.State
	if state == nil {
		state = map[string]any{}
	}
	if e.Event == "selectRoot" {
		if s := str(e.Values, "root"); s != "" {
			state["root"] = s
		}
		return &sdk.ViewEventResult{Refresh: true, State: state}, nil
	}
	r, _, err := p.viewRepo(ctx, e.Project, e.Module, state, e.TaskID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return &sdk.ViewEventResult{Refresh: true, State: state}, nil
	}
	state["root"] = r.root
	res, err := r.handleEvent(e.Event, e.Values, state)
	if err != nil {
		return &sdk.ViewEventResult{Refresh: true, State: state, Message: errText(err)}, nil
	}
	out := &sdk.ViewEventResult{Refresh: true, State: state}
	if res != nil {
		out.Message = res.Message
	}
	return out, nil
}

func str(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func boolean(m map[string]any, k string) bool {
	switch v := m[k].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	}
	return false
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (r *repo) handleEvent(event string, values map[string]any, state map[string]any) (*opResult, error) {
	switch event {
	// 远程
	case "remote.fetchAll":
		return r.fetch(mustJSON(fetchArgs{VCSFetchArgs: sdk.VCSFetchArgs{Prune: settingBool(r.ctx, "fetchPrune", false)}}))
	case "remote.fetch":
		return r.fetch(mustJSON(sdk.VCSFetchArgs{Remote: str(values, "name")}))
	case "remote.edit":
		state["editing"] = str(values, "name")
		return nil, nil
	case "remote.cancel":
		delete(state, "editing")
		return nil, nil
	case "remote.save":
		old, _ := state["editing"].(string)
		res, err := r.remoteSet(mustJSON(remoteSetArgs{VCSRemote: sdk.VCSRemote{Name: str(values, "name"), FetchURL: str(values, "url"), PushURL: str(values, "pushUrl")}, OldName: old}))
		if err == nil && res.OK {
			delete(state, "editing")
		}
		return res, err
	case "remote.remove":
		return r.remoteRemove(mustJSON(sdk.VCSRefArgs{Ref: str(values, "name")}))

	// 标签
	case "tag.create":
		return r.tagCreate(mustJSON(tagCreateArgs{VCSTagCreateArgs: sdk.VCSTagCreateArgs{Name: str(values, "name"), Ref: str(values, "ref"), Message: str(values, "message")}, Push: boolean(values, "push")}))
	case "tag.filter":
		state["filter"] = str(values, "filter")
		return nil, nil
	case "tag.push":
		return r.tagPush(mustJSON(tagPushArgs{Ref: str(values, "name")}))
	case "tag.checkout":
		return r.checkout(mustJSON(sdk.VCSCheckoutArgs{Ref: "refs/tags/" + str(values, "name")}))
	case "tag.delete":
		return r.tagDelete(mustJSON(sdk.VCSRefArgs{Ref: str(values, "name")}))
	case "tag.deleteRemote":
		return r.tagPush(mustJSON(tagPushArgs{Ref: str(values, "name"), Delete: true}))

	// 子模块
	case "submodule.updateAll":
		return r.submoduleUpdate(mustJSON(sdk.VCSSubmoduleUpdateArgs{Init: true, Recursive: true}))
	case "submodule.updateRemote":
		return r.submoduleUpdate(mustJSON(sdk.VCSSubmoduleUpdateArgs{Init: true, Recursive: true, Remote: true}))
	case "submodule.update":
		return r.submoduleUpdate(mustJSON(sdk.VCSSubmoduleUpdateArgs{Paths: []string{str(values, "path")}, Init: true, Recursive: true}))

	// 工作树
	case "worktree.add":
		return r.worktreeAdd(mustJSON(sdk.VCSWorktreeAddArgs{Path: str(values, "path"), Branch: str(values, "branch"), NewBranch: boolean(values, "newBranch"), Start: str(values, "start")}))
	case "worktree.remove":
		return r.worktreeRemove(mustJSON(sdk.VCSWorktreeRemoveArgs{Path: str(values, "path")}))
	case "worktree.forceRemove":
		return r.worktreeRemove(mustJSON(sdk.VCSWorktreeRemoveArgs{Path: str(values, "path"), Force: true}))

	// 设置
	case "settings.globalIdentity", "settings.localIdentity":
		global := event == "settings.globalIdentity"
		for _, kv := range [][2]string{{"user.name", str(values, "name")}, {"user.email", str(values, "email")}} {
			args := sdk.VCSConfigSetArgs{Key: kv[0], Value: kv[1], Global: global, Unset: kv[1] == ""}
			if global && kv[1] == "" {
				continue
			}
			res, err := r.configSet(mustJSON(args))
			if err != nil {
				return nil, err
			}
			if !res.OK {
				return res, nil
			}
		}
		return okResult(tr(r.ctx, "view.settings.saved")), nil
	case "settings.behavior":
		sets := []sdk.VCSConfigSetArgs{}
		if v := str(values, "pullRebase"); v == "unset" || v == "" {
			sets = append(sets, sdk.VCSConfigSetArgs{Key: "pull.rebase", Unset: true})
		} else {
			sets = append(sets, sdk.VCSConfigSetArgs{Key: "pull.rebase", Value: v})
		}
		sets = append(sets, sdk.VCSConfigSetArgs{Key: "fetch.prune", Value: fmt.Sprint(boolean(values, "fetchPrune"))})
		sets = append(sets, sdk.VCSConfigSetArgs{Key: "push.autoSetupRemote", Value: fmt.Sprint(boolean(values, "autoSetupRemote")), Global: true})
		if v := str(values, "autocrlf"); v == "unset" || v == "" {
			sets = append(sets, sdk.VCSConfigSetArgs{Key: "core.autocrlf", Unset: true, Global: true})
		} else {
			sets = append(sets, sdk.VCSConfigSetArgs{Key: "core.autocrlf", Value: v, Global: true})
		}
		for _, s := range sets {
			res, err := r.configSet(mustJSON(s))
			if err != nil {
				return nil, err
			}
			if !res.OK {
				return res, nil
			}
		}
		return okResult(tr(r.ctx, "view.settings.saved")), nil
	}
	return nil, sdk.ErrNotSupported
}
