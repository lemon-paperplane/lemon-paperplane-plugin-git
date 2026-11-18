package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// 输出上限：普通命令的标准输出最多读 64 MB，标准错误最多 1 MB。
const (
	defaultMaxOutput = 64 << 20
	maxStderr        = 1 << 20
	maxResultOutput  = 16 << 10 // VCSOpResult.Output 截断长度
)

// 最低支持的 git 版本：porcelain v2、--no-optional-locks、git restore、for-each-ref %(worktreepath) 都需要 2.23。
const minGitMajor, minGitMinor = 2, 23

type gitVersion struct {
	major, minor, patch int
	raw                 string
}

func (v gitVersion) atLeast(major, minor int) bool {
	return v.major > major || (v.major == major && v.minor >= minor)
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

func parseGitVersion(s string) (gitVersion, bool) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return gitVersion{}, false
	}
	v := gitVersion{raw: strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "git version "))}
	v.major, _ = strconv.Atoi(m[1])
	v.minor, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		v.patch, _ = strconv.Atoi(m[3])
	}
	return v, true
}

// resolveGit 找到可用的 git 并检查版本；结果按「自定义路径」缓存在插件实例里。
func (p *Plugin) resolveGit(ctx *sdk.Context) (string, gitVersion, error) {
	custom := strings.TrimSpace(settingString(ctx, "gitPath", ""))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gitResolved && p.gitFor == custom {
		return p.gitPath, p.gitVer, p.gitErr
	}
	path, ver, err := locateGit(ctx, custom)
	if err != nil {
		// 找不到 git 时不缓存，用户装好后下一次请求就能用
		return "", gitVersion{}, err
	}
	p.gitResolved, p.gitFor, p.gitPath, p.gitVer, p.gitErr = true, custom, path, ver, nil
	return path, ver, nil
}

func locateGit(ctx *sdk.Context, custom string) (string, gitVersion, error) {
	var candidates []string
	if custom != "" {
		candidates = []string{sdk.Expand(custom)}
	} else {
		if lp, err := exec.LookPath("git"); err == nil {
			candidates = append(candidates, lp)
		}
		switch runtime.GOOS {
		case "darwin":
			candidates = append(candidates, "/opt/homebrew/bin/git", "/usr/local/bin/git", "/usr/bin/git")
		case "windows":
			for _, env := range []string{"ProgramW6432", "ProgramFiles", "ProgramFiles(x86)"} {
				if d := os.Getenv(env); d != "" {
					candidates = append(candidates, filepath.Join(d, "Git", "cmd", "git.exe"))
				}
			}
			if d := os.Getenv("LOCALAPPDATA"); d != "" {
				candidates = append(candidates, filepath.Join(d, "Programs", "Git", "cmd", "git.exe"))
			}
		default:
			candidates = append(candidates, "/usr/bin/git", "/usr/local/bin/git")
		}
	}
	seen := map[string]bool{}
	sawStub := false
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		st, err := os.Stat(c)
		if err != nil || st.IsDir() {
			continue
		}
		// macOS 未装命令行工具时 /usr/bin/git 只是个桩，一运行就弹安装框
		if runtime.GOOS == "darwin" && c == "/usr/bin/git" && !macCLTInstalled() {
			sawStub = true
			continue
		}
		vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cmd := exec.CommandContext(vctx, c, "--version")
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		setHiddenWindow(cmd)
		out, err := cmd.Output()
		cancel()
		if err != nil {
			continue
		}
		v, ok := parseGitVersion(string(out))
		if !ok {
			continue
		}
		if !v.atLeast(minGitMajor, minGitMinor) {
			return "", v, userError(ctx, "gitTooOld", "error.gitTooOld", v.raw, strconv.Itoa(minGitMajor)+"."+strconv.Itoa(minGitMinor))
		}
		return c, v, nil
	}
	if custom != "" {
		return "", gitVersion{}, userError(ctx, "gitNotFound", "error.gitPathInvalid", custom)
	}
	if sawStub {
		return "", gitVersion{}, userError(ctx, "gitNotFound", "error.gitNeedsCLT")
	}
	return "", gitVersion{}, userError(ctx, "gitNotFound", "error.gitNotFound")
}

