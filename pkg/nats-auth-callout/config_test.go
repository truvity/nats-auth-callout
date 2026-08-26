package natsauthcallout

import (
	"strings"
	"testing"
)

// setValidEnv populates every required variable; tests then knock out or
// override what they exercise.
func setValidEnv(t *testing.T) {
	t.Helper()

	t.Setenv(envURL, "nats://nats.nats.svc:4222")
	t.Setenv(envUser, "auth")
	t.Setenv(envPassword, "secret")
	t.Setenv(envIssuerSeed, "SA-fake-seed")
	t.Setenv(envProjectAccounts, "url-shortener, billing")
	t.Setenv(envTokenAudience, "nats")
}

func TestNewConfigFromEnv(t *testing.T) {
	setValidEnv(t)

	cfg, err := NewConfigFromEnv()
	if err != nil {
		t.Fatalf("NewConfigFromEnv failed: %v", err)
	}

	if cfg.URL != "nats://nats.nats.svc:4222" {
		t.Errorf("URL = %q", cfg.URL)
	}

	if len(cfg.Audiences) != 1 || cfg.Audiences[0] != "nats" {
		t.Errorf("Audiences = %v, want [nats]", cfg.Audiences)
	}

	if _, ok := cfg.ProjectAccounts["billing"]; !ok {
		t.Errorf("ProjectAccounts = %v, want billing present", cfg.ProjectAccounts)
	}
}

func TestNewConfigFromEnvRequiresAudience(t *testing.T) {
	for name, value := range map[string]string{
		"unset": "",
		"blank": " , ",
	} {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(envTokenAudience, value)

			if _, err := NewConfigFromEnv(); err == nil || !strings.Contains(err.Error(), envTokenAudience) {
				t.Fatalf("want %s-is-required error, got %v", envTokenAudience, err)
			}
		})
	}
}

func TestNewConfigFromEnvCredentialsArePairValidated(t *testing.T) {
	t.Run("user without password rejected", func(t *testing.T) {
		setValidEnv(t)
		t.Setenv(envPassword, "")

		if _, err := NewConfigFromEnv(); err == nil {
			t.Fatal("want error when only NATS_AUTH_USER is set")
		}
	})

	t.Run("password without user rejected", func(t *testing.T) {
		setValidEnv(t)
		t.Setenv(envUser, "")

		if _, err := NewConfigFromEnv(); err == nil {
			t.Fatal("want error when only NATS_AUTH_PASSWORD is set")
		}
	})

	t.Run("neither set is valid (nkey login)", func(t *testing.T) {
		setValidEnv(t)
		t.Setenv(envUser, "")
		t.Setenv(envPassword, "")

		if _, err := NewConfigFromEnv(); err != nil {
			t.Fatalf("want nkey-login config to pass, got %v", err)
		}
	})
}
