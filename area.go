// Copyright (c) nano Authors. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package nano

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/lonng/nano/internal/env"
	"github.com/lonng/nano/internal/log"
	"github.com/lonng/nano/internal/message"
	"github.com/lonng/nano/session"
)

const (
	defaultAreaCellSize     = 256.0
	defaultAreaMaxCells     = 4096
	defaultAreaReconnectTTL = 30 * time.Second
	maxAreaNameLength       = 128
	maxAreaRouteLength      = 256

	// areaCellIndexLimit bounds cell indices so that they fit safely in an
	// int64. A viewport beyond this bound is kept in the room's "large" bucket
	// and tested by rectangle intersection instead of being inserted into
	// cells.
	areaCellIndexLimit = float64(1 << 40)
	// maxAreaCellLimit bounds the per-viewport cell budget so that the cell
	// count product can never overflow int64.
	maxAreaCellLimit = 1 << 20
)

// Point is a position in world coordinates.
//
// World coordinates are not limited to the first quadrant: negative values are
// handled exactly like non-negative ones.
type Point struct {
	X float64
	Y float64
}

// IsValid reports whether both coordinates are finite (no NaN and no Inf).
func (p Point) IsValid() bool {
	return isFinite(p.X) && isFinite(p.Y)
}

func (p Point) String() string {
	return fmt.Sprintf("(%g,%g)", p.X, p.Y)
}

// Rect is an axis-aligned rectangle in world coordinates.
//
// Bounds are half-open: a point p is contained in r if and only if
//
//	r.Min.X <= p.X < r.Max.X && r.Min.Y <= p.Y < r.Max.Y
//
// The half-open rule makes adjacent rectangles unambiguous: when two
// rectangles share an edge, a point exactly on that edge belongs to the
// rectangle whose Min is the edge and to no other rectangle. It also prevents a
// point event from being delivered twice when a player's viewport happens to be
// split into touching rectangles.
//
// Min/Max may be given in any order; all operations normalize the rectangle
// first. A rectangle with zero width or zero height is empty and is rejected by
// AreaManager.Subscribe and AreaSubscription.Update.
type Rect struct {
	Min Point
	Max Point
}

// NewRect returns a rectangle with the given origin and size. A negative width
// or height is allowed and normalized, so NewRect(10, 10, -10, -10) is the
// rectangle [0,10)x[0,10).
func NewRect(x, y, width, height float64) Rect {
	return Rect{
		Min: Point{X: math.Min(x, x+width), Y: math.Min(y, y+height)},
		Max: Point{X: math.Max(x, x+width), Y: math.Max(y, y+height)},
	}
}

// Normalized returns the rectangle with Min <= Max on both axes.
func (r Rect) Normalized() Rect {
	if r.Min.X > r.Max.X {
		r.Min.X, r.Max.X = r.Max.X, r.Min.X
	}
	if r.Min.Y > r.Max.Y {
		r.Min.Y, r.Max.Y = r.Max.Y, r.Min.Y
	}
	return r
}

// Width returns Max.X - Min.X. It is negative when the rectangle is not
// normalized.
func (r Rect) Width() float64 { return r.Max.X - r.Min.X }

// Height returns Max.Y - Min.Y. It is negative when the rectangle is not
// normalized.
func (r Rect) Height() float64 { return r.Max.Y - r.Min.Y }

// IsEmpty reports whether the rectangle has no area. Non-finite coordinates
// and zero-width/zero-height rectangles are empty.
func (r Rect) IsEmpty() bool {
	n := r.Normalized()
	return !isFinite(n.Min.X) || !isFinite(n.Min.Y) ||
		!isFinite(n.Max.X) || !isFinite(n.Max.Y) ||
		!(n.Max.X > n.Min.X) || !(n.Max.Y > n.Min.Y)
}

// Contains reports whether p lies inside r using half-open bounds. Points on
// the Max edge, invalid points and empty rectangles never match.
func (r Rect) Contains(p Point) bool {
	if !p.IsValid() || r.IsEmpty() {
		return false
	}
	n := r.Normalized()
	return p.X >= n.Min.X && p.X < n.Max.X &&
		p.Y >= n.Min.Y && p.Y < n.Max.Y
}

// Intersects reports whether two rectangles overlap with positive area.
// Rectangles that merely touch along an edge do not intersect.
func (r Rect) Intersects(other Rect) bool {
	if r.IsEmpty() || other.IsEmpty() {
		return false
	}
	a, b := r.Normalized(), other.Normalized()
	return a.Min.X < b.Max.X && b.Min.X < a.Max.X &&
		a.Min.Y < b.Max.Y && b.Min.Y < a.Max.Y
}

