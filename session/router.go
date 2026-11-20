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

// Router is used to select remote service address
//
// Reads (Find) are lock-free; mutations (Bind, Delete, PurgeAddress) are
// serialized so that a purge cannot race with a rebind of the same service.
type Router struct {
	routes sync.Map // map[string]string

	mu          sync.Mutex // serializes mutations and protects listeners
	listeners   map[uint64]func(service, address string)
	nextWatcher uint64
}

type routerChange struct {
	service string
	address string
}

func newRouter() *Router {
	return &Router{
		listeners: make(map[uint64]func(service, address string)),
	}
}

// Bind bound an address to remote service
func (r *Router) Bind(service, address string) {
	r.mu.Lock()
	r.routes.Store(service, address)
	changes := []routerChange{{service: service, address: address}}
	listeners := r.snapshotListenersLocked()
	r.mu.Unlock()
	notifyAll(listeners, changes)
}

// Delete route delete bind for
// 1. some son-service restart, sadly it's not auto bind new son-service
// 2. some game have match to some service, delete bind and match customer service
//
// Watchers registered with OnChange are notified with an empty address.
func (r *Router) Delete(service string) {
	r.mu.Lock()
	r.routes.Delete(service)
	changes := []routerChange{{service: service, address: ""}}
	listeners := r.snapshotListenersLocked()
	r.mu.Unlock()
	notifyAll(listeners, changes)
}

// Find finds the address corresponding a remote service
func (r *Router) Find(service string) (string, bool) {
	v, found := r.routes.Load(service)
	if !found {
		return "", false
	}
	return v.(string), true
}

// PurgeAddress deletes every binding whose address equals address and returns
// the number of bindings removed. It is used when a cluster member leaves: the
// session must stop routing to, and keep alive subscriptions bound to, that
// address. Watchers registered with OnChange are notified with an empty address
// for each removed service.
func (r *Router) PurgeAddress(address string) int {
	if address == "" {
		return 0
	}

	r.mu.Lock()
	var changes []routerChange
	r.routes.Range(func(k, v interface{}) bool {
		if addr, _ := v.(string); addr != address {
			return true
		}
		service, _ := k.(string)
		r.routes.Delete(service)
		changes = append(changes, routerChange{service: service, address: ""})
		return true
	})
	var listeners []func(service, address string)
	if len(changes) > 0 {
		listeners = r.snapshotListenersLocked()
	}
	r.mu.Unlock()

	notifyAll(listeners, changes)
	return len(changes)
}

// OnChange registers a callback that is invoked whenever Bind, Delete or
// PurgeAddress changes a route. address is the new address for Bind and the
// empty string for Delete and PurgeAddress. Callbacks are invoked synchronously,
// after the route map has been updated, and must not call back into
// Router.Bind/Delete/PurgeAddress (doing so may deadlock).
//
// The returned cancel function removes the callback and is safe to call more
// than once and concurrently with the mutation methods.
func (r *Router) OnChange(fn func(service, address string)) (cancel func()) {
	if fn == nil {
		return func() {}
	}

	r.mu.Lock()
	r.nextWatcher++
	id := r.nextWatcher
	r.listeners[id] = fn
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.listeners, id)
			r.mu.Unlock()
		})
	}
}

// snapshotListenersLocked copies the watcher list. The caller must hold r.mu.
func (r *Router) snapshotListenersLocked() []func(service, address string) {
	if len(r.listeners) == 0 {
		return nil
	}
	listeners := make([]func(service, address string), 0, len(r.listeners))
	for _, fn := range r.listeners {
		listeners = append(listeners, fn)
	}
	return listeners
}

func notifyAll(listeners []func(service, address string), changes []routerChange) {
	for _, ch := range changes {
		for _, fn := range listeners {
			fn(ch.service, ch.address)
		}
	}
}

// watcherCount reports the number of registered watchers. It exists for tests.
func (r *Router) watcherCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.listeners)
}
