package cloudauth_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/cloudauth"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const (
	issuer   = "https://account.example"
	audience = "this-server"
)

type signer struct {
	kid     string
	public  ed25519.PublicKey
	private ed25519.PrivateKey
}

func newSigner(t *testing.T, kid string) signer {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return signer{kid: kid, public: public, private: private}
}

func (s signer) key() cloudauth.Key {
	return cloudauth.Key{
		ID:        s.kid,
		Algorithm: "EdDSA",
		Key:       base64.RawURLEncoding.EncodeToString(s.public),
	}
}

// sign builds an assertion the way the service does, so a test that changes
// one field changes only that field.
func (s signer) sign(t *testing.T, claims map[string]any, header map[string]any) string {
	t.Helper()
	if header == nil {
		header = map[string]any{"alg": "EdDSA", "typ": "bask+jws", "kid": s.kid}
	}
	encode := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	body := encode(header) + "." + encode(claims)
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.private, []byte(body)))
}

func claimsAt(now time.Time, overrides map[string]any) map[string]any {
	claims := map[string]any{
		"iss":   issuer,
		"sub":   "account-1",
		"aud":   audience,
		"email": "rae@example.com",
		"name":  "Rae",
		"iat":   now.Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
		"jti":   "one",
	}
	for key, value := range overrides {
		claims[key] = value
	}
	return claims
}

func verifier(t *testing.T, now time.Time, keys ...cloudauth.Key) *cloudauth.Verifier {
	t.Helper()
	v := &cloudauth.Verifier{
		Issuer:   issuer,
		Audience: audience,
		Now:      func() time.Time { return now },
	}
	if err := v.SetKeys(keys); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAnAssertionFromTheServiceIsAccepted(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newSigner(t, "k1")
	v := verifier(t, now, s.key())

	claims, err := v.Verify(s.sign(t, claimsAt(now, nil), nil))
	if err != nil {
		t.Fatalf("a good assertion was refused: %v", err)
	}
	if claims.Subject != "account-1" || claims.Email != "rae@example.com" {
		t.Fatalf("claims came back wrong: %+v", claims)
	}
}

// The check that makes a stolen assertion worthless anywhere but where it was
// meant to go. Without it, being a member of anybody's server would be a
// credential for everybody's.
func TestAnAssertionForAnotherServerIsRefused(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newSigner(t, "k1")
	v := verifier(t, now, s.key())

	_, err := v.Verify(s.sign(t, claimsAt(now, map[string]any{"aud": "somebody-elses-server"}), nil))
	if !errors.Is(err, cloudauth.ErrNotVerified) {
		t.Fatalf("an assertion for another server was accepted: %v", err)
	}
}

// A signature only says who made something. It does not say they were the
// service this server agreed to trust.
func TestAnAssertionFromAnotherIssuerIsRefused(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newSigner(t, "k1")
	v := verifier(t, now, s.key())

	_, err := v.Verify(s.sign(t, claimsAt(now, map[string]any{"iss": "https://somewhere.else"}), nil))
	if !errors.Is(err, cloudauth.ErrNotVerified) {
		t.Fatalf("an assertion from another issuer was accepted: %v", err)
	}
}

func TestAnAssertionSignedByAStrangerIsRefused(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	real := newSigner(t, "k1")
	impostor := newSigner(t, "k1") // Same key id, different key.
	v := verifier(t, now, real.key())

	_, err := v.Verify(impostor.sign(t, claimsAt(now, nil), nil))
	if !errors.Is(err, cloudauth.ErrNotVerified) {
		t.Fatalf("an assertion signed by a stranger was accepted: %v", err)
	}
}

func TestATamperedPayloadIsRefused(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newSigner(t, "k1")
	v := verifier(t, now, s.key())

	assertion := s.sign(t, claimsAt(now, nil), nil)
	// Re-encode the payload with a different subject and keep the signature,
	// which is what somebody with an assertion and an idea would try.
	tampered := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + issuer +
		`","sub":"somebody-else","aud":"` + audience + `","exp":9999999999,"jti":"two"}`))
	head, rest, _ := strings.Cut(assertion, ".")
	_, signature, _ := strings.Cut(rest, ".")

	_, err := v.Verify(head + "." + tampered + "." + signature)
	if !errors.Is(err, cloudauth.ErrNotVerified) {
		t.Fatalf("a tampered payload was accepted: %v", err)
	}
}

