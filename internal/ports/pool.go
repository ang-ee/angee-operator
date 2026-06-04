package ports

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

type Pool struct {
	Name  string
	Start int
	End   int

	mu     sync.Mutex
	leases map[int]string
}

func ParsePool(name, spec string) (*Pool, error) {
	startText, endText, ok := strings.Cut(spec, "-")
	if !ok {
		return nil, fmt.Errorf("port pool %q range must be start-end", name)
	}
	start, err := strconv.Atoi(strings.TrimSpace(startText))
	if err != nil {
		return nil, fmt.Errorf("port pool %q start: %w", name, err)
	}
	end, err := strconv.Atoi(strings.TrimSpace(endText))
	if err != nil {
		return nil, fmt.Errorf("port pool %q end: %w", name, err)
	}
	if start < 1 || end < start || end > 65535 {
		return nil, fmt.Errorf("port pool %q has invalid range %d-%d", name, start, end)
	}
	return &Pool{Name: name, Start: start, End: end, leases: map[int]string{}}, nil
}

func FromManifest(specs map[string]manifest.PortPool, leases map[string][]manifest.PortLease) (map[string]*Pool, error) {
	pools := make(map[string]*Pool, len(specs))
	for name, spec := range specs {
		pool, err := ParsePool(name, spec.Range)
		if err != nil {
			return nil, err
		}
		for _, lease := range leases[name] {
			if lease.Port < pool.Start || lease.Port > pool.End {
				return nil, fmt.Errorf("lease for pool %q uses out-of-range port %d", name, lease.Port)
			}
			pool.leases[lease.Port] = lease.Owner
		}
		pools[name] = pool
	}
	return pools, nil
}

func (p *Pool) Allocate(owner string) (int, error) {
	return p.AllocateAvailable(owner, nil)
}

// AllocateAvailable reserves the next free port that the supplied
// `unavailable` predicate does not flag. A nil predicate behaves like
// Allocate. The predicate is invoked outside the pool mutex so callers
// may pass functions that perform I/O (e.g. probing the host network).
func (p *Pool) AllocateAvailable(owner string, unavailable func(int) bool) (int, error) {
	p.mu.Lock()
	for port, existingOwner := range p.leases {
		if existingOwner == owner {
			p.mu.Unlock()
			return port, nil
		}
	}
	candidates := make([]int, 0, p.End-p.Start+1)
	for port := p.Start; port <= p.End; port++ {
		if _, ok := p.leases[port]; !ok {
			candidates = append(candidates, port)
		}
	}
	p.mu.Unlock()

	for _, port := range candidates {
		if unavailable != nil && unavailable(port) {
			continue
		}
		p.mu.Lock()
		if _, taken := p.leases[port]; taken {
			p.mu.Unlock()
			continue
		}
		p.leases[port] = owner
		p.mu.Unlock()
		return port, nil
	}
	return 0, fmt.Errorf("port pool %q is exhausted", p.Name)
}

func (p *Pool) Release(owner string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for port, existingOwner := range p.leases {
		if existingOwner == owner {
			delete(p.leases, port)
		}
	}
}

func (p *Pool) Leases() []manifest.PortLease {
	p.mu.Lock()
	defer p.mu.Unlock()
	leases := make([]manifest.PortLease, 0, len(p.leases))
	for port, owner := range p.leases {
		leases = append(leases, manifest.PortLease{Port: port, Owner: owner})
	}
	return leases
}