func (r Rect) String() string {
	return fmt.Sprintf("[%g,%g)x[%g,%g)", r.Min.X, r.Max.X, r.Min.Y, r.Max.Y)
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func containsControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func validRoomName(room string) bool {
	if room == "" || len(room) > maxAreaNameLength {
		return false
	}
	// A name that is only whitespace would be indistinguishable from a missing
	// room in logs and metrics, so it is rejected.
	if strings.TrimSpace(room) == "" {
		return false
	}
	return !containsControl(room)
}

func validAreaRoute(route string) bool {
	if route == "" || len(route) > maxAreaRouteLength {
		return false
	}
	return !containsControl(route)
}

// playerID is the identity used to deduplicate deliveries for a single event.
// A bound session is identified by its UID; an unbound session is identified by
// its (process-wide unique) session ID.
type playerID struct {
	uid int64
	sid int64
}

func playerIDOf(s *session.Session) playerID {
	if uid := s.UID(); uid > 0 {
		return playerID{uid: uid}
	}
	return playerID{sid: s.ID()}
}

// AreaManagerOption configures an AreaManager.
type AreaManagerOption func(*areaManagerOptions)

type areaManagerOptions struct {
	cellSize     float64
	maxCells     int
	reconnectTTL time.Duration
}

// WithAreaCellSize sets the edge length of the spatial grid cells used to
// bucket viewports. It must be positive; invalid values are ignored. The
// default is 256 world units. Choose a value close to a typical viewport size.
func WithAreaCellSize(size float64) AreaManagerOption {
	return func(o *areaManagerOptions) {
		if size > 0 && isFinite(size) {
			o.cellSize = size
		}
	}
}

// WithAreaMaxCells sets the maximum number of grid cells a single viewport may
// occupy. Larger viewports fall back to a linear scan ("large" bucket) instead
// of being inserted into the grid. It must be positive; invalid values are
// ignored. The default is 4096 cells.
func WithAreaMaxCells(n int) AreaManagerOption {
	return func(o *areaManagerOptions) {
		if n > 0 {
			o.maxCells = n
		}
	}
}

// WithAreaReconnectTTL controls how long the subscriptions of a disconnected,
// bound session are kept so that they can be restored with
// AreaManager.Restore after a reconnect. A zero duration disables retention.
// The default is 30 seconds; retained subscriptions are inactive (receive no
// events) and are released automatically when the TTL elapses.
func WithAreaReconnectTTL(ttl time.Duration) AreaManagerOption {
	return func(o *areaManagerOptions) {
		if ttl >= 0 {
			o.reconnectTTL = ttl
		}
	}
}

// AreaOption configures a single area subscription.
type AreaOption func(*areaSubscriptionOptions)

type areaSubscriptionOptions struct {
	serviceRoute string
}

// WithAreaServiceRoute binds a subscription to a cluster service route.
//
// While the route is unbound, or while session.Router reports a different
// address than the one observed when the subscription became active, the
// subscription is dormant: it is not part of the room and receives no events.
// It is restored automatically when the recorded address is bound again, so a
// node routing change neither leaks events to the old node nor loses the
// player's viewport permanently.
func WithAreaServiceRoute(route string) AreaOption {
	return func(o *areaSubscriptionOptions) {
		o.serviceRoute = route
	}
}

type areaCell struct {
	x int64
	y int64
}

// AreaSubscription represents one rectangular viewport inside one room.
//
// A session may hold several subscriptions in the same room (for example, a
// detailed inner view and a coarse outer view). They may overlap freely; an
// event is still pushed to the player at most once.
//
// All exported methods are safe for concurrent use.
type AreaSubscription struct {
	manager  *AreaManager
	id       uint64
	room     string
	seq      uint64 // delivery order, assigned at Subscribe time
	session  *session.Session
	viewport Rect

	serviceRoute string // empty when the subscription is route independent
	routeAddr    string // address observed when the subscription was activated
	inRoom       bool   // indexed in the room and able to receive events
	indexed      bool   // present in the room's grid or large bucket
	large        bool   // indexed in the room's large bucket
	parked       bool   // retained for reconnect, not indexed anywhere
	park         *areaParked
	closed       bool

	// All fields are guarded by manager.mu.
}

// ID returns the unique subscription ID.
func (sub *AreaSubscription) ID() uint64 {
	if sub == nil {
		return 0
	}
	return sub.id
}

// Room returns the room name of the subscription.
func (sub *AreaSubscription) Room() string {
	if sub == nil {
		return ""
	}
	return sub.room
}

// Session returns the session the subscription belongs to. After a reconnect
// (AreaManager.Transfer or AreaManager.Restore) it is the new session.
func (sub *AreaSubscription) Session() *session.Session {
	if sub == nil {
		return nil
	}
	sub.manager.mu.RLock()
	defer sub.manager.mu.RUnlock()
	return sub.session
}

// Viewport returns the current viewport.
func (sub *AreaSubscription) Viewport() Rect {
	if sub == nil {
		return Rect{}
	}
	sub.manager.mu.RLock()
	defer sub.manager.mu.RUnlock()
	return sub.viewport
}

// Active reports whether the subscription can currently receive events. It is
// false while the subscription is parked for reconnect or dormant because its
// service route points elsewhere.
func (sub *AreaSubscription) Active() bool {
	if sub == nil {
		return false
	}
	sub.manager.mu.RLock()
	defer sub.manager.mu.RUnlock()
	return !sub.closed && sub.inRoom
}

// Update replaces the viewport of the subscription without changing its
// delivery order. An empty or non-finite rectangle is rejected and leaves the
// previous viewport untouched.
func (sub *AreaSubscription) Update(viewport Rect) error {
	if sub == nil {
		return ErrAreaSubscriptionClosed
	}
	m := sub.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrAreaManagerClosed
	}
	if sub.closed {
		return ErrAreaSubscriptionClosed
	}
	vp := viewport.Normalized()
	if vp.IsEmpty() {
		return ErrInvalidAreaRect
	}
	m.unindexLocked(sub)
	sub.viewport = vp
	m.indexLocked(sub)
	return nil
}

