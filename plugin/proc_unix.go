//go:build !windows

package git

import (
	"os/exec"
	"syscall"
	"time"
)

// setProcGroup 让 git 自成进程组，取消时能连同它拉起的 ssh、hook 一起结束。
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// setDetached 让外部工具（合并工具）脱离插件进程，插件闲置退出后它照常运行。
func setDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func setHiddenWindow(cmd *exec.Cmd) {}

// killTree 先 SIGTERM 整个进程组（git 会清理 .lock 文件），2 秒后补 SIGKILL。
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	go func() {
		// 进程号短时间内不会被复用；进程组已经没了时这里只是返回 ESRCH
		time.Sleep(2 * time.Second)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}()
}
