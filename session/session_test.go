package session

import (
	"sync"
	"testing"
)

func TestNewSession(t *testing.T) {
	s := New(nil)
	if s.ID() < 1 {
		t.Fail()
	}
}

func TestSession_Bind(t *testing.T) {
	s := New(nil)
	uids := []int64{100, 1000, 10000000}
	for i, uid := range uids {
		s.Bind(uid)
		if s.UID() != uids[i] {
			t.Fail()
		}
	}
}

func TestSession_HasKey(t *testing.T) {
	s := New(nil)
	key := "hello"
	value := "world"
	s.Set(key, value)
	if !s.HasKey(key) {
		t.Fail()
	}
}

func TestSession_Float32(t *testing.T) {
	s := New(nil)
	key := "hello"
	value := float32(1.2000)
	s.Set(key, value)
	if value != s.Float32(key) {
		t.Fail()
	}
}

func TestSession_Float64(t *testing.T) {
	s := New(nil)
	key := "hello"
	value := 1.2000
	s.Set(key, value)
	if value != s.Float64(key) {
		t.Fail()
	}
}

func TestSession_Int(t *testing.T) {
	s := New(nil)
	key := "testkey"
	value := 234
	s.Set(key, value)
	if value != s.Int(key) {
		t.Fail()
	}
}

func TestSession_Int8(t *testing.T) {
	s := New(nil)
	key := "testkey"
	value := int8(123)
	s.Set(key, value)
	if value != s.Int8(key) {
		t.Fail()
	}
}

func TestSession_Int16(t *testing.T) {
	s := New(nil)
	key := "testkey"
	value := int16(3245)
	s.Set(key, value)
	if value != s.Int16(key) {
		t.Fail()
	}
}

func TestSession_Int32(t *testing.T) {
	s := New(nil)
	key := "testkey"
	value := int32(5454)
	s.Set(key, value)
	if value != s.Int32(key) {
		t.Fail()
	}
}

func TestSession_Int64(t *testing.T) {
	s := New(nil)
	key := "testkey"
	value := int64(444454)
	s.Set(key, value)
	if value != s.Int64(key) {
		t.Fail()
	}
}

func TestSession_Uint(t *testing.T) {
	s := New(nil)
	key := "testkey"
	value := uint(24254)
	s.Set(key, value)
	if value != s.Uint(key) {
		t.Fail()
	}
}

func TestSession_Uint8(t *testing.T) {
	s := New(nil)
	key := "testkey"
	value := uint8(34)
	s.Set(key, value)
	if value != s.Uint8(key) {
		t.Fail()
	}
}

func TestSession_Uint16(t *testing.T) {
	s := New(nil)
	key := "testkey"
	value := uint16(4645)
	s.Set(key, value)
	if value != s.Uint16(key) {
		t.Fail()
	}
}

func TestSession_Uint32(t *testing.T) {
	s := New(nil)
	key := "testkey"
	value := uint32(12365)
	s.Set(key, value)
	if value != s.Uint32(key) {
		t.Fail()
	}
}

func TestSession_Uint64(t *testing.T) {
	s := New(nil)
	key := "testkey"
	value := uint64(1000)
	s.Set(key, value)
	if value != s.Uint64(key) {
		t.Fail()
	}
}

func TestSession_State(t *testing.T) {
	s := New(nil)
	key := "testkey"
	value := uint64(1000)
	s.Set(key, value)
	state := s.State()
	if value != state[key].(uint64) {
		t.Fail()
	}
}

func TestSession_Restore(t *testing.T) {
	s := New(nil)
	s2 := New(nil)
	key := "testkey"
	value := uint64(1000)
	s.Set(key, value)
	state := s.State()
	s2.Restore(state)
	if value != s2.Uint64(key) {
		t.Fail()
	}
}

func TestRouter_OnChange(t *testing.T) {
	r := newRouter()
	var got [][2]string
	cancel := r.OnChange(func(service, address string) {
		got = append(got, [2]string{service, address})
	})

	r.Bind("Room", "node-a")
	r.Bind("Room", "node-b")
	r.Delete("Room")
	if len(got) != 3 {
		t.Fatalf("notifications = %d, want 3", len(got))
	}
	if got[0] != [2]string{"Room", "node-a"} || got[1] != [2]string{"Room", "node-b"} {
		t.Errorf("bind notifications = %v", got[:2])
	}
	if got[2] != [2]string{"Room", ""} {
		t.Errorf("delete notification = %v", got[2])
	}

	cancel()
	cancel()
	r.Bind("Room", "node-c")
	if len(got) != 3 {
		t.Errorf("cancel did not stop notifications: %v", got)
	}
	if n := r.watcherCount(); n != 0 {
		t.Errorf("watcher count = %d", n)
	}

	// A nil callback is a no-op, and notifications are not delivered to it.
	cancelNil := r.OnChange(nil)
	cancelNil()

	// Multiple watchers all receive events.
	var a, b int
	cancelA := r.OnChange(func(string, string) { a++ })
	cancelB := r.OnChange(func(string, string) { b++ })
	r.Bind("Other", "node")
	if a != 1 || b != 1 {
		t.Errorf("watchers a=%d b=%d", a, b)
	}
	cancelA()
	r.Bind("Other", "node-2")
	if a != 1 || b != 2 {
		t.Errorf("after cancel a=%d b=%d", a, b)
	}
	cancelB()
}

func TestRouter_OnChangeConcurrent(t *testing.T) {
	r := newRouter()
	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				cancel := r.OnChange(func(string, string) {})
				r.Bind("Room", "node")
				if j%3 == 0 {
					r.Delete("Room")
				}
				cancel()
			}
		}(i)
	}
	wg.Wait()
	if n := r.watcherCount(); n != 0 {
		t.Errorf("leaked watchers = %d", n)
	}
}

func TestRouter_PurgeAddress(t *testing.T) {
	r := newRouter()
	r.Bind("Room", "node-a")
	r.Bind("Chat", "node-b")
	r.Bind("Other", "node-a")

	var removedServices []string
	cancel := r.OnChange(func(service, address string) {
		if address == "" {
			removedServices = append(removedServices, service)
		}
	})
	defer cancel()

	if n := r.PurgeAddress(""); n != 0 {
		t.Errorf("purge empty address = %d", n)
	}
	if n := r.PurgeAddress("node-a"); n != 2 {
		t.Errorf("purge node-a = %d, want 2", n)
	}
	if _, ok := r.Find("Room"); ok {
		t.Error("Room binding survived purge")
	}
	if _, ok := r.Find("Other"); ok {
		t.Error("Other binding survived purge")
	}
	if addr, ok := r.Find("Chat"); !ok || addr != "node-b" {
		t.Errorf("unrelated binding was purged: %q, %v", addr, ok)
	}
	if len(removedServices) != 2 {
		t.Errorf("delete notifications = %v", removedServices)
	}
	// Purging again removes nothing.
	if n := r.PurgeAddress("node-a"); n != 0 {
		t.Errorf("second purge = %d", n)
	}
}
