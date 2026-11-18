# lemon-paperplane-plugin-git

Lemon Paperplane（柠檬纸飞机）的 Git 版本控制插件。用系统 `git` 命令行实现 SDK `vcs.go` 里的全部 `VCSOp*` 操作，
并贡献远程、标签、子模块、工作树、Git 设置五个低频页面（`vcs.tab`）与若干动作。变更/提交、差异、日志图、分支这些高频界面
由三端原生界面直接调用 `vcs/call`。

```
plugin.json          清单：contributes.vcs {id:git, rootMarker:.git}、views、actions、settings
icons/git.svg        Git 品牌图标（Simple Icons，CC0）
l10n/                zh-Hans.json / en.json
plugin/              Go 包 git（导出 New() any）
cmd/paperplane-plugin-git/main.go
build.sh             go build -o bin/paperplane-plugin-git
```

## 构建与调试

```bash
./build.sh
go test ./plugin/                       # 场景测试，在临时目录建仓库，隔离全局配置（GIT_CONFIG_GLOBAL）
# 对真实仓库只读跑 status/log/branches/diff/remotes/stashList 并计时，前后比对 .git/index 等文件
PAPERPLANE_GIT_REAL_REPOS=/path/a:/path/b go test ./plugin/ -run TestRealReposReadOnly -v

# SDK 探针
cd ../lemon-paperplane-main/lemon-paperplane-main-plugin-sdk && go build -o /tmp/pp-probe ./cmd/paperplane-plugin-probe
/tmp/pp-probe -plugins ../../lemon-paperplane-plugin-git vcs <仓库> status
/tmp/pp-probe -plugins ../../lemon-paperplane-plugin-git vcs <仓库> commit '{"message":"x","paths":["a.txt"]}'
```

## 公共约定

- 每条命令：`--no-pager`，配置覆盖 `core.quotepath=false color.ui=false core.editor=true log.showSignature=false
  diff.noprefix=false diff.mnemonicPrefix=false advice.detachedHead=false i18n.logOutputEncoding=UTF-8 diff.autoRefreshIndex=false`；
  环境 `LC_ALL=C GIT_TERMINAL_PROMPT=0 GIT_PAGER=cat GIT_EDITOR=true GIT_MERGE_AUTOEDIT=no`，去掉继承来的 `GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE`。
  走任务控制台的命令（git ≥ 2.31）把这些配置改用 `GIT_CONFIG_COUNT` 环境变量传，控制台里显示的就是干净的 `git push origin …`。
- **读操作不写仓库**：带 `--no-optional-locks` 与 `GIT_OPTIONAL_LOCKS=0`，并关掉 `diff.autoRefreshIndex`
  （否则 `git diff` 遇到只改了时间戳的文件会悄悄重写 `.git/index`，且不受 `--no-optional-locks` 约束）。有回归测试 `TestReadOpsDoNotWriteIndex`。
- 用户路径一律加 `:(literal)` 前缀，文件名里的 `* ? [` 不会被当通配。不用全局 `--literal-pathspecs`：它会经环境变量传给钩子里的
  git（lint-staged、git-lfs、`git stash` 内部），改变它们的行为。
- 解析一律用 `-z`、porcelain v2、`%x1f/%x1e` 分隔的自定义格式；路径含空格、中文、换行、引号都正确。
- 网络操作（push / pull / fetch / clone / submoduleUpdate / 删除远程分支 / 推送标签）：输出经 `host/exec` 实时进 `TaskID` 控制台（`--progress`），
  `AskpassPath` 非空时设 `GIT_ASKPASS`、`SSH_ASKPASS`、`SSH_ASKPASS_REQUIRE=force`、`DISPLAY`，需要密码/令牌/私钥口令时界面弹框。
  提交（钩子输出）、检出、合并、变基等也走控制台。没有 `TaskID` 或没有内核连接时在插件进程里直接执行。
