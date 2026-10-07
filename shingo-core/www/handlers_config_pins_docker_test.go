//go:build docker

package www

// U0 pins for Core's password door (handleConfigPassword) at 5c0beb74. Split
// from handlers_config_pins_test.go because the door reads the admin store,
// which needs the Postgres test container. Uses the helpers in
// handlers_config_test.go (testHandlersWithConfigPath, loggedInSession,
// postPassword). The door stays after U2 (C8 only gives it a page caller), so
// every case here is predicted unchanged.

import (
	"encoding/json"
	"net/http"
	"os"
	"shingo/protocol/testutil"
	"testing"
)

func TestPinConfig_Password(t *testing.T) {
	cases := []struct {
		name        string
		login       bool
		body        string
		wantStatus  int
		wantOK      bool
		wantMessage string
		wantRotated bool
	}{
		{
			name: "NotLoggedIn", login: false,
			body:       `{"old_password":"admin","new_password":"x"}`,
			wantStatus: http.StatusUnauthorized, wantMessage: "not logged in",
		},
		{
			name: "MalformedJSON", login: true, body: `{"old_password":`,
			wantStatus: http.StatusBadRequest, wantMessage: "unexpected EOF",
		},
		{
			name: "EmptyNewPassword", login: true,
			body:       `{"old_password":"admin","new_password":""}`,
			wantStatus: http.StatusBadRequest, wantMessage: "new password is required",
		},
		{
			// The empty check runs before the current-password check.
			name: "EmptyNewAndWrongCurrent", login: true,
			body:       `{"old_password":"wrong","new_password":""}`,
			wantStatus: http.StatusBadRequest, wantMessage: "new password is required",
		},
		{
			name: "WrongCurrent", login: true,
			body:       `{"old_password":"wrong","new_password":"pin-new-password"}`,
			wantStatus: http.StatusBadRequest, wantMessage: "current password is incorrect",
		},
		{
			name: "Success", login: true,
			body:       `{"old_password":"admin","new_password":"pin-new-password"}`,
			wantStatus: http.StatusOK, wantOK: true, wantRotated: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, cfgPath := testHandlersWithConfigPath(t)
			h.ensureDefaultAdmin()
			cookie := (*http.Cookie)(nil)
			if tc.login {
				cookie = loggedInSession(t, h)
			}
			before, err := h.engine.AdminService().GetUser("admin")
			if err != nil {
				t.Fatalf("GetUser before: %v", err)
			}
			cfgBefore, err := os.ReadFile(cfgPath)
			testutil.MustNoErr(t, err, "read config before")

			rec := postPassword(t, h, cookie, tc.body)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status: got %d, want %d; body=%q", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type: got %q", ct)
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
			if resp["ok"] != tc.wantOK {
				t.Errorf("ok: got %v, want %v", resp["ok"], tc.wantOK)
			}
			if tc.wantOK {
				if len(resp) != 1 {
					t.Errorf("success body: got %v, want exactly {ok:true}", resp)
				}
			} else if msg, _ := resp["message"].(string); msg != tc.wantMessage {
				t.Errorf("message: got %q, want %q", msg, tc.wantMessage)
			}

			after, err := h.engine.AdminService().GetUser("admin")
			if err != nil {
				t.Fatalf("GetUser after: %v", err)
			}
			if rotated := after.PasswordHash != before.PasswordHash; rotated != tc.wantRotated {
				t.Errorf("hash rotated: got %v, want %v", rotated, tc.wantRotated)
			}
			// The password door never touches the config file.
			cfgAfter, err := os.ReadFile(cfgPath)
			testutil.MustNoErr(t, err, "read config after")
			if string(cfgAfter) != string(cfgBefore) {
				t.Errorf("config file changed by the password door")
			}
		})
	}
}
