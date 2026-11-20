package nano

import (
	"fmt"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/lonng/nano/session"
)

type recordedPush struct {
	route string
	data  string
}

// pushJournal records the global delivery order across sessions so tests can
// assert that recipients of one event are visited in subscription order.
type pushJournal struct {
	mu      sync.Mutex
	entries []string
}

func (j *pushJournal) add(entry string) {
	j.mu.Lock()
	j.entries = append(j.entries, entry)
	j.mu.Unlock()
}

func (j *pushJournal) snapshot() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]string, len(j.entries))
	copy(out, j.entries)
	return out
}

func (j *pushJournal) reset() {
	j.mu.Lock()
	j.entries = nil
	j.mu.Unlock()
}

// recordingEntity is a thread-safe session.NetworkEntity used by area tests.
type recordingEntity struct {
	name    string
	journal *pushJournal
	mu      sync.Mutex
	pushes  []recordedPush
	err     error
	closed  bool
}

func (e *recordingEntity) Push(route string, v interface{}) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("entity closed")
	}
	if e.err != nil {
		return e.err
	}
	data, _ := v.([]byte)
	e.pushes = append(e.pushes, recordedPush{route: route, data: string(data)})
	if e.journal != nil {
		e.journal.add(e.name)
	}
	return nil
}

func (e *recordingEntity) RPC(string, interface{}) error { return nil }
func (e *recordingEntity) LastMid() uint64               { return 0 }
func (e *recordingEntity) Response(interface{}) error    { return nil }
func (e *recordingEntity) ResponseMid(uint64, interface{}) error {
	return nil
}
func (e *recordingEntity) Close() error {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	return nil
}
func (e *recordingEntity) RemoteAddr() net.Addr { return mockAddr{} }

func (e *recordingEntity) pushCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.pushes)
}

func (e *recordingEntity) pushed() []recordedPush {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]recordedPush, len(e.pushes))
	copy(out, e.pushes)
	return out
}

type mockAddr struct{}

func (mockAddr) Network() string { return "mock" }
func (mockAddr) String() string  { return "mock" }

func newTestSession(t *testing.T, uid int64) (*session.Session, *recordingEntity) {
	t.Helper()
	return newNamedTestSession(t, uid, "")
}

func newNamedTestSession(t *testing.T, uid int64, name string) (*session.Session, *recordingEntity) {
	t.Helper()
	e := &recordingEntity{name: name}
	s := session.New(e)
	if uid > 0 {
		if err := s.Bind(uid); err != nil {
			t.Fatalf("bind uid: %v", err)
		}
	}
	return s, e
}

func mustSubscribe(t *testing.T, m *AreaManager, room string, s *session.Session, r Rect, opts ...AreaOption) *AreaSubscription {
	t.Helper()
	sub, err := m.Subscribe(room, s, r, opts...)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return sub
}

