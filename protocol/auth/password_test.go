package auth

import "testing"

func TestCheckPassword(t *testing.T) {
	hash, err := HashPassword("correct-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	tests := []struct {
		name     string
		hash     string
		password string
		want     bool
	}{
		{"correct password", hash, "correct-password", true},
		{"wrong password", hash, "wrong-password", false},
		{"empty hash", "", "anything", false},
		{"empty password", hash, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckPassword(tc.hash, tc.password)
			if got != tc.want {
				t.Errorf("CheckPassword(%q, %q) = %v, want %v", tc.hash, tc.password, got, tc.want)
			}
		})
	}
}

func TestHashPassword(t *testing.T) {
	hash, err := HashPassword("test-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if len(hash) == 0 {
		t.Fatal("HashPassword returned empty hash")
	}
	if !CheckPassword(hash, "test-password") {
		t.Error("CheckPassword failed for hash produced by HashPassword")
	}
}
