package storage_test

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"currency/internal/rates"
	"currency/internal/storage"
)

const (
	insertSQL = `INSERT INTO currency (bank, "from", "to", value) VALUES ($1, $2, $3, $4)`
	lockSQL   = `SELECT pg_try_advisory_lock($1)`
	unlockSQL = `SELECT pg_advisory_unlock($1)`
)

var errBoom = errors.New("boom")

func assertArgs(t *testing.T, got, want []driver.Value) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg $%d = %#v (%T), want %#v (%T)", i+1, got[i], got[i], want[i], want[i])
		}
	}
}

// --- Save ---

func TestPostgres_Save(t *testing.T) {
	t.Parallel()

	db, fake := newFakeDB(t)
	s := storage.NewPostgres(db)

	r := rates.Rate{Bank: "Bank 1", From: "RUB", To: "USD", Value: 0.0105}
	if err := s.Save(context.Background(), r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d DB calls, want exactly 1: %+v", len(calls), calls)
	}
	c := calls[0]
	if c.kind != kindExec {
		t.Errorf("call kind = %s, want exec", c.kind)
	}
	if !sameSQL(c.query, insertSQL) {
		t.Errorf("query = %q, want %q", c.query, insertSQL)
	}
	assertArgs(t, c.args, []driver.Value{"Bank 1", "RUB", "USD", 0.0105})
}

func TestPostgres_Save_ParameterizedAgainstInjection(t *testing.T) {
	t.Parallel()

	db, fake := newFakeDB(t)
	s := storage.NewPostgres(db)

	const evil = `x'); DROP TABLE currency; --`
	r := rates.Rate{Bank: evil, From: `R'UB`, To: `US"D`, Value: 1.5}
	if err := s.Save(context.Background(), r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d DB calls, want exactly 1", len(calls))
	}
	c := calls[0]
	if strings.Contains(strings.ToUpper(c.query), "DROP") || strings.Contains(c.query, "R'UB") {
		t.Fatalf("user data leaked into SQL text: %q", c.query)
	}
	if !sameSQL(c.query, insertSQL) {
		t.Errorf("query = %q, want %q", c.query, insertSQL)
	}
	assertArgs(t, c.args, []driver.Value{evil, `R'UB`, `US"D`, 1.5})
}

func TestPostgres_Save_WrapsDriverError(t *testing.T) {
	t.Parallel()

	db, fake := newFakeDB(t)
	fake.setExec(func(string, []driver.Value) error { return errBoom })
	s := storage.NewPostgres(db)

	err := s.Save(context.Background(), rates.Rate{Bank: "Bank 1", From: "RUB", To: "USD", Value: 1})
	if !errors.Is(err, errBoom) {
		t.Fatalf("Save() error = %v, want errors.Is(err, errBoom)", err)
	}
}

func TestPostgres_Save_UsesContext(t *testing.T) {
	t.Parallel()

	db, fake := newFakeDB(t)
	s := storage.NewPostgres(db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.Save(ctx, rates.Rate{Bank: "Bank 1", From: "RUB", To: "USD", Value: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Save() error = %v, want context.Canceled", err)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("query must not reach the driver with canceled ctx, got %d calls", n)
	}
}

// --- AcquireLock ---

func TestPostgres_AcquireLock_Acquired(t *testing.T) {
	t.Parallel()

	db, fake := newFakeDB(t)
	fake.setQuery(func(q string, _ []driver.Value) (driver.Value, error) {
		return true, nil // и lock, и (если реализация использует Query) unlock
	})
	s := storage.NewPostgres(db)

	const key int64 = 424242
	release, err := s.AcquireLock(context.Background(), key)
	if err != nil {
		t.Fatalf("AcquireLock() error = %v", err)
	}
	if release == nil {
		t.Fatal("AcquireLock() release = nil on success")
	}

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d DB calls before release, want 1: %+v", len(calls), calls)
	}
	lock := calls[0]
	if lock.kind != kindQuery {
		t.Errorf("lock call kind = %s, want query", lock.kind)
	}
	if !sameSQL(lock.query, lockSQL) {
		t.Errorf("lock query = %q, want %q", lock.query, lockSQL)
	}
	assertArgs(t, lock.args, []driver.Value{key})

	// Advisory lock привязан к сессии: соединение должно быть удержано до release.
	if inUse := db.Stats().InUse; inUse != 1 {
		t.Errorf("connections in use while lock held = %d, want 1 (dedicated conn)", inUse)
	}

	if err := release(); err != nil {
		t.Fatalf("release() error = %v", err)
	}

	calls = fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("got %d DB calls after release, want 2: %+v", len(calls), calls)
	}
	unlock := calls[1]
	if unlock.kind != kindExec && unlock.kind != kindQuery {
		t.Errorf("unlock call kind = %s", unlock.kind)
	}
	if !sameSQL(unlock.query, unlockSQL) {
		t.Errorf("unlock query = %q, want %q", unlock.query, unlockSQL)
	}
	assertArgs(t, unlock.args, []driver.Value{key})
	if unlock.connID != lock.connID {
		t.Errorf("unlock ran on connection %d, lock on %d: must be the same session", unlock.connID, lock.connID)
	}

	if inUse := db.Stats().InUse; inUse != 0 {
		t.Errorf("connections in use after release = %d, want 0 (conn must be closed)", inUse)
	}
}

