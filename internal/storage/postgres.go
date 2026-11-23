// Package storage сохраняет курсы в PostgreSQL.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"currency/internal/rates"
)

// ErrAlreadyRunning возвращается AcquireLock, если блокировку держит другой экземпляр программы.
var ErrAlreadyRunning = errors.New("another instance is already running")

// Postgres — хранилище курсов. Получает уже открытый *sql.DB: пул соединений
// создаётся один раз на процесс, а не на каждую запись.
type Postgres struct {
	db *sql.DB
}

// NewPostgres создаёт хранилище поверх готового пула соединений.
func NewPostgres(db *sql.DB) *Postgres {
	return &Postgres{db: db}
}

// Save сохраняет один курс. Значения передаются параметрами, а не подставляются
// в текст запроса, — это исключает SQL-инъекцию через данные от банка.
func (p *Postgres) Save(ctx context.Context, r rates.Rate) error {
	const query = `INSERT INTO currency (bank, "from", "to", value) VALUES ($1, $2, $3, $4)`
	if _, err := p.db.ExecContext(ctx, query, r.Bank, r.From, r.To, r.Value); err != nil {
		return fmt.Errorf("insert rate %s %s/%s: %w", r.Bank, r.From, r.To, err)
	}
	return nil
}

// AcquireLock берёт сессионную advisory-блокировку PostgreSQL, чтобы два экземпляра
// программы (в том числе на разных машинах) не писали одни и те же курсы параллельно.
//
// Advisory-блокировка привязана к сессии, поэтому держим выделенное соединение
// до вызова release: если бы запрос ушёл в произвольное соединение пула,
// блокировка «потерялась» бы вместе с ним. При падении процесса соединение
// рвётся и PostgreSQL снимает блокировку сам.
func (p *Postgres) AcquireLock(ctx context.Context, key int64) (release func() error, err error) {
	conn, err := p.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("get connection: %w", err)
	}

	var locked bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&locked); err != nil {
		return nil, errors.Join(fmt.Errorf("try advisory lock: %w", err), conn.Close())
	}
	if !locked {
		if err := conn.Close(); err != nil {
			return nil, errors.Join(ErrAlreadyRunning, fmt.Errorf("close connection: %w", err))
		}
		return nil, ErrAlreadyRunning
	}

	release = func() error {
		// context.Background(): release вызывается при завершении, когда основной
		// контекст обычно уже отменён, а снять блокировку всё равно нужно.
		_, unlockErr := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", key)
		if unlockErr != nil {
			unlockErr = fmt.Errorf("advisory unlock: %w", unlockErr)
		}
		return errors.Join(unlockErr, conn.Close())
	}
	return release, nil
}
