package opensubtitles

import "sort"

// sortCandidates puts the subtitles that were authored against this exact
// file first, then the ones most people have used.
//
// The split matters more than the ordering inside it. A hash match is a fact
// about this file; a popularity number is a fact about a release somebody
// else had, and treating the two as points on one scale is how a server ends
// up confidently offering a subtitle that drifts a minute out by the end.
func sortCandidates(list []Candidate) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Exact != list[j].Exact {
			return list[i].Exact
		}
		if list[i].Downloads != list[j].Downloads {
			return list[i].Downloads > list[j].Downloads
		}
		return list[i].Rating > list[j].Rating
	})
}
