package enginedb

import (
	"sort"
)

type nativeChildNode struct {
	id    int64
	title string
	next  int64
	owned bool
}

func orderNativeChildren(nodes []nativeChildNode) []nativeChildNode {
	byID := make(map[int64]nativeChildNode, len(nodes))
	incoming := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		byID[n.id] = n
	}
	for _, n := range nodes {
		if _, ok := byID[n.next]; ok {
			incoming[n.next] = true
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].id < nodes[j].id })
	out := make([]nativeChildNode, 0, len(nodes))
	seen := make(map[int64]bool, len(nodes))
	walk := func(id int64) {
		for !seen[id] {
			n, ok := byID[id]
			if !ok {
				break
			}
			seen[id] = true
			if !n.owned {
				out = append(out, n)
			}
			id = n.next
		}
	}
	for _, n := range nodes {
		if !incoming[n.id] {
			walk(n.id)
		}
	}
	for _, n := range nodes {
		walk(n.id)
	}
	return out
}