func TestRectHalfOpenBounds(t *testing.T) {
	r := NewRect(0, 0, 10, 10)
	cases := []struct {
		p    Point
		want bool
	}{
		{Point{0, 0}, true},
		{Point{9.999, 9.999}, true},
		{Point{10, 5}, false}, // Max edge is exclusive
		{Point{5, 10}, false},
		{Point{-0.001, 5}, false},
		{Point{math.NaN(), 5}, false},
		{Point{math.Inf(1), 5}, false},
	}
	for _, c := range cases {
		if got := r.Contains(c.p); got != c.want {
			t.Errorf("Contains(%v) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestRectNormalizationAndEmpty(t *testing.T) {
	r := Rect{Min: Point{X: 10, Y: 10}, Max: Point{X: 0, Y: 0}}
	if !r.Contains(Point{5, 5}) {
		t.Error("inverted rectangle should be normalized before use")
	}
	if NewRect(3, 3, 0, 10).IsEmpty() != true {
		t.Error("zero width must be empty")
	}
	if NewRect(3, 3, 10, 0).IsEmpty() != true {
		t.Error("zero height must be empty")
	}
	if !(Rect{Min: Point{math.NaN(), 0}, Max: Point{1, 1}}).IsEmpty() {
		t.Error("NaN bounds must be empty")
	}
	// Negative-width constructor is normalized.
	if !NewRect(10, 10, -10, -10).Contains(Point{1, 1}) {
		t.Error("negative size should normalize")
	}
}

func TestRectTangency(t *testing.T) {
	left := NewRect(0, 0, 10, 10)
	right := NewRect(10, 0, 10, 10)
	if left.Intersects(right) {
		t.Error("rectangles touching along an edge must not intersect")
	}
	if !left.Contains(Point{9.5, 5}) || right.Contains(Point{9.5, 5}) {
		t.Error("point 9.5 must belong to the left rectangle only")
	}
	if left.Contains(Point{10, 5}) || !right.Contains(Point{10, 5}) {
		t.Error("the shared edge must belong to the right rectangle only")
	}
	if !NewRect(0, 0, 10, 10).Intersects(NewRect(5, 5, 10, 10)) {
		t.Error("overlapping rectangles must intersect")
	}
}

func TestAreaSubscribeValidation(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(0))
	defer m.Close()
	s, _ := newTestSession(t, 1)

	invalidRooms := []string{"", "   ", "\n", "\x00bad", string(make([]byte, maxAreaNameLength+1))}
	for _, room := range invalidRooms {
		if _, err := m.Subscribe(room, s, NewRect(0, 0, 1, 1)); err != ErrInvalidRoom {
			t.Errorf("Subscribe(room=%q) error = %v, want ErrInvalidRoom", room, err)
		}
	}
	if _, err := m.Subscribe("ok", nil, NewRect(0, 0, 1, 1)); err != ErrNilAreaSession {
		t.Errorf("nil session error = %v", err)
	}
	for _, r := range []Rect{
		NewRect(0, 0, 0, 10),
		NewRect(0, 0, 10, 0),
		{Min: Point{math.NaN(), 0}, Max: Point{1, 1}},
	} {
		if _, err := m.Subscribe("ok", s, r); err != ErrInvalidAreaRect {
			t.Errorf("Subscribe(%v) error = %v, want ErrInvalidAreaRect", r, err)
		}
	}
	if _, err := m.Subscribe("ok", s, NewRect(0, 0, 1, 1), WithAreaServiceRoute("bad\nroute")); err != ErrInvalidAreaRoute {
		t.Errorf("bad route error = %v", err)
	}

	if _, err := m.Broadcast("", "evt", Point{0, 0}, []byte("x")); err != ErrInvalidRoom {
		t.Errorf("broadcast invalid room error = %v", err)
	}
	if _, err := m.Broadcast("ok", "", Point{0, 0}, []byte("x")); err != ErrInvalidAreaRoute {
		t.Errorf("broadcast invalid route error = %v", err)
	}
	if _, err := m.Broadcast("ok", "evt", Point{math.Inf(1), 0}, []byte("x")); err != ErrInvalidAreaPoint {
		t.Errorf("broadcast invalid point error = %v", err)
	}
	if n := m.Count(""); n != 0 {
		t.Errorf("Count(invalid) = %d", n)
	}
}

func TestAreaRoomsAndOverlap(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(0))
	defer m.Close()

	s1, e1 := newTestSession(t, 101)
	s2, e2 := newTestSession(t, 102)
	s3, e3 := newTestSession(t, 103)

	// Room A, two overlapping viewports owned by s1.
	mustSubscribe(t, m, "A", s1, NewRect(0, 0, 100, 100))
	mustSubscribe(t, m, "A", s1, NewRect(50, 50, 100, 100))
	// Room A, disjoint viewport owned by s2.
	mustSubscribe(t, m, "A", s2, NewRect(300, 300, 10, 10))
	// Room B, overlapping the room A area for s3 only.
	mustSubscribe(t, m, "B", s3, NewRect(0, 0, 100, 100))

	if m.Count("A") != 3 || m.Count("B") != 1 {
		t.Fatalf("counts: A=%d B=%d, want 3/1", m.Count("A"), m.Count("B"))
	}
	rooms := m.Rooms()
	if len(rooms) != 2 || rooms[0] != "A" || rooms[1] != "B" {
		t.Fatalf("rooms = %v", rooms)
	}

	// Point covered by both s1 viewports and by room B's s3.
	n, err := m.Broadcast("A", "move", Point{60, 60}, []byte("p1"))
	if err != nil || n != 1 {
		t.Fatalf("room A broadcast = %d, err=%v, want 1 (overlap must deduplicate)", n, err)
	}
	if e1.pushCount() != 1 {
		t.Errorf("s1 received %d pushes, want exactly 1", e1.pushCount())
	}
	if e2.pushCount() != 0 || e3.pushCount() != 0 {
		t.Errorf("room isolation broken: s2=%d s3=%d", e2.pushCount(), e3.pushCount())
	}

	// Room B is independent and delivers to s3 only.
	if n, _ := m.Broadcast("B", "move", Point{60, 60}, []byte("p2")); n != 1 {
		t.Fatalf("room B broadcast = %d, want 1", n)
	}
	if e3.pushCount() != 1 || e1.pushCount() != 1 {
		t.Errorf("room B isolation broken: s3=%d s1=%d", e3.pushCount(), e1.pushCount())
	}

	// Point outside every room A viewport.
	if n, _ := m.Broadcast("A", "move", Point{1000, 1000}, []byte("p3")); n != 0 {
		t.Fatalf("distant point broadcast = %d, want 0", n)
	}
}

func TestAreaRecipientOrderIsDeterministic(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(0))
	defer m.Close()

	journal := &pushJournal{}
	const n = 16
	entities := make([]*recordingEntity, n)
	for i := 0; i < n; i++ {
		s, e := newNamedTestSession(t, int64(1000+i), fmt.Sprintf("p%02d", i))
		e.journal = journal
		entities[i] = e
		mustSubscribe(t, m, "room", s, NewRect(0, 0, 100, 100))
	}

	want := make([]string, n)
	for i := range want {
		want[i] = fmt.Sprintf("p%02d", i)
	}

	for round := 0; round < 20; round++ {
		for i := range entities {
			entities[i].mu.Lock()
			entities[i].pushes = nil
			entities[i].mu.Unlock()
		}
		journal.reset()
		count, err := m.Broadcast("room", "event", Point{50, 50}, []byte("x"))
		if err != nil || count != n {
			t.Fatalf("round %d: delivered=%d err=%v", round, count, err)
		}
		for i, e := range entities {
			got := e.pushed()
			if len(got) != 1 || got[0].route != "event" || got[0].data != "x" {
				t.Fatalf("round %d subscriber %d got %#v", round, i, got)
			}
		}
		if got := journal.snapshot(); !equalStrings(got, want) {
			t.Fatalf("round %d delivery order = %v, want %v", round, got, want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAreaNegativeCoordinates(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(0))
	defer m.Close()

	s, e := newTestSession(t, 7)
	mustSubscribe(t, m, "neg", s, NewRect(-100, -100, 50, 50)) // [-100,-50)x[-100,-50)

	cases := []struct {
		p    Point
		want int
	}{
		{Point{-75, -75}, 1},
		{Point{-100, -100}, 1},
		{Point{-50, -75}, 0}, // Max edge exclusive
		{Point{-75, -50}, 0},
		{Point{-101, -75}, 0},
		{Point{0, 0}, 0},
	}
	for _, c := range cases {
		e.mu.Lock()
		e.pushes = nil
		e.mu.Unlock()
		n, err := m.Broadcast("neg", "evt", c.p, []byte("x"))
		if err != nil || n != c.want {
			t.Errorf("point %v: delivered=%d err=%v, want %d", c.p, n, err, c.want)
		}
	}
}

func TestAreaBoundaryBetweenViewports(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(0))
	defer m.Close()

	s1, e1 := newTestSession(t, 201)
	s2, e2 := newTestSession(t, 202)
	// The two viewports share the x=10 edge.
	mustSubscribe(t, m, "edge", s1, NewRect(0, 0, 10, 10))
	mustSubscribe(t, m, "edge", s2, NewRect(10, 0, 10, 10))

	if n, _ := m.Broadcast("edge", "evt", Point{10, 5}, []byte("x")); n != 1 {
		t.Fatalf("shared edge delivered to %d players, want 1", n)
	}
	if e1.pushCount() != 0 || e2.pushCount() != 1 {
		t.Errorf("edge point recipients: left=%d right=%d", e1.pushCount(), e2.pushCount())
	}
	if n, _ := m.Broadcast("edge", "evt", Point{0, 5}, []byte("x")); n != 1 {
		t.Fatalf("left edge delivered to %d players", n)
	}
	if e1.pushCount() != 1 || e2.pushCount() != 1 {
		t.Errorf("after left-edge point: left=%d right=%d", e1.pushCount(), e2.pushCount())
	}
}

func TestAreaViewportUpdateMovesRecipient(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(0))
	defer m.Close()

	s, e := newTestSession(t, 9)
	sub := mustSubscribe(t, m, "room", s, NewRect(0, 0, 10, 10))

	if n, _ := m.Broadcast("room", "evt", Point{100, 100}, []byte("x")); n != 0 {
		t.Fatalf("before update delivered=%d", n)
	}
	if err := sub.Update(NewRect(90, 90, 20, 20)); err != nil {
		t.Fatalf("update: %v", err)
	}
	if n, _ := m.Broadcast("room", "evt", Point{100, 100}, []byte("x")); n != 1 {
		t.Fatalf("after update delivered=%d", n)
	}
	if e.pushCount() != 1 {
		t.Errorf("received %d", e.pushCount())
	}
	// Window moves away again.
	if err := m.UpdateViewport(sub, NewRect(0, 0, 10, 10)); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.Broadcast("room", "evt", Point{100, 100}, []byte("x")); n != 0 {
		t.Fatalf("after move-away delivered=%d", n)
	}
	// Invalid update keeps the old viewport.
	old := sub.Viewport()
	if err := sub.Update(Rect{Min: Point{0, 0}, Max: Point{0, 0}}); err != ErrInvalidAreaRect {
		t.Errorf("zero-area update error = %v", err)
	}
	if sub.Viewport() != old {
		t.Errorf("viewport changed after rejected update: %v", sub.Viewport())
	}
}

func TestAreaLeaveAndLifetimeCleanup(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(0))
	defer m.Close()

	// Unbound session: removed on close.
	s1, e1 := newTestSession(t, 0)
	mustSubscribe(t, m, "room", s1, NewRect(0, 0, 10, 10))
	// Bound session with TTL disabled: also removed on close.
	s2, e2 := newTestSession(t, 22)
	mustSubscribe(t, m, "room", s2, NewRect(0, 0, 10, 10))
	mustSubscribe(t, m, "room", s1, NewRect(5, 5, 10, 10))

	// Leaving one room keeps subscriptions in other rooms.
	mustSubscribe(t, m, "other", s1, NewRect(0, 0, 10, 10))
	if err := m.LeaveRoom("room", s1); err != nil {
		t.Fatal(err)
	}
	if m.Count("room") != 1 || m.Count("other") != 1 {
		t.Fatalf("after LeaveRoom: room=%d other=%d", m.Count("room"), m.Count("other"))
	}
	if n, _ := m.Broadcast("room", "evt", Point{1, 1}, []byte("x")); n != 1 {
		t.Fatalf("room after leave delivered=%d, want 1 (s2 only)", n)
	}
	if e1.pushCount() != 0 || e2.pushCount() != 1 {
		t.Errorf("recipients after leave: s1=%d s2=%d", e1.pushCount(), e2.pushCount())
	}

	// Session lifetime cleanup covers the normal disconnect path.
	if err := m.LeaveRoom("other", s1); err != nil {
		t.Fatal(err)
	}
	mustSubscribe(t, m, "other", s1, NewRect(0, 0, 10, 10))
	session.Lifetime.Close(s1)
	if m.Count("other") != 0 {
		t.Errorf("lifetime cleanup left %d subscriptions", m.Count("other"))
	}
	if n, _ := m.Broadcast("other", "evt", Point{1, 1}, []byte("x")); n != 0 {
		t.Errorf("closed session still receives events: %d", n)
	}

	// Leaving an unknown session/room is harmless.
	if err := m.Leave(s1); err != nil {
		t.Errorf("Leave unknown session = %v", err)
	}
	if err := m.LeaveRoom("room", s2); err != nil {
		t.Errorf("LeaveRoom = %v", err)
	}
	if err := m.RemoveRoom("room"); err != ErrRoomNotFound {
		t.Errorf("RemoveRoom(now empty) = %v, want ErrRoomNotFound", err)
	}
}