// Close removes the subscription from its room and from the reconnect
// retention set. It is safe to call Close on a subscription that was already
// removed; in that case ErrAreaSubscriptionClosed is returned.
func (sub *AreaSubscription) Close() error {
	if sub == nil {
		return ErrAreaSubscriptionClosed
	}
	m := sub.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrAreaManagerClosed
	}
	if sub.closed {
		return ErrAreaSubscriptionClosed
	}
	m.closeSubscriptionLocked(sub)
	return nil
}

type areaRoom struct {
	subs  map[uint64]*AreaSubscription
	grid  map[areaCell]map[uint64]*AreaSubscription
	large map[uint64]*AreaSubscription
}

func newAreaRoom() *areaRoom {
	return &areaRoom{
		subs:  make(map[uint64]*AreaSubscription),
		grid:  make(map[areaCell]map[uint64]*AreaSubscription),
		large: make(map[uint64]*AreaSubscription),
	}
}

type areaSessionState struct {
	session *session.Session
	subs    map[uint64]*AreaSubscription
	// cancelRoute is a per-session router watcher. It is installed once when
	// the session state is created and cancelled when the state is dropped, so
	// overlapping subscriptions never stack watchers.
	cancelRoute func()
}

// areaParked is a batch of subscriptions retained after a disconnect so that a
// reconnecting session can restore them.
type areaParked struct {
	uid   int64
	sid   int64
	token uint64
	subs  map[uint64]*AreaSubscription
	timer *time.Timer
}

// AreaManager owns area subscriptions and delivers point events only to the
// viewports that cover the event position.
//
// Subscriptions are scoped by room. Rooms are independent: subscribing,
// mutating and broadcasting in one room never affects another. Overlapping
// viewports are fully supported and every event is delivered at most once per
// player (see AreaManager.Multicast).
//
// Subscriptions of a closed session are removed automatically via
// session.Lifetime.OnClosed. Bound sessions may additionally retain their
// subscriptions for a grace period so a reconnect can restore them with
// AreaManager.Restore.
//
// Call Close when the manager is no longer needed.
type AreaManager struct {
	mu       sync.RWMutex
	closed   bool
	cellSize float64
	maxCells int
	ttl      time.Duration

	rooms    map[string]*areaRoom
	sessions map[int64]*areaSessionState
	nextSub  uint64

	parkedByUID map[int64]*areaParked
	parkedBySID map[int64]*areaParked
	nextPark    uint64
}

var (
	areaManagerMu   sync.Mutex
	areaManagerSet  = make(map[*AreaManager]struct{})
	areaLifetimeOne sync.Once
)

