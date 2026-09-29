package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCreateIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, clock := newTestService(t)

	keyed := `{"kind":"friction","key":"k1","summary":"a","machine":"m","model":"x","payload":{"category":"tooling","details":"d"}}`
	first := mustCreate(t, s, keyed)
	if !first.Created || first.Record.ID != 1 {
		t.Fatalf("first create: created %v id %d", first.Created, first.Record.ID)
	}
	replay := mustCreate(t, s, keyed)
	if replay.Created || replay.Record.ID != 1 {
		t.Fatalf("keyed replay: created %v id %d", replay.Created, replay.Record.ID)
	}
	if len(replay.Warnings) != len(first.Warnings) {
		t.Errorf("replay warnings %v, want %v", replay.Warnings, first.Warnings)
	}
	_, err := s.Create(ctx, []byte(`{"kind":"friction","key":"k1","summary":"b","payload":{}}`))
	p := asProblem(t, err)
	if p.Status != 409 || p.Code != CodeReplayMismatch || len(p.Details) != 1 ||
		p.Details[0].Code != "key_reused" || p.Details[0].Pointer != "/key" || p.Details[0].ExistingID != 1 {
		t.Fatalf("mismatch problem %+v", p)
	}

	keyless := `{"kind":"friction","summary":"keyless","payload":{"details":"d"}}`
	a := mustCreate(t, s, keyless)
	clock.advance(DedupeWindow - time.Microsecond)
	b := mustCreate(t, s, keyless)
	if b.Created || b.Record.ID != a.Record.ID {
		t.Fatalf("keyless duplicate inside the window: created %v id %d", b.Created, b.Record.ID)
	}
	if len(b.Warnings) == 0 {
		t.Error("keyless replay returned no warnings")
	}
	clock.advance(2 * time.Microsecond)
	c := mustCreate(t, s, keyless)
	if !c.Created {
		t.Fatal("keyless duplicate outside the window was not inserted")
	}
	if _, err := s.MarkBatch(ctx, []byte(`{"ids":[`+itoa(c.Record.ID)+`]}`)); err != nil {
		t.Fatal(err)
	}
	d := mustCreate(t, s, keyless)
	if !d.Created {
		t.Fatal("keyless duplicate of a processed row was not inserted")
	}
	if c.Record.UID == d.Record.UID || !uuidForm.MatchString(d.Record.UID) || d.Record.UID[14] != '7' {
		t.Errorf("uid %q is not a fresh UUIDv7", d.Record.UID)
	}
}

func TestCreateUIDError(t *testing.T) {
	t.Parallel()
	_, db, _ := newTestService(t)
	boom := errors.New("no entropy")
	s := New(db, Config{}, WithUIDGenerator(func() (string, error) { return "", boom }))
	_, err := s.Create(context.Background(), []byte(`{"kind":"x"}`))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	var p *Problem
	if errors.As(err, &p) {
		t.Fatal("a uid failure is a Problem, want an internal error")
	}
}

func TestCreateConcurrent(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"kind":"friction","key":"same","summary":"concurrent","payload":{"details":"x"}}`,
		`{"kind":"friction","summary":"concurrent keyless","payload":{"details":"x"}}`,
	} {
		s, db, _ := newTestService(t)
		const n = 16
		ids := make([]int64, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := s.Create(context.Background(), []byte(body))
				ids[i], errs[i] = res.Record.ID, err
			}()
		}
		wg.Wait()
		for i := range n {
			if errs[i] != nil {
				t.Fatalf("%s: goroutine %d: %v", body, i, errs[i])
			}
			if ids[i] != ids[0] {
				t.Errorf("%s: ids disagree: %v", body, ids)
				break
			}
		}
		if rows := rowCount(t, db); rows != 1 {
			t.Errorf("%s: %d rows, want 1", body, rows)
		}
	}
}