func TestAreaReconnectTransferLiveSession(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(0))
	defer m.Close()

	oldSession, oldEntity := newTestSession(t, 500)
	mustSubscribe(t, m, "room", oldSession, NewRect(0, 0, 10, 10))
	mustSubscribe(t, m, "other", oldSession, NewRect(50, 50, 10, 10))

	newSession, newEntity := newTestSession(t, 500)
	if err := m.Transfer(oldSession, newSession); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if m.Count("room") != 1 || m.Count("other") != 1 {
		t.Fatalf("after transfer: room=%d other=%d", m.Count("room"), m.Count("other"))
	}
	for _, room := range []string{"room", "other"} {
		if n, err := m.Broadcast(room, "evt", Point{1, 1}, []byte("x")); err != nil && n != 0 {
			t.Fatalf("%s: %v", room, err)
		}
	}
	// Only the "room" point is covered.
	if oldEntity.pushCount() != 0 {
		t.Errorf("old session still receives events: %d", oldEntity.pushCount())
	}
	if newEntity.pushCount() != 1 {
		t.Errorf("new session received %d, want 1", newEntity.pushCount())
	}
	// The old session is no longer tracked.
	if err := m.Leave(oldSession); err != nil {
		t.Fatal(err)
	}
	if m.Count("room") != 1 {
		t.Errorf("Leave(old) removed transferred subscriptions")
	}
}

