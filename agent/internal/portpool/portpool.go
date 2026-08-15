package portpool

import (
	"fmt"
	"sync"
)

type Pool struct {
	start int
	size  int
	mu    sync.Mutex
	used  map[int]bool
}

func New(start, size int) *Pool {
	return &Pool{
		start: start,
		size:  size,
		used:  make(map[int]bool),
	}
}

func (p *Pool) Acquire() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i := 0; i < p.size; i++ {
		port := p.start + i
		if !p.used[port] {
			p.used[port] = true
			return port, nil
		}
	}
	return 0, fmt.Errorf("port pool exhausted")
}

func (p *Pool) Release(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.used, port)
}
