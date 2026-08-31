package main

import (
	"bytes"
	"net"
	"testing"
)

func TestAMessagePastAnyHonestSizeIsRefused(t *testing.T) {
	// The decoder materialises whatever length the wire claims. One absurd
	// frame must come back as an error, not as an allocation that takes the
	// daemon — and every shell it holds — with it.
	ours, theirs := net.Pipe()
	defer ours.Close()
	defer theirs.Close()

	go func() {
		if _, err := theirs.Write([]byte(`{"paste":"`)); err != nil {
			return
		}
		junk := bytes.Repeat([]byte("a"), 1<<20)
		for range maxMessage/len(junk) + 1 {
			if _, err := theirs.Write(junk); err != nil {
				return
			}
		}
	}()

	if _, err := newConn(ours).read(); err == nil {
		t.Fatal("a message past any honest size was read whole")
	}
}

func TestHonestMessagesKeepFlowingPastTheBudget(t *testing.T) {
	// The budget is per message, not per connection: a long-lived window
	// exchanges far more than any ceiling over its lifetime.
	ours, theirs := net.Pipe()
	defer ours.Close()
	defer theirs.Close()

	go func() {
		out := newConn(theirs)
		for i := range 3 {
			if err := out.write(message{Kind: kindInput, PID: i}); err != nil {
				return
			}
		}
	}()

	in := newConn(ours)
	for i := range 3 {
		m, err := in.read()
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if m.PID != i {
			t.Fatalf("message %d arrived as %d", i, m.PID)
		}
	}
}
