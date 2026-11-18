// paperplane-plugin-git 是 Lemon Paperplane 的 Git 版本控制插件进程入口。
package main

import (
	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"

	git "github.com/lemon-paperplane/lemon-paperplane-plugin-git/plugin"
)

func main() { sdk.Serve(git.New()) }
