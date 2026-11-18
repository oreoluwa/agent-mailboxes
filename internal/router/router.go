package router

import (
	"sort"
	"strings"
)

type Alias struct {
	ID      string
	Email   string
	Enabled bool
}

func ResolveAudience(recipients []string, _ string, aliases []Alias, catchallAdmin bool) []string {
	matched := map[string]bool{}
	aliasByEmail := map[string]Alias{}
	for _, a := range aliases {
		if !a.Enabled {
			continue
		}
		em := strings.ToLower(strings.TrimSpace(a.Email))
		if em == "" {
			continue
		}
		aliasByEmail[em] = a
	}
	for _, r := range recipients {
		if a, ok := aliasByEmail[strings.ToLower(strings.TrimSpace(r))]; ok {
			matched[a.ID] = true
		}
	}
	if len(matched) == 0 && catchallAdmin {
		return nil
	}
	out := make([]string, 0, len(matched))
	for id := range matched {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
