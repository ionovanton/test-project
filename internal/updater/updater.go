// Package updater опрашивает банки и сохраняет полученные курсы.
// Здесь живёт бизнес-логика, которая в исходнике была размазана по main.
package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"currency/internal/rates"
)

// Saver — то, что нужно updater'у от хранилища. Интерфейс объявлен на стороне
// потребителя, чтобы в тестах подменять БД без поднятия PostgreSQL.
type Saver interface {
	Save(ctx context.Context, r rates.Rate) error
}

// Updater связывает источники курсов с хранилищем.
type Updater struct {
	providers []rates.Provider
	saver     Saver
	logger    *slog.Logger
}

// New создаёт Updater. Новый банк добавляется реализацией rates.Provider
// и передачей её сюда — сам Updater при этом не меняется.
func New(providers []rates.Provider, saver Saver, logger *slog.Logger) *Updater {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Updater{providers: providers, saver: saver, logger: logger}
}

// fetchResult — итог опроса одного банка.
type fetchResult struct {
	rates []rates.Rate
	err   error
}

// RunOnce выполняет один проход: опрашивает все банки и сохраняет курсы.
// Ошибка одного банка не мешает остальным; все ошибки возвращаются вместе.
func (u *Updater) RunOnce(ctx context.Context) error {
	// Банки опрашиваются конкурентно: время прохода определяется самым медленным
	// банком, а не суммой задержек всех. Каждая горутина пишет только в свой
	// элемент слайса, поэтому мьютекс не нужен.
	results := make([]fetchResult, len(u.providers))
	var wg sync.WaitGroup
	for i, p := range u.providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rs, err := p.GetRates(ctx)
			results[i] = fetchResult{rates: rs, err: err}
		}()
	}
	wg.Wait()

	// Сохраняем последовательно: запись в БД быстрая по сравнению с сетью
	// до банков, а Saver тогда не обязан быть безопасным для конкурентного вызова.
	var errs []error
	for i, p := range u.providers {
		name := p.Name()
		res := results[i]
		if res.err != nil {
			errs = append(errs, fmt.Errorf("bank %q: %w", name, res.err))
			continue
		}
		for _, r := range res.rates {
			if err := u.saver.Save(ctx, r); err != nil {
				errs = append(errs, fmt.Errorf("bank %q: save %s/%s: %w", name, r.From, r.To, err))
				continue
			}
			u.logger.InfoContext(ctx, "rate saved", "bank", r.Bank, "from", r.From, "to", r.To, "value", r.Value)
		}
	}
	return errors.Join(errs...)
}

// Run выполняет проход сразу и затем каждые interval, пока не отменён ctx.
// Ошибки отдельных проходов логируются и не останавливают работу:
// временная недоступность банка или БД не должна ронять сервис.
func (u *Updater) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		// time.NewTicker паникует на неположительном интервале.
		return fmt.Errorf("interval must be positive, got %s", interval)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		u.runPass(ctx, interval)

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// runPass ограничивает проход интервалом, чтобы зависший банк или БД
// не накапливали отставание: дедлайн наследуется всеми запросами прохода,
// а отмена родительского ctx (сигнал завершения) прерывает их сразу.
func (u *Updater) runPass(ctx context.Context, timeout time.Duration) {
	passCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := u.RunOnce(passCtx); err != nil && ctx.Err() == nil {
		u.logger.ErrorContext(ctx, "update failed", "error", err)
	}
}
