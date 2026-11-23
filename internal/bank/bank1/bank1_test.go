package bank1_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"currency/internal/bank/bank1"
	"currency/internal/rates"
)

// newServer поднимает тестовый HTTP-сервер и регистрирует его закрытие.
func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestBank_ImplementsProvider(t *testing.T) {
	t.Parallel()
	var p rates.Provider = bank1.New(http.DefaultClient, "http://unused")
	if got := p.Name(); got != "Bank 1" {
		t.Fatalf("Name() = %q, want %q", got, "Bank 1")
	}
}

func TestBank_GetRates_Request(t *testing.T) {
	t.Parallel()

	type req struct {
		method, path, query string
	}
	got := make(chan req, 1)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		got <- req{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
		_, _ = w.Write([]byte("0,0105"))
	})

	b := bank1.New(srv.Client(), srv.URL)
	if _, err := b.GetRates(context.Background()); err != nil {
		t.Fatalf("GetRates() error = %v", err)
	}

	select {
	case r := <-got:
		if r.method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.method)
		}
		if r.path != "/rates/rub-usd" {
			t.Errorf("path = %q, want /rates/rub-usd", r.path)
		}
		if r.query != "" {
			t.Errorf("unexpected query %q", r.query)
		}
	default:
		t.Fatal("server did not receive a request")
	}
}

func TestBank_GetRates_Parsing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		body    string
		want    float64
		wantErr error
	}{
		{name: "comma decimal", status: http.StatusOK, body: "0,0105", want: 0.0105},
		{name: "surrounding whitespace", status: http.StatusOK, body: " 0,0105\n", want: 0.0105},
		{name: "integer", status: http.StatusOK, body: "2", want: 2},
		{name: "not a number", status: http.StatusOK, body: "abc", wantErr: rates.ErrParse},
		{name: "empty body", status: http.StatusOK, body: "", wantErr: rates.ErrParse},
		{name: "negative", status: http.StatusOK, body: "-1", wantErr: rates.ErrParse},
		{name: "zero", status: http.StatusOK, body: "0", wantErr: rates.ErrParse},
		{name: "zero with comma", status: http.StatusOK, body: "0,0", wantErr: rates.ErrParse},
		{name: "NaN", status: http.StatusOK, body: "NaN", wantErr: rates.ErrParse},
		{name: "Inf", status: http.StatusOK, body: "Inf", wantErr: rates.ErrParse},
		{name: "+Inf", status: http.StatusOK, body: "+Inf", wantErr: rates.ErrParse},
		{name: "status 500", status: http.StatusInternalServerError, body: "0,0105", wantErr: rates.ErrUnexpectedStatus},
		{name: "status 404", status: http.StatusNotFound, body: "not found", wantErr: rates.ErrUnexpectedStatus},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})

			got, err := bank1.New(srv.Client(), srv.URL).GetRates(context.Background())

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("GetRates() error = %v, want errors.Is(err, %v)", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("GetRates() unexpected error = %v", err)
			}
			want := []rates.Rate{{Bank: "Bank 1", From: "RUB", To: "USD", Value: tt.want}}
			if len(got) != len(want) || got[0] != want[0] {
				t.Fatalf("GetRates() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestBank_GetRates_ContextCanceled(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	// Разблокируем обработчик до srv.Close (Cleanup выполняется в LIFO-порядке).
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := bank1.New(srv.Client(), srv.URL).GetRates(ctx)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("GetRates() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetRates() did not respect context cancellation")
	}
}

func TestBank_GetRates_ContextCanceledInFlight(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() {
		_, err := bank1.New(srv.Client(), srv.URL).GetRates(ctx)
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach the server")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("GetRates() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetRates() did not return after context cancellation")
	}
}

func TestBank_GetRates_ConnectionError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // адрес гарантированно больше не слушается

	_, err := bank1.New(srv.Client(), url).GetRates(context.Background())
	if err == nil {
		t.Fatal("GetRates() error = nil, want transport error")
	}
	if errors.Is(err, rates.ErrParse) || errors.Is(err, rates.ErrUnexpectedStatus) {
		t.Fatalf("transport error must not be classified as parse/status error: %v", err)
	}
}
