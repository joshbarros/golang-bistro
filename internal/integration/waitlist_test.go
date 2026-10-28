package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-bistro/internal/platform/messaging"
)

func (e *env) enterWait(s session, sitting, reference string, covers int) reply {
	return e.post("/api/orders", map[string]any{"sittingCode": sitting, "reference": reference, "covers": covers, "waitlist": true}, s.Token)
}

func (e *env) refund(s session, id string) {
	e.t.Helper()
	if r := e.post("/api/orders/"+id+"/refund", nil, s.Token); r.Status != 202 {
		e.t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}
}

func TestAFullSittingRefusesWithoutTheFlagAndQueuesWithIt(t *testing.T) {
	e := newEnv(t)
	sitting := e.openSittingCap(4, 4)
	ana, bob, cy := e.register(), e.register(), e.register()
	e.enter(ana, sitting, "RSV-A", 4)
	e.drain()
	plain := e.enter(bob, sitting, "RSV-B", 2).str("id")
	queued := e.enterWait(cy, sitting, "RSV-C", 2).str("id")
	e.drain()
	if o := e.order(bob, plain); o.str("status") != "Rejected" || o.str("reason") != "Short: 2 covers wanted, 0 left." {
		t.Fatalf("without the flag the guest is turned away: %s", o.Raw)
	}
	if o := e.order(cy, queued); o.str("status") != "Waitlisted" || o.str("reason") != "Waitlisted: number 1 in line for 2 covers." {
		t.Fatalf("with the flag the guest waits: %s", o.Raw)
	}
	if e.coversLeft(sitting) != 0 || e.count(`SELECT count(*) FROM sittings.cover_holds`) != 1 {
		t.Fatal("waiting takes nothing")
	}
}

func TestReleasesPromoteTheLineStrictlyInOrder(t *testing.T) {
	e := newEnv(t)
	sitting := e.openSittingCap(4, 4)
	ana, bob, cy, di := e.register(), e.register(), e.register(), e.register()
	a := e.enter(ana, sitting, "RSV-A", 3).str("id") // 1 left
	e.drain()
	b := e.enterWait(bob, sitting, "RSV-B", 2).str("id") // does not fit: line number 1
	e.drain()
	c := e.enterWait(cy, sitting, "RSV-C", 1).str("id") // would fit, but a line exists: it waits behind bob
	e.drain()
	d := e.enterWait(di, sitting, "RSV-D", 1).str("id")
	e.drain()
	if o := e.order(cy, c); o.str("status") != "Waitlisted" || o.str("reason") != "Waitlisted: number 2 in line for 1 covers." {
		t.Fatalf("nobody slips past a line, even when the covers fit: %s", o.Raw)
	}
	if e.order(di, d).str("reason") != "Waitlisted: number 3 in line for 1 covers." {
		t.Fatal("third in line")
	}
	e.refund(ana, a) // 3 covers come back: 1 + 3 = 4 left; bob (2), cy (1), di (1) all fit, in that order
	e.drain()
	for _, p := range []struct {
		s  session
		id string
	}{{bob, b}, {cy, c}, {di, d}} {
		if status(e, p.s, p.id) != "Sold" {
			t.Fatalf("seated in order and confirmed: %s", e.order(p.s, p.id).Raw)
		}
	}
	if e.coversLeft(sitting) != 0 || e.count(`SELECT count(*) FROM sittings.waitlist`) != 0 {
		t.Fatal("the line is empty and every cover is held")
	}
}

func TestTheHeadOfTheLineBlocksSmallerPartiesBehindIt(t *testing.T) {
	e := newEnv(t)
	sitting := e.openSittingCap(4, 4)
	ana, bob, cy, di := e.register(), e.register(), e.register(), e.register()
	a := e.enter(ana, sitting, "RSV-A", 2).str("id")
	b := e.enter(bob, sitting, "RSV-B", 2).str("id")
	e.drain()
	c := e.enterWait(cy, sitting, "RSV-C", 3).str("id") // the head: wants 3
	d := e.enterWait(di, sitting, "RSV-D", 1).str("id") // behind it: wants 1
	e.drain()
	e.refund(ana, a) // 2 back: the head needs 3, so it does not fit, and nobody behind it moves
	e.drain()
	if status(e, cy, c) != "Waitlisted" || status(e, di, d) != "Waitlisted" || e.coversLeft(sitting) != 2 {
		t.Fatalf("the head blocks: %s / %s / %v left", status(e, cy, c), status(e, di, d), e.coversLeft(sitting))
	}
	e.refund(bob, b) // 2 more: 4 left, the head takes 3, the next takes 1
	e.drain()
	if status(e, cy, c) != "Sold" || status(e, di, d) != "Sold" || e.coversLeft(sitting) != 0 {
		t.Fatalf("both seated once the head fits: %s / %s", status(e, cy, c), status(e, di, d))
	}
}

