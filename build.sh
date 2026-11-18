#!/bin/sh
# 构建 git 插件可执行文件到 bin/
set -e
cd "$(dirname "$0")"
go build -trimpath -ldflags "-s -w" -o bin/paperplane-plugin-git ./cmd/paperplane-plugin-git
