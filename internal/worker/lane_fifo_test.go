package worker

import "testing"

// These fixtures inspect synchronous primitive transitions directly. They do
// not depend on goroutine scheduling, timers, or an execution-slot race.
func assertLaneExecQueue(t *testing.T, l *lane, want ...*laneExecTicket) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(want) == 0 {
		if l.execHead != nil || l.execTail != nil {
			t.Fatal("empty ticket queue retained a head or tail")
		}
		return
	}
	if l.execHead != want[0] || l.execTail != want[len(want)-1] {
		t.Fatal("ticket queue endpoints do not match reservation order")
	}
	for i, ticket := range want {
		if ticket == nil || ticket.owner != l || !ticket.queued || ticket.ready == nil {
			t.Fatalf("ticket %d lost its ownership, queue membership, or ready channel", i)
		}
		var previous, next *laneExecTicket
		if i > 0 {
			previous = want[i-1]
		}
		if i+1 < len(want) {
			next = want[i+1]
		}
		if ticket.prev != previous || ticket.next != next {
			t.Fatalf("ticket %d has stale, missing, or out-of-order links", i)
		}
		select {
		case _, open := <-ticket.ready:
			if open {
				t.Fatalf("ticket %d ready channel received a value instead of closing", i)
			}
			if i != 0 {
				t.Fatalf("queued ticket %d became ready before its predecessor released", i)
			}
		default:
			if i == 0 {
				t.Fatal("ticket queue head was not made ready synchronously")
			}
		}
	}
}

func assertLaneExecTicketRemoved(t *testing.T, ticket *laneExecTicket) {
	t.Helper()
	if ticket.queued || ticket.prev != nil || ticket.next != nil {
		t.Fatal("released ticket retained queue membership or neighboring tickets")
	}
}

func TestLaneExecTicketsPromoteOnlyTheOldestReservation(t *testing.T) {
	l := newLane("fifo", 8, 1, false)
	first := l.reserveExecTicket()
	second := l.reserveExecTicket()
	third := l.reserveExecTicket()
	fourth := l.reserveExecTicket()
	assertLaneExecQueue(t, l, first, second, third, fourth)

	l.releaseExecTicket(first)
	assertLaneExecTicketRemoved(t, first)
	assertLaneExecQueue(t, l, second, third, fourth)
	// A stale completion must not release the current head or open its successor.
	l.releaseExecTicket(first)
	assertLaneExecQueue(t, l, second, third, fourth)

	l.releaseExecTicket(second)
	assertLaneExecTicketRemoved(t, second)
	assertLaneExecQueue(t, l, third, fourth)
	l.releaseExecTicket(third)
	assertLaneExecTicketRemoved(t, third)
	assertLaneExecQueue(t, l, fourth)
	l.releaseExecTicket(fourth)
	assertLaneExecTicketRemoved(t, fourth)
	assertLaneExecQueue(t, l)
}

func TestLaneExecTicketMiddleCancellationDoesNotReleaseSuccessor(t *testing.T) {
	l := newLane("fifo", 8, 1, false)
	head := l.reserveExecTicket()
	cancelled := l.reserveExecTicket()
	successor := l.reserveExecTicket()
	tail := l.reserveExecTicket()
	assertLaneExecQueue(t, l, head, cancelled, successor, tail)

	l.releaseExecTicket(cancelled)
	assertLaneExecTicketRemoved(t, cancelled)
	assertLaneExecQueue(t, l, head, successor, tail)
	l.releaseExecTicket(cancelled)
	assertLaneExecQueue(t, l, head, successor, tail)

	l.releaseExecTicket(head)
	assertLaneExecQueue(t, l, successor, tail)
	// Even after promotion, removing the old middle ticket again is harmless.
	l.releaseExecTicket(cancelled)
	assertLaneExecQueue(t, l, successor, tail)
	l.releaseExecTicket(successor)
	assertLaneExecQueue(t, l, tail)
	l.releaseExecTicket(tail)
	assertLaneExecQueue(t, l)
}

