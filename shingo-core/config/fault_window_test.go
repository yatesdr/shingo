package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The fault window is two numbers and the inner one decides what an operator is
// told. These tests pin the shipped default and the degrade-don't-die
// behaviour, because a core that will not boot over a wording threshold is a
// worse outcome than a core that boots with the shipped one.

func TestDefaults_FaultWindow(t *testing.T) {
	t.Parallel()
	c := Defaults()
	if c.RDS.FaultNoticeAfter != 60*time.Second {
		t.Errorf("FaultNoticeAfter default = %s, want 60s — three times the 20s median replan",
			c.RDS.FaultNoticeAfter)
	}
	if err := c.RDS.Validate(); err != nil {
		t.Errorf("the shipped defaults must validate: %v", err)
	}
}

func TestRDSConfig_Validate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		grace   time.Duration
		notice  time.Duration
		wantErr bool
	}{
		{"shipped", 45 * time.Minute, 60 * time.Second, false},
		{"notice unset is not a threshold", 45 * time.Minute, 0, true},
		{"negative notice", 45 * time.Minute, -time.Second, true},
		{"notice equal to grace can never fire", 60 * time.Second, 60 * time.Second, true},
		{"notice past grace can never fire", 30 * time.Second, 60 * time.Second, true},
		{"grace unset leaves the notice alone", 0, 60 * time.Second, false},
		{"a tight but ordered window is fine", 90 * time.Second, 30 * time.Second, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := RDSConfig{FaultGrace: tc.grace, FaultNoticeAfter: tc.notice}.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate(grace=%s, notice=%s) error = %v, wantErr %v",
					tc.grace, tc.notice, err, tc.wantErr)
			}
		})
	}
}

// A plant file with a fault window that cannot work still boots. It boots with
// a working window and a log line, which is the difference between a wording
// bug and a plant outage.
func TestLoad_BadFaultWindowDegradesRatherThanFailing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		yaml string
		want time.Duration
	}{
		{
			// The realistic mistake: someone shortens the grace window and does
			// not think about the notice sitting inside it.
			name: "grace shorter than the default notice takes half the window",
			yaml: "rds:\n  fault_grace: 40s\n  fault_notice_after: 90s\n",
			want: 20 * time.Second,
		},
		{
			name: "an explicit zero notice falls back to the shipped default",
			yaml: "rds:\n  fault_grace: 45m\n  fault_notice_after: 0s\n",
			want: 60 * time.Second,
		},
		{
			name: "an absent notice keeps the default rather than degrading",
			yaml: "rds:\n  fault_grace: 45m\n",
			want: 60 * time.Second,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load must not fail over a fault window: %v", err)
			}
			if cfg.RDS.FaultNoticeAfter != tc.want {
				t.Errorf("FaultNoticeAfter = %s, want %s", cfg.RDS.FaultNoticeAfter, tc.want)
			}
			if err := cfg.RDS.Validate(); err != nil {
				t.Errorf("the degraded window must itself be valid: %v", err)
			}
		})
	}
}
