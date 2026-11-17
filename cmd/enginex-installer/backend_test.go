package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSetupBackendUsesDeckAPI(t *testing.T) {
	for _, password := range []string{"backend-password", ""} {
		t.Run(fmt.Sprintf("password=%t", password != ""), func(t *testing.T) {
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/relay" || r.Header.Get("Authorization") != "Bearer ABCD-2345" {
					t.Errorf("wrong deck API request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(401)
					return
				}
				if r.Method == "POST" {
					writes++
					var payload map[string]string
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["relayUrl"] != "https://backend.example" {
						t.Errorf("backend settings: %v %v", payload, err)
					}
					if got, present := payload["deemixPassword"]; got != password || present != (password != "") {
						t.Error("blank password must preserve the stored password")
					}
				}
				fmt.Fprint(w, `{"relayUrl":"https://backend.example","deemixPasswordSet":true}`)
			}))
			defer srv.Close()
			o := testOpts(t)
			o.SetupBackend = true
			o.In = strings.NewReader(srv.URL + "\nABCD-2345\n\n" + password + "\n")
			if err := run(ctxT(t), o); err != nil {
				t.Fatal(err)
			}
			if writes != 1 || password != "" && strings.Contains(fmt.Sprint(o.Out), password) {
				t.Fatalf("writes=%d or secret in output: %s", writes, o.Out)
			}
		})
	}
}

func TestSetupBackendDoesNotForwardCredentials(t *testing.T) {
	for _, status := range []int{401, 302} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/other")
				w.WriteHeader(status)
			}))
			defer srv.Close()
			o := testOpts(t)
			o.SetupBackend = true
			o.In = strings.NewReader(srv.URL + "\nBAD-CODE\nhttps://backend.example\nsecret\n")
			if err := run(ctxT(t), o); err == nil || calls != 1 {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
		})
	}
}
