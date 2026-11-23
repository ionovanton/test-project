// Package rates описывает общий контракт для всех банков-источников курсов.
package rates

import (
	"context"
	"errors"
)

// Rate — курс одной валютной пары, полученный от конкретного банка.
type Rate struct {
	Bank  string
	From  string
	To    string
	Value float64
}

// Provider — источник курсов (банк). Каждая реализация живёт в своём пакете
// и инкапсулирует особенности API конкретного банка.
type Provider interface {
	Name() string
	GetRates(ctx context.Context) ([]Rate, error)
}

var (
	// ErrUnexpectedStatus — банк ответил статусом, отличным от 200 OK.
	ErrUnexpectedStatus = errors.New("unexpected response status")
	// ErrParse — тело ответа банка не удалось разобрать как курс.
	ErrParse = errors.New("cannot parse rate")
)
