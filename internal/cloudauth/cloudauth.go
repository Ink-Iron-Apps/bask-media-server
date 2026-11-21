// Package cloudauth verifies the assertions the Bask account service signs.
//
// It is the whole of what a server trusts the account service for: a short
// statement, signed with Ed25519, saying that a named account is who a client
// says it is. Everything a server then does with that — which local account it
// becomes, which libraries it reaches — is decided here on this machine by the
// administrator, and the service is not consulted.
//
// See docs/CLOUD-ACCOUNTS.md for the protocol and for what the service
// deliberately does not know.
//
// No dependency: a compact JWS over Ed25519 is forty lines against
// crypto/ed25519, and a JWT library is a large surface for the one shape of
// token this ever reads.
package cloudauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrNotVerified is every refusal, so a caller cannot accidentally treat one
// kind of bad assertion as better than another. Which one it was is in the
// wrapped error, for the log.
var ErrNotVerified = errors.New("assertion not verified")

// Skew is how far apart this machine's clock and the service's may be before
// a freshly signed assertion is refused. Small, because both ends are
// expected to keep time, and an assertion only lives five minutes.
const Skew = 90 * time.Second

// Claims is what an assertion says.
type Claims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
	ID       string `json:"jti"`
}

// Key is one published public key.
type Key struct {
	ID        string `json:"kid"`
	Algorithm string `json:"alg"`
	Key       string `json:"key"`
}

// Verifier checks assertions against a set of published keys.
//
// Safe for concurrent use: a request verifies while another refreshes.
type Verifier struct {
	// Issuer is the service this server was claimed with. An assertion from
	// anywhere else is refused however well it is signed, because a signature
	// only says who made it.
	Issuer string

	// Audience is this server's own id. The check that makes an assertion
	// meant for somebody else's server worthless here.
	Audience string

	// Now is the clock, for tests.
	Now func() time.Time

	mu   sync.RWMutex
	keys map[string]ed25519.PublicKey
	seen map[string]int64 // jti to the second it expires
}

// SetKeys replaces the keys this verifier trusts.
//
// Called with what the service published, which a server fetches when it
// claims and again only when it meets a key id it does not know. An
// unreachable service must not stop a server verifying the assertions it
// already understands.
func (v *Verifier) SetKeys(keys []Key) error {
	parsed := make(map[string]ed25519.PublicKey, len(keys))
	for _, key := range keys {
		// The algorithm is checked here rather than read from an assertion's
		// header: this verifier does EdDSA and nothing else, which is what
		// keeps `alg: none` and the rest of that family off the table.
		if key.Algorithm != "EdDSA" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(key.Key)
		if err != nil {
			return fmt.Errorf("key %q: %w", key.ID, err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return fmt.Errorf("key %q: %d bytes, want %d", key.ID, len(raw), ed25519.PublicKeySize)
		}
		parsed[key.ID] = ed25519.PublicKey(raw)
	}
	if len(parsed) == 0 {
		return errors.New("no usable keys")
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.keys = parsed
	return nil
}

// Knows reports whether a key id is one this verifier holds, which is how a
// caller decides whether a refresh is worth making.
func (v *Verifier) Knows(kid string) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	_, found := v.keys[kid]
	return found
}

// KeyID reads the key an assertion was signed with, without verifying
// anything. For deciding whether to refresh, and for nothing else.
func KeyID(assertion string) string {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ""
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
		KeyID     string `json:"kid"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return ""
	}
	return header.KeyID
}

// Verify checks an assertion and returns what it says.
//
// Every check, in this order, and all of them: the signature, the issuer, the
// audience, the clock, and whether this exact assertion has been seen before.
// A caller gets claims only when all five passed.
func (v *Verifier) Verify(assertion string) (*Claims, error) {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: not three parts", ErrNotVerified)
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header is not base64url", ErrNotVerified)
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
		KeyID     string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return nil, fmt.Errorf("%w: header is not JSON", ErrNotVerified)
	}
	// Both checked, and neither believed: the algorithm has to be the one
	// this package implements, and saying so is not the same as letting the
	// token choose.
	if header.Algorithm != "EdDSA" {
		return nil, fmt.Errorf("%w: algorithm %q", ErrNotVerified, header.Algorithm)
	}
	if header.Type != "bask+jws" {
		return nil, fmt.Errorf("%w: type %q", ErrNotVerified, header.Type)
	}

	v.mu.RLock()
	key, known := v.keys[header.KeyID]
	v.mu.RUnlock()
	if !known {
		return nil, fmt.Errorf("%w: no key %q", ErrNotVerified, header.KeyID)
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature is not base64url", ErrNotVerified)
	}
	if !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signature) {
		return nil, fmt.Errorf("%w: signature", ErrNotVerified)
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload is not base64url", ErrNotVerified)
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("%w: payload is not JSON", ErrNotVerified)
	}

	if v.Issuer == "" || claims.Issuer != v.Issuer {
		return nil, fmt.Errorf("%w: issued by %q", ErrNotVerified, claims.Issuer)
	}
	if v.Audience == "" || claims.Audience != v.Audience {
		return nil, fmt.Errorf("%w: addressed to %q", ErrNotVerified, claims.Audience)
	}
	if claims.Subject == "" {
		return nil, fmt.Errorf("%w: no subject", ErrNotVerified)
	}

	now := v.now()
	if claims.Expires <= 0 || now.Add(-Skew).Unix() >= claims.Expires {
		return nil, fmt.Errorf("%w: expired", ErrNotVerified)
	}
	if claims.IssuedAt > 0 && now.Add(Skew).Unix() < claims.IssuedAt {
		return nil, fmt.Errorf("%w: issued in the future", ErrNotVerified)
	}
	// Longer than the service issues is a signed assertion from something
	// that is not the service we agreed to talk to, or one whose lifetime
	// somebody widened without saying. Either way it is not one of ours.
	if claims.IssuedAt > 0 && claims.Expires-claims.IssuedAt > int64((30*time.Minute).Seconds()) {
		return nil, fmt.Errorf("%w: lives too long", ErrNotVerified)
	}

	if !v.remember(claims.ID, claims.Expires, now) {
		return nil, fmt.Errorf("%w: already used", ErrNotVerified)
	}
	return &claims, nil
}

// remember records an assertion's id and reports whether it is the first time.
//
// Replay inside the five minutes an assertion lives is otherwise free. The
// window is small enough that holding every id in it costs nothing, and the
// expired ones are dropped on the way past rather than by a timer: the map is
// already in hand whenever this runs.
func (v *Verifier) remember(id string, expires int64, now time.Time) bool {
	if id == "" {
		// An assertion with no id cannot be replay-checked, so it is not one
		// this package accepts. The service always sets one.
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.seen == nil {
		v.seen = make(map[string]int64)
	}
	stamp := now.Unix()
	for seen, until := range v.seen {
		if until <= stamp {
			delete(v.seen, seen)
		}
	}
	if _, already := v.seen[id]; already {
		return false
	}
	v.seen[id] = expires
	return true
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}
