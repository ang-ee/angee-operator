package ports

import "testing"

func TestPoolAllocatesStablePorts(t *testing.T) {
	pool, err := ParsePool("web", "8100-8101")
	if err != nil {
		t.Fatalf("ParsePool() error = %v", err)
	}
	first, err := pool.Allocate("workspace/a")
	if err != nil {
		t.Fatalf("Allocate() error = %v", err)
	}
	if first != 8100 {
		t.Fatalf("first port = %d", first)
	}
	again, err := pool.Allocate("workspace/a")
	if err != nil {
		t.Fatalf("Allocate() stable error = %v", err)
	}
	if again != first {
		t.Fatalf("stable allocation = %d, want %d", again, first)
	}
	second, err := pool.Allocate("workspace/b")
	if err != nil {
		t.Fatalf("Allocate() second error = %v", err)
	}
	if second != 8101 {
		t.Fatalf("second port = %d", second)
	}
	if _, err := pool.Allocate("workspace/c"); err == nil {
		t.Fatal("Allocate() error = nil, want exhaustion")
	}
}

func TestPoolRelease(t *testing.T) {
	pool, err := ParsePool("web", "8100-8100")
	if err != nil {
		t.Fatalf("ParsePool() error = %v", err)
	}
	if _, err := pool.Allocate("a"); err != nil {
		t.Fatalf("Allocate() error = %v", err)
	}
	pool.Release("a")
	port, err := pool.Allocate("b")
	if err != nil {
		t.Fatalf("Allocate() after release error = %v", err)
	}
	if port != 8100 {
		t.Fatalf("port = %d", port)
	}
}

func TestPoolSkipsUnavailablePorts(t *testing.T) {
	pool, err := ParsePool("acp", "3007-3009")
	if err != nil {
		t.Fatalf("ParsePool() error = %v", err)
	}
	port, err := pool.AllocateAvailable("service/agent/acp", func(port int) bool {
		return port == 3007 || port == 3008
	})
	if err != nil {
		t.Fatalf("AllocateAvailable() error = %v", err)
	}
	if port != 3009 {
		t.Fatalf("port = %d, want 3009", port)
	}
	again, err := pool.AllocateAvailable("service/agent/acp", func(port int) bool {
		return port == 3009
	})
	if err != nil {
		t.Fatalf("stable AllocateAvailable() error = %v", err)
	}
	if again != port {
		t.Fatalf("stable port = %d, want %d", again, port)
	}
}
