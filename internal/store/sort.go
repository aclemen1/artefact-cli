package store

import (
	"sort"
	"strings"

	"github.com/aclemen1/artefact-cli/internal/index"
)

// SortKeys are the orders a list or a search can take.
var SortKeys = []string{"relevance", "date", "deposited", "updated", "title", "size"}

// Sort orders artefacts by Key; Desc reverses it. The zero value is the
// default order: relevance for a search, deposit date for a list.
type Sort struct {
	Key  string
	Desc bool
}

// defaultDesc is the natural direction of a key: newest, largest and most
// relevant first; titles from A to Z.
func defaultDesc(key string) bool { return key != "title" }

// ParseSort reads key[:asc|desc], e.g. "date", "title:desc".
func ParseSort(s string) (Sort, error) {
	if s == "" {
		return Sort{}, nil
	}
	key, dir, hasDir := strings.Cut(strings.ToLower(strings.TrimSpace(s)), ":")
	ok := false
	for _, k := range SortKeys {
		ok = ok || k == key
	}
	if !ok {
		return Sort{}, userErr("--sort takes one of %s, optionally :asc or :desc; got %q. Example: --sort date:asc", strings.Join(SortKeys, ", "), s)
	}
	o := Sort{Key: key, Desc: defaultDesc(key)}
	switch {
	case !hasDir:
	case dir == "asc":
		o.Desc = false
	case dir == "desc":
		o.Desc = true
	default:
		return Sort{}, userErr("--sort direction is asc or desc, got %q", dir)
	}
	return o, nil
}

func (o Sort) String() string {
	d := "asc"
	if o.Desc {
		d = "desc"
	}
	return o.Key + ":" + d
}

func (o Sort) orDefault(key string) Sort {
	if o.Key == "" {
		return Sort{Key: key, Desc: defaultDesc(key)}
	}
	return o
}

// sortValue returns the compared value of a row; ok is false when the row has
// none (no date), and such rows go last in either direction.
func sortValue(r index.Row, key string) (s string, n float64, ok bool) {
	switch key {
	case "date":
		return r.Date, 0, r.Date != ""
	case "deposited":
		return r.Deposited, 0, true
	case "updated":
		return r.Updated, 0, true
	case "title":
		return strings.ToLower(r.Title), 0, true
	case "size":
		return "", float64(r.Size), true
	}
	return "", 0, true
}

// less compares two rows for o; score holds their relevance for a search
// (lower is better, as bm25 gives it).
func (o Sort) less(a, b index.Row, sa, sb float64) bool {
	if o.Key == "relevance" {
		if sa != sb {
			return (sa < sb) == o.Desc
		}
		return a.Deposited > b.Deposited
	}
	as, an, aok := sortValue(a, o.Key)
	bs, bn, bok := sortValue(b, o.Key)
	if aok != bok {
		return aok
	}
	if as != bs || an != bn {
		if o.Key == "size" {
			return (an < bn) != o.Desc
		}
		return (as < bs) != o.Desc
	}
	if a.Deposited != b.Deposited {
		return a.Deposited > b.Deposited
	}
	return a.ID > b.ID
}

func sortItems(items []ListItem, o Sort) {
	sort.SliceStable(items, func(i, j int) bool { return o.less(items[i].Row, items[j].Row, 0, 0) })
}

func sortHits(hits []SearchHit, o Sort) {
	sort.SliceStable(hits, func(i, j int) bool {
		return o.less(hits[i].Row, hits[j].Row, hits[i].Score, hits[j].Score)
	})
}

// SortList orders items in place; score gives an item's relevance (lower is
// better) when o is by relevance.
func SortList(items []ListItem, o Sort, score func(ListItem) float64) {
	sort.SliceStable(items, func(i, j int) bool {
		var si, sj float64
		if score != nil {
			si, sj = score(items[i]), score(items[j])
		}
		return o.less(items[i].Row, items[j].Row, si, sj)
	})
}

// DefaultDesc is the natural direction of a sort key.
func DefaultDesc(key string) bool { return defaultDesc(key) }