- 取消：请求被取消时整个进程组收到 SIGTERM（git 会清理 `.lock`），2 秒后 SIGKILL；Windows 用 `taskkill /T`。
- 找 git：设置 `gitPath` > PATH > 常见安装位置（Homebrew、`C:\Program Files\Git`）。macOS 没装命令行工具时 `/usr/bin/git` 只是桩，
  不去运行它，直接报 `gitNotFound`（提示 `xcode-select --install`）。最低版本 2.23。

## 错误与结果

- 前置条件不满足返回 JSON-RPC 用户错误 `-33002`，`message` 已本地化，`data.reason` 是机器可读原因，界面据此弹对应的框：

  | reason | 含义 |
  |---|---|
  | `gitNotFound` / `gitTooOld` | 没有可用的 git |
  | `identityMissing` | 没设置 user.name / user.email；`data.missing` 列出缺哪个。界面弹框后用 `configSet` 设置 |
  | `nothingToCommit` / `emptyMessage` | 没有可提交的内容 / 信息为空 |
  | `operationInProgress` / `noOperation` | 有进行中的 merge/rebase… / 没有可继续或中止的操作 |
  | `detachedHead` / `noRemote` / `noCommits` / `alreadyPushed` / `notAncestor` / `mergesInRange` / `planMismatch` | 见名知义 |
  | `staleDiff` | 按块操作时文件已变化（或块头对不上），刷新差异重试 |
  | `invalidRef` / `invalidPath` / `invalidBranchName` / `invalidTagName` / `invalidRemoteName` / `invalidConfigKey` / `invalidUrl` | 参数不合法（以 `-` 开头的引用一律拒绝） |
  | `commitFailed` | 提交失败（多半是钩子拒绝），`data.output` 是输出 |

- 写操作返回 `VCSOpResult` 的超集 `{ok, message, output, conflicts, needsAuth, rejected, reason, files, hash, tool}`。
  `output` 已把进度输出的 `\r` 折叠、截到 16 KB。`reason` 取值：`conflicts`、`upToDate`、`fastForward`、`squashed`、`needsAuth`、`hostKey`、
  `rejected`（远端领先，先拉取）、`remoteRejected`（钩子/保护分支拒绝）、`repoNotFound`、`network`、`localChanges`（`files` 是会被覆盖的文件）、
  `untrackedOverwritten`、`unresolvedConflicts`、`emptyCommit`（可 skip）、`notFullyMerged`（可强制删除，`hash` 用于恢复）、`noUpstream`、
  `divergent`、`unrelatedHistories`、`checkedOutElsewhere`、`locked`、`markersRemain`、`applyFailed`、`noChanges`、`autostashConflict`、`failed`。

## 提交的两种模式（`commit`）

| 参数 | 语义 |
|---|---|
| `paths` 非空（IDEA 默认模式，无暂存区） | **只提交这些路径在工作区里的当前内容**：修改、删除、未跟踪的新文件、已暂存的重命名（自动带上旧路径）。用户另外暂存的其它内容不进这次提交，提交后仍保持暂存。实现：`git add` 选中的未跟踪文件 → `git commit --only -- <路径>`；提交失败（如钩子拒绝）会把刚 add 的新文件退回未跟踪。合并/拣选/还原进行中 git 不允许部分提交，这时把选中路径加入暂存区后提交整个暂存区。 |
| `paths` 为空（暂存区模式） | 提交暂存区（等同 `git commit`）；暂存区为空且不是 amend 时报 `nothingToCommit`。扩展 `all: true` 先 `git add -A` 再提交全部变更。 |
| `hunks: {path: [块下标]}`（扩展，部分提交） | 在临时索引（`GIT_INDEX_FILE`）里从 HEAD 出发，加入整文件路径与选中的块（块下标基于 `mode=head` 的 diff，上下文 `hunkContext` 默认 3）后提交，再把这些路径的真实索引项对齐到新提交，剩余改动留在工作区。其它已暂存内容不受影响。 |

其它：`amend`（信息为空时沿用原信息）、`signOff`、`author`（`名字 <邮箱>`）、`noVerify`（扩展，跳过钩子）、`push`（提交后推送，结果放在 `push`）。
提交前检查身份必须显式配置（git 会自动拼 `user@host`，IDEA 也要求显式设置），缺了报 `identityMissing`。