func TestAreaReconnectAfterClose(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(time.Minute))
	defer m.Close()

	oldSession, _ := newTestSession(t, 600)
	mustSubscribe(t, m, "room", oldSession, NewRect(0, 0, 10, 10))

	// Simulate a disconnect: the cluster/gate closes the session.
	session.Lifetime.Close(oldSession)
	if m.Count("room") != 0 {
		t.Fatalf("retained subscriptions must be inactive, count=%d", m.Count("room"))
	}
	if m.Retained() != 1 {
		t.Fatalf("Retained() = %d, want 1", m.Retained())
	}
	if n, _ := m.Broadcast("room", "evt", Point{1, 1}, []byte("x")); n != 0 {
		t.Fatalf("disconnected player received %d events", n)
	}

	// Reconnect with the same UID and restore.
	newSession, newEntity := newTestSession(t, 600)
	restored, err := m.Restore(newSession)
	if err != nil || restored != 1 {
		t.Fatalf("Restore = %d, %v, want 1, nil", restored, err)
	}
	if m.Retained() != 0 {
		t.Errorf("retained batch was not consumed")
	}
	if m.Count("room") != 1 {
		t.Errorf("restored count=%d", m.Count("room"))
	}
	if n, _ := m.Broadcast("room", "evt", Point{1, 1}, []byte("x")); n != 1 {
		t.Fatalf("after restore delivered=%d", n)
	}
	if newEntity.pushCount() != 1 {
		t.Errorf("restored session received %d events", newEntity.pushCount())
	}

	// A second disconnect can be transferred directly by session id as well.
	session.Lifetime.Close(newSession)
	third, thirdEntity := newTestSession(t, 601)
	if err := m.Transfer(newSession, third); err != nil {
		t.Fatalf("transfer after close: %v", err)
	}
	if n, _ := m.Broadcast("room", "evt", Point{1, 1}, []byte("x")); n != 1 {
		t.Fatalf("after transfer delivered=%d", n)
	}
	if thirdEntity.pushCount() != 1 {
		t.Errorf("transferred session received %d", thirdEntity.pushCount())
	}

	// Restore with no retained subscriptions is a no-op; unbound session errors.
	if n, err := m.Restore(newTestSessionForUID(t, 999)); err != nil || n != 0 {
		t.Errorf("Restore without retention = %d, %v", n, err)
	}
	unbound, _ := newTestSession(t, 0)
	if _, err := m.Restore(unbound); err != ErrAreaSessionUnbound {
		t.Errorf("Restore unbound error = %v", err)
	}
}

