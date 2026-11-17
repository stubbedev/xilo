package push

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/stubbedev/xilo/internal/narinfo"
)

var errReferenceNotPushed = errors.New("a reference was not pushed")

type pathOutcome struct {
	done chan struct{}
	err  error
}

type publishPlan struct {
	order    []string
	refs     map[string][]string
	outcomes map[string]*pathOutcome
	byHash   map[string]pathInfo
}

func planPublish(missing []string, byHash map[string]pathInfo) (*publishPlan, error) {
	pending := make(map[string]bool, len(missing))
	for _, h := range missing {
		pending[h] = true
	}
	p := &publishPlan{
		order:    make([]string, 0, len(missing)),
		refs:     make(map[string][]string, len(missing)),
		outcomes: make(map[string]*pathOutcome, len(missing)),
		byHash:   byHash,
	}
	blockers := make(map[string]int, len(missing))
	dependents := make(map[string][]string, len(missing))
	for _, h := range missing {
		var refs []string
		for _, ref := range byHash[h].References {
			r := narinfo.StoreHash(ref)
			if r != h && pending[r] && !slices.Contains(refs, r) {
				refs = append(refs, r)
			}
		}
		p.refs[h] = refs
		p.outcomes[h] = &pathOutcome{done: make(chan struct{})}
		blockers[h] = len(refs)
		for _, r := range refs {
			dependents[r] = append(dependents[r], h)
		}
	}
	byPath := func(a, b string) int { return strings.Compare(byHash[a].Path, byHash[b].Path) }
	var ready []string
	for _, h := range missing {
		if blockers[h] == 0 {
			ready = append(ready, h)
		}
	}
	slices.SortFunc(ready, byPath)
	for len(ready) > 0 {
		h := ready[0]
		ready = ready[1:]
		p.order = append(p.order, h)
		var freed []string
		for _, d := range dependents[h] {
			blockers[d]--
			if blockers[d] == 0 {
				freed = append(freed, d)
			}
		}
		slices.SortFunc(freed, byPath)
		ready = append(ready, freed...)
	}
	if len(p.order) < len(missing) {
		var cyclic []string
		for _, h := range missing {
			if blockers[h] > 0 {
				cyclic = append(cyclic, byHash[h].Path)
			}
		}
		slices.Sort(cyclic)
		return nil, fmt.Errorf("store paths reference each other in a cycle, so none of them can be published first: %s", strings.Join(cyclic, ", "))
	}
	return p, nil
}

func (p *publishPlan) await(ctx context.Context, h string) error {
	for _, r := range p.refs[h] {
		o := p.outcomes[r]
		select {
		case <-o.done:
		case <-ctx.Done():
			return fmt.Errorf("waiting for reference %s to be published: %w", p.byHash[r].Path, context.Cause(ctx))
		}
		if o.err != nil {
			return fmt.Errorf("%w: %s", errReferenceNotPushed, p.byHash[r].Path)
		}
	}
	return nil
}

func (p *publishPlan) finish(h string, err error) {
	o := p.outcomes[h]
	o.err = err
	close(o.done)
}