> SDK `vcs.go` 里 `VCSCommitArgs.Paths` 的注释是「为空表示全部变更」，本插件按任务要求与内核 `vcs.commit` 的说明实现为「为空表示提交暂存区」，建议改 SDK 注释。

## 操作一览

括号里是扩展参数/字段（SDK 之外，JSON 兼容的超集）。

| op | 参数 | 语义 |
|---|---|---|
| `detect` | – | Root 是否在 Git 仓库里，返回顶层目录 |
| `status` | (`numstat` 默认 true、`untrackedLimit`、`untracked`: all/normal/no、`paths`) | 分支、上游、ahead/behind、上游已删（`upstreamGone`）、HEAD（`headHash`、`unborn`）、进行中的操作（`operationDetail`：rebase 进度、目标、正在应用的提交，worktree/子模块的 `.git` 文件会跟进去）、文件列表、冲突数、储藏数（直接数引用日志文件）。文件：`status` 是相对 HEAD 的合并状态（M A D R C U ? T），另给 `indexStatus`/`worktreeStatus`（暂存区模式分两栏用）、`conflictType`（UU/AA/DU/UD/AU/UA/DD）、`submodule`；增删行数来自 `git diff HEAD --numstat`，二进制标 `binary`。未跟踪文件超过 `untrackedLimit`（默认 5000，可在设置里改）只列前面的并给 `untrackedTruncated`/`untrackedCount`；数到 10 万直接结束 git，不会卡死。 |
| `diff` | `path, oldPath, commit, context`（`mode`、`staged`、`base`/`head`/`mergeBase`、`parent`、`ignoreWhitespace`、`noLimit`） | 解析成 hunks + 行号。模式：`head` HEAD→工作区（`commit` 为空时默认；还没有提交时对空树；未跟踪文件与 /dev/null 比）、`unstaged` 暂存区→工作区、`staged` HEAD→暂存区（`staged:true` 同义）、`commit` 提交→第 `parent` 个父提交（根提交对空树）、`range` base→head（head 空为工作区；`mergeBase` 用 `base...head`）。`context:-1` 整个文件。超过 1 MB 或 5000 行标 `tooLarge` 不返回行，`noLimit` 解除（上限 32 MB）。返回 `status`（A/D/M/R/C/T）、`oldMode/newMode`、`similarity`、`binary`、每行 `noEol`、`additions/deletions`、`mode`（按块操作要传回）、`language`。 |
| `stage` / `unstage` | `paths` | 整文件暂存（`add -A`，含删除）/ 取消暂存（`reset HEAD --`，没有提交时 `rm --cached`） |
| `stageHunks` / `unstageHunks` / `discardHunks` | `path, hunks, context`（`mode`、`headers`、`oldPath`） | 用同一份 diff 按下标挑块生成补丁：暂存 `apply --cached`、取消 `apply --cached -R`、丢弃 `apply -R`（`--recount --whitespace=nowarn`，上下文 0 时加 `--unidiff-zero`）。补丁按已选块的累计行差重算另一侧起点，跳块、文件首尾、无换行结尾、新文件、删除文件、CRLF 都正确。默认模式：stage/discard 用 `unstaged`，unstage 用 `staged`；界面显示的是默认 `head` diff 时传 `mode:"head"`。`headers`（所选块的 header）用来确认 diff 没过期；没传 mode 且块头对不上 `unstaged` 时会再按 `head` 对一次。 |
| `commit` | 见上 | |
| `push` | `remote, branch, force, setUpstream, tags`（`tagsMode`: all/follow、`noVerify`） | 推当前分支到上游（名字可不同），没有上游时推到同名分支并设置上游（设置 `setUpstreamOnPush`）。`force` 用 `--force-with-lease`（2.30+ 再加 `--force-if-includes`）。`--porcelain` 判断已是最新/被拒。 |
| `pushPreview` | 同 push | 将要推送的提交（新分支时为不在该远程任何分支上的提交，最多 500）、`newBranch`、`behind`（远端领先数，基于上次获取）、`setUpstream` |
| `pull` | `mode`（merge/rebase/ff-only，扩展 `config` 按 git 配置）、`remote, branch, autostash` | 默认取设置 `pullMode`（merge）。没有上游返回 `noUpstream`；冲突返回 `conflicts`；成功报拉取的提交数。 |
| `fetch` | `remote`（空为 `--all`）、`prune`（`tags`） | 报有更新的引用数 |
| `clone` | `url, dest, branch, depth`（`recurseSubmodules`），Root 为空 | 目标目录必须不存在或为空；结果带 `path` |
| `init` | (`initialBranch`) | 在 Root 建仓库 |
| `branches` | – | 本地（上游、ahead/behind、gone、最后提交、`worktree`：在别的工作树检出）、远程（`remote`，跳过 origin/HEAD）、`recent`（HEAD 引用日志里最近检出的 5 个）、`detached` |
| `checkout` | `ref, create, start, force`（`smart`） | 本地分支；远程分支自动建同名跟踪分支（本地已有同名则检出本地的）；标签/提交为分离头。本地修改会被覆盖时返回 `localChanges` + `files`，界面给「强制」或 `smart`（储藏→检出→恢复，恢复冲突时储藏保留） |
| `branchCreate` / `branchRename` / `branchDelete` | SDK 参数 | 删除未合并分支返回 `notFullyMerged`（`hash` 可用于恢复）；`remote:true` 删远程分支（`origin/x` 形式，远程名可带斜杠） |
| `setUpstream` | `branch, upstream` | 空 upstream 取消跟踪；远程分支还没取回时直接写 `branch.<b>.remote/merge` |
| `merge` | `ref/refs, noFF, squash, message`（`ffOnly`、`allowUnrelated`） | 冲突返回 `conflicts`；`upToDate` / `fastForward` / `squashed` |
| `rebase` | `ref`（`onto`） | `--autostash`（与 IDEA 一致）；冲突返回 `conflicts` |
| `abort` / `continue` / (`skip`) | – | 按进行中的操作自动选 rebase/am/merge/cherry-pick/revert；continue 前若还有冲突返回 `unresolvedConflicts`，提交为空返回 `emptyCommit`（可 skip） |
| `cherryPick` / `revert` | `ref/refs`（`recordOrigin`、`noCommit`、`mainline`） | 多个提交按祖先关系排序：拣选从旧到新、还原从新到旧；合并提交自动 `-m 1` |
| `reset` | `ref, mode`（soft/mixed/hard/keep/merge） | |
| `undoCommit` | (`force`) | `reset --soft HEAD~1`，改动保留在工作区并暂存；首个提交用 `update-ref -d HEAD`；已在上游里的提交默认拒绝（`alreadyPushed`），`force` 强制 |
| `reword` | `hash, message` | HEAD 用 `commit --amend --only`（不带上暂存内容，走 commit-msg 钩子）；更早的提交用 `commit-tree` 沿祖先路径重建这段历史再 `update-ref`（保留作者与时间、支持其间的合并提交、不动工作区与暂存区，有本地修改也能做；以 `#` 开头的行保留） |
| `rebaseInteractive` | `base`（空为 `--root`）、`steps` 从旧到新 `{hash, action: pick/reword/squash/fixup/drop, message}` | 计划必须覆盖 base..HEAD 的全部提交、范围内不能有合并提交。`GIT_SEQUENCE_EDITOR` 指向缓存目录里的脚本，把计划写进 todo；`GIT_EDITOR` 脚本按 `rebase-merge/done` 最后一行的提交找对应信息：reword 用该步信息，squash 链用链上最后一个非空 message，否则用 git 默认的合并信息（去掉注释行）。`commit.cleanup=whitespace`，信息原样保留。`--autostash`。冲突停下后 `continue`/`skip` 会继续用同一套信息。Windows 上 Git for Windows 也用自带的 sh 执行编辑器命令，所以脚本统一是 sh。 |
| `checkoutFile` | `path, ref` | `git restore --source=<ref> --staged --worktree`（该版本里没有这个文件则删除，与「从版本获取」一致） |
| `discard` | `paths`（`mode`: head/unstaged、`keepNew`） | 让这些路径回到 HEAD 的样子：已跟踪文件恢复到 HEAD（暂存区一起）；已暂存的新文件、未跟踪文件**删除**（与内核 `vcs.discard` 说明一致，不可撤销，界面要确认）。`keepNew:true` 新文件只取消暂存、未跟踪文件保留（IDEA Rollback 默认）。`mode:"unstaged"` 只丢弃未暂存部分（恢复到暂存区版本）。重命名会连旧路径一起恢复。 |
| `log` | `ref`（空=HEAD，`--all`=HEAD+全部本地/远程分支+标签，不含 stash）、`skip, limit`（默认 200）、`author, grep, path`（`refs`、`authors`、`paths`、`since`、`until`、`order`: topo/date、`firstParent`、`noGraph`） | 关键词/作者按字面量、不区分大小写。提交图：每个提交的 `graph {lane, lanes, edges[{from,to,color}], color}`；分页时从第 0 行连续算到本页末再多看一行，保证跨页连线一致；按作者/关键词/日期过滤时不画图（`graph:false`）；按路径过滤用改写后的父提交（`--parents`）画图。`refs` 用完整引用名区分 head/local/remote/tag。 |
| `show` | `hash`（`parent`） | 提交详情：`body`、完整 `message`、提交者、文件（对第 `parent` 个父提交，带增删行数） |
| `fileHistory` | `path, limit`（`skip`、`ref`、`follow` 默认 true） | 跟踪重命名；每个提交带 `path`（当时的路径）与 `pathStatus` |
| `compare` | `base, head` | `ahead`（在 head 不在 base）、`behind`、`files`（merge-base→head）、`mergeBase`，各最多 1000 个提交 |
| `blame` | `path, ref`（`ignoreWhitespace`、`detectMoves`） | 每行 `hash, author, email, date(RFC3339), summary, text, origLine, filename, uncommitted, boundary`；最多 5 万行 |
| `stashList` / `stashSave` / `stashApply` / `stashDrop` / (`stashShow`) | SDK 参数（`paths`、`keepIndex`、`staged`、`restoreIndex`、`branch`、`all`） | 储藏条目带 `hash`；没东西可储藏返回 `noChanges`；恢复冲突时储藏保留；`branch` 恢复为新分支；`stashShow` 列出文件与其中的未跟踪文件 |
| `resolve` | `path, side`（`paths`、`force`） | ours/theirs 取一方（该方删除了文件就删除），resolved 标记已解决（文件里还有冲突标记时返回 `markersRemain`，`force` 跳过检查）。rebase 时 ours 是变基目标、theirs 是正在重放的提交。 |
| `mergeTool` / (`mergeTools`) | `paths`（空为全部冲突文件） | 后台运行 `git mergetool`（脱离插件进程，插件闲置退出不影响），工具关闭后 git 自动标记解决。工具：设置 `mergeTool` > 用户 `merge.tool` > 检测到的第一个（Kaleidoscope、Beyond Compare、VS Code、Cursor、IntelliJ IDEA、WebStorm、FileMerge(需完整 Xcode)、P4Merge、Meld、KDiff3、WinMerge、TortoiseGitMerge）。`mergeTools` 列出检测结果。 |
| `tags` / `tagCreate` / `tagDelete` / (`tagPush`) | SDK 参数（create：`force`、`push`、`remote`；delete：`remote` 同时删远程；tagPush：`ref, remote, delete, force`） | 有 `message` 建附注标签；列表带 `annotated`、`message`、`tagger`，`hash` 是指向的提交 |
| `remotes` / `remoteSet` / `remoteRemove` | `VCSRemote`（`oldName` 改名） | 列表带 `fetch` 规格与 `webUrl` |
| `submodules` / `submoduleUpdate` | SDK 参数 | 状态 ok/uninitialized/modified/conflict，另给 `name`、`branch`、`describe` |
| `worktrees` / `worktreeAdd` / `worktreeRemove` | SDK 参数 | 相对路径相对仓库根；`branch` 是远程分支时建同名跟踪分支；只有 `start` 时分离头；有修改的工作树删除返回 `localChanges`，`force` 强删 |
| `configGet` / `configSet` | `keys`（`global` 只读全局）/ `key, value, global, unset` | 生效值 + 作用域（local/global/system/worktree）；键的节名与变量名不区分大小写 |
| `ignore` | `patterns, file` | 默认根 `.gitignore`；`exclude` 写 `info/exclude`（worktree 里写公共目录）；也可指定子目录的 `.gitignore`。去重、补换行、保留 CRLF 风格 |
| `clean` | `paths`（`all`、`ignored`） | `git clean -f -d`；`paths` 为空必须 `all:true` |
| `repos` | `maxDepth`（默认 6） | 只看文件系统：嵌套仓库、子模块（`kind`）、工作树、`parent`；跳过 node_modules 等 |
| (`fileContent`) | `path, ref`（空=工作区，`:`=暂存区）、`maxBytes` | 文本给 `text`；二进制（图片对比）与非 UTF-8 文本给 `base64` |
| (`containingBranches`) | `hash` | 包含该提交的分支与标签 |