// NewAreaManager returns an empty area manager. Options may tune the spatial
// grid and the reconnect retention period.
//
// The first manager created in a process installs a
// session.Lifetime.OnClosed callback that removes that session's subscriptions
// from every live manager. This makes cleanup work for ordinary disconnects and
// for cluster-wide session-closed notifications without extra wiring.
func NewAreaManager(opts ...AreaManagerOption) *AreaManager {
	cfg := areaManagerOptions{
		cellSize:     defaultAreaCellSize,
		maxCells:     defaultAreaMaxCells,
		reconnectTTL: defaultAreaReconnectTTL,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.cellSize <= 0 {
		cfg.cellSize = defaultAreaCellSize
	}
	if cfg.maxCells <= 0 {
		cfg.maxCells = defaultAreaMaxCells
	}
	// Keep the cell count product far away from int64 overflow.
	if cfg.maxCells > maxAreaCellLimit {
		cfg.maxCells = maxAreaCellLimit
	}

	m := &AreaManager{
		cellSize:    cfg.cellSize,
		maxCells:    cfg.maxCells,
		ttl:         cfg.reconnectTTL,
		rooms:       make(map[string]*areaRoom),
		sessions:    make(map[int64]*areaSessionState),
		parkedByUID: make(map[int64]*areaParked),
		parkedBySID: make(map[int64]*areaParked),
	}
	registerAreaManager(m)
	return m
}

func registerAreaManager(m *AreaManager) {
	areaLifetimeOne.Do(func() {
		session.Lifetime.OnClosed(func(s *session.Session) {
			areaManagerMu.Lock()
			managers := make([]*AreaManager, 0, len(areaManagerSet))
			for manager := range areaManagerSet {
				managers = append(managers, manager)
			}
			areaManagerMu.Unlock()
			for _, manager := range managers {
				manager.onSessionClosed(s)
			}
		})
	})
	areaManagerMu.Lock()
	areaManagerSet[m] = struct{}{}
	areaManagerMu.Unlock()
}

func unregisterAreaManager(m *AreaManager) {
	areaManagerMu.Lock()
	delete(areaManagerSet, m)
	areaManagerMu.Unlock()
}

// Close removes every subscription (including retained reconnect batches) and
// releases all resources held by the manager. Subsequent calls that mutate the
// manager return ErrAreaManagerClosed; reads keep working on the empty state.
func (m *AreaManager) Close() error {
	if m == nil {
		return ErrAreaManagerClosed
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrAreaManagerClosed
	}
	m.closed = true
	for _, st := range m.sessions {
		if st.cancelRoute != nil {
			st.cancelRoute()
			st.cancelRoute = nil
		}
		for _, sub := range st.subs {
			sub.closed = true
		}
	}
	for _, p := range m.parkedByUID {
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		for _, sub := range p.subs {
			sub.closed = true
		}
	}
	m.rooms = make(map[string]*areaRoom)
	m.sessions = make(map[int64]*areaSessionState)
	m.parkedByUID = make(map[int64]*areaParked)
	m.parkedBySID = make(map[int64]*areaParked)
	m.mu.Unlock()

	unregisterAreaManager(m)
	return nil
}

// Subscribe registers a rectangular viewport for s in room and returns its
// handle. The rectangle is normalized and must have positive area.
//
// If room does not exist it is created. A session may subscribe several times
// in the same room; overlapping subscriptions never cause duplicate delivery
// for one event.
func (m *AreaManager) Subscribe(room string, s *session.Session, viewport Rect, opts ...AreaOption) (*AreaSubscription, error) {
	if !validRoomName(room) {
		return nil, ErrInvalidRoom
	}
	if s == nil {
		return nil, ErrNilAreaSession
	}
	vp := viewport.Normalized()
	if vp.IsEmpty() {
		return nil, ErrInvalidAreaRect
	}

	var cfg areaSubscriptionOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.serviceRoute != "" && !validAreaRoute(cfg.serviceRoute) {
		return nil, ErrInvalidAreaRoute
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrAreaManagerClosed
	}

	// A fresh subscription for a UID supersedes any viewport retained from an
	// earlier connection of the same player. Keeping both would resurrect a
	// stale viewport (and deliver events for an area the player already left).
	if uid := s.UID(); uid > 0 {
		if stale := m.parkedByUID[uid]; stale != nil {
			m.dropParkedLocked(stale)
		}
	}

	st := m.sessions[s.ID()]
	if st == nil {
		st = &areaSessionState{
			session: s,
			subs:    make(map[uint64]*AreaSubscription),
		}
		// Keep a watcher on the session router so route-bound subscriptions
		// react to Bind/Delete. It is installed once per session.
		st.cancelRoute = s.Router().OnChange(func(service, address string) {
			m.onRouteChange(s, service, address)
		})
		m.sessions[s.ID()] = st
	} else {
		st.session = s
	}

	m.nextSub++
	sub := &AreaSubscription{
		manager:      m,
		id:           m.nextSub,
		room:         room,
		seq:          m.nextSub,
		session:      s,
		viewport:     vp,
		serviceRoute: cfg.serviceRoute,
	}
	if sub.serviceRoute == "" {
		sub.inRoom = true
	} else if addr, ok := s.Router().Find(sub.serviceRoute); ok {
		sub.routeAddr = addr
		sub.inRoom = true
	}
	// Otherwise the subscription is dormant until the route is bound.

	st.subs[sub.id] = sub
	m.indexLocked(sub)
	return sub, nil
}

// UpdateViewport replaces the viewport of an existing subscription. It is a
// convenience wrapper around AreaSubscription.Update.
func (m *AreaManager) UpdateViewport(sub *AreaSubscription, viewport Rect) error {
	if sub == nil {
		return ErrAreaSubscriptionClosed
	}
	if sub.manager != m {
		return ErrAreaSubscriptionNotOwned
	}
	return sub.Update(viewport)
}

// Unsubscribe removes sub from m. It is a convenience wrapper around
// AreaSubscription.Close.
func (m *AreaManager) Unsubscribe(sub *AreaSubscription) error {
	if sub == nil {
		return ErrAreaSubscriptionClosed
	}
	if sub.manager != m {
		return ErrAreaSubscriptionNotOwned
	}
	return sub.Close()
}

// LeaveRoom removes every subscription of s in room without retaining it for
// reconnect. It is not an error if the session has no subscription in that
// room.
func (m *AreaManager) LeaveRoom(room string, s *session.Session) error {
	if !validRoomName(room) {
		return ErrInvalidRoom
	}
	if s == nil {
		return ErrNilAreaSession
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	st := m.sessions[s.ID()]
	if st == nil {
		return nil
	}
	subs := make([]*AreaSubscription, 0, len(st.subs))
	for _, sub := range st.subs {
		if sub.room == room {
			subs = append(subs, sub)
		}
	}
	for _, sub := range subs {
		m.closeSubscriptionLocked(sub)
	}
	return nil
}

// Leave removes every subscription of s from every room without retaining them
// for reconnect. It is safe to call for an unknown or already closed session,
// and it is a no-op after Close.
//
// Leave is called automatically when a session is closed (retention aside), so
// applications normally do not need to call it on disconnect.
func (m *AreaManager) Leave(s *session.Session) error {
	if s == nil {
		return ErrNilAreaSession
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	st := m.sessions[s.ID()]
	if st == nil {
		if p := m.parkedBySID[s.ID()]; p != nil {
			m.dropParkedLocked(p)
		}
		return nil
	}
	subs := make([]*AreaSubscription, 0, len(st.subs))
	for _, sub := range st.subs {
		subs = append(subs, sub)
	}
	for _, sub := range subs {
		m.closeSubscriptionLocked(sub)
	}
	return nil
}

// Transfer moves every subscription of oldSession to newSession. It is the
// reconnect counterpart of Leave: room, viewport and delivery order are
// preserved, subscriptions are rebound to the new session, and route-bound
// subscriptions are reconciled against the new session's router.
//
// Transfer works in two situations:
//
//   - the old session is still alive (handoff before the connection is torn
//     down), and
//   - the old session has already been closed but its subscriptions are still
//     retained by WithAreaReconnectTTL.
//
// In both cases the subscriptions become active on newSession immediately. If
// nothing is retained for oldSession, Transfer is a no-op. Both sessions must
// be non-nil.
func (m *AreaManager) Transfer(oldSession, newSession *session.Session) error {
	if oldSession == nil || newSession == nil {
		return ErrNilAreaSession
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrAreaManagerClosed
	}
	if oldSession.ID() == newSession.ID() {
		return nil
	}

	src := m.sessions[oldSession.ID()]
	var parked *areaParked
	if src == nil {
		parked = m.parkedBySID[oldSession.ID()]
	}
	if src == nil && parked == nil {
		return nil
	}

	dst := m.sessions[newSession.ID()]
	if dst == nil {
		dst = &areaSessionState{
			session: newSession,
			subs:    make(map[uint64]*AreaSubscription),
		}
		m.sessions[newSession.ID()] = dst
	}

	if src != nil {
		if src.cancelRoute != nil {
			src.cancelRoute()
			src.cancelRoute = nil
		}
		delete(m.sessions, oldSession.ID())
		for id, sub := range src.subs {
			sub.session = newSession
			dst.subs[id] = sub
		}
	}
	if parked != nil {
		m.takeParkedLocked(parked)
		for id, sub := range parked.subs {
			sub.session = newSession
			sub.parked = false
			sub.park = nil
			dst.subs[id] = sub
		}
	}

	m.installWatcherLocked(dst)
	for _, sub := range dst.subs {
		m.activateLocked(sub)
	}
	return nil
}

// Restore binds the subscriptions retained for s.UID() to s after a reconnect.
// It is intended to be called once the new session has been bound to the same
// UID. Restore returns the number of subscriptions that were restored (zero is
// not an error).
//
// A session without a UID cannot be matched to retained subscriptions and
// yields ErrAreaSessionUnbound.
func (m *AreaManager) Restore(s *session.Session) (int, error) {
	if s == nil {
		return 0, ErrNilAreaSession
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, ErrAreaManagerClosed
	}
	uid := s.UID()
	if uid <= 0 {
		return 0, ErrAreaSessionUnbound
	}
	p := m.parkedByUID[uid]
	if p == nil {
		return 0, nil
	}
	m.takeParkedLocked(p)

	st := m.sessions[s.ID()]
	if st == nil {
		st = &areaSessionState{
			session: s,
			subs:    make(map[uint64]*AreaSubscription),
		}
		m.sessions[s.ID()] = st
	}
	m.installWatcherLocked(st)

	for id, sub := range p.subs {
		sub.session = s
		sub.parked = false
		sub.park = nil
		st.subs[id] = sub
	}
	for _, sub := range st.subs {
		m.activateLocked(sub)
	}
	return len(p.subs), nil
}

// Retained reports how many subscription batches are currently retained for
// reconnect.
func (m *AreaManager) Retained() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.parkedByUID)
}

// RemoveRoom closes every subscription in room, including dormant and retained
// subscriptions, and forgets the room. ErrRoomNotFound is returned when the
// room does not exist (neither actively nor as retained subscriptions).
func (m *AreaManager) RemoveRoom(room string) error {
	if !validRoomName(room) {
		return ErrInvalidRoom
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrAreaManagerClosed
	}

	var subs []*AreaSubscription
	for _, st := range m.sessions {
		for _, sub := range st.subs {
			if sub.room == room {
				subs = append(subs, sub)
			}
		}
	}
	for _, p := range m.parkedByUID {
		for _, sub := range p.subs {
			if sub.room == room {
				subs = append(subs, sub)
			}
		}
	}
	if len(subs) == 0 {
		return ErrRoomNotFound
	}
	for _, sub := range subs {
		m.closeSubscriptionLocked(sub)
	}
	delete(m.rooms, room)
	return nil
}

// Count returns the number of active subscriptions in room. Dormant and
// retained subscriptions are not counted. Invalid or unknown rooms return zero.
func (m *AreaManager) Count(room string) int {
	if m == nil || !validRoomName(room) {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	r := m.rooms[room]
	if r == nil {
		return 0
	}
	return len(r.subs)
}

// Rooms returns the names of rooms that currently have active subscriptions,
// sorted in ascending order.
func (m *AreaManager) Rooms() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	rooms := make([]string, 0, len(m.rooms))
	for name := range m.rooms {
		rooms = append(rooms, name)
	}
	sort.Strings(rooms)
	return rooms
}

// Broadcast pushes v on route to every active subscription whose viewport
// covers p. It returns the number of distinct players that received the event.
//
// Delivery contract:
//
//   - Every player (UID for bound sessions, session ID otherwise) receives an
//     event at most once, even if several of its subscriptions cover p.
//   - Recipients are visited in subscription order (the order in which their
//     first matching subscription was created), so a simulated run is
//     deterministic.
//   - A failing or slow session does not prevent delivery to the others. A
//     local session whose write backlog is full reports ErrBufferExceed and is
//     treated as a failed target; delivery then falls back to another of the
//     player's matching subscriptions (typically the session created by a
//     reconnect). The first push error is returned after all recipients have
//     been attempted.
//   - An event snapshots the subscriptions at the moment Broadcast starts.
//     Concurrent Subscribe/Update/Close calls take effect for subsequent
//     events; they never cause an event to be delivered twice.
//   - Subscriptions parked for reconnect or dormant because their service
//     route moved away are skipped.
//
// Broadcast does not replace Group.Broadcast. Group semantics are unchanged
// and both can be used together (for example, chat messages through the group
// and position updates through an area).
func (m *AreaManager) Broadcast(room, route string, p Point, v interface{}) (int, error) {
	return m.Multicast(room, route, p, v, nil)
}

// Multicast is Broadcast with a session filter. The filter is evaluated for
// each candidate session before the push; a nil filter accepts every session.
func (m *AreaManager) Multicast(room, route string, p Point, v interface{}, filter SessionFilter) (int, error) {
	if !validRoomName(room) {
		return 0, ErrInvalidRoom
	}
	if !validAreaRoute(route) {
		return 0, ErrInvalidAreaRoute
	}
	if !p.IsValid() {
		return 0, ErrInvalidAreaPoint
	}

	data, err := message.Serialize(v)
	if err != nil {
		return 0, err
	}

	type target struct {
		seq     uint64
		session *session.Session
	}

	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return 0, ErrAreaManagerClosed
	}
	r := m.rooms[room]
	if r == nil {
		m.mu.RUnlock()
		return 0, nil
	}
	targets := make([]target, 0, 8)
	collect := func(sub *AreaSubscription) {
		if sub.closed || !sub.inRoom {
			return
		}
		if sub.viewport.Contains(p) {
			targets = append(targets, target{seq: sub.seq, session: sub.session})
		}
	}
	if cell, ok := m.cellOf(p); ok {
		for _, sub := range r.grid[cell] {
			collect(sub)
		}
	}
	for _, sub := range r.large {
		collect(sub)
	}
	m.mu.RUnlock()

	if len(targets) == 0 {
		return 0, nil
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].seq < targets[j].seq })

	// Group by player while preserving the first (lowest sequence) occurrence
	// of each player. This gives deterministic recipient ordering and lets a
	// failed push fall back to another subscription of the same player.
	type group struct {
		key     playerID
		targets []target
	}
	groups := make([]group, 0, len(targets))
	groupIndex := make(map[playerID]int, len(targets))
	for _, t := range targets {
		key := playerIDOf(t.session)
		idx, ok := groupIndex[key]
		if !ok {
			idx = len(groups)
			groupIndex[key] = idx
			groups = append(groups, group{key: key})
		}
		groups[idx].targets = append(groups[idx].targets, t)
	}

	var delivered int
	var firstErr error
	for i := range groups {
		for _, t := range groups[i].targets {
			if filter != nil && !filter(t.session) {
				continue
			}
			if err := t.session.Push(route, data); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				log.Println(fmt.Sprintf("Area push error, room=%s, route=%s, ID=%d, UID=%d, error=%s",
					room, route, t.session.ID(), t.session.UID(), err.Error()))
				continue
			}
			delivered++
			break
		}
	}

	if env.Debug {
		log.Println(fmt.Sprintf("Area broadcast %s, room=%s, point=%s, recipients=%d",
			route, room, p, delivered))
	}
	return delivered, firstErr
}