// macCLTInstalled 判断 macOS 是否装了 Xcode 或命令行工具（不会触发安装弹框）。
func macCLTInstalled() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/xcode-select", "-p").Output()
	if err != nil {
		return false
	}
	dir := strings.TrimSpace(string(out))
	return dir != "" && sdk.FileExists(filepath.Join(dir, "usr", "bin", "git"))
}

// ---------------------------------------------------------------------------
// 命令执行

// cmdOpts 描述一次 git 调用。
type cmdOpts struct {
	read    bool              // 只读命令：加 --no-optional-locks，不和 IDEA 抢 index.lock
	stdin   string            // 标准输入
	env     map[string]string // 额外环境变量
	config  []string          // 额外 -c key=value
	console bool              // 输出实时进任务控制台（TaskID 非空且有内核连接时走 host/exec）
	remote  bool              // 网络操作：设置 askpass
	title   string            // 控制台步骤名
	maxOut  int               // 标准输出上限，0 表示默认
	dir     string            // 工作目录，默认仓库根
	timeout time.Duration
}

// result 是一次 git 调用的结果。非零退出不算 error，由调用方看 code 判断。
type result struct {
	out       string
	errOut    string
	code      int
	truncated bool // 标准输出被截断
}

func (r *result) ok() bool { return r != nil && r.code == 0 }

// combined 返回标准错误 + 标准输出，供错误识别与展示。
func (r *result) combined() string {
	if r == nil {
		return ""
	}
	s := strings.TrimSpace(r.errOut)
	o := strings.TrimSpace(r.out)
	if s != "" && o != "" {
		return s + "\n" + o
	}
	return s + o
}

// commonConfig 是每条 git 命令都带的配置覆盖：输出可解析、不弹编辑器、不受用户配置影响格式。
var commonConfig = []string{
	"core.quotepath=false",
	"color.ui=false",
	"core.editor=true",
	"log.showSignature=false",
	"diff.noprefix=false",
	"diff.mnemonicPrefix=false",
	"advice.detachedHead=false",
	"i18n.logOutputEncoding=UTF-8",
	// git diff 默认会悄悄刷新 .git/index 的 stat 缓存（不受 --no-optional-locks 约束），关掉以保证读操作不写仓库
	"diff.autoRefreshIndex=false",
}

// globalArgs 是每条 git 命令前面的公共参数。
func (r *repo) globalArgs(o cmdOpts) []string {
	a := []string{"--no-pager"}
	for _, c := range append(append([]string{}, commonConfig...), o.config...) {
		a = append(a, "-c", c)
	}
	if o.read {
		a = append(a, "--no-optional-locks")
	}
	return a
}

// configEnv 把配置覆盖改用 GIT_CONFIG_COUNT 环境变量传（git 2.31+），任务控制台里显示的命令行就是干净的
// 「git push origin main」而不是一长串 -c。效果与 -c 相同（同样会传给钩子里的 git）。
func (r *repo) configEnv(o cmdOpts, env map[string]string) bool {
	if !r.ver.atLeast(2, 31) {
		return false
	}
	all := append(append([]string{}, commonConfig...), o.config...)
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(len(all))
	for i, c := range all {
		k, v, _ := strings.Cut(c, "=")
		env["GIT_CONFIG_KEY_"+strconv.Itoa(i)] = k
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] = v
	}
	return true
}

// envOverlay 是覆盖在进程环境之上的变量。
func (r *repo) envOverlay(o cmdOpts) map[string]string {
	m := map[string]string{
		"LC_ALL":              "C",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_PAGER":           "cat",
		"PAGER":               "cat",
		"GIT_EDITOR":          "true",
		"GIT_MERGE_AUTOEDIT":  "no",
	}
	if o.read {
		m["GIT_OPTIONAL_LOCKS"] = "0"
	}
	if o.remote {
		if ap := r.askpass(); ap != "" {
			m["GIT_ASKPASS"] = ap
			m["SSH_ASKPASS"] = ap
			m["SSH_ASKPASS_REQUIRE"] = "force"
			if os.Getenv("DISPLAY") == "" {
				m["DISPLAY"] = ":0"
			}
		}
	}
	for k, v := range r.p.extraEnv {
		m[k] = v
	}
	for k, v := range o.env {
		m[k] = v
	}
	return m
}

