package core

var registry = map[VenueID]*Venue{}

func Register(v *Venue)     { registry[v.ID] = v }
func Get(id VenueID) *Venue { return registry[id] }

func Enabled(ids []VenueID) []*Venue {
	out := make([]*Venue, 0, len(ids))
	for _, id := range ids {
		if v := Get(id); v != nil {
			out = append(out, v)
		}
	}
	return out
}