func newTestSessionForUID(t *testing.T, uid int64) *session.Session {
	s, _ := newTestSession(t, uid)
	return s
}

func TestAreaReconnectTTLExpires(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(10 * time.Millisecond))
	defer m.Close()

	s, _ := newTestSession(t, 700)
	mustSubscribe(t, m, "room", s, NewRect(0, 0, 10, 10))
	session.Lifetime.Close(s)
	if m.Retained() != 1 {
		t.Fatalf("Retained() = %d", m.Retained())
	}
	deadline := time.Now().Add(time.Second)
	for m.Retained() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if m.Retained() != 0 {
		t.Fatalf("retained subscriptions did not expire")
	}
	late, lateEntity := newTestSession(t, 700)
	if n, err := m.Restore(late); err != nil || n != 0 {
		t.Errorf("Restore after expiry = %d, %v", n, err)
	}
	if n, _ := m.Broadcast("room", "evt", Point{1, 1}, []byte("x")); n != 0 {
		t.Errorf("expired subscription still active: %d", n)
	}
	if lateEntity.pushCount() != 0 {
		t.Errorf("late session received %d", lateEntity.pushCount())
	}
}

func TestAreaRouteBindingLifecycle(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(time.Minute))
	defer m.Close()

	s, e := newTestSession(t, 800)
	mustSubscribe(t, m, "room", s, NewRect(0, 0, 10, 10), WithAreaServiceRoute("Room"))

	// No route bound yet: dormant.
	if m.Count("room") != 0 {
		t.Fatalf("unbound route must be dormant")
	}
	if n, _ := m.Broadcast("room", "evt", Point{1, 1}, []byte("x")); n != 0 {
		t.Fatalf("dormant subscription received %d", n)
	}

	// Route bound: active.
	s.Router().Bind("Room", "node-a")
	if m.Count("room") != 1 {
		t.Fatalf("bound route not activated")
	}
	if n, _ := m.Broadcast("room", "evt", Point{1, 1}, []byte("x")); n != 1 {
		t.Fatalf("active subscription delivered=%d", n)
	}

	// Route moved to another node: dormant again, no leakage.
	s.Router().Bind("Room", "node-b")
	if m.Count("room") != 0 {
		t.Fatalf("moved route must be dormant")
	}
	if n, _ := m.Broadcast("room", "evt", Point{1, 1}, []byte("x")); n != 0 {
		t.Fatalf("moved route leaked %d events", n)
	}

	// Delete is also dormant, but the recorded address is remembered.
	s.Router().Delete("Room")
	if m.Count("room") != 0 {
		t.Fatalf("deleted route must be dormant")
	}
	s.Router().Bind("Room", "node-a")
	if m.Count("room") != 1 {
		t.Fatalf("restored route not activated")
	}
	if n, _ := m.Broadcast("room", "evt", Point{1, 1}, []byte("x")); n != 1 {
		t.Fatalf("restored route delivered=%d", n)
	}
	if e.pushCount() != 2 {
		t.Errorf("total pushes=%d, want 2", e.pushCount())
	}

	// Route moves while disconnected; reconnect on a new session that binds
	// the original address restores the subscription.
	session.Lifetime.Close(s)
	if m.Retained() != 1 {
		t.Fatalf("retained=%d", m.Retained())
	}
	reconnected, reconnectedEntity := newTestSession(t, 800)
	reconnected.Router().Bind("Room", "node-b")
	if n, err := m.Restore(reconnected); err != nil || n != 1 {
		t.Fatalf("Restore = %d, %v", n, err)
	}
	if m.Count("room") != 0 {
		t.Fatalf("restored subscription on a different node must stay dormant")
	}
	if n, _ := m.Broadcast("room", "evt", Point{1, 1}, []byte("x")); n != 0 {
		t.Fatalf("event leaked to the new node: %d", n)
	}
	reconnected.Router().Bind("Room", "node-a")
	if n, _ := m.Broadcast("room", "evt", Point{1, 1}, []byte("x")); n != 1 {
		t.Fatalf("after rebinding origin delivered=%d", n)
	}
	if reconnectedEntity.pushCount() != 1 {
		t.Errorf("reconnected session received %d", reconnectedEntity.pushCount())
	}
}

