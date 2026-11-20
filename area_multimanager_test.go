package nano

import (
	"sync"
	"testing"

	"github.com/lonng/nano/session"
)

func TestAreaConcurrentSubscribeSeveralManagers(t *testing.T) {
	managers := []*AreaManager{
		NewAreaManager(WithAreaReconnectTTL(0)),
		NewAreaManager(WithAreaReconnectTTL(0)),
	}
	defer func() {
		for _, m := range managers {
			m.Close()
		}
	}()

	s, _ := newTestSession(t, 321)
	var wg sync.WaitGroup
	for i, m := range managers {
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func(m *AreaManager, i, j int) {
				defer wg.Done()
				for k := 0; k < 100; k++ {
					sub, err := m.Subscribe("room", s, NewRect(float64(j), float64(i), 10, 10))
					if err != nil {
						t.Errorf("subscribe: %v", err)
						return
					}
					if err := sub.Update(NewRect(float64(j+k), 0, 10, 10)); err != nil {
						t.Errorf("update: %v", err)
						return
					}
					if k%5 == 0 {
						_ = sub.Close()
					}
				}
			}(m, i, j)
		}
	}
	wg.Wait()

	// Sessions closing must clean up on every manager.
	session.Lifetime.Close(s)
	for i, m := range managers {
		if n := m.Count("room"); n != 0 {
			t.Errorf("manager %d still has %d subscriptions", i, n)
		}
	}
}
