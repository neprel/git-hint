package scan

// Copy preserves all local fields, including names, paths and content hashes.
func (r *Result) Copy() *Result {
	if r == nil {
		return nil
	}
	c := *r
	c.Entities = copyResultSlice(r.Entities)
	for i, e := range r.Entities {
		if e.ParentID != nil {
			x := *e.ParentID
			c.Entities[i].ParentID = &x
		}
		if e.Language != nil {
			x := *e.Language
			c.Entities[i].Language = &x
		}
		c.Entities[i].Flags = copyResultSlice(e.Flags)
		if e.Features != nil {
			c.Entities[i].Features = make(map[string]Quantity, len(e.Features))
			for k, v := range e.Features {
				if v.Value != nil {
					x := *v.Value
					v.Value = &x
				}
				c.Entities[i].Features[k] = v
			}
		}
	}
	c.Relations = copyResultSlice(r.Relations)
	c.Languages = copyResultSlice(r.Languages)
	if r.Ambiguous != nil {
		c.Ambiguous = map[string][]string{}
		for k, v := range r.Ambiguous {
			c.Ambiguous[k] = copyResultSlice(v)
		}
	}
	return &c
}

func copyResultSlice[T any](s []T) []T {
	if s == nil {
		return nil
	}
	out := make([]T, len(s))
	copy(out, s)
	return out
}
