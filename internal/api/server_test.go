package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/nexusriot/beastiemon/internal/config"
	"github.com/nexusriot/beastiemon/internal/store"
)

func testServer(auth config.AuthConfig) *Server {
	return New(store.NewRing(4), fstest.MapFS{}, auth)
}

func get(s *Server, target string, set func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if set != nil {
		set(req)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestAuthDisabledAllowsEverything(t *testing.T) {
	s := testServer(config.AuthConfig{})
	if rec := get(s, "/api/host", nil); rec.Code != http.StatusOK {
		t.Fatalf("auth disabled: want 200, got %d", rec.Code)
	}
}

func TestBasicAuth(t *testing.T) {
	s := testServer(config.AuthConfig{Username: "admin", Password: "s3cret"})

	t.Run("no credentials -> 401 with challenge", func(t *testing.T) {
		rec := get(s, "/api/host", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got == "" {
			t.Fatal("expected WWW-Authenticate challenge for Basic auth")
		}
	})

	t.Run("wrong password -> 401", func(t *testing.T) {
		rec := get(s, "/api/host", func(r *http.Request) { r.SetBasicAuth("admin", "nope") })
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rec.Code)
		}
	})

	t.Run("correct credentials -> 200", func(t *testing.T) {
		rec := get(s, "/api/host", func(r *http.Request) { r.SetBasicAuth("admin", "s3cret") })
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", rec.Code)
		}
	})

	t.Run("healthz stays open", func(t *testing.T) {
		if rec := get(s, "/healthz", nil); rec.Code != http.StatusOK {
			t.Fatalf("healthz should be unauthenticated: got %d", rec.Code)
		}
	})
}

func TestTokenAuth(t *testing.T) {
	s := testServer(config.AuthConfig{Token: "abc123"})

	t.Run("no token -> 401 without Basic challenge", func(t *testing.T) {
		rec := get(s, "/api/host", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "" {
			t.Fatalf("token-only auth should not send a Basic challenge, got %q", got)
		}
	})

	t.Run("bearer header -> 200", func(t *testing.T) {
		rec := get(s, "/api/host", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer abc123")
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", rec.Code)
		}
	})

	t.Run("query parameter -> 200", func(t *testing.T) {
		rec := get(s, "/api/host?token=abc123", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", rec.Code)
		}
	})

	t.Run("wrong token -> 401", func(t *testing.T) {
		rec := get(s, "/api/host", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer wrong")
		})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rec.Code)
		}
	})
}

// TestCloseUnblocksStream proves Server.Close makes a blocked SSE handler
// return, so http.Server.Shutdown isn't held open by never-idle connections.
func TestCloseUnblocksStream(t *testing.T) {
	s := testServer(config.AuthConfig{})
	done := make(chan struct{})
	go func() {
		get(s, "/api/stream", nil)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond) // let the handler subscribe and block
	s.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleStream did not return after Server.Close")
	}
}
