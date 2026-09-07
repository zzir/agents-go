package memory

import "sort"

// sortInfos orders a listing by key, so a store's map order never shows.
func sortInfos(infos []Info) {
	sort.Slice(infos, func(i, j int) bool { return infos[i].Key < infos[j].Key })
}