func TestAreaSlowAndFailingSessions(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(0))
	defer m.Close()

	slow, slowEntity := newTestSession(t, 900)
	slowEntity.err = fmt.Errorf("session send buffer exceed")
	healthy, healthyEntity := newTestSession(t, 901)

	mustSubscribe(t, m, "room", slow, NewRect(0, 0, 10, 10))
	mustSubscribe(t, m, "room", healthy, NewRect(0, 0, 10, 10))

	n, err := m.Broadcast("room", "evt", Point{1, 1}, []byte("x"))
	if err == nil {
		t.Error("failing session error should be reported")
	}
	if n != 1 {
		t.Errorf("delivered=%d, want 1 (healthy session must still receive)", n)
	}
	if healthyEntity.pushCount() != 1 {
		t.Errorf("healthy session received %d", healthyEntity.pushCount())
	}

	// A failed push falls back to another subscription of the same player,
	// typically the session created by a reconnect.
	fallbackA, fallbackAE := newTestSession(t, 902)
	fallbackAE.err = fmt.Errorf("stale connection")
	fallbackB, fallbackBE := newTestSession(t, 902)
	mustSubscribe(t, m, "room", fallbackA, NewRect(0, 0, 10, 10))
	mustSubscribe(t, m, "room", fallbackB, NewRect(0, 0, 10, 10))

	n, err = m.Broadcast("room", "evt", Point{1, 1}, []byte("x"))
	if err == nil {
		t.Error("stale session error should be reported")
	}
	if n != 2 { // healthy + fallbackB
		t.Errorf("delivered=%d, want 2", n)
	}
	if fallbackBE.pushCount() != 1 {
		t.Errorf("fallback session received %d, want 1", fallbackBE.pushCount())
	}
	// The stale session keeps its error but gets no duplicate event.
	if fallbackAE.pushCount() != 0 {
		t.Errorf("stale session recorded %d pushes", fallbackAE.pushCount())
	}
}

func TestAreaLargeViewportFallback(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(0), WithAreaMaxCells(4), WithAreaCellSize(1))
	defer m.Close()

	s, e := newTestSession(t, 42)
	// Spans far more than maxCells cells and has huge finite bounds.
	mustSubscribe(t, m, "room", s, Rect{Min: Point{-1e300, -1e300}, Max: Point{1e300, 1e300}})
	mustSubscribe(t, m, "room", s, NewRect(0, 0, 100, 100))
	if n, _ := m.Broadcast("room", "evt", Point{1e299, -1e299}, []byte("x")); n != 1 {
		t.Fatalf("large viewport delivered=%d", n)
	}
	if e.pushCount() != 1 {
		t.Errorf("large viewport dedup failed: %d", e.pushCount())
	}
	// Moving the large viewport back into the grid keeps working.
	sub, _ := m.Subscribe("room", s, NewRect(0, 0, 1, 1))
	if n, _ := m.Broadcast("room", "evt", Point{0.5, 0.5}, []byte("x")); n != 1 {
		t.Fatalf("small subscription dedup delivered=%d", n)
	}
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAreaSubscribeCloseSemantics(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(0))
	defer m.Close()

	s, _ := newTestSession(t, 1)
	sub := mustSubscribe(t, m, "room", s, NewRect(0, 0, 10, 10))
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sub.Close(); err != ErrAreaSubscriptionClosed {
		t.Errorf("second Close = %v", err)
	}
	if err := m.Unsubscribe(sub); err != ErrAreaSubscriptionClosed {
		t.Errorf("Unsubscribe closed = %v", err)
	}
	if m.Count("room") != 0 {
		t.Errorf("count=%d", m.Count("room"))
	}

	other := NewAreaManager(WithAreaReconnectTTL(0))
	defer other.Close()
	foreign := mustSubscribe(t, other, "room", s, NewRect(0, 0, 10, 10))
	if err := m.Unsubscribe(foreign); err != ErrAreaSubscriptionNotOwned {
		t.Errorf("foreign unsubscribe = %v", err)
	}
	if err := m.UpdateViewport(foreign, NewRect(0, 0, 1, 1)); err != ErrAreaSubscriptionNotOwned {
		t.Errorf("foreign update = %v", err)
	}

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Subscribe("room", s, NewRect(0, 0, 1, 1)); err != ErrAreaManagerClosed {
		t.Errorf("subscribe after close = %v", err)
	}
	if _, err := m.Broadcast("room", "evt", Point{0, 0}, []byte("x")); err != ErrAreaManagerClosed {
		t.Errorf("broadcast after close = %v", err)
	}
	if err := m.Close(); err != ErrAreaManagerClosed {
		t.Errorf("second manager Close = %v", err)
	}
}