func TestLaneExecTicketTailAndForeignRemovalPreserveQueue(t *testing.T) {
	l := newLane("fifo", 8, 1, false)
	other := newLane("other", 8, 1, false)
	head := l.reserveExecTicket()
	middle := l.reserveExecTicket()
	tail := l.reserveExecTicket()
	foreign := other.reserveExecTicket()
	assertLaneExecQueue(t, l, head, middle, tail)
	assertLaneExecQueue(t, other, foreign)

	l.releaseExecTicket(nil)
	l.releaseExecTicket(foreign)
	other.releaseExecTicket(head)
	other.releaseExecTicket(middle)
	assertLaneExecQueue(t, l, head, middle, tail)
	assertLaneExecQueue(t, other, foreign)

	l.releaseExecTicket(tail)
	l.releaseExecTicket(tail)
	assertLaneExecTicketRemoved(t, tail)
	assertLaneExecQueue(t, l, head, middle)
	replacement := l.reserveExecTicket()
	assertLaneExecQueue(t, l, head, middle, replacement)
	l.releaseExecTicket(head)
	assertLaneExecQueue(t, l, middle, replacement)
	l.releaseExecTicket(middle)
	assertLaneExecQueue(t, l, replacement)
	l.releaseExecTicket(replacement)
	l.releaseExecTicket(nil)
	assertLaneExecQueue(t, l)
	other.releaseExecTicket(foreign)
	assertLaneExecQueue(t, other)
}

func TestLaneExecTicketRepeatedMiddleCancellationDoesNotRetainTombstones(t *testing.T) {
	l := newLane("fifo", 8, 1, false)
	head := l.reserveExecTicket()
	waiting := l.reserveExecTicket()
	for i := 0; i < 1024; i++ {
		cancelled := l.reserveExecTicket()
		tail := l.reserveExecTicket()
		assertLaneExecQueue(t, l, head, waiting, cancelled, tail)
		l.releaseExecTicket(cancelled)
		assertLaneExecTicketRemoved(t, cancelled)
		assertLaneExecQueue(t, l, head, waiting, tail)
		l.releaseExecTicket(tail)
		assertLaneExecTicketRemoved(t, tail)
		assertLaneExecQueue(t, l, head, waiting)
	}
	// There is still exactly one waiting ticket, with no cancelled relay chain.
	l.releaseExecTicket(head)
	assertLaneExecQueue(t, l, waiting)
	l.releaseExecTicket(waiting)
	assertLaneExecQueue(t, l)
}

func TestLaneExecTicketAllCancelledAllowsFreshChain(t *testing.T) {
	for _, order := range []struct {
		name    string
		indices [3]int
	}{
		{name: "head first", indices: [3]int{0, 1, 2}},
		{name: "tail first", indices: [3]int{2, 1, 0}},
		{name: "middle first", indices: [3]int{1, 0, 2}},
	} {
		t.Run(order.name, func(t *testing.T) {
			l := newLane("fifo", 8, 1, false)
			tickets := []*laneExecTicket{l.reserveExecTicket(), l.reserveExecTicket(), l.reserveExecTicket()}
			assertLaneExecQueue(t, l, tickets...)
			var removed [3]bool
			for _, index := range order.indices {
				l.releaseExecTicket(tickets[index])
				removed[index] = true
				assertLaneExecTicketRemoved(t, tickets[index])
				var remaining []*laneExecTicket
				for i, ticket := range tickets {
					if !removed[i] {
						remaining = append(remaining, ticket)
					}
				}
				assertLaneExecQueue(t, l, remaining...)
			}
			first := l.reserveExecTicket()
			second := l.reserveExecTicket()
			assertLaneExecQueue(t, l, first, second)
			for _, stale := range tickets {
				l.releaseExecTicket(stale)
			}
			assertLaneExecQueue(t, l, first, second)
			l.releaseExecTicket(first)
			assertLaneExecQueue(t, l, second)
			l.releaseExecTicket(second)
			assertLaneExecQueue(t, l)
		})
	}
}