// The family of attacks that starts with the token naming its own algorithm.
func TestAnUnsignedAssertionIsRefused(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newSigner(t, "k1")
	v := verifier(t, now, s.key())

	for _, header := range []map[string]any{
		{"alg": "none", "typ": "bask+jws", "kid": "k1"},
		{"alg": "HS256", "typ": "bask+jws", "kid": "k1"},
		{"alg": "EdDSA", "typ": "JWT", "kid": "k1"},
	} {
		if _, err := v.Verify(s.sign(t, claimsAt(now, nil), header)); !errors.Is(err, cloudauth.ErrNotVerified) {
			t.Fatalf("header %v was accepted: %v", header, err)
		}
	}
}

func TestAnExpiredAssertionIsRefused(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newSigner(t, "k1")
	issued := now.Add(-10 * time.Minute)
	v := verifier(t, now, s.key())

	_, err := v.Verify(s.sign(t, claimsAt(issued, nil), nil))
	if !errors.Is(err, cloudauth.ErrNotVerified) {
		t.Fatalf("an expired assertion was accepted: %v", err)
	}
}

// A small disagreement between two clocks must not sign somebody out, which
// is the whole reason the skew exists.
func TestASmallClockDifferenceIsTolerated(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newSigner(t, "k1")
	v := verifier(t, now, s.key())

	// The service's clock is thirty seconds ahead of this machine's.
	if _, err := v.Verify(s.sign(t, claimsAt(now.Add(30*time.Second), nil), nil)); err != nil {
		t.Fatalf("a thirty second difference refused a good assertion: %v", err)
	}
}

// Five minutes of free replay is five minutes too many when the thing being
// replayed is a credential.
func TestTheSameAssertionCannotBeUsedTwice(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newSigner(t, "k1")
	v := verifier(t, now, s.key())

	assertion := s.sign(t, claimsAt(now, nil), nil)
	if _, err := v.Verify(assertion); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if _, err := v.Verify(assertion); !errors.Is(err, cloudauth.ErrNotVerified) {
		t.Fatalf("the same assertion was accepted twice: %v", err)
	}
}

// And the memory of them does not grow without bound: an id is forgotten once
// the assertion carrying it could not be used anyway.
func TestSeenAssertionsAreForgottenOnceTheyExpire(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	clock := start
	s := newSigner(t, "k1")
	v := &cloudauth.Verifier{Issuer: issuer, Audience: audience, Now: func() time.Time { return clock }}
	if err := v.SetKeys([]cloudauth.Key{s.key()}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 50; i++ {
		clock = start.Add(time.Duration(i) * time.Second)
		assertion := s.sign(t, claimsAt(clock, map[string]any{"jti": string(rune('a'+i%26)) + string(rune('0'+i/26))}), nil)
		if _, err := v.Verify(assertion); err != nil {
			t.Fatalf("assertion %d: %v", i, err)
		}
	}

	// An hour later the same ids are free again, because nothing carrying
	// them could still be valid.
	clock = start.Add(time.Hour)
	reused := s.sign(t, claimsAt(clock, map[string]any{"jti": "a0"}), nil)
	if _, err := v.Verify(reused); err != nil {
		t.Fatalf("an id from an hour ago was still remembered: %v", err)
	}
}

func TestAnAssertionWithNoIDIsRefused(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newSigner(t, "k1")
	v := verifier(t, now, s.key())

	_, err := v.Verify(s.sign(t, claimsAt(now, map[string]any{"jti": ""}), nil))
	if !errors.Is(err, cloudauth.ErrNotVerified) {
		t.Fatalf("an assertion that cannot be replay-checked was accepted: %v", err)
	}
}

// One signed by the right key but good for a week is not one of ours.
func TestAnAssertionThatLivesTooLongIsRefused(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newSigner(t, "k1")
	v := verifier(t, now, s.key())

	_, err := v.Verify(s.sign(t, claimsAt(now, map[string]any{
		"exp": now.Add(7 * 24 * time.Hour).Unix(),
	}), nil))
	if !errors.Is(err, cloudauth.ErrNotVerified) {
		t.Fatalf("a week-long assertion was accepted: %v", err)
	}
}

// Rotation publishes both keys for longer than an assertion can live, so
// there is never a moment when a valid one cannot be checked.
func TestBothKeysWorkWhileOneIsBeingRetired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	old := newSigner(t, "k1")
	fresh := newSigner(t, "k2")
	v := verifier(t, now, fresh.key(), old.key())

	if _, err := v.Verify(fresh.sign(t, claimsAt(now, map[string]any{"jti": "new"}), nil)); err != nil {
		t.Fatalf("the new key was refused: %v", err)
	}
	if _, err := v.Verify(old.sign(t, claimsAt(now, map[string]any{"jti": "old"}), nil)); err != nil {
		t.Fatalf("the retiring key was refused: %v", err)
	}
}

