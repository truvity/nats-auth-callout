package natsauthcallout

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
	authv1 "k8s.io/api/authentication/v1"
)

type (
	// fakeReviewer is an in-memory TokenReviewer capturing what it was
	// asked to validate.
	fakeReviewer struct {
		status       *authv1.TokenReviewStatus
		err          error
		gotToken     string
		gotAudiences []string
		gotDeadline  time.Time
		hadDeadline  bool
	}

	// testEnv bundles the keypairs and handler wired for one test.
	testEnv struct {
		handler   *Handler
		serverKP  nkeys.KeyPair
		serverPub string
		userPub   string
		reviewer  *fakeReviewer
		now       time.Time
	}
)

func (f *fakeReviewer) Review(ctx context.Context, token string, audiences []string) (*authv1.TokenReviewStatus, error) {
	f.gotToken = token
	f.gotAudiences = audiences
	f.gotDeadline, f.hadDeadline = ctx.Deadline()

	return f.status, f.err
}

// authenticatedAs builds a TokenReview status for the given username.
func authenticatedAs(username string) *authv1.TokenReviewStatus {
	return &authv1.TokenReviewStatus{
		Authenticated: true,
		User:          authv1.UserInfo{Username: username},
	}
}

// newTestEnv builds a handler with generated nkeys, a fixed clock, and the
// given fake reviewer.
func newTestEnv(t *testing.T, reviewer *fakeReviewer) *testEnv {
	t.Helper()

	issuerKP, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatalf("create issuer account nkey: %v", err)
	}

	serverKP, err := nkeys.CreateServer()
	if err != nil {
		t.Fatalf("create server nkey: %v", err)
	}

	serverPub, err := serverKP.PublicKey()
	if err != nil {
		t.Fatalf("server public key: %v", err)
	}

	userKP, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create user nkey: %v", err)
	}

	userPub, err := userKP.PublicKey()
	if err != nil {
		t.Fatalf("user public key: %v", err)
	}

	cfg := &Config{
		ProjectAccounts: map[string]struct{}{"url-shortener": {}, "billing": {}},
		Audiences:       []string{"nats"},
	}

	handler := NewHandler(cfg, reviewer, issuerKP, slog.New(slog.DiscardHandler))
	now := time.Now().Truncate(time.Second)
	handler.now = func() time.Time { return now }

	return &testEnv{
		handler:   handler,
		serverKP:  serverKP,
		serverPub: serverPub,
		userPub:   userPub,
		reviewer:  reviewer,
		now:       now,
	}
}

// authRequest builds a signed authorization request as the NATS server would.
func (e *testEnv) authRequest(t *testing.T, mutate func(*jwt.AuthorizationRequestClaims)) []byte {
	t.Helper()

	req := jwt.NewAuthorizationRequestClaims(e.userPub)
	req.Server.ID = e.serverPub
	req.UserNkey = e.userPub

	if mutate != nil {
		mutate(req)
	}

	encoded, err := req.Encode(e.serverKP)
	if err != nil {
		t.Fatalf("encode authorization request: %v", err)
	}

	return []byte(encoded)
}

// respond runs Handle and decodes the response claims.
func (e *testEnv) respond(t *testing.T, payload []byte) *jwt.AuthorizationResponseClaims {
	t.Helper()

	raw, err := e.handler.Handle(context.Background(), payload)
	if err != nil {
		t.Fatalf("Handle failed: %v", err)
	}

	rc, err := jwt.DecodeAuthorizationResponseClaims(string(raw))
	if err != nil {
		t.Fatalf("decode authorization response: %v", err)
	}

	return rc
}

// saToken fabricates a JWT-shaped ServiceAccount token whose exp claim is
// the given time. Only the shape matters — validation is the (fake)
// TokenReview's job.
func saToken(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, exp.Unix()))

	return header + "." + payload + ".sig"
}

func TestHandleAllowsMappedProject(t *testing.T) {
	reviewer := &fakeReviewer{status: authenticatedAs("system:serviceaccount:url-shortener:worker")}
	env := newTestEnv(t, reviewer)

	token := saToken(env.now.Add(30 * time.Minute))
	payload := env.authRequest(t, func(req *jwt.AuthorizationRequestClaims) {
		req.ConnectOptions.Token = token
	})

	rc := env.respond(t, payload)

	if rc.Error != "" {
		t.Fatalf("expected allow, got error %q", rc.Error)
	}

	if rc.Subject != env.userPub || rc.Audience != env.serverPub {
		t.Errorf("response subject/audience = (%q, %q), want (%q, %q)",
			rc.Subject, rc.Audience, env.userPub, env.serverPub)
	}

	if reviewer.gotToken != token {
		t.Error("reviewer did not receive the client token")
	}

	if len(reviewer.gotAudiences) != 1 || reviewer.gotAudiences[0] != "nats" {
		t.Errorf("reviewer audiences = %v, want [nats]", reviewer.gotAudiences)
	}

	uc, err := jwt.DecodeUserClaims(rc.Jwt)
	if err != nil {
		t.Fatalf("decode embedded user jwt: %v", err)
	}

	if uc.Subject != env.userPub {
		t.Errorf("user jwt subject = %q, want %q", uc.Subject, env.userPub)
	}

	if uc.Audience != "url-shortener" {
		t.Errorf("user jwt audience = %q, want url-shortener", uc.Audience)
	}

	if uc.Name != "url-shortener/worker" {
		t.Errorf("user jwt name = %q, want url-shortener/worker", uc.Name)
	}

	if want := env.now.Add(30 * time.Minute).Unix(); uc.Expires != want {
		t.Errorf("user jwt expires = %d, want token expiry %d", uc.Expires, want)
	}

	if !uc.Pub.Allow.Contains(">") || !uc.Sub.Allow.Contains(">") {
		t.Errorf("user jwt permissions = %+v, want allow-all pub/sub", uc.Permissions)
	}
}

