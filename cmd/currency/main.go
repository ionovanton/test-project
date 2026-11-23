// Команда currency собирает курсы валют из банков и сохраняет их в PostgreSQL.
//
// Использование:
//
//	currency update                 — один проход по всем банкам
//	currency watch [-interval 1m]   — проходы по расписанию до SIGINT/SIGTERM
//	currency help                   — справка
//
// Конфигурация берётся из переменных окружения, чтобы секреты не попадали в код и историю shell:
//
//	DATABASE_URL  — строка подключения к PostgreSQL (обязательна)
//	BANK2_TOKEN   — токен API «Банка 2» (обязателен)
//	BANK1_URL     — базовый URL «Банка 1» (по умолчанию http://bank.example.com)
//	BANK2_URL     — базовый URL «Банка 2» (по умолчанию http://bank2.example.com)
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"currency/internal/bank/bank1"
	"currency/internal/bank/bank2"
	"currency/internal/rates"
	"currency/internal/storage"
	"currency/internal/updater"
)

// lockKey — ключ advisory-блокировки в PostgreSQL. Константа должна совпадать
// у всех экземпляров, иначе они не увидят друг друга.
const lockKey int64 = 0x63757272656e6379 // "currency" в ASCII

const usage = `Usage: currency <command> [flags]

Commands:
  update                 fetch rates from all banks once and save them
  watch [-interval 1m]   fetch and save rates periodically until interrupted
  help                   show this help

Environment:
  DATABASE_URL  PostgreSQL connection string (required)
  BANK2_TOKEN   Bank 2 API token (required)
  BANK1_URL     Bank 1 base URL (default http://bank.example.com)
  BANK2_URL     Bank 2 base URL (default http://bank2.example.com)
`

// errUsage означает неверный вызов: справка уже выведена, нужен код выхода 2.
var errUsage = errors.New("invalid usage")

func main() {
	// Отмена контекста по сигналу даёт текущим HTTP-запросам и запросам в БД
	// завершиться корректно, а deferred-вызовы (снятие блокировки, закрытие пула) — отработать.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stderr)
	stop()

	switch {
	case err == nil:
	case errors.Is(err, errUsage):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "currency:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}

	cmd, cmdArgs := args[0], args[1:]
	switch cmd {
	case "update":
		fs := newFlagSet(cmd, stderr)
		if err := fs.Parse(cmdArgs); err != nil {
			return flagErr(err)
		}
		return withUpdater(ctx, stderr, func(u *updater.Updater) error {
			return u.RunOnce(ctx)
		})

	case "watch":
		fs := newFlagSet(cmd, stderr)
		interval := fs.Duration("interval", time.Minute, "polling interval")
		if err := fs.Parse(cmdArgs); err != nil {
			return flagErr(err)
		}
		if *interval <= 0 {
			fmt.Fprintln(stderr, "interval must be positive")
			return errUsage
		}
		return withUpdater(ctx, stderr, func(u *updater.Updater) error {
			return u.Run(ctx, *interval)
		})

	case "help", "-h", "-help", "--help":
		fmt.Fprint(stderr, usage)
		return nil

	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return errUsage
	}
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// flagErr переводит ошибку разбора флагов в код выхода: -h — не ошибка.
func flagErr(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return errUsage
}

// config — настройки из окружения.
type config struct {
	databaseURL string
	bank1URL    string
	bank2URL    string
	bank2Token  string
}

func loadConfig() (config, error) {
	cfg := config{
		databaseURL: os.Getenv("DATABASE_URL"),
		bank1URL:    envOr("BANK1_URL", "http://bank.example.com"),
		bank2URL:    envOr("BANK2_URL", "http://bank2.example.com"),
		bank2Token:  os.Getenv("BANK2_TOKEN"),
	}

	var errs []error
	if cfg.databaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if cfg.bank2Token == "" {
		errs = append(errs, errors.New("BANK2_TOKEN is required"))
	}
	return cfg, errors.Join(errs...)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// withUpdater собирает зависимости (БД, блокировку, банки), вызывает fn
// и гарантированно освобождает ресурсы в обратном порядке.
func withUpdater(ctx context.Context, stderr io.Writer, fn func(*updater.Updater) error) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))

	db, err := openDB(ctx, cfg.databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	store := storage.NewPostgres(db)

	release, err := store.AcquireLock(ctx, lockKey)
	if err != nil {
		if errors.Is(err, storage.ErrAlreadyRunning) {
			return fmt.Errorf("%w, exiting", err)
		}
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer func() {
		if err := release(); err != nil {
			logger.Error("release lock", "error", err)
		}
	}()

	// Без таймаута http.Client может ждать зависший банк бесконечно.
	client := &http.Client{Timeout: 10 * time.Second}

	providers := []rates.Provider{
		bank1.New(client, cfg.bank1URL),
		bank2.New(client, cfg.bank2URL, cfg.bank2Token),
	}

	return fn(updater.New(providers, store, logger))
}

// openDB открывает пул один раз на процесс и сразу проверяет подключение:
// sql.Open соединение не устанавливает, и без Ping ошибка конфигурации
// всплыла бы только на первой записи.
func openDB(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Нагрузка — несколько вставок в минуту плюс одно соединение под блокировку,
	// так что большой пул не нужен; ограничение срока жизни помогает
	// переживать перезапуски БД и балансировщиков.
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		return nil, errors.Join(fmt.Errorf("ping db: %w", err), db.Close())
	}
	return db, nil
}
