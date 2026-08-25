package main

import (
	"bufio"
	"strings"
	"testing"
)

// The interactive restore prompts for eight settings and then restores a machine
// from a backup. None of it was reachable from a test, because
// runInteractiveRestore builds its own bufio.Reader over os.Stdin — so the
// prompt ORDER, the region default and the two yes/no defaults were only ever
// verified by someone running it, which by definition happens on a bad day.
//
// promptS3Settings takes the reader, so a strings.Reader drives it.

func TestPromptS3Settings_ReadsInOrder(t *testing.T) {
	in := strings.Join([]string{
		"ST-01",            // Station ID
		"https://s3.local", // S3 Endpoint URL
		"shingo-backups",   // Bucket
		"eu-west-2",        // Region
		"AKIA-EXAMPLE",     // Access Key
		"secret-example",   // Secret Key
		"n",                // Use path-style S3
		"y",                // Skip TLS verification
	}, "\n") + "\n"

	stationID, cfg, err := promptS3Settings(bufio.NewReader(strings.NewReader(in)))
	if err != nil {
		t.Fatalf("promptS3Settings: %v", err)
	}
	if stationID != "ST-01" {
		t.Errorf("stationID = %q, want ST-01", stationID)
	}
	// The order matters: every field below is positional against the input
	// above, so a reordered prompt sequence silently swaps a bucket for a
	// region, or a secret key for an access key.
	if cfg.Endpoint != "https://s3.local" {
		t.Errorf("Endpoint = %q", cfg.Endpoint)
	}
	if cfg.Bucket != "shingo-backups" {
		t.Errorf("Bucket = %q", cfg.Bucket)
	}
	if cfg.Region != "eu-west-2" {
		t.Errorf("Region = %q", cfg.Region)
	}
	if cfg.AccessKey != "AKIA-EXAMPLE" {
		t.Errorf("AccessKey = %q", cfg.AccessKey)
	}
	if cfg.SecretKey != "secret-example" {
		t.Errorf("SecretKey = %q", cfg.SecretKey)
	}
	if cfg.UsePathStyle {
		t.Error("UsePathStyle = true, want false (answered n)")
	}
	if !cfg.InsecureSkipTLSVerify {
		t.Error("InsecureSkipTLSVerify = false, want true (answered y)")
	}
}

// The three defaults are the part a hurried operator relies on: blank answers
// must give us-east-1, path-style ON and TLS verification ON. Getting the last
// one backwards would silently disable certificate checking during a restore.
func TestPromptS3Settings_BlankAnswersTakeTheDefaults(t *testing.T) {
	in := strings.Join([]string{
		"ST-02", "https://s3.local", "shingo-backups",
		"", // Region -> us-east-1
		"AKIA", "secret",
		"", // Use path-style -> true
		"", // Skip TLS verification -> false
	}, "\n") + "\n"

	_, cfg, err := promptS3Settings(bufio.NewReader(strings.NewReader(in)))
	if err != nil {
		t.Fatalf("promptS3Settings: %v", err)
	}
	if cfg.Region != "us-east-1" {
		t.Errorf("Region = %q, want the us-east-1 default", cfg.Region)
	}
	if !cfg.UsePathStyle {
		t.Error("UsePathStyle = false, want the true default")
	}
	if cfg.InsecureSkipTLSVerify {
		t.Error("InsecureSkipTLSVerify defaulted to TRUE — a blank answer must not disable TLS verification")
	}
}

// A required field left blank is an error, not an empty string quietly written
// into the restore config.
func TestPromptS3Settings_BlankRequiredFieldIsAnError(t *testing.T) {
	in := "\n\n\n\n\n\n\n\n"
	if _, _, err := promptS3Settings(bufio.NewReader(strings.NewReader(in))); err == nil {
		t.Fatal("a blank Station ID was accepted; required fields must refuse empty input")
	}
}