func TestHandleBoundsTokenReview(t *testing.T) {
	reviewer := &fakeReviewer{status: authenticatedAs("system:serviceaccount:url-shortener:worker")}
	env := newTestEnv(t, reviewer)

	before := time.Now()
	env.respond(t, env.authRequest(t, func(req *jwt.AuthorizationRequestClaims) {
		req.ConnectOptions.Token = saToken(env.now.Add(30 * time.Minute))
	}))

	if !reviewer.hadDeadline {
		t.Fatal("TokenReview context must carry a per-request deadline")
	}

	if latest := before.Add(tokenReviewTimeout + time.Second); reviewer.gotDeadline.After(latest) {
		t.Errorf("TokenReview deadline %v exceeds tokenReviewTimeout budget (latest %v)",
			reviewer.gotDeadline, latest)
	}
}

func TestHandleFallsBackToPassword(t *testing.T) {
	reviewer := &fakeReviewer{status: authenticatedAs("system:serviceaccount:billing:ledger")}
	env := newTestEnv(t, reviewer)

	token := saToken(env.now.Add(10 * time.Minute))
	payload := env.authRequest(t, func(req *jwt.AuthorizationRequestClaims) {
		req.ConnectOptions.Password = token
	})

	rc := env.respond(t, payload)

	if rc.Error != "" {
		t.Fatalf("expected allow, got error %q", rc.Error)
	}

	if reviewer.gotToken != token {
		t.Error("reviewer did not receive the password-carried token")
	}
}

func TestHandleCapsExpiryAtOneHour(t *testing.T) {
	reviewer := &fakeReviewer{status: authenticatedAs("system:serviceaccount:emp-otsar:shell")}
	env := newTestEnv(t, reviewer)

	payload := env.authRequest(t, func(req *jwt.AuthorizationRequestClaims) {
		req.ConnectOptions.Token = saToken(env.now.Add(6 * time.Hour))
	})

	rc := env.respond(t, payload)

	if rc.Error != "" {
		t.Fatalf("expected allow, got error %q", rc.Error)
	}

	uc, err := jwt.DecodeUserClaims(rc.Jwt)
	if err != nil {
		t.Fatalf("decode embedded user jwt: %v", err)
	}

	if uc.Audience != "emp-otsar" {
		t.Errorf("user jwt audience = %q, want emp-otsar", uc.Audience)
	}

	if want := env.now.Add(time.Hour).Unix(); uc.Expires != want {
		t.Errorf("user jwt expires = %d, want capped %d", uc.Expires, want)
	}
}

func TestHandleDenies(t *testing.T) {
	tests := []struct {
		name       string
		reviewer   *fakeReviewer
		mutate     func(*jwt.AuthorizationRequestClaims)
		wantReason string
	}{
		{
			name:       "no token presented",
			reviewer:   &fakeReviewer{},
			mutate:     nil,
			wantReason: "no auth token",
		},
		{
			name:     "token review call fails",
			reviewer: &fakeReviewer{err: fmt.Errorf("apiserver down")},
			mutate: func(req *jwt.AuthorizationRequestClaims) {
				req.ConnectOptions.Token = "some-token"
			},
			wantReason: "token review unavailable",
		},
		{
			name: "token not authenticated",
			reviewer: &fakeReviewer{status: &authv1.TokenReviewStatus{
				Authenticated: false,
				Error:         "token expired",
			}},
			mutate: func(req *jwt.AuthorizationRequestClaims) {
				req.ConnectOptions.Token = "expired-token"
			},
			wantReason: "not authenticated",
		},
		{
			name:     "non-serviceaccount identity",
			reviewer: &fakeReviewer{status: authenticatedAs("kubernetes-admin")},
			mutate: func(req *jwt.AuthorizationRequestClaims) {
				req.ConnectOptions.Token = "human-token"
			},
			wantReason: "not a serviceaccount identity",
		},
		{
			name:     "unmapped namespace",
			reviewer: &fakeReviewer{status: authenticatedAs("system:serviceaccount:kube-system:default")},
			mutate: func(req *jwt.AuthorizationRequestClaims) {
				req.ConnectOptions.Token = "sa-token"
			},
			wantReason: "no NATS account mapping",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, tt.reviewer)
			rc := env.respond(t, env.authRequest(t, tt.mutate))

			if rc.Jwt != "" {
				t.Error("deny response must not carry a user jwt")
			}

			if !strings.Contains(rc.Error, tt.wantReason) {
				t.Errorf("deny reason = %q, want it to contain %q", rc.Error, tt.wantReason)
			}

			if rc.Subject != env.userPub || rc.Audience != env.serverPub {
				t.Errorf("deny response subject/audience = (%q, %q), want (%q, %q)",
					rc.Subject, rc.Audience, env.userPub, env.serverPub)
			}
		})
	}
}

func TestHandleRejectsUndecodableRequest(t *testing.T) {
	env := newTestEnv(t, &fakeReviewer{})

	if _, err := env.handler.Handle(context.Background(), []byte("not-a-jwt")); err == nil {
		t.Fatal("Handle should fail on an undecodable request payload")
	}
}
