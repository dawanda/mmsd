package main

import "sort"

type sortedStrStrKeys struct {
	m map[string]string
	s []string
}

func (sm *sortedStrStrKeys) Len() int {
	return len(sm.m)
}

func (sm *sortedStrStrKeys) Less(a, b int) bool {
	// return sm.m[sm.s[a]] > sm.m[sm.s[b]]
	return sm.s[a] < sm.s[b]
}

func (sm *sortedStrStrKeys) Swap(a, b int) {
	sm.s[a], sm.s[b] = sm.s[b], sm.s[a]
}

func SortedStrStrKeys(m map[string]string) []string {
	sm := new(sortedStrStrKeys)
	sm.m = m
	sm.s = make([]string, len(m))

	i := 0
	for key, _ := range m {
		sm.s[i] = key
		i++
	}
	sort.Sort(sm)

	return sm.s
}