// indexLocked inserts sub into its room's grid (or large bucket) when it is
// active. The caller must hold m.mu.
func (m *AreaManager) indexLocked(sub *AreaSubscription) {
	if sub.closed || !sub.inRoom || sub.indexed {
		return
	}
	r := m.rooms[sub.room]
	if r == nil {
		r = newAreaRoom()
		m.rooms[sub.room] = r
	}
	r.subs[sub.id] = sub
	sub.indexed = true
	sub.large = false
	cells, ok := m.cellsFor(sub.viewport)
	if !ok {
		r.large[sub.id] = sub
		sub.large = true
		return
	}
	for _, cell := range cells {
		bucket := r.grid[cell]
		if bucket == nil {
			bucket = make(map[uint64]*AreaSubscription)
			r.grid[cell] = bucket
		}
		bucket[sub.id] = sub
	}
}

// unindexLocked removes sub from the room's index. It recomputes the cells from
// the current viewport, so it must run before the viewport changes. The caller
// must hold m.mu.
func (m *AreaManager) unindexLocked(sub *AreaSubscription) {
	if !sub.indexed {
		return
	}
	sub.indexed = false
	r := m.rooms[sub.room]
	if r == nil {
		sub.large = false
		return
	}
	delete(r.subs, sub.id)
	if sub.large {
		delete(r.large, sub.id)
		sub.large = false
	} else if cells, ok := m.cellsFor(sub.viewport); ok {
		for _, cell := range cells {
			bucket := r.grid[cell]
			if bucket == nil {
				continue
			}
			delete(bucket, sub.id)
			if len(bucket) == 0 {
				delete(r.grid, cell)
			}
		}
	}
	if len(r.subs) == 0 && len(r.large) == 0 {
		delete(m.rooms, sub.room)
	}
}

