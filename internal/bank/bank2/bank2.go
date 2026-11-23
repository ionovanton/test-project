// Package bank2 реализует получение курсов из API «Банка 2».
package bank2

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"currency/internal/bank/bankhttp"
	"currency/internal/rates"
)

const name = "Bank 2"

// Bank — клиент API «Банка 2». В отличие от «Банка 1» требует авторизации.
type Bank struct {
	client  *http.Client
	baseURL string
	token   string
}

var _ rates.Provider = (*Bank)(nil)

// New создаёт клиент. Токен приходит из конфигурации, а не из кода,
// чтобы секрет не попадал в репозиторий и мог меняться без пересборки.
func New(client *http.Client, baseURL, token string) *Bank {
	return &Bank{
		client:  client,
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
	}
}

// Name возвращает человекочитаемое имя банка, под которым курс пишется в БД.
func (b *Bank) Name() string { return name }

// GetRates возвращает курс RUB→USD.
func (b *Bank) GetRates(ctx context.Context) ([]rates.Rate, error) {
	// В API банка рубль обозначается устаревшим кодом RUR; наружу отдаём
	// нормализованный ISO 4217 код RUB, чтобы курсы разных банков были сопоставимы.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+"/rates?currFrom=RUR&currTo=USD", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Нестандартный формат заголовка — требование API банка.
	req.Header.Set("Authorization", `auth_token="`+b.token+`"`)

	body, err := bankhttp.GetBody(b.client, req)
	if err != nil {
		return nil, err
	}

	value, err := bankhttp.ParseRate(body)
	if err != nil {
		return nil, err
	}

	return []rates.Rate{{Bank: name, From: "RUB", To: "USD", Value: value}}, nil
}
