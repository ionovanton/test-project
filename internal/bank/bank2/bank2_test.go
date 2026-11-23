package bank2_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"currency/internal/bank/bank2"
	"currency/internal/rates"
)

const testToken = "secret-token"

func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestBank_ImplementsProvider(t *testing.T) {
	t.Parallel()
	var p rates.Provider = bank2.New(http.DefaultClient, "http://unused", testToken)
	if got := p.Name(); got != "Bank 2" {
		t.Fatalf("Name() = %q, want %q", got, "Bank 2")
	}
}

func TestBank_GetRates_Request(t *testing.T) {
	t.Parallel()

	type req struct {
		method string
		path   string
		query  url.Values
		auth   []string
	}
	got := make(chan req, 1)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		got <- req{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.Query(),
			auth:   r.Header.Values("Authorization"),
		}
		_, _ = w.Write([]byte("0.0107"))
	})

	b := bank2.New(srv.Client(), srv.URL, testToken)
	if _, err := b.GetRates(context.Background()); err != nil {
		t.Fatalf("GetRates() error = %v", err)
	}

	select {
	case r := <-got:
		if r.method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.method)
		}
		if r.path != "/rates" {
			t.Errorf("path = %q, want /rates", r.path)
		}
		wantQuery := url.Values{"currFrom": {"RUR"}, "currTo": {"USD"}}
		if len(r.query) != len(wantQuery) {
			t.Errorf("query = %v, want %v", r.query, wantQuery)
		}
		for k, v := range wantQuery {
			if g := r.query[k]; len(g) != 1 || g[0] != v[0] {
				t.Errorf("query[%q] = %v, want %v", k, g, v)
			}
		}
		wantAuth := `auth_token="` + testToken + `"`
		if len(r.auth) != 1 || r.auth[0] != wantAuth {
			t.Errorf("Authorization = %q, want exactly [%q]", r.auth, wantAuth)
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
		{name: "dot decimal", status: http.StatusOK, body: "0.0107", want: 0.0107},
		{name: "surrounding whitespace", status: http.StatusOK, body: " 0.0107\n", want: 0.0107},
		{name: "integer", status: http.StatusOK, body: "3", want: 3},
		{name: "not a number", status: http.StatusOK, body: "abc", wantErr: rates.ErrParse},
		{name: "empty body", status: http.StatusOK, body: "", wantErr: rates.ErrParse},
		{name: "negative", status: http.StatusOK, body: "-1", wantErr: rates.ErrParse},
		{name: "zero", status: http.StatusOK, body: "0", wantErr: rates.ErrParse},
		{name: "NaN", status: http.StatusOK, body: "NaN", wantErr: rates.ErrParse},
		{name: "Inf", status: http.StatusOK, body: "Inf", wantErr: rates.ErrParse},
		{name: "-Inf", status: http.StatusOK, body: "-Inf", wantErr: rates.ErrParse},
		{name: "status 500", status: http.StatusInternalServerError, body: "0.0107", wantErr: rates.ErrUnexpectedStatus},
		{name: "status 404", status: http.StatusNotFound, body: "not found", wantErr: rates.ErrUnexpectedStatus},
		{name: "status 401", status: http.StatusUnauthorized, body: "", wantErr: rates.ErrUnexpectedStatus},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})

			got, err := bank2.New(srv.Client(), srv.URL, testToken).GetRates(context.Background())

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("GetRates() error = %v, want errors.Is(err, %v)", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("GetRates() unexpected error = %v", err)
			}
			// RUR из API банка нормализуется в ISO-код RUB.
			want := []rates.Rate{{Bank: "Bank 2", From: "RUB", To: "USD", Value: tt.want}}
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
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := bank2.New(srv.Client(), srv.URL, testToken).GetRates(ctx)
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
		_, err := bank2.New(srv.Client(), srv.URL, testToken).GetRates(ctx)
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
	addr := srv.URL
	srv.Close()

	_, err := bank2.New(srv.Client(), addr, testToken).GetRates(context.Background())
	if err == nil {
		t.Fatal("GetRates() error = nil, want transport error")
	}
	if errors.Is(err, rates.ErrParse) || errors.Is(err, rates.ErrUnexpectedStatus) {
		t.Fatalf("transport error must not be classified as parse/status error: %v", err)
	}
}