// cellsFor returns the grid cells covered by vp. ok is false when the viewport
// must use the large bucket (too many cells, or cell indices out of range).
func (m *AreaManager) cellsFor(vp Rect) ([]areaCell, bool) {
	n := vp.Normalized()
	cs := m.cellSize
	x0f := math.Floor(n.Min.X / cs)
	y0f := math.Floor(n.Min.Y / cs)
	x1f := math.Floor(math.Nextafter(n.Max.X, math.Inf(-1)) / cs)
	y1f := math.Floor(math.Nextafter(n.Max.Y, math.Inf(-1)) / cs)
	if !isFinite(x0f) || !isFinite(y0f) || !isFinite(x1f) || !isFinite(y1f) ||
		math.Abs(x0f) > areaCellIndexLimit || math.Abs(x1f) > areaCellIndexLimit ||
		math.Abs(y0f) > areaCellIndexLimit || math.Abs(y1f) > areaCellIndexLimit {
		return nil, false
	}
	x0, x1 := int64(x0f), int64(x1f)
	y0, y1 := int64(y0f), int64(y1f)
	nx := x1 - x0 + 1
	ny := y1 - y0 + 1
	if nx <= 0 || ny <= 0 {
		return nil, false
	}
	if nx > int64(m.maxCells) || ny > int64(m.maxCells) || nx*ny > int64(m.maxCells) {
		return nil, false
	}
	cells := make([]areaCell, 0, nx*ny)
	for x := x0; x <= x1; x++ {
		for y := y0; y <= y1; y++ {
			cells = append(cells, areaCell{x: x, y: y})
		}
	}
	return cells, true
}

