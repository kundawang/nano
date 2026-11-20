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

// Package main demonstrates area (viewport) subscriptions: position updates are
// only pushed to the players whose viewport covers the new position instead of
// being broadcast to the whole map.
//
// Try it with two clients: joining near (0,0) and near (5000,5000) and moving
// in the first area only notifies players whose viewport covers the position.
package main

import (
	"log"

	"github.com/lonng/nano"
	"github.com/lonng/nano/component"
	"github.com/lonng/nano/serialize/json"
	"github.com/lonng/nano/session"
)

const (
	constRoomKey   = "room"
	constSubKey    = "area-subscription"
	viewportWidth  = 40.0
	viewportHeight = 30.0
)

type (
	// Vec2 is a serializable world position.
	Vec2 struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}

	// EnterRequest joins a room at a position.
	EnterRequest struct {
		Room string `json:"room"`
		Pos  Vec2   `json:"pos"`
	}

	// EnterResponse reports the joined room and viewport.
	EnterResponse struct {
		Room     string  `json:"room"`
		Viewport [2]Vec2 `json:"viewport"`
		Nearby   []int64 `json:"nearby"`
	}

	// MoveRequest moves the player and broadcasts the position.
	MoveRequest struct {
		Pos Vec2 `json:"pos"`
	}

	// MoveEvent is pushed to the players whose viewport covers Pos.
	MoveEvent struct {
		UID int64 `json:"uid"`
		Pos Vec2  `json:"pos"`
	}

	// World is an area-aware game component.
	World struct {
		component.Base
		areas *nano.AreaManager
		chat  *nano.Group // the original group broadcast is still available
	}
)

func newWorld() *World {
	return &World{}
}

// Init creates the area manager. Subscriptions of closed sessions are removed
// automatically through session.Lifetime.OnClosed.
func (w *World) Init() {
	if w.areas == nil {
		w.areas = nano.NewAreaManager()
	}
	if w.chat == nil {
		w.chat = nano.NewGroup("world-chat")
	}
}

// viewportFor returns the rectangle around pos.
func viewportFor(pos Vec2) nano.Rect {
	return nano.NewRect(pos.X-viewportWidth/2, pos.Y-viewportHeight/2, viewportWidth, viewportHeight)
}

// Enter subscribes the player's viewport in a room.
func (w *World) Enter(s *session.Session, req *EnterRequest) error {
	if req.Room == "" {
		req.Room = "world"
	}
	s.Set(constRoomKey, req.Room)
	s.Bind(s.ID())

	sub, err := w.areas.Subscribe(req.Room, s, viewportFor(req.Pos))
	if err != nil {
		return err
	}
	s.Set(constSubKey, sub)
	_ = w.chat.Add(s)

	vp := sub.Viewport()
	return s.Response(&EnterResponse{
		Room:     req.Room,
		Viewport: [2]Vec2{{X: vp.Min.X, Y: vp.Min.Y}, {X: vp.Max.X, Y: vp.Max.Y}},
		Nearby:   w.chat.Members(),
	})
}

// Move updates the viewport and only notifies the players who can see the new
// position.
func (w *World) Move(s *session.Session, req *MoveRequest) error {
	room, _ := s.Value(constRoomKey).(string)
	sub, _ := s.Value(constSubKey).(*nano.AreaSubscription)
	if room == "" || sub == nil {
		return s.Push("onError", "join a room first")
	}

	if err := w.areas.UpdateViewport(sub, viewportFor(req.Pos)); err != nil {
		return err
	}
	_, err := w.areas.Broadcast(room, "onMove", nano.Point{X: req.Pos.X, Y: req.Pos.Y}, &MoveEvent{
		UID: s.UID(),
		Pos: req.Pos,
	})
	return err
}

// Say still uses the original group broadcast: everyone in the room receives it.
func (w *World) Say(s *session.Session, content *string) error {
	return w.chat.Broadcast("onChat", content)
}

func main() {
	components := &component.Components{}
	components.Register(newWorld())
	log.Println("Area subscription example listening on :3250")
	nano.Listen(":3250",
		nano.WithComponents(components),
		nano.WithSerializer(json.NewSerializer()),
		nano.WithIsWebsocket(true),
		nano.WithWSPath("/nano"),
	)
}
