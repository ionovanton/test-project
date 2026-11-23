// Package bank1 реализует получение курсов из API «Банка 1».
package bank1

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"currency/internal/bank/bankhttp"
	"currency/internal/rates"
)

const name = "Bank 1"

// Bank — клиент API «Банка 1».
type Bank struct {
	client  *http.Client
	baseURL string
}

var _ rates.Provider = (*Bank)(nil)

// New создаёт клиент. HTTP-клиент передаётся снаружи, чтобы таймауты
// настраивались в одном месте, а в тестах можно было подставить httptest-сервер.
func New(client *http.Client, baseURL string) *Bank {
	return &Bank{
		client:  client,
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

// Name возвращает человекочитаемое имя банка, под которым курс пишется в БД.
func (b *Bank) Name() string { return name }

// GetRates возвращает курс RUB→USD.
func (b *Bank) GetRates(ctx context.Context) ([]rates.Rate, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+"/rates/rub-usd", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	body, err := bankhttp.GetBody(b.client, req)
	if err != nil {
		return nil, err
	}

	// Банк отдаёт число в русской локали с десятичной запятой ("0,0105"),
	// а strconv.ParseFloat понимает только точку.
	value, err := bankhttp.ParseRate(strings.ReplaceAll(body, ",", "."))
	if err != nil {
		return nil, err
	}

	return []rates.Rate{{Bank: name, From: "RUB", To: "USD", Value: value}}, nil
}
