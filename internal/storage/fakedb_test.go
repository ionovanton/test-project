package storage_test

// Минимальный фейковый драйвер database/sql: go-sqlmock недоступен (нет сети),
// поэтому реализуем ровно то, что нужно для тестов Postgres-хранилища.
//
// Драйвер записывает каждый запрос (вид, SQL, аргументы, id соединения)
// и отдаёт ответы, заданные тестом через колбэки.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"unicode"
)

type callKind string

const (
	kindExec  callKind = "exec"
	kindQuery callKind = "query"
)

type call struct {
	kind   callKind
	query  string
	args   []driver.Value
	connID int
}

// fakeDB — состояние одного фейкового подключения к БД (на тест).
type fakeDB struct {
	mu     sync.Mutex
	calls  []call
	nextID int

	// onExec возвращает ошибку для ExecContext (nil — успех).
	onExec func(query string, args []driver.Value) error
	// onQuery возвращает единственное значение единственной строки результата
	// либо ошибку.
	onQuery func(query string, args []driver.Value) (driver.Value, error)
}

// newFakeDB создаёт *sql.DB поверх фейкового драйвера и закрывает его по завершении теста.
func newFakeDB(t *testing.T) (*sql.DB, *fakeDB) {
	t.Helper()
	f := &fakeDB{}
	db := sql.OpenDB(&fakeConnector{db: f})
	t.Cleanup(func() { _ = db.Close() })
	return db, f
}

func (f *fakeDB) record(c call) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeDB) Calls() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]call, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeDB) setExec(fn func(string, []driver.Value) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onExec = fn
}

func (f *fakeDB) setQuery(fn func(string, []driver.Value) (driver.Value, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onQuery = fn
}

func (f *fakeDB) handlers() (func(string, []driver.Value) error, func(string, []driver.Value) (driver.Value, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.onExec, f.onQuery
}

// --- driver.Connector / driver.Driver ---

type fakeConnector struct{ db *fakeDB }

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) {
	c.db.mu.Lock()
	c.db.nextID++
	id := c.db.nextID
	c.db.mu.Unlock()
	return &fakeConn{db: c.db, id: id}, nil
}

func (c *fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("fakeDriver: use sql.OpenDB with fakeConnector")
}

// --- driver.Conn + ExecerContext + QueryerContext ---

type fakeConn struct {
	db *fakeDB
	id int
}

var (
	_ driver.Conn           = (*fakeConn)(nil)
	_ driver.ExecerContext  = (*fakeConn)(nil)
	_ driver.QueryerContext = (*fakeConn)(nil)
)

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fakeConn: Prepare is not supported")
}

func (c *fakeConn) Close() error { return nil }

func (c *fakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fakeConn: transactions are not supported")
}

func values(named []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(named))
	for i, nv := range named {
		out[i] = nv.Value
	}
	return out
}

func (c *fakeConn) ExecContext(_ context.Context, query string, named []driver.NamedValue) (driver.Result, error) {
	args := values(named)
	c.db.record(call{kind: kindExec, query: query, args: args, connID: c.id})
	onExec, _ := c.db.handlers()
	if onExec != nil {
		if err := onExec(query, args); err != nil {
			return nil, err
		}
	}
	return driver.RowsAffected(1), nil
}

func (c *fakeConn) QueryContext(_ context.Context, query string, named []driver.NamedValue) (driver.Rows, error) {
	args := values(named)
	c.db.record(call{kind: kindQuery, query: query, args: args, connID: c.id})
	_, onQuery := c.db.handlers()
	if onQuery == nil {
		return nil, errors.New("fakeConn: unexpected query")
	}
	v, err := onQuery(query, args)
	if err != nil {
		return nil, err
	}
	return &singleRow{value: v}, nil
}

// singleRow — результат из одной строки с одной колонкой.
type singleRow struct {
	value driver.Value
	done  bool
}

func (r *singleRow) Columns() []string { return []string{"result"} }
func (r *singleRow) Close() error      { return nil }
func (r *singleRow) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.value
	return nil
}

// normalizeSQL убирает все пробельные символы, завершающую ';' и регистр,
// чтобы сравнение SQL не зависело от форматирования.
func normalizeSQL(q string) string {
	var b strings.Builder
	for _, r := range q {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return strings.ToLower(strings.TrimRight(b.String(), ";"))
}

func sameSQL(got, want string) bool { return normalizeSQL(got) == normalizeSQL(want) }