func (r *repo) askpass() string {
	if r.ctx != nil && r.ctx.Init != nil {
		return r.ctx.Init.AskpassPath
	}
	return ""
}

// 继承环境里这些变量会把 git 指到别的仓库，必须去掉。
var strippedEnv = map[string]bool{
	"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_PREFIX": true,
	"GIT_COMMON_DIR": true, "GIT_OBJECT_DIRECTORY": true, "GIT_NAMESPACE": true,
	"GIT_CEILING_DIRECTORIES": false,
}

func buildEnv(overlay map[string]string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(overlay))
	for _, kv := range base {
		k := kv
		if i := strings.IndexByte(kv, '='); i > 0 {
			k = kv[:i]
		}
		if runtime.GOOS == "windows" {
			k = strings.ToUpper(k)
		}
		if strippedEnv[k] {
			continue
		}
		if _, ok := overlay[k]; ok {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overlay {
		out = append(out, k+"="+v)
	}
	return out
}

// run 执行 git 并收集输出。
func (r *repo) run(o cmdOpts, args ...string) (*result, error) {
	return r.exec(o, args, nil)
}

// stream 执行 git，把标准输出交给 consume 逐步读取；consume 提前返回时终止 git。
func (r *repo) stream(o cmdOpts, args []string, consume func(io.Reader) error) (*result, error) {
	return r.exec(o, args, consume)
}

// errStop 让 consume 表示「够了，不用再读」。
var errStop = errors.New("stop")

func (r *repo) exec(o cmdOpts, args []string, consume func(io.Reader) error) (*result, error) {
	dir := o.dir
	if dir == "" {
		dir = r.root
	}
	env := r.envOverlay(o)
	if o.console && r.task != "" && r.ctx.Host != nil {
		full := append(r.globalArgs(o), args...)
		if r.configEnv(o, env) {
			full = args
			if o.read {
				full = append([]string{"--no-optional-locks"}, args...)
			}
		}
		return r.execHost(o, full, dir, env, consume)
	}
	full := append(r.globalArgs(o), args...)
	ctx := context.Context(r.ctx)
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}
	cmd := exec.Command(r.git, full...)
	cmd.Dir = dir
	cmd.Env = buildEnv(env)
	setProcGroup(cmd)
	if o.stdin != "" {
		cmd.Stdin = strings.NewReader(o.stdin)
	}
	errBuf := &capBuffer{max: maxStderr}
	cmd.Stderr = errBuf
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, userError(r.ctx, "gitNotFound", "error.gitStart", err.Error())
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killTree(cmd)
		case <-done:
		}
	}()
	res := &result{}
	max := o.maxOut
	if max <= 0 {
		max = defaultMaxOutput
	}
	if consume != nil {
		cerr := consume(pipe)
		if cerr != nil {
			// 读够了或解析出错：结束 git，丢弃剩余输出
			res.truncated = errors.Is(cerr, errStop)
			killTree(cmd)
			_, _ = io.Copy(io.Discard, pipe)
			if !errors.Is(cerr, errStop) {
				_ = cmd.Wait()
				close(done)
				return nil, cerr
			}
		}
		_, _ = io.Copy(io.Discard, pipe)
	} else {
		var out bytes.Buffer
		n, _ := io.Copy(&out, io.LimitReader(pipe, int64(max)))
		if n >= int64(max) {
			res.truncated = true
			killTree(cmd)
			_, _ = io.Copy(io.Discard, pipe)
		}
		res.out = out.String()
	}
	werr := cmd.Wait()
	close(done)
	if ctx.Err() != nil {
		if r.ctx.Err() != nil {
			return nil, sdk.NewError(sdk.ErrCodeCancelled, "cancelled")
		}
		return nil, userError(r.ctx, "timeout", "error.timeout")
	}
	res.errOut = errBuf.String()
	res.code = exitCode(werr)
	if res.truncated && consume != nil {
		res.code = 0
	}
	return res, nil
}

