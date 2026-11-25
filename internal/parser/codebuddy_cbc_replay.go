package parser

import (
	"slices"
	"sort"
)

// codebuddyCBCReplay returns the indexes of the producer's active history,
// following the CodeBuddy CLI's resolveReplayHistory with custom items
// excluded from the returned path, plus the standalone model-usage items
// whose parent is on it.
func codebuddyCBCReplay(items []codebuddyCBCItem) []int {
	return codebuddyWithUsageItems(items, codebuddyReplayHistory(items))
}

func codebuddyReplayHistory(items []codebuddyCBCItem) []int {
	hasCompacted := false
	hasParent := false
	hasFilter := false
	hasResend := false
	for _, it := range items {
		if it.logicalParentID != "" && it.parentID == "" {
			hasCompacted = true
		}
		if it.parentID != "" {
			hasParent = true
		}
		if it.providerData().Get("contentFilterNotice").Bool() {
			hasFilter = true
		}
		if it.itemType == "resend-fork-notice" {
			hasResend = true
		}
	}
	// A trailing resend notice means the user rewound and has not sent the
	// replacement prompt yet; the history ends at the notice's parent.
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.itemType == "resend-fork-notice" {
			if it.parentID == "" {
				return nil
			}
			return codebuddyActiveHistory(items, it.parentID, hasCompacted)
		}
		if !it.custom() {
			break
		}
	}
	if !hasParent || (!hasFilter && !hasResend) {
		out := make([]int, 0, len(items))
		for i, it := range items {
			if !it.custom() {
				out = append(out, i)
			}
		}
		return out
	}
	return codebuddyActiveHistory(
		items, codebuddyColdStartTarget(items), hasCompacted,
	)
}

// codebuddyWithUsageItems adds the model-usage items whose parent is on path
// or that have no parent. The producer leaves them out of the replayed
// history, but each records a billed response, so it belongs to the path its
// parent is on.
func codebuddyWithUsageItems(items []codebuddyCBCItem, path []int) []int {
	onPath := make(map[string]bool, len(path))
	for _, idx := range path {
		if items[idx].id != "" {
			onPath[items[idx].id] = true
		}
	}
	out := slices.Clone(path)
	for i, it := range items {
		if it.itemType == "model-usage" && (it.parentID == "" || onPath[it.parentID]) {
			out = append(out, i)
		}
	}
	if len(out) == len(path) {
		return path
	}
	sort.Ints(out)
	return out
}

func codebuddyColdStartTarget(items []codebuddyCBCItem) string {
	notice := -1
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].itemType == "resend-fork-notice" {
			notice = i
			break
		}
	}
	if notice >= 0 {
		for i := len(items) - 1; i > notice; i-- {
			if items[i].id != "" && !items[i].custom() {
				return items[i].id
			}
		}
		return items[notice].parentID
	}
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].id != "" && !items[i].custom() {
			return items[i].id
		}
	}
	return ""
}

// codebuddyActiveHistory walks parent links backward from target exactly as
// the CLI's getActiveHistory does, including adjacent items that repeat an
// id and the logical-parent fallback for compacted history.
func codebuddyActiveHistory(
	items []codebuddyCBCItem, target string, hasCompacted bool,
) []int {
	if target == "" {
		return nil
	}
	var picked []int
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].id != target {
			continue
		}
		picked = append(picked, i)
		for i-1 >= 0 && items[i-1].id == target {
			i--
			picked = append(picked, i)
		}
		it := items[i]
		compact := it.providerData().Get("agent").Str == "compact"
		next := it.parentID
		if next == "" && (hasCompacted || i == len(items)-1 || compact) {
			next = it.logicalParentID
		}
		target = next
		if target == "" {
			break
		}
	}
	slices.Reverse(picked)
	out := picked[:0]
	for _, idx := range picked {
		if !items[idx].custom() {
			out = append(out, idx)
		}
	}
	return out
}

// codebuddyCBCBranch is one preserved alternative path. parentIx is the
// index of the active-path item it forked from, or -1 when unknown.
type codebuddyCBCBranch struct {
	indexes  []int
	parentIx int
}

// codebuddyCBCAlternatives groups conversation items outside the active
// path into connected alternative branches. A branch starts at an item whose
// parent is not itself an alternative item.
func codebuddyCBCAlternatives(
	items []codebuddyCBCItem, active []int,
) []codebuddyCBCBranch {
	onPath := make(map[int]bool, len(active))
	for _, idx := range active {
		onPath[idx] = true
	}
	byID := make(map[string]int, len(items))
	for i, it := range items {
		if it.id == "" {
			continue
		}
		if _, exists := byID[it.id]; !exists {
			byID[it.id] = i
		}
	}
	var off []int
	offSet := make(map[int]bool)
	for i, it := range items {
		if (it.custom() && it.itemType != "model-usage") || onPath[i] {
			continue
		}
		if it.providerData().Get("contentFilterNotice").Bool() {
			// The notice replaced a filtered turn; it is not a user branch.
			continue
		}
		off = append(off, i)
		offSet[i] = true
	}
	if len(off) == 0 {
		return nil
	}
	branchOf := make(map[int]int)
	var branches []codebuddyCBCBranch
	for _, idx := range off {
		it := items[idx]
		parent := it.parentID
		if parent == "" {
			parent = it.logicalParentID
		}
		parentIdx, hasParent := byID[parent]
		if hasParent && offSet[parentIdx] {
			if b, ok := branchOf[parentIdx]; ok {
				branches[b].indexes = append(branches[b].indexes, idx)
				branchOf[idx] = b
				continue
			}
		}
		parentIx := -1
		if hasParent && onPath[parentIdx] {
			parentIx = parentIdx
		}
		branches = append(branches, codebuddyCBCBranch{
			indexes: []int{idx}, parentIx: parentIx,
		})
		branchOf[idx] = len(branches) - 1
	}
	return branches
}
