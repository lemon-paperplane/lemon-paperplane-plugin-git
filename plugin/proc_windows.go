//go:build windows

package git

import (
	"os/exec"
	"strconv"
	"syscall"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
	createNoWindow        = 0x08000000
)

// setProcGroup：新进程组 + 不弹控制台窗口（插件进程没有控制台，否则每条 git 命令都会闪一个黑框）。
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | createNoWindow, HideWindow: true}
}

// setDetached 让合并工具脱离插件进程。
func setDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}

func setHiddenWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow, HideWindow: true}
}

// killTree 用 taskkill /T 结束整棵进程树（git 会拉起 ssh、git-remote-https、hook）。
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	setHiddenWindow(kill)
	if kill.Run() != nil {
		_ = cmd.Process.Kill()
	}
}
