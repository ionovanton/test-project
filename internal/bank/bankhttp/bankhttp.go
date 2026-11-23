// Package bankhttp содержит общий для банков код похода по HTTP за курсом.
// Банки отличаются URL, заголовками и форматом числа, а транспорт,
// проверка статуса и разбор ответа у всех одинаковые — держим их в одном месте,
// чтобы исправление (например, лимита на тело) не пришлось копировать в каждый банк.
package bankhttp

import (
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"currency/internal/rates"
)

// maxBodySize ограничивает тело ответа: курс — это одно число, и если банк
// (или прокси перед ним) вернул мегабайты, читать их в память незачем.
const maxBodySize = 1 << 10 // 1 KiB

// GetBody выполняет запрос и возвращает тело ответа без пробельных символов по краям.
// Ошибки статуса оборачивают rates.ErrUnexpectedStatus, ошибки содержимого — rates.ErrParse,
// чтобы вызывающий код мог различать их через errors.Is.
func GetBody(client *http.Client, req *http.Request) (string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("do request: %w", err)
	}
	// Ошибку Close у тела ответа игнорируем осознанно: данные уже прочитаны
	// (или мы и так возвращаем ошибку), а повлиять на результат она не может.
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: %d %s", rates.ErrUnexpectedStatus, resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	// Пустой Content-Type допускаем (Go и многие серверы его не выставляют для простого текста),
	// но явный не-текстовый ответ (например, HTML-заглушка прокси со статусом 200) отбрасываем сразу.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		mediaType, _, err := mime.ParseMediaType(ct)
		if err != nil || mediaType != "text/plain" {
			return "", fmt.Errorf("%w: unexpected content type %q", rates.ErrParse, ct)
		}
	}

	// Читаем на байт больше лимита, чтобы отличить «ровно лимит» от «обрезали».
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize+1))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	if len(body) > maxBodySize {
		return "", fmt.Errorf("%w: body exceeds %d bytes", rates.ErrParse, maxBodySize)
	}

	return strings.TrimSpace(string(body)), nil
}

// ParseRate разбирает курс и отбрасывает заведомо бессмысленные значения:
// strconv.ParseFloat принимает "NaN" и "Inf", а нулевой или отрицательный курс
// в БД хуже, чем пропущенный.
func ParseRate(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q: %w", rates.ErrParse, s, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0, fmt.Errorf("%w: invalid value %q", rates.ErrParse, s)
	}
	return v, nil
}
