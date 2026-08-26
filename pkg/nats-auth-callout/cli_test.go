package natsauthcallout

import (
	"testing"

	"github.com/nats-io/nkeys"
)

func TestUserNkeyFromIssuerSeed(t *testing.T) {
	issuer, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatalf("create account nkey: %v", err)
	}

	seed, err := issuer.Seed()
	if err != nil {
		t.Fatalf("extract seed: %v", err)
	}

	userKey, err := userNkeyFromIssuerSeed(string(seed))
	if err != nil {
		t.Fatalf("userNkeyFromIssuerSeed: %v", err)
	}

	pub, err := userKey.PublicKey()
	if err != nil {
		t.Fatalf("derive public key: %v", err)
	}

	if !nkeys.IsValidPublicUserKey(pub) {
		t.Errorf("derived key %q is not a valid public USER key", pub)
	}

	// Same key material as the issuer: a signature from the user key must
	// verify under the issuer keypair (this is what lets one seed serve
	// both signing and broker login).
	nonce := []byte("server-nonce")

	sig, err := userKey.Sign(nonce)
	if err != nil {
		t.Fatalf("sign nonce: %v", err)
	}

	if err := issuer.Verify(nonce, sig); err != nil {
		t.Errorf("user-key signature does not verify against the issuer key material: %v", err)
	}
}

func TestUserNkeyFromIssuerSeedRejectsGarbage(t *testing.T) {
	if _, err := userNkeyFromIssuerSeed("not-a-seed"); err == nil {
		t.Error("expected error for invalid seed, got nil")
	}
}

func TestLoginOptionPrefersUserPasswordOverride(t *testing.T) {
	opt, err := loginOption(&Config{User: "auth", Password: "pw"})
	if err != nil {
		t.Fatalf("loginOption with override: %v", err)
	}

	if opt == nil {
		t.Fatal("expected a nats.Option, got nil")
	}
}

func TestRedactedURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "plain url unchanged",
			raw:  "nats://nats.nats.svc:4222",
			want: "nats://nats.nats.svc:4222",
		},
		{
			name: "userinfo stripped",
			raw:  "nats://auth:s3cr3t@nats.nats.svc:4222",
			want: "nats://nats.nats.svc:4222",
		},
		{
			name: "comma-separated list redacted per element",
			raw:  "nats://u:p@a:4222, tls://u:p@b:4222",
			want: "nats://a:4222,tls://b:4222",
		},
		{
			name: "schemeless input is invalid",
			raw:  "127.0.0.1:4222",
			want: "invalid",
		},
		{
			name: "unparseable input is invalid",
			raw:  "nats://[::1",
			want: "invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactedURL(tt.raw); got != tt.want {
				t.Errorf("redactedURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
