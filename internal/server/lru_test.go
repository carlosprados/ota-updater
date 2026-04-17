package server

import (
	"bytes"
	"strconv"
	"sync"
	"testing"
)

func TestEntryLRU_BasicGetPut(t *testing.T) {
	c := newEntryLRU[string](3)
	c.Put("a", "1")
	c.Put("b", "2")

	if v, ok := c.Get("a"); !ok || v != "1" {
		t.Fatalf("Get(a) = (%q,%v)", v, ok)
	}
	if _, ok := c.Get("missing"); ok {
		t.Fatalf("Get(missing) should report absent")
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
}

func TestEntryLRU_EvictsLeastRecentlyUsed(t *testing.T) {
	c := newEntryLRU[int](3)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)
	_, _ = c.Get("a") // a is now MRU → b is LRU
	c.Put("d", 4)     // evicts b

	if _, ok := c.Get("b"); ok {
		t.Fatalf("b should have been evicted")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%s should still be present", k)
		}
	}
}

func TestEntryLRU_Clear(t *testing.T) {
	c := newEntryLRU[string](4)
	for i := 0; i < 4; i++ {
		c.Put(strconv.Itoa(i), "v")
	}
	c.Clear()
	if c.Len() != 0 {
		t.Fatalf("Len after Clear = %d", c.Len())
	}
	c.Put("z", "Z")
	if v, ok := c.Get("z"); !ok || v != "Z" {
		t.Fatalf("post-Clear Put/Get failed: (%q,%v)", v, ok)
	}
}

func TestEntryLRU_ZeroCapClampsToOne(t *testing.T) {
	c := newEntryLRU[int](0)
	c.Put("a", 1)
	c.Put("b", 2)
	if _, ok := c.Get("a"); ok {
		t.Fatalf("a should have been evicted from cap-1 cache")
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Fatalf("Get(b) = (%d,%v)", v, ok)
	}
}

func TestByteBudgetLRU_BasicGetPut(t *testing.T) {
	c := newByteBudgetLRU(1024)
	ok := c.Put("a", bytes.Repeat([]byte{1}, 300))
	if !ok {
		t.Fatalf("Put(a) rejected")
	}
	if v, got := c.Get("a"); !got || len(v) != 300 {
		t.Fatalf("Get(a) len=%d got=%v", len(v), got)
	}
	if c.Bytes() != 300 {
		t.Fatalf("Bytes = %d, want 300", c.Bytes())
	}
}

func TestByteBudgetLRU_EvictsUntilFits(t *testing.T) {
	c := newByteBudgetLRU(1024)
	c.Put("a", bytes.Repeat([]byte{1}, 400))
	c.Put("b", bytes.Repeat([]byte{2}, 400))
	// Now total=800, cap=1024.
	c.Put("c", bytes.Repeat([]byte{3}, 400)) // total=1200 > 1024 → evict a (LRU).

	if _, ok := c.Get("a"); ok {
		t.Fatalf("a should have been evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatalf("b should still be there")
	}
	if c.Bytes() > 1024 {
		t.Fatalf("Bytes = %d, exceeds cap 1024", c.Bytes())
	}
}

func TestByteBudgetLRU_RejectsOversizedValue(t *testing.T) {
	c := newByteBudgetLRU(1024)
	c.Put("a", bytes.Repeat([]byte{1}, 100))
	if ok := c.Put("big", bytes.Repeat([]byte{2}, 2048)); ok {
		t.Fatalf("oversized Put should have been rejected")
	}
	// And it must not evict the existing entry.
	if _, ok := c.Get("a"); !ok {
		t.Fatalf("a should still be there after rejected big Put")
	}
}

func TestByteBudgetLRU_ReplaceAdjustsTotal(t *testing.T) {
	c := newByteBudgetLRU(1024)
	c.Put("a", bytes.Repeat([]byte{1}, 300))
	c.Put("a", bytes.Repeat([]byte{2}, 100)) // replace, smaller
	if c.Bytes() != 100 {
		t.Fatalf("Bytes after replace = %d, want 100", c.Bytes())
	}
	v, _ := c.Get("a")
	if len(v) != 100 || v[0] != 2 {
		t.Fatalf("replacement not taken: len=%d v[0]=%d", len(v), v[0])
	}
}

func TestByteBudgetLRU_GetRefreshesMRU(t *testing.T) {
	c := newByteBudgetLRU(900)
	c.Put("a", bytes.Repeat([]byte{1}, 300))
	c.Put("b", bytes.Repeat([]byte{2}, 300))
	c.Put("c", bytes.Repeat([]byte{3}, 300))
	// Touch "a" so it's MRU; next Put should evict "b".
	_, _ = c.Get("a")
	c.Put("d", bytes.Repeat([]byte{4}, 300))
	if _, ok := c.Get("b"); ok {
		t.Fatalf("b should have been evicted (a was refreshed)")
	}
}

func TestByteBudgetLRU_Clear(t *testing.T) {
	c := newByteBudgetLRU(1024)
	c.Put("a", []byte{1})
	c.Clear()
	if c.Len() != 0 || c.Bytes() != 0 {
		t.Fatalf("post-Clear Len=%d Bytes=%d", c.Len(), c.Bytes())
	}
}

func TestByteBudgetLRU_ConcurrentStaysBounded(t *testing.T) {
	const cap = 4096
	c := newByteBudgetLRU(cap)
	val := bytes.Repeat([]byte{0xAB}, 100)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				c.Put(strconv.Itoa((id<<16)|i), val)
				_, _ = c.Get(strconv.Itoa((id << 16) | i))
			}
		}(w)
	}
	wg.Wait()
	if c.Bytes() > cap {
		t.Fatalf("post-churn Bytes = %d, exceeds cap %d", c.Bytes(), cap)
	}
}