func (m *AreaManager) cellOf(p Point) (areaCell, bool) {
	x := math.Floor(p.X / m.cellSize)
	y := math.Floor(p.Y / m.cellSize)
	if !isFinite(x) || !isFinite(y) || math.Abs(x) > areaCellIndexLimit || math.Abs(y) > areaCellIndexLimit {
		return areaCell{}, false
	}
	return areaCell{x: int64(x), y: int64(y)}, true
}

// closeSubscriptionLocked closes sub and detaches it from every index,
// session state and retained batch. The caller must hold m.mu.
func (m *AreaManager) closeSubscriptionLocked(sub *AreaSubscription) {
	if sub.closed {
		return
	}
	sub.closed = true
	wasParked := sub.parked
	sub.parked = false
	m.unindexLocked(sub)
	sub.inRoom = false
	if wasParked {
		if p := sub.park; p != nil {
			delete(p.subs, sub.id)
			sub.park = nil
			if len(p.subs) == 0 {
				m.dropParkedLocked(p)
			}
		}
		return
	}
	st := m.sessions[sub.session.ID()]
	if st == nil {
		return
	}
	delete(st.subs, sub.id)
	if len(st.subs) == 0 {
		if st.cancelRoute != nil {
			st.cancelRoute()
			st.cancelRoute = nil
		}
		delete(m.sessions, sub.session.ID())
	}
}