// execHost 通过内核 host/exec 执行，输出实时进 TaskID 控制台。
func (r *repo) execHost(o cmdOpts, full []string, dir string, env map[string]string, consume func(io.Reader) error) (*result, error) {
	er, err := r.ctx.Host.Exec(r.ctx, &sdk.ExecParams{
		Command:   r.git,
		Args:      full,
		Cwd:       dir,
		Env:       env,
		Console:   r.task,
		Capture:   true,
		Stdin:     o.stdin,
		Title:     o.title,
		TimeoutMs: int(o.timeout / time.Millisecond),
	})
	if err != nil {
		return nil, err
	}
	if er.Cancelled {
		return nil, sdk.NewError(sdk.ErrCodeCancelled, "cancelled")
	}
	if er.TimedOut {
		return nil, userError(r.ctx, "timeout", "error.timeout")
	}
	res := &result{out: er.Stdout, errOut: er.Stderr, code: er.ExitCode}
	if consume != nil {
		if cerr := consume(strings.NewReader(er.Stdout)); cerr != nil && !errors.Is(cerr, errStop) {
			return nil, cerr
		}
	}
	return res, nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.ExitCode() >= 0 {
			return ee.ExitCode()
		}
		return -1
	}
	return -1
}

// capBuffer 是有上限的缓冲区，超出部分丢弃（保留开头）。
type capBuffer struct {
	b   bytes.Buffer
	max int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room > 0 {
		if len(p) > room {
			c.b.Write(p[:room])
		} else {
			c.b.Write(p)
		}
	}
	return len(p), nil
}

func (c *capBuffer) String() string { return c.b.String() }

// ---------------------------------------------------------------------------
// 便捷封装

// out 执行只读命令，非零退出返回错误。
func (r *repo) out(args ...string) (string, error) {
	res, err := r.run(cmdOpts{read: true}, args...)
	if err != nil {
		return "", err
	}
	if !res.ok() {
		return "", r.gitFailure(res)
	}
	return res.out, nil
}

// do 执行写命令，非零退出返回错误。
func (r *repo) do(o cmdOpts, args ...string) (*result, error) {
	res, err := r.run(o, args...)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return res, r.gitFailure(res)
	}
	return res, nil
}

// gitFailure 把失败的 git 输出转成给用户看的错误。
func (r *repo) gitFailure(res *result) error {
	text := res.combined()
	reason := classifyFailure(text)
	switch reason {
	case "identityMissing":
		return r.identityError(nil)
	case "notRepo":
		return userError(r.ctx, "notRepo", "error.notRepo", r.root)
	}
	return userErrorData(r.ctx, reason, map[string]any{"output": truncateOutput(text)}, "error.gitFailed", firstErrorLine(text))
}

// firstErrorLine 取最有信息量的一行（优先 fatal:/error:）。
func firstErrorLine(s string) string {
	lines := splitLines(s)
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "fatal:") || strings.HasPrefix(t, "error:") {
			t = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(t, "fatal:"), "error:"))
			return t
		}
	}
	for _, l := range lines {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "hint:") {
			return t
		}
	}
	return strings.TrimSpace(s)
}

func truncateOutput(s string) string {
	s = strings.TrimSpace(collapseProgress(s))
	if len(s) > maxResultOutput {
		// 保留结尾，结尾通常是错误原因
		cut := len(s) - maxResultOutput
		for cut < len(s) && (s[cut]&0xC0) == 0x80 {
			cut++
		}
		return "…\n" + s[cut:]
	}
	return s
}

// collapseProgress 模拟终端处理 \r：每行只留最后一次覆盖的内容（git 的进度输出用 \r 刷新同一行）。
func collapseProgress(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		if j := strings.LastIndexByte(l, '\r'); j >= 0 {
			l = l[j+1:]
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

// splitLines 按 \n 和 \r 切行（进度输出用 \r 覆盖同一行）。
func splitLines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		for i, c := range data {
			if c == '\n' || c == '\r' {
				return i + 1, data[:i], nil
			}
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}