## 界面（`vcs.tab`）

`ViewRenderParams` 没有仓库根，视图按 `state.root` > 模块目录所在仓库 > 工程目录所在仓库 > 工程目录下找到的第一个仓库确定；工程里有多个仓库时顶部给一个选择框。
行按钮的 `data`（如 `{"name":"origin"}`）约定由界面合并进事件的 `values`。

- 远程：列表（网页链接、获取、编辑、删除）+ 添加/编辑表单（名称、地址、推送地址）+ 全部获取。
- 标签：新建（名称、提交、说明→附注标签、创建后推送）、按名称筛选、列表（推送、检出、删除、删除远程），最多显示 500 个。
- 子模块：状态列表、初始化并更新全部、更新到远程最新、单个更新。
- 工作树：列表（主/当前/锁定/目录已不存在）、删除/强制删除、添加（路径、分支、新建分支、起点）。
- Git 设置：全局身份、本仓库身份（留空用全局）、行为（pull.rebase、fetch.prune、push.autoSetupRemote、core.autocrlf）、环境信息（git 版本与路径、凭据助手、合并工具）。

## 动作

上下文参数约定（界面放进 `Args`）：`root` 仓库根；`vcs.commitMenu` 给 `hash`/`hashes`；`vcs.fileMenu` 给 `path`/`paths`。