func TestLeavingTheLineRemovesThePlaceAndHoldsNothing(t *testing.T) {
	e := newEnv(t)
	sitting := e.openSittingCap(2, 2)
	ana, bob := e.register(), e.register()
	e.enter(ana, sitting, "RSV-A", 2)
	e.drain()
	b := e.enterWait(bob, sitting, "RSV-B", 2).str("id")
	e.drain()
	e.refund(bob, b)
	e.drain()
	o := e.order(bob, b)
	if o.str("status") != "Refunded" || o.Body["coversReleasedAt"] == nil {
		t.Fatalf("left the line: %s", o.Raw)
	}
	if e.count(`SELECT count(*) FROM sittings.waitlist`) != 0 || e.count(`SELECT count(*) FROM sittings.cover_holds`) != 1 || e.coversLeft(sitting) != 0 {
		t.Fatal("leaving the line frees nothing and holds nothing")
	}
}

func TestARefundAndALeaveRacingForTheSameCoversNeverOverbook(t *testing.T) {
	for round := 0; round < 5; round++ {
		e := newEnv(t)
		sitting := e.openSittingCap(2, 2)
		ana, bob := e.register(), e.register()
		a := e.enter(ana, sitting, "RSV-A", 2).str("id")
		e.drain()
		b := e.enterWait(bob, sitting, "RSV-B", 2).str("id")
		e.drain()
		var wg sync.WaitGroup
		for _, id := range []string{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := e.app.Sittings.Release(context.Background(), uuid.New(), messaging.ReleaseCovers{OrderID: uuid.MustParse(id), Reason: "refund"}); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		e.drain()
		var active int
		_ = e.pool.QueryRow(context.Background(), `SELECT coalesce(sum(covers),0) FROM sittings.cover_holds WHERE status = 'Held'`).Scan(&active)
		left := int(e.coversLeft(sitting))
		var line int
		_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM sittings.waitlist`).Scan(&line)
		if active+left != 2 || line != 0 {
			t.Fatalf("round %d: every cover is free or held, never both, never lost: held %d, left %d, waiting %d", round, active, left, line)
		}
	}
}

func TestAPromotionPastTheGuestLimitIsRefusedAsCapAndTheCoversMoveOn(t *testing.T) {
	e := newEnv(t)
	sitting := e.openSittingCap(4, 3)
	ana, bob, gus := e.register(), e.register(), e.register()
	a := e.enter(ana, sitting, "RSV-A", 3).str("id")
	b := e.enter(bob, sitting, "RSV-B", 1).str("id")
	e.drain()
	g1 := e.enterWait(gus, sitting, "RSV-G1", 2).str("id")
	g2 := e.enterWait(gus, sitting, "RSV-G2", 2).str("id")
	e.drain()
	e.refund(ana, a) // 3 back: the first of Gus's places fits (2), the second needs 2 and only 1 is left
	e.drain()
	if status(e, gus, g1) != "Sold" || status(e, gus, g2) != "Waitlisted" {
		t.Fatalf("first seated, second still waiting: %s / %s", status(e, gus, g1), status(e, gus, g2))
	}
	e.refund(bob, b) // 1 back: 2 left, the second place fits the stock but Gus would hold 4 of a limit of 3
	e.drain()
	o := e.order(gus, g2)
	if o.str("status") != "Rejected" || o.str("reason") != "Cap: at most 3 covers per guest in this sitting." {
		t.Fatalf("refused as Cap at promotion: %s", o.Raw)
	}
	if e.coversLeft(sitting) != 2 {
		t.Fatalf("the covers moved on, not lost: %v left", e.coversLeft(sitting))
	}
}
