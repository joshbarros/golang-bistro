package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-bistro/internal/platform/messaging"
	"github.com/joshbarros/golang-bistro/internal/sittings"
)

// rawSitting inserts a sitting directly (below the API) and returns its id.
func (e *env) rawSitting(covers, perGuest int) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	_, err := e.pool.Exec(context.Background(), `INSERT INTO sittings.sittings (id, code, name, room, chef, service_date, price_cents, covers_total, covers_left, max_per_guest, created_at)
		VALUES ($1,$2,'n','s','a',now(),5000,$3,$3,$4,now())`, id, code(), covers, perGuest)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func reserve(e *env, sitting, owner uuid.UUID, covers int) sittings.ReserveOutcome {
	out, err := e.app.Sittings.Reserve(context.Background(), uuid.New(), messaging.ReserveCovers{OrderID: uuid.New(), OwnerID: owner, SittingID: sitting, Covers: covers, Reference: "R"})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestConcurrentReservesNeverOversellAndAlwaysSumToTheStock(t *testing.T) {
	e := newEnv(t)
	sitting := e.rawSitting(30, 3)
	var wg sync.WaitGroup
	var mu sync.Mutex
	held := 0
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reserve(e, sitting, uuid.New(), 1) == sittings.Held {
				mu.Lock()
				held++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT covers_left FROM sittings.sittings WHERE id = $1`, sitting).Scan(&left)
	if held != 30 || left != 0 {
		t.Fatalf("60 guests, 30 covers: exactly 30 held and none left, got %d held and %d left", held, left)
	}
	if e.count(`SELECT count(*) FROM sittings.cover_holds WHERE sitting_id = $1`, sitting) != 30 {
		t.Fatal("one hold per success")
	}
}

func TestTheSameGuestRacingThemselvesCannotPassTheCap(t *testing.T) {
	e := newEnv(t)
	sitting := e.rawSitting(50, 2)
	owner := uuid.New()
	var wg sync.WaitGroup
	var mu sync.Mutex
	outcomes := map[sittings.ReserveOutcome]int{}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := reserve(e, sitting, owner, 1)
			mu.Lock()
			outcomes[o]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if outcomes[sittings.Held] != 2 || outcomes[sittings.CapReached] != 10 {
		t.Fatalf("a cap of 2 lets exactly 2 through: %v", outcomes)
	}
	var left, holding int
	_ = e.pool.QueryRow(context.Background(), `SELECT covers_left FROM sittings.sittings WHERE id = $1`, sitting).Scan(&left)
	_ = e.pool.QueryRow(context.Background(), `SELECT covers FROM sittings.guests WHERE sitting_id = $1 AND owner_id = $2`, sitting, owner).Scan(&holding)
	if left != 48 || holding != 2 {
		t.Fatalf("refused tries leave no trace: %d left, guest holds %d", left, holding)
	}
}

func TestACapRefusalDoesNotConsumeStockAndAStockRefusalDoesNotConsumeAllowance(t *testing.T) {
	e := newEnv(t)
	sitting := e.rawSitting(3, 2)
	owner := uuid.New()
	if reserve(e, sitting, owner, 3) != sittings.CapReached {
		t.Fatal("asking for more than the cap in one go is a cap refusal")
	}
	other := uuid.New()
	if reserve(e, sitting, other, 2) != sittings.Held {
		t.Fatal("2 of 3 held")
	}
	if reserve(e, sitting, owner, 2) != sittings.Short {
		t.Fatal("1 left, 2 wanted, within the cap: short")
	}
	if reserve(e, sitting, owner, 1) != sittings.Held {
		t.Fatal("the allowance was not spent by the short try")
	}
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT covers_left FROM sittings.sittings WHERE id = $1`, sitting).Scan(&left)
	if left != 0 {
		t.Fatalf("all three are held: %d", left)
	}
}

func TestReleaseGivesBackStockAndAllowanceExactlyOnce(t *testing.T) {
	e := newEnv(t)
	sitting := e.rawSitting(5, 2)
	owner, order := uuid.New(), uuid.New()
	if out, _ := e.app.Sittings.Reserve(context.Background(), uuid.New(), messaging.ReserveCovers{OrderID: order, OwnerID: owner, SittingID: sitting, Covers: 2, Reference: "R"}); out != sittings.Held {
		t.Fatal(out)
	}
	for i := 0; i < 3; i++ {
		_, err := e.app.Sittings.Release(context.Background(), uuid.New(), messaging.ReleaseCovers{OrderID: order, Reason: "refund"})
		if err != nil {
			t.Fatal(err)
		}
	}
	var left, holding int
	_ = e.pool.QueryRow(context.Background(), `SELECT covers_left FROM sittings.sittings WHERE id = $1`, sitting).Scan(&left)
	_ = e.pool.QueryRow(context.Background(), `SELECT covers FROM sittings.guests WHERE sitting_id = $1 AND owner_id = $2`, sitting, owner).Scan(&holding)
	if left != 5 || holding != 0 {
		t.Fatalf("given back once, not three times: %d left, guest holds %d", left, holding)
	}
	if e.outbox("sittings", messaging.ReleasedKey) != 1 {
		t.Fatal("one released event")
	}
}

func TestOpeningASittingValidatesItsCap(t *testing.T) {
	e := newEnv(t)
	adm := e.admin()
	good := map[string]any{"code": code(), "name": "n", "room": "s", "chef": "a", "priceCents": 100, "coversTotal": 10}
	if r := e.post("/api/sittings", good, adm.Token); r.Status != 201 || r.Body["maxPerGuest"] != float64(8) {
		t.Fatalf("the cap defaults to 8 (or the covers on sale, if fewer): %d %s", r.Status, r.Raw)
	}
	bad := map[string]any{"code": code(), "name": "n", "room": "s", "chef": "a", "priceCents": 100, "coversTotal": 10, "maxPerGuest": 11}
	if r := e.post("/api/sittings", bad, adm.Token); r.Status != 400 {
		t.Fatalf("a cap above the covers on sale: %d", r.Status)
	}
	bad["maxPerGuest"] = 0
	if r := e.post("/api/sittings", bad, adm.Token); r.Status != 400 {
		t.Fatalf("a cap of zero: %d", r.Status)
	}
	if r := e.post("/api/sittings", good, adm.Token); r.Status != 409 {
		t.Fatalf("the same code twice: %d", r.Status)
	}
	if r := e.post("/api/sittings", good, e.register().Token); r.Status != 403 {
		t.Fatalf("a customer cannot open a sitting: %d", r.Status)
	}
}