// installWatcherLocked installs a router watcher on st.session if one is not
// installed yet. The caller must hold m.mu.
func (m *AreaManager) installWatcherLocked(st *areaSessionState) {
	if st.cancelRoute != nil {
		return
	}
	s := st.session
	st.cancelRoute = s.Router().OnChange(func(service, address string) {
		m.onRouteChange(s, service, address)
	})
}

// onRouteChange reconciles the subscriptions of s after a Bind/Delete on its
// router. Bind passes the new address; Delete passes the empty string.
func (m *AreaManager) onRouteChange(s *session.Session, service, address string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	st := m.sessions[s.ID()]
	if st == nil || st.session != s {
		return
	}
	for _, sub := range st.subs {
		if sub.closed || sub.serviceRoute != service {
			continue
		}
		switch {
		case address == "":
			// The route disappeared: stop delivering. The recorded address is
			// kept so that binding it again restores the subscription.
			sub.inRoom = false
		case sub.routeAddr == "":
			// First binding after subscription: adopt it.
			sub.routeAddr = address
			sub.inRoom = true
		case address == sub.routeAddr:
			// The original route is back (possibly after a reconnect).
			sub.inRoom = true
		default:
			// The route moved to another node: stay dormant and keep waiting
			// for the recorded address.
			sub.inRoom = false
		}
		m.applyRoomStateLocked(sub)
	}
}

// activateLocked re-evaluates a subscription after it was attached to a new
// session (Transfer or Restore). Route-independent subscriptions become active
// immediately; route-bound subscriptions become active only when the new
// session's router already points at the recorded address (or has no recorded
// address yet). The caller must hold m.mu.
func (m *AreaManager) activateLocked(sub *AreaSubscription) {
	if sub.closed {
		return
	}
	before := sub.inRoom
	if sub.serviceRoute == "" {
		sub.inRoom = true
	} else if addr, ok := sub.session.Router().Find(sub.serviceRoute); ok {
		if sub.routeAddr == "" || addr == sub.routeAddr {
			sub.routeAddr = addr
			sub.inRoom = true
		} else {
			sub.inRoom = false
		}
	} else {
		sub.inRoom = false
	}
	if before != sub.inRoom {
		m.applyRoomStateLocked(sub)
	}
}

// applyRoomStateLocked indexes or unindexes sub after sub.inRoom changed. The
// caller must hold m.mu.
func (m *AreaManager) applyRoomStateLocked(sub *AreaSubscription) {
	if sub.closed {
		return
	}
	if sub.inRoom {
		m.indexLocked(sub)
	} else {
		m.unindexLocked(sub)
	}
}

// onSessionClosed is invoked when a session is closed. Bound sessions with a
// positive reconnect TTL retain their subscriptions (inactive); everything else
// is removed.
func (m *AreaManager) onSessionClosed(s *session.Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	st := m.sessions[s.ID()]
	if st == nil {
		return
	}
	if st.cancelRoute != nil {
		st.cancelRoute()
		st.cancelRoute = nil
	}
	delete(m.sessions, s.ID())

	uid := s.UID()
	if m.ttl <= 0 || uid <= 0 || len(st.subs) == 0 {
		for _, sub := range st.subs {
			m.closeSubscriptionLocked(sub)
		}
		return
	}

	m.nextPark++
	p := &areaParked{
		uid:   uid,
		sid:   s.ID(),
		token: m.nextPark,
		subs:  make(map[uint64]*AreaSubscription, len(st.subs)),
	}
	for id, sub := range st.subs {
		sub.inRoom = false
		sub.parked = true
		sub.park = p
		m.unindexLocked(sub)
		p.subs[id] = sub
	}

	// The newest disconnect for a UID wins; older retained batches are stale.
	if old := m.parkedByUID[uid]; old != nil {
		m.dropParkedLocked(old)
	}
	m.parkedByUID[uid] = p
	m.parkedBySID[s.ID()] = p
	p.timer = time.AfterFunc(m.ttl, func() {
		m.expireParked(p)
	})
}

func (m *AreaManager) expireParked(p *areaParked) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || p.token == 0 {
		return
	}
	if m.parkedByUID[p.uid] != p && m.parkedBySID[p.sid] != p {
		return
	}
	m.dropParkedLocked(p)
}

// dropParkedLocked releases a retained batch and closes its subscriptions. The
// caller must hold m.mu.
func (m *AreaManager) dropParkedLocked(p *areaParked) {
	if p == nil {
		return
	}
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	delete(m.parkedByUID, p.uid)
	if m.parkedBySID[p.sid] == p {
		delete(m.parkedBySID, p.sid)
	}
	for _, sub := range p.subs {
		sub.closed = true
		sub.parked = false
		sub.park = nil
	}
	p.subs = make(map[uint64]*AreaSubscription)
}

// takeParkedLocked detaches a retained batch from the lookup maps without
// closing its subscriptions. The caller must hold m.mu.
func (m *AreaManager) takeParkedLocked(p *areaParked) {
	if p == nil {
		return
	}
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	delete(m.parkedByUID, p.uid)
	if m.parkedBySID[p.sid] == p {
		delete(m.parkedBySID, p.sid)
	}
}
