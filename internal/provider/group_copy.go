package provider

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// CopyGroup makes a group of the user's that is the group id as it is now
// (lc on Discord): its models and patterns, routing, rules, classifier,
// effort, context, levels, fast and off members, under name — "<name>
// copy" when empty, numbered past a name or id in use — listed right after
// the one it copies. A found group's copy is the user's, with the members
// it has now. The copy is on, whether the group was or not, and the
// original is left as it is.
func CopyGroup(id, name string) (Group, error) {
	all := Groups()
	i := slices.IndexFunc(all, func(g Group) bool { return g.ID == id && !g.Hidden })
	if i < 0 {
		return Group{}, fmt.Errorf("no group %q", id)
	}
	src := all[i]
	// a copy shares nothing with the group it was read from: the rules'
	// time windows are pointers, the lists slices
	raw, err := json.Marshal(src)
	if err != nil {
		return Group{}, err
	}
	var g Group
	if err := json.Unmarshal(raw, &g); err != nil {
		return Group{}, err
	}
	g.Auto, g.Hidden, g.Disabled = false, false, false

	name = strings.TrimSpace(name)
	if name == "" {
		name = src.Name + " copy"
	}
	names, ids := map[string]bool{}, map[string]bool{}
	for _, o := range all {
		ids[o.ID] = true
		if !o.Hidden {
			names[strings.ToLower(o.Name)] = true
		}
	}
	for _, removed := range RemovedGroups() {
		ids[removed] = true
	}
	base := name
	for n := 2; names[strings.ToLower(name)]; n++ {
		name = fmt.Sprintf("%s %d", base, n)
	}
	slug := GroupSlug(name)
	if slug == "" {
		slug = "group"
	}
	g.Name, g.ID = name, slug
	for n := 2; ids[g.ID]; n++ {
		g.ID = fmt.Sprintf("%s-%d", slug, n)
	}
	if err := SaveGroup(g); err != nil {
		return Group{}, err
	}

	// listed after the one it copies, not at the end of the user's
	order := make([]string, 0, len(all)+1)
	for _, o := range all {
		order = append(order, o.ID)
		if o.ID == src.ID {
			order = append(order, g.ID)
		}
	}
	if err := SetGroupOrder(order); err != nil {
		return Group{}, err
	}
	for _, o := range Groups() {
		if o.ID == g.ID {
			return o, nil
		}
	}
	return g, nil
}