func TestAreaGroupBroadcastUnchanged(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(0))
	defer m.Close()

	near, nearEntity := newTestSession(t, 1)
	far, farEntity := newTestSession(t, 2)
	group := NewGroup("all")
	if err := group.Add(near); err != nil {
		t.Fatal(err)
	}
	if err := group.Add(far); err != nil {
		t.Fatal(err)
	}

	mustSubscribe(t, m, "room", near, NewRect(0, 0, 10, 10))
	mustSubscribe(t, m, "room", far, NewRect(1000, 1000, 10, 10))

	// The original group broadcast still reaches every member.
	nearEntity.mu.Lock()
	nearEntity.pushes = nil
	nearEntity.mu.Unlock()
	farEntity.mu.Lock()
	farEntity.pushes = nil
	farEntity.mu.Unlock()

	if err := group.Broadcast("chat", []byte("hello")); err != nil {
		t.Fatalf("group broadcast: %v", err)
	}
	if nearEntity.pushCount() != 1 || farEntity.pushCount() != 1 {
		t.Fatalf("group broadcast recipients near=%d far=%d", nearEntity.pushCount(), farEntity.pushCount())
	}

	// The area broadcast reaches only the nearby session.
	if n, err := m.Broadcast("room", "move", Point{1, 1}, []byte("m")); err != nil || n != 1 {
		t.Fatalf("area broadcast = %d, %v", n, err)
	}
	if nearEntity.pushCount() != 2 || farEntity.pushCount() != 1 {
		t.Errorf("after area broadcast near=%d far=%d", nearEntity.pushCount(), farEntity.pushCount())
	}
	if group.Count() != 2 {
		t.Errorf("area subscriptions must not mutate the group, count=%d", group.Count())
	}
}

func TestAreaFilteredMulticast(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(0))
	defer m.Close()

	a, aEntity := newTestSession(t, 1)
	b, bEntity := newTestSession(t, 2)
	mustSubscribe(t, m, "room", a, NewRect(0, 0, 10, 10))
	mustSubscribe(t, m, "room", b, NewRect(0, 0, 10, 10))

	n, err := m.Multicast("room", "evt", Point{1, 1}, []byte("x"), func(s *session.Session) bool {
		return s.UID() == 2
	})
	if err != nil || n != 1 {
		t.Fatalf("filtered multicast = %d, %v", n, err)
	}
	if aEntity.pushCount() != 0 || bEntity.pushCount() != 1 {
		t.Errorf("filter recipients: a=%d b=%d", aEntity.pushCount(), bEntity.pushCount())
	}
}

func TestAreaConcurrentSubscriptionChanges(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(0))
	defer m.Close()

	const workers = 8
	const rounds = 150
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			s, e := newTestSession(t, int64(w+1))
			for i := 0; i < rounds; i++ {
				room := fmt.Sprintf("room-%d", i%3)
				sub, err := m.Subscribe(room, s, NewRect(float64(i), float64(w), 50, 50))
				if err != nil && err != ErrAreaManagerClosed {
					t.Errorf("subscribe: %v", err)
					return
				}
				if sub != nil {
					if i%7 == 0 {
						_ = sub.Close()
					} else {
						_ = sub.Update(NewRect(float64(i-10), float64(w-10), 64, 64))
					}
				}
				pt := Point{X: float64(i), Y: float64(w)}
				if _, err := m.Broadcast(room, "evt", pt, []byte("x")); err != nil && err != ErrAreaManagerClosed {
					t.Errorf("broadcast: %v", err)
					return
				}
				if i%11 == 0 {
					_ = m.LeaveRoom(room, s)
				}
				if i%13 == 0 {
					_ = m.RemoveRoom(fmt.Sprintf("room-%d", (i+1)%3))
				}
				_ = m.Retained()
				_ = m.Rooms()
				_ = m.Count(room)
				_ = e.pushCount()
			}
		}(w)
	}
	wg.Wait()

	// A final broadcast must be race-free and bounded by the number of players.
	n, err := m.Broadcast("room-0", "final", Point{0, 0}, []byte("x"))
	if err != nil && err != ErrAreaManagerClosed {
		t.Fatalf("final broadcast: %v", err)
	}
	if n > workers {
		t.Fatalf("final broadcast delivered %d events to %d players", n, workers)
	}
}

