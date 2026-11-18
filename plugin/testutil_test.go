package git

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// tenv 是一个隔离的测试环境：独立的全局 git 配置、HOME、缓存目录。
type tenv struct {
	t     *testing.T
	p     *Plugin
	ctx   *sdk.Context
	home  string
	clock int64

	fakeDates bool
}

func newEnv(t *testing.T) *tenv {
	t.Helper()
	home := t.TempDir()
	gcfg := filepath.Join(home, "gitconfig")
	cfg := "[user]\n\tname = Tester\n\temail = tester@example.com\n" +
		"[init]\n\tdefaultBranch = main\n" +
		"[core]\n\tautocrlf = false\n" +
		"[protocol \"file\"]\n\tallow = always\n" +
		"[commit]\n\tgpgsign = false\n" +
		"[tag]\n\tgpgsign = false\n"
	if err := os.WriteFile(gcfg, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &Plugin{extraEnv: map[string]string{
		"GIT_CONFIG_GLOBAL":   gcfg,
		"GIT_CONFIG_NOSYSTEM": "1",
		"HOME":                home,
		"XDG_CONFIG_HOME":     home,
	}}
	ctx := &sdk.Context{Context: context.Background(), Init: &sdk.InitializeParams{
		CacheDir: filepath.Join(home, "cache"), Locale: "en", Settings: map[string]any{},
	}}
	return &tenv{t: t, p: p, ctx: ctx, home: home, clock: 1700000000}
}

// git 直接运行 git（用来搭场景），失败即终止测试。
func (e *tenv) git(dir string, args ...string) string {
	e.t.Helper()
	out, err := e.gitErr(dir, args...)
	if err != nil {
		e.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (e *tenv) gitErr(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	overlay := map[string]string{"LC_ALL": "C", "GIT_EDITOR": "true"}
	if e.fakeDates {
		// 提交图测试：每次调用把时间往后推一秒，提交顺序确定
		e.clock++
		date := time.Unix(e.clock, 0).UTC().Format(time.RFC3339)
		overlay["GIT_AUTHOR_DATE"], overlay["GIT_COMMITTER_DATE"] = date, date
	}
	for k, v := range e.p.extraEnv {
		overlay[k] = v
	}
	cmd.Env = buildEnv(overlay)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// repo 新建一个带首个提交的仓库。
func (e *tenv) repo() string {
	e.t.Helper()
	dir := filepath.Join(e.t.TempDir(), "repo")
	e.git("", "init", "-q", dir)
	e.write(dir, "README.md", "hello\n")
	e.git(dir, "add", "-A")
	e.git(dir, "commit", "-q", "-m", "init")
	return dir
}

func (e *tenv) write(dir, rel, content string) {
	e.t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *tenv) read(dir, rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *tenv) commitAll(dir, msg string) string {
	e.t.Helper()
	e.git(dir, "add", "-A")
	e.git(dir, "commit", "-q", "-m", msg)
	return strings.TrimSpace(e.git(dir, "rev-parse", "HEAD"))
}

// call 调用一个 vcs 操作，结果按 JSON 解到 out（out 为 nil 时忽略结果）。
func (e *tenv) call(root, op string, args any, out any) error {
	e.t.Helper()
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			e.t.Fatal(err)
		}
		raw = b
	}
	res, err := e.p.VCS(e.ctx, &sdk.VCSCallParams{Op: op, Root: root, Args: raw})
	if err != nil {
		return err
	}
	if out != nil {
		b, err := json.Marshal(res)
		if err != nil {
			e.t.Fatal(err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("decode %s result: %v\n%s", op, err, b)
		}
	}
	return nil
}

// must 调用并要求没有错误。
func (e *tenv) must(root, op string, args any, out any) {
	e.t.Helper()
	if err := e.call(root, op, args, out); err != nil {
		e.t.Fatalf("%s: %v", op, err)
	}
}

// op 调用一个返回 VCSOpResult 的操作并要求 OK。
func (e *tenv) op(root, op string, args any) opResult {
	e.t.Helper()
	var r opResult
	e.must(root, op, args, &r)
	if !r.OK {
		e.t.Fatalf("%s not ok: %s (%s)\n%s", op, r.Message, r.Reason, r.Output)
	}
	return r
}

// opFail 调用并要求结果 OK=false，返回结果。
func (e *tenv) opFail(root, op string, args any) opResult {
	e.t.Helper()
	var r opResult
	e.must(root, op, args, &r)
	if r.OK {
		e.t.Fatalf("%s unexpectedly ok: %s", op, r.Message)
	}
	return r
}

func reasonOfErr(err error) string {
	if e, ok := err.(*sdk.Error); ok {
		return reasonOf(e)
	}
	return ""
}

func (e *tenv) status(root string) statusResult {
	e.t.Helper()
	var st statusResult
	e.must(root, sdk.VCSOpStatus, nil, &st)
	return st
}

func findFile(st statusResult, path string) *fileChange {
	for i := range st.Files {
		if st.Files[i].Path == path {
			return &st.Files[i]
		}
	}
	return nil
}

func (e *tenv) logMsgs(dir string, args ...string) []string {
	e.t.Helper()
	out := e.git(dir, append([]string{"log", "--format=%s"}, args...)...)
	return strings.Split(strings.TrimSpace(out), "\n")
}
