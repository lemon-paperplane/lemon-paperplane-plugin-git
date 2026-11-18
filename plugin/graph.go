package git

import (
	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// 提交图（三端界面直接画，不各自实现图算法）。
//
// 模型：每行有若干列（lane），每列是一条「等待某个提交」的线。第 i 行：
//   - 本提交落在等待它的那一列；没有列在等它（分支末端）就在最右新开一列。
//   - 本提交那一列改为等待第一父提交；其余父提交（合并）优先接到已在等待它的列，否则占用空位或在右侧新开一列（新颜色）。
//   - 往下一行之前做两件事：若有多列都在等「下一行的提交」，只留最左一列，其余列的线在这一行斜着汇入它；
//     然后去掉空列（右侧的列整体左移，连线是斜的）。
//   - Edges 记录本行每条线到下一行的列号：穿过本行的线、本提交到各父提交的线。
//
// 颜色是递增的序号（新开一列就分配一个），界面按调色板取模。
type graphLane struct {
	hash  string
	color int
}

func computeGraph(commits []commitInfo) {
	var lanes []graphLane
	nextColor := 0
	newColor := func() int {
		c := nextColor
		nextColor++
		return c
	}
	for i := range commits {
		c := &commits[i]
		node := -1
		for j, l := range lanes {
			if l.hash == c.Hash {
				node = j
				break
			}
		}
		cols := append([]graphLane(nil), lanes...)
		if node < 0 {
			node = len(cols)
			cols = append(cols, graphLane{hash: c.Hash, color: newColor()})
		}
		nodeColor := cols[node].color

		// 下一行的槽位
		slots := append([]graphLane(nil), cols...)
		parentSlot := make([]int, len(c.Parents))
		parentColor := make([]int, len(c.Parents))
		if len(c.Parents) > 0 {
			slots[node] = graphLane{hash: c.Parents[0], color: nodeColor}
			parentSlot[0], parentColor[0] = node, nodeColor
		} else {
			slots[node] = graphLane{}
		}
		for k := 1; k < len(c.Parents); k++ {
			p := c.Parents[k]
			found := -1
			for j, l := range slots {
				if l.hash == p {
					found = j
					break
				}
			}
			if found < 0 {
				col := newColor()
				for j, l := range slots {
					if l.hash == "" && j != node {
						found = j
						break
					}
				}
				if found < 0 {
					slots = append(slots, graphLane{hash: p, color: col})
					found = len(slots) - 1
				} else {
					slots[found] = graphLane{hash: p, color: col}
				}
			}
			parentSlot[k], parentColor[k] = found, slots[found].color
		}

		// 多列等同一个「下一行的提交」：保留最左一列，其余汇入
		redirect := map[int]int{}
		if i+1 < len(commits) {
			nh := commits[i+1].Hash
			canon := -1
			for j, l := range slots {
				if l.hash != nh {
					continue
				}
				if canon < 0 {
					canon = j
				} else {
					redirect[j] = canon
					slots[j] = graphLane{}
				}
			}
		}

		// 去空列
		mapping := make([]int, len(slots))
		var next []graphLane
		for j, l := range slots {
			if l.hash == "" {
				mapping[j] = -1
				continue
			}
			mapping[j] = len(next)
			next = append(next, l)
		}
		resolve := func(j int) int {
			if t, ok := redirect[j]; ok {
				j = t
			}
			return mapping[j]
		}

		var edges []sdk.VCSGraphEdge
		for j, l := range cols {
			if j == node || l.hash == "" {
				continue
			}
			if to := resolve(j); to >= 0 {
				edges = append(edges, sdk.VCSGraphEdge{From: j, To: to, Color: l.color})
			}
		}
		for k := range c.Parents {
			if to := resolve(parentSlot[k]); to >= 0 {
				col := nodeColor
				if k > 0 {
					col = parentColor[k]
				}
				edges = append(edges, sdk.VCSGraphEdge{From: node, To: to, Color: col})
			}
		}
		c.Graph = &sdk.VCSGraphRow{Lane: node, Lanes: len(cols), Edges: edges, Color: nodeColor}
		lanes = next
	}
}
