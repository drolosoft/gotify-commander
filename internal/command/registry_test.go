package command

import (
	"testing"
)

func TestRegistry(t *testing.T) {
	reg := NewRegistry()

	// Register a ping handler
	reg.Register(ActionPing, func(cmd Command) Response {
		return Response{
			Title:    "Ping",
			Message:  "pong from " + cmd.Target,
			Priority: 1,
		}
	})

	// Lookup registered handler
	handler, ok := reg.Lookup(ActionPing)
	if !ok {
		t.Fatal("expected ping handler to be found")
	}

	resp := handler(Command{Action: ActionPing, Target: "vps"})
	if resp.Title != "Ping" {
		t.Errorf("expected title 'Ping', got %q", resp.Title)
	}
	if resp.Message != "pong from vps" {
		t.Errorf("expected message 'pong from vps', got %q", resp.Message)
	}

	// Lookup missing handler
	_, ok = reg.Lookup(ActionReboot)
	if ok {
		t.Error("expected ActionReboot to not be found in registry")
	}
}

func TestRegistryNames(t *testing.T) {
	reg := NewRegistry()
	reg.Register(ActionPing, func(Command) Response { return Response{} })
	reg.Register(ActionDf, func(Command) Response { return Response{} })

	names := reg.Names()
	if len(names) != 2 || names[0] != "df" || names[1] != "ping" {
		t.Errorf("Names() = %v, want [df ping] in order", names)
	}
}
