package gateway

import "slices"

func (a *App) deleteConfig(version int64, action, id string) (Config, error) {
	return a.Store.Change(version, action, id, func(c *Config) error {
		found := false
		switch action {
		case "provider.delete":
			return c.deleteProvider(id)
		case "model.delete":
			n := len(c.Models)
			c.Models = slices.DeleteFunc(c.Models, func(m Model) bool { return m.ID == id })
			found = len(c.Models) != n
		case "route.delete":
			n := len(c.Routes)
			c.Routes = slices.DeleteFunc(c.Routes, func(r Route) bool { return r.ID == id })
			found = len(c.Routes) != n
		case "alias.delete":
			n := len(c.Aliases)
			c.Aliases = slices.DeleteFunc(c.Aliases, func(v Alias) bool { return v.ID == id })
			found = len(c.Aliases) != n
		}
		if !found {
			return fail("NOT_FOUND", "对象不存在", 404)
		}
		return nil
	})
}

func (c *Config) deleteProvider(id string) error {
	if _, ok := c.provider(id); !ok {
		return fail("NOT_FOUND", "对象不存在", 404)
	}
	models := map[string]bool{}
	c.Models = slices.DeleteFunc(c.Models, func(m Model) bool {
		if m.ProviderID != id {
			return false
		}
		models[m.ID] = true
		return true
	})
	c.Providers = slices.DeleteFunc(c.Providers, func(p Provider) bool { return p.ID == id })
	c.Aliases = slices.DeleteFunc(c.Aliases, func(a Alias) bool { return models[a.Target] })
	for i := range c.Routes {
		r := &c.Routes[i]
		affected, remaining := false, 0
		// 保留组序，避免后续组的会话亲和键因删除而移位。
		for j := range r.Groups {
			g := &r.Groups[j]
			n := len(g.Candidates)
			g.Candidates = slices.DeleteFunc(g.Candidates, func(v Candidate) bool { return models[v.ModelID] })
			affected = affected || len(g.Candidates) != n
			remaining += len(g.Candidates)
		}
		if affected && remaining == 0 {
			r.Enabled = false
		}
	}
	return nil
}