// TestAreaTwoZonesOverlapReconnectSimulation simulates a large map split into
// two zones. Each player subscribes to the zone it plays in; one player holds an
// overlapping pair of viewports in the same zone; one player reconnects in the
// middle of the run. It asserts recipients, ordering, exactly-once delivery and
// that the original Group broadcast still reaches everyone.
func TestAreaTwoZonesOverlapReconnectSimulation(t *testing.T) {
	m := NewAreaManager(WithAreaReconnectTTL(time.Minute))
	defer m.Close()

	journal := &pushJournal{}
	newPlayer := func(uid int64, name string) (*session.Session, *recordingEntity) {
		s, e := newNamedTestSession(t, uid, name)
		e.journal = journal
		return s, e
	}

	// Two zones that share the x=1000 border. Zone west is [0,1000)x[0,1000),
	// zone east is [1000,2000)x[0,1000).
	west := Rect{Min: Point{0, 0}, Max: Point{1000, 1000}}
	east := Rect{Min: Point{1000, 0}, Max: Point{2000, 1000}}

	alice, aliceE := newPlayer(1, "alice") // west
	bob, bobE := newPlayer(2, "bob")       // west
	carol, carolE := newPlayer(3, "carol") // east
	dave, daveE := newPlayer(4, "dave")    // west, calls and other zone

	mustSubscribe(t, m, "world", alice, west, WithAreaServiceRoute("World"))
	mustSubscribe(t, m, "world", bob, west, WithAreaServiceRoute("World"))
	mustSubscribe(t, m, "world", carol, east)
	// Dave holds two overlapping viewports in the west zone.
	mustSubscribe(t, m, "world", dave, NewRect(200, 200, 400, 400))
	mustSubscribe(t, m, "world", dave, NewRect(500, 500, 400, 400))

	for _, s := range []*session.Session{alice, bob, dave} {
		s.Router().Bind("World", "node-west")
	}
	if m.Count("world") != 5 {
		t.Fatalf("active subscriptions = %d, want 5", m.Count("world"))
	}

	broadcast := func(p Point) []string {
		journal.reset()
		n, err := m.Broadcast("world", "move", p, []byte("mob"))
		if err != nil {
			t.Fatalf("broadcast at %v: %v", p, err)
		}
		if n != len(journal.snapshot()) {
			t.Fatalf("broadcast at %v: count=%d journal=%v", p, n, journal.snapshot())
		}
		return journal.snapshot()
	}

	// A monster inside the west zone: only west players, overlap deduplicated.
	if got := broadcast(Point{600, 600}); !equalStrings(got, []string{"alice", "bob", "dave"}) {
		t.Fatalf("west event recipients = %v", got)
	}
	// A monster inside both of Dave's overlapping viewports must still reach
	// him exactly once, and at the position of his first subscription.
	if got := broadcast(Point{550, 550}); !equalStrings(got, []string{"alice", "bob", "dave"}) {
		t.Fatalf("overlap event recipients = %v", got)
	}
	if daveE.pushCount() != 2 {
		t.Fatalf("dave received %d events, want 2", daveE.pushCount())
	}
	// A monster on the shared border x=1000 belongs to the east zone only.
	if got := broadcast(Point{1000, 500}); !equalStrings(got, []string{"carol"}) {
		t.Fatalf("border event recipients = %v", got)
	}
	// A monster at the far end of the map reaches nobody in the west room.
	if got := broadcast(Point{1500, 500}); !equalStrings(got, []string{"carol"}) {
		t.Fatalf("east event recipients = %v", got)
	}
	if aliceE.pushCount() != 2 || bobE.pushCount() != 2 || carolE.pushCount() != 2 {
		t.Fatalf("counts alice=%d bob=%d carol=%d", aliceE.pushCount(), bobE.pushCount(), carolE.pushCount())
	}

	// Bob disconnects. His viewport is retained but inactive; events that cover
	// his old position are not delivered anywhere.
	session.Lifetime.Close(bob)
	if m.Retained() != 1 {
		t.Fatalf("retained = %d", m.Retained())
	}
	if got := broadcast(Point{500, 500}); !equalStrings(got, []string{"alice", "dave"}) {
		t.Fatalf("after bob disconnect recipients = %v", got)
	}

	// Bob reconnects: restore his viewport, which is active again.
	bob2, bob2E := newPlayer(2, "bob2")
	bob2.Router().Bind("World", "node-west")
	restored, err := m.Restore(bob2)
	if err != nil || restored != 1 {
		t.Fatalf("restore = %d, %v", restored, err)
	}
	// Restored subscriptions keep their original delivery order: bob subscribed
	// second, so he is still visited before dave.
	if got := broadcast(Point{500, 500}); !equalStrings(got, []string{"alice", "bob2", "dave"}) {
		t.Fatalf("after bob reconnect recipients = %v", got)
	}
	if bobE.pushCount() != 2 {
		t.Errorf("old bob session received %d extra events", bobE.pushCount()-2)
	}
	if bob2E.pushCount() != 1 {
		t.Errorf("new bob session received %d events, want 1", bob2E.pushCount())
	}

	// The route for the west node disappears (failover): route-bound west
	// subscriptions go dormant, while east and dave's route-independent
	// viewports keep delivering. When the route comes back they resume.
	for _, s := range []*session.Session{alice, bob2, dave} {
		s.Router().Delete("World")
	}
	if got := broadcast(Point{500, 500}); !equalStrings(got, []string{"dave"}) {
		t.Fatalf("during route outage recipients = %v", got)
	}
	if got := broadcast(Point{1500, 500}); !equalStrings(got, []string{"carol"}) {
		t.Fatalf("during route outage east recipients = %v", got)
	}
	for _, s := range []*session.Session{alice, bob2, dave} {
		s.Router().Bind("World", "node-west")
	}
	if got := broadcast(Point{500, 500}); !equalStrings(got, []string{"alice", "bob2", "dave"}) {
		t.Fatalf("after route recovery recipients = %v", got)
	}

	// The original Group broadcast is untouched and still reaches every session,
	// including the ones the area filter would skip.
	group := NewGroup("world-chat")
	for _, s := range []*session.Session{alice, bob2, carol, dave} {
		if err := group.Add(s); err != nil {
			t.Fatalf("group add: %v", err)
		}
	}
	journal.reset()
	if err := group.Broadcast("chat", []byte("hi")); err != nil {
		t.Fatalf("group broadcast: %v", err)
	}
	if got := journal.snapshot(); len(got) != 4 {
		t.Fatalf("group broadcast recipients = %v, want 4 members", got)
	}
	if group.Count() != 4 {
		t.Errorf("group count = %d", group.Count())
	}
}