func TestAKeyIDIsReadableWithoutTrustingAnything(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newSigner(t, "k9")
	if got := cloudauth.KeyID(s.sign(t, claimsAt(now, nil), nil)); got != "k9" {
		t.Fatalf("key id = %q", got)
	}
	if got := cloudauth.KeyID("nonsense"); got != "" {
		t.Fatalf("key id of nonsense = %q", got)
	}
}

func TestAVerifierWithNoKeysRefusesEverything(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newSigner(t, "k1")
	v := &cloudauth.Verifier{Issuer: issuer, Audience: audience, Now: func() time.Time { return now }}

	if _, err := v.Verify(s.sign(t, claimsAt(now, nil), nil)); !errors.Is(err, cloudauth.ErrNotVerified) {
		t.Fatalf("a verifier holding no keys accepted something: %v", err)
	}
	if err := v.SetKeys(nil); err == nil {
		t.Fatal("an empty key set was accepted")
	}
}

// The two implementations are in different languages, and the only thing
// stopping them drifting apart is a test that holds one's output against the
// other's input. This fixture was signed by the real Worker in cloud/;
// regenerate it with that running locally if the format ever changes.
//
// Nothing in it is a live credential: the key is a development one and the
// assertion expired five minutes after it was made.
func TestTheWorkersOwnAssertionVerifiesHere(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "assertion.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Assertion string          `json:"assertion"`
		Keys      []cloudauth.Key `json:"keys"`
		Audience  string          `json:"audience"`
		Issuer    string          `json:"issuer"`
		Subject   string          `json:"subject"`
		Email     string          `json:"email"`
		IssuedAt  int64           `json:"issued_at"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}

	// The clock is set to when it was signed, so this tests the format rather
	// than how long ago the fixture was captured.
	v := &cloudauth.Verifier{
		Issuer:   fixture.Issuer,
		Audience: fixture.Audience,
		Now:      func() time.Time { return time.Unix(fixture.IssuedAt, 0) },
	}
	if err := v.SetKeys(fixture.Keys); err != nil {
		t.Fatal(err)
	}

	claims, err := v.Verify(fixture.Assertion)
	if err != nil {
		t.Fatalf("the service's own assertion was refused here: %v", err)
	}
	if claims.Subject != fixture.Subject || claims.Email != fixture.Email {
		t.Fatalf("claims disagree with the fixture: %+v", claims)
	}
	if _, err := v.Verify(fixture.Assertion); !errors.Is(err, cloudauth.ErrNotVerified) {
		t.Fatalf("the service's own assertion replayed: %v", err)
	}
}