| id | 位置 | 作用 |
|---|---|---|
| `openRepoInBrowser` | vcs.toolbar, project.menu | 由 origin 地址算网页地址（GitHub/GitLab/Gitee/Bitbucket/Azure DevOps/自建 Gitea 等，ssh/https 都认） |
| `openCommitInBrowser` | vcs.commitMenu | 提交网页 |
| `openFileInBrowser` | vcs.fileMenu | 文件网页（已推送用上游分支名，否则用提交哈希） |
| `createPatchFromCommits` | vcs.commitMenu | `git format-patch`，多个提交按顺序拼进一个文件（表单：保存到） |
| `createPatchFromChanges` | vcs.fileMenu | 选中文件相对 HEAD 的修改（含未跟踪新文件、二进制）导出补丁 |
| `cleanupBranches` | vcs.toolbar | 删除已合并到当前分支或上游已删除的本地分支（`-d`，未合并的跳过并报告；不动当前分支、main/master/develop 等、别的工作树检出的分支） |
| `optimizeRepository` | vcs.toolbar | `git gc` + `git commit-graph write --reachable --changed-paths`（日志、按路径的历史更快） |

## 设置（manifest settings）

`pullMode`（merge/rebase/ff-only/config）、`autoFetchMinutes`（内核侧定时获取，0 关闭）、`fetchPrune`、`setUpstreamOnPush`、`mergeTool`、`gitPath`、`untrackedLimit`。
