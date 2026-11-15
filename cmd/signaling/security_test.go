package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestRequireBearer(t *testing.T) {
	for _, tc := range []struct {
		name, token string
		headers     []string
		want        bool
	}{
		{"valid", "secret", []string{"Bearer secret"}, true},
		{"missing", "secret", nil, false},
		{"wrong", "secret", []string{"Bearer other"}, false},
		{"session credential", "secret", []string{"astra.auth.secret"}, false},
		{"duplicate", "secret", []string{"Bearer secret", "Bearer secret"}, false},
		{"unconfigured", "", []string{"Bearer "}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/ws/agent/service", nil)
			for _, value := range tc.headers {
				r.Header.Add("Authorization", value)
			}
			w := httptest.NewRecorder()
			if got := requireBearer(w, r, tc.token); got != tc.want {
				t.Fatalf("allowed=%v", got)
			}
			if !tc.want && (w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer") {
				t.Fatal("missing authentication challenge")
			}
		})
	}
}

func TestWebSocketOrigin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			conn.Close()
		}
	}))
	defer server.Close()
	for _, origin := range []string{"", server.URL, "https://untrusted.invalid"} {
		header := http.Header{}
		if origin != "" {
			header.Set("Origin", origin)
		}
		conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), header)
		if conn != nil {
			conn.Close()
		}
		if response != nil {
			response.Body.Close()
		}
		if origin == "https://untrusted.invalid" {
			if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
				t.Fatal("cross-origin accepted")
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
}
