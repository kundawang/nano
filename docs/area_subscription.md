# Area subscriptions (viewports)

`Group` broadcasts to every member. That is the right tool for chat, but it is
the wrong tool for positions in a large map: one monster move would be pushed to
every player on the map. `AreaManager` adds *viewport subscriptions* on top of
the existing primitives and delivers a point event only to the sessions whose
rectangle covers the event position.

`Group` is unchanged and can be used next to `AreaManager` in the same
component (for example, chat through a group and positions through an area).

## Quick start

```go
type World struct {
    component.Base
    areas *nano.AreaManager
}

func (w *World) Init() {
    w.areas = nano.NewAreaManager() // cleanup on session close is automatic
}

// Enter subscribes the player's viewport in a room.
func (w *World) Enter(s *session.Session, msg *EnterRequest) error {
    s.Bind(msg.UID)
    _, err := w.areas.Subscribe("map-1", s, nano.NewRect(msg.X-20, msg.Y-15, 40, 30))
    return err
}

// Move updates the viewport and pushes the new position to nearby players.
func (w *World) Move(s *session.Session, msg *MoveRequest) error {
    if err := w.areas.UpdateViewport(s.Value("sub").(*nano.AreaSubscription),
        nano.NewRect(msg.X-20, msg.Y-15, 40, 30)); err != nil {
        return err
    }
    _, err := w.areas.Broadcast("map-1", "onMove", nano.Point{X: msg.X, Y: msg.Y}, msg)
    return err
}
```

A complete component lives in [`examples/area`](../examples/area).

## Geometry

```go
type Point struct{ X, Y float64 }
type Rect  struct{ Min, Max Point }

nano.NewRect(x, y, width, height) // negative width/height is normalized
r.Contains(p)                    // half-open bounds
r.Intersects(other)              // touching edges do not intersect
```

Bounds are **half-open**: `Min <= p < Max` on both axes. Consequences, all
covered by tests:

* **Negative coordinates** work like positive ones (`[-100,-50)x[-100,-50)`).
* **Touching / shared boundaries** are unambiguous. Two rectangles that share
  the edge `x = 10` do not both contain `(10, y)`: the point belongs to the
  rectangle whose `Min.X` is `10`. No event is delivered twice for one point.
* **Zero area** (zero width or height), inverted rectangles that normalize to
  zero area, and NaN/Inf bounds are rejected with `ErrInvalidAreaRect`. A
  rejected `Update` leaves the previous viewport untouched.
* World coordinates are finite `float64`; `NaN`/`Inf` points are rejected with
  `ErrInvalidAreaPoint`.

Internally viewports are bucketed in a spatial grid (default cell size 256 world
units). `WithAreaCellSize` and `WithAreaMaxCells` tune it; a viewport that spans
more cells than `maxCells` (or has gigantic finite bounds) is kept in a linear
"large" bucket and tested with `Rect.Intersects`, so huge or absurd viewports
still work correctly.

## Rooms and overlapping viewports

* Every subscription belongs to a **room** (string). Rooms are independent and
  may overlap in world space; an event is only ever delivered within the room it
  is broadcast to.
* A session may hold **several subscriptions in the same room**. Overlapping
  viewports are supported, and an event is pushed to the same player **at most
  once**, even if five of their viewports cover the point.
* Deduplication key: `UID` for bound sessions, session ID for unbound ones.
* Recipients are visited in **subscription order** (the order in which each
  player's first matching viewport was created), so simulated runs are
  deterministic.
* `Count(room)` and `Rooms()` report active subscriptions; `RemoveRoom` closes
  everything in a room.

```go
m.Subscribe("map-1", s, inner, nano.WithAreaServiceRoute("World"))
m.Subscribe("map-1", s, outer) // same room, overlapping; still one push per event
```

## Session lifecycle, reconnect, and cluster routing

| Situation | Behaviour |
| --- | --- |
| `sub.Close()` / `Unsubscribe` | Removed immediately; other subscriptions and rooms are untouched. |
| `LeaveRoom` / `Leave` | Removes one room / all rooms without retention. |
| Session closed (`session.Lifetime.Close`, gate disconnect, `SessionClosed` RPC) | Subscriptions are removed automatically. With `WithAreaReconnectTTL` (default 30s) and a bound UID, they are instead **retained but inactive**: no event is delivered while disconnected. |
| Reconnect | `Restore(newSession)` moves the retained viewports (matched by UID) onto the new session. Alternatively `Transfer(old, new)` works both before and after the old session closes. Viewport, room and delivery order are preserved. |
| Retained TTL elapses | Retained subscriptions are released and closed. |
| New `Subscribe` for a UID with a retained batch | The retained batch is discarded (stale viewport does not come back). |
| Service route moves away (`Bind` to another address) | Route-bound subscriptions become dormant; no events are pushed until the recorded address is bound again. |
| Member leaves the cluster (`Unregister`/`DelMember`) | Session router bindings to that address are purged (`Router.PurgeAddress`), so route-bound subscriptions go dormant and messages are not dispatched to the dead node. |
| `AreaManager.Close()` | Everything is closed; mutating calls return `ErrAreaManagerClosed`. |

`session.Lifetime.OnClosed` is now safe to call concurrently, which matters when
the callback list is grown while sessions disconnect on other goroutines.

## Delivering events

```go
n, err := m.Broadcast(room, "onMove", nano.Point{X: 12, Y: 7}, payload)
n, err := m.Multicast(room, "onMove", point, payload, func(s *session.Session) bool { ... })
```

* `v` is serialized once and pushed with `session.Push`, exactly like
  `Group.Broadcast`.
* **Slow / failing sessions** do not block the rest: a full write backlog
  (`ErrBufferExceed`) or a closed connection is logged, delivery continues to
  every other player, and the first error is returned. If a push fails, the
  player's next matching subscription is tried (usually the session created by a
  reconnect). The returned count is the number of distinct players that actually
  received the event.
* **Concurrent subscription changes** are safe: each event snapshots the
  matching subscriptions under the manager lock and pushes outside it, so
  `Subscribe`/`Update`/`Close` racing with `Broadcast` never blocks readers and
  never causes a duplicate. Go's race detector is used by the test suite.

## Errors

| Error | When |
| --- | --- |
| `ErrInvalidRoom` | Empty/whitespace/control-character/oversized room name. |
| `ErrRoomNotFound` | `RemoveRoom` on a room without subscriptions. |
| `ErrNilAreaSession` | `Subscribe`/`Leave`/`Transfer`/`Restore` with a nil session. |
| `ErrInvalidAreaRect` | Zero-area or non-finite viewport. |
| `ErrInvalidAreaPoint` | Non-finite event point. |
| `ErrInvalidAreaRoute` | Empty/control-character/oversized route. |
| `ErrAreaManagerClosed` | Mutating an already closed manager. |
| `ErrAreaSubscriptionClosed` | Using a subscription that was already closed. |
| `ErrAreaSubscriptionNotOwned` | Passing another manager's subscription. |
| `ErrAreaSessionUnbound` | `Restore` on a session without a UID. |

## Tests

```bash
go test -race -run 'TestArea|TestRect' ./...
go test -race ./session/ -run TestRouter
```

`TestAreaTwoZonesOverlapReconnectSimulation` simulates a map split into two
zones (sharing the `x=1000` border), a player holding two overlapping viewports,
a disconnect + reconnect, a node route outage and recovery, and the original
`Group` broadcast in the same run, asserting recipients and order at every step.