func TestPostgres_AcquireLock_AlreadyRunning(t *testing.T) {
	t.Parallel()

	db, fake := newFakeDB(t)
	fake.setQuery(func(string, []driver.Value) (driver.Value, error) { return false, nil })
	s := storage.NewPostgres(db)

	_, err := s.AcquireLock(context.Background(), 7)
	if !errors.Is(err, storage.ErrAlreadyRunning) {
		t.Fatalf("AcquireLock() error = %v, want ErrAlreadyRunning", err)
	}

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d DB calls, want 1 (no unlock for lock we do not own): %+v", len(calls), calls)
	}
	if inUse := db.Stats().InUse; inUse != 0 {
		t.Errorf("connections in use = %d, want 0 (conn leaked)", inUse)
	}
}

func TestPostgres_AcquireLock_QueryError(t *testing.T) {
	t.Parallel()

	db, fake := newFakeDB(t)
	fake.setQuery(func(string, []driver.Value) (driver.Value, error) { return nil, errBoom })
	s := storage.NewPostgres(db)

	_, err := s.AcquireLock(context.Background(), 7)
	if !errors.Is(err, errBoom) {
		t.Fatalf("AcquireLock() error = %v, want errors.Is(err, errBoom)", err)
	}
	if errors.Is(err, storage.ErrAlreadyRunning) {
		t.Errorf("driver error must not be reported as ErrAlreadyRunning: %v", err)
	}
	if inUse := db.Stats().InUse; inUse != 0 {
		t.Errorf("connections in use = %d, want 0 (conn leaked)", inUse)
	}
}

func TestPostgres_AcquireLock_ReleaseError(t *testing.T) {
	t.Parallel()

	db, fake := newFakeDB(t)
	fake.setQuery(func(q string, _ []driver.Value) (driver.Value, error) {
		if sameSQL(q, unlockSQL) {
			return nil, errBoom
		}
		return true, nil
	})
	fake.setExec(func(q string, _ []driver.Value) error {
		if sameSQL(q, unlockSQL) {
			return errBoom
		}
		return nil
	})
	s := storage.NewPostgres(db)

	release, err := s.AcquireLock(context.Background(), 1)
	if err != nil {
		t.Fatalf("AcquireLock() error = %v", err)
	}
	if err := release(); !errors.Is(err, errBoom) {
		t.Fatalf("release() error = %v, want errors.Is(err, errBoom)", err)
	}
	if inUse := db.Stats().InUse; inUse != 0 {
		t.Errorf("connections in use after failed unlock = %d, want 0 (conn must still be closed)", inUse)
	}
}

func TestPostgres_AcquireLock_CanceledContext(t *testing.T) {
	t.Parallel()

	db, fake := newFakeDB(t)
	fake.setQuery(func(string, []driver.Value) (driver.Value, error) { return true, nil })
	s := storage.NewPostgres(db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.AcquireLock(ctx, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AcquireLock() error = %v, want context.Canceled", err)
	}
	if inUse := db.Stats().InUse; inUse != 0 {
		t.Errorf("connections in use = %d, want 0", inUse)
	}
}
