package store

import (
	"testing"

	"github.com/lyminhtanh/dozzle-shim/internal/model"
)

func ev(id uint32, ts int64, msg, level string) model.DozzleLogEvent {
	return model.DozzleLogEvent{T: "single", C: "c1", Id: id, Ts: ts, M: msg, Rm: msg, L: level, S: "stdout"}
}

func TestAppendBetween(t *testing.T) {
	s, _ := New(t.TempDir(), 0)
	for i := 1; i <= 5; i++ {
		s.Append(ev(uint32(i), int64(i*100), "line", "unknown"))
	}
	if n := s.Count("c1"); n != 5 {
		t.Fatalf("count=%d want 5", n)
	}
	got := s.Between("c1", 200, 400)
	if len(got) != 3 || got[0].Ts != 200 || got[2].Ts != 400 {
		t.Fatalf("between wrong: %+v", got)
	}
}

func TestSearchOldestFirstAndCap(t *testing.T) {
	s, _ := New(t.TempDir(), 0)
	msgs := []string{"alpha", "ERROR beta", "gamma admin", "delta", "admin again"}
	for i, m := range msgs {
		s.Append(ev(uint32(i+1), int64((i+1)*100), m, "unknown"))
	}
	match := func(c string) bool { return containsFold(c, "admin") }
	got, capped := s.Search("c1", match, 0, 10)
	if len(got) != 2 {
		t.Fatalf("want 2 admin matches, got %d (%+v)", len(got), got)
	}
	// oldest-first: id 3 before id 5
	if got[0].Id != 3 || got[1].Id != 5 {
		t.Fatalf("order wrong: %d,%d", got[0].Id, got[1].Id)
	}
	if capped {
		t.Fatalf("should not be capped at limit 10")
	}
	// cap at 1 → newest match kept (id 5), capped true
	got2, capped2 := s.Search("c1", match, 0, 1)
	if len(got2) != 1 || got2[0].Id != 5 || !capped2 {
		t.Fatalf("cap case wrong: %+v capped=%v", got2, capped2)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s1, _ := New(dir, 0)
	for i := 1; i <= 3; i++ {
		s1.Append(ev(uint32(i), int64(i*100), "cached line", "unknown"))
	}
	s1.Close()

	// New store, same dir → load cache
	s2, _ := New(dir, 0)
	if !s2.HasCache("c1") {
		t.Fatalf("expected cache to exist")
	}
	maxID, n := s2.LoadCache("c1")
	if n != 3 || maxID != 3 {
		t.Fatalf("loadcache n=%d maxID=%d want 3,3", n, maxID)
	}
	if s2.Count("c1") != 3 {
		t.Fatalf("after load count=%d want 3", s2.Count("c1"))
	}
}

// containsFold is a tiny case-insensitive substring used only by the test.
func containsFold(s, sub string) bool {
	ls, lsub := []rune(toLower(s)), []rune(toLower(sub))
	for i := 0; i+len(lsub) <= len(ls); i++ {
		ok := true
		for j := range lsub {
			if ls[i+j] != lsub[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func toLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}
