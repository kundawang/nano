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

package session

import "sync"

type (
	// LifetimeHandler represents a callback
	// that will be called when a session close or
	// session low-level connection broken.
	LifetimeHandler func(*Session)

	lifetime struct {
		mu sync.RWMutex
		// callbacks that emitted on session closed
		onClosed []LifetimeHandler
	}
)

var Lifetime = &lifetime{}

// OnClosed set the Callback which will be called
// when session is closed Waring: session has closed.
func (lt *lifetime) OnClosed(h LifetimeHandler) {
	lt.mu.Lock()
	lt.onClosed = append(lt.onClosed, h)
	lt.mu.Unlock()
}

func (lt *lifetime) Close(s *Session) {
	lt.mu.RLock()
	if len(lt.onClosed) < 1 {
		lt.mu.RUnlock()
		return
	}
	handlers := make([]LifetimeHandler, len(lt.onClosed))
	copy(handlers, lt.onClosed)
	lt.mu.RUnlock()

	for _, h := range handlers {
		h(s)
	}
}
