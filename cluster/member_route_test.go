package cluster

import (
	"context"
	"testing"

	"github.com/lonng/nano/cluster/clusterpb"
	"github.com/lonng/nano/component"
	"github.com/lonng/nano/session"
)

type routeTestComp struct{ component.Base }

func (c *routeTestComp) Ping(s *session.Session, _ []byte) error { return nil }

// TestDelMemberPurgesSessionRoutes verifies that when a cluster member leaves,
// the explicit router bindings created for sessions no longer point at the dead
// address. Route-bound area subscriptions are dormant afterwards and resume only
// when the registered address comes back.
func TestDelMemberPurgesSessionRoutes(t *testing.T) {
	node := &Node{
		Options:     Options{Components: &component.Components{}},
		ServiceAddr: "127.0.0.1:0",
	}
	node.sessions = make(map[int64]*session.Session)
	node.handler = NewHandler(node, nil)
	node.cluster = newCluster(node)

	s := session.New(nil)
	s.Bind(77)
	s.Router().Bind("World", "node-a")
	s.Router().Bind("Chat", "node-b")
	node.sessions[s.ID()] = s

	node.handler.remoteServices["World"] = []*clusterpb.MemberInfo{{ServiceAddr: "node-a"}}
	node.handler.remoteServices["Chat"] = []*clusterpb.MemberInfo{{ServiceAddr: "node-b"}}

	if _, err := node.DelMember(context.Background(), &clusterpb.DelMemberRequest{ServiceAddr: "node-a"}); err != nil {
		t.Fatalf("DelMember: %v", err)
	}

	if addr, ok := s.Router().Find("World"); ok {
		t.Errorf("World still bound to %q after the member left", addr)
	}
	if addr, ok := s.Router().Find("Chat"); !ok || addr != "node-b" {
		t.Errorf("unrelated binding changed: %q, %v", addr, ok)
	}

	// A binding that is recreated for the same service after the purge works
	// normally again (for example dave is routed to a new node).
	s.Router().Bind("World", "node-c")
	if addr, ok := s.Router().Find("World"); !ok || addr != "node-c" {
		t.Errorf("rebound route = %q, %v", addr, ok)
	}
}
