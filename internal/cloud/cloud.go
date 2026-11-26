// Package cloud is this server's half of the Bask account service: claiming,
// the keys it trusts afterwards, and verifying what a client presents.
//
// A server with no link works exactly as it always has. Everything here is
// additional, and a household that never claims a server never reaches any of
// it. See docs/CLOUD-ACCOUNTS.md for the protocol.
//
// The link lives in the server table rather than the library, because it is
// the one secret this process holds that is not a library fact -- and because
// a library export must not carry the credential that speaks for the server.
package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/inknironapps/bask-media-server/internal/cloudauth"
)

// LinkKey is where the link is stored in the server table.
const LinkKey = "cloud.link"

// ErrNotLinked is every refusal that comes of this server not being claimed,
// so a caller cannot mistake "no account service" for "that assertion is bad".
var ErrNotLinked = errors.New("this server is not linked to an account service")

// Link is what a claimed server holds.
//
// Secret is the server's own credential with the service and is never served
// to anybody: [Link.Public] is what the admin interface is allowed to see.
type Link struct {
	Issuer    string          `json:"issuer"`
	ServerID  string          `json:"serverId"`
	Secret    string          `json:"secret"`
	Name      string          `json:"name"`
	Keys      []cloudauth.Key `json:"keys"`
	ClaimedAt time.Time       `json:"claimedAt"`
}

// PublicLink is the link as the admin interface may see it: everything except
// the credential.
type PublicLink struct {
	Issuer    string    `json:"issuer"`
	ServerID  string    `json:"serverId"`
	Name      string    `json:"name"`
	KeyCount  int       `json:"keyCount"`
	ClaimedAt time.Time `json:"claimedAt"`
}

// Public strips the secret.
func (l *Link) Public() PublicLink {
	return PublicLink{
		Issuer:    l.Issuer,
		ServerID:  l.ServerID,
		Name:      l.Name,
		KeyCount:  len(l.Keys),
		ClaimedAt: l.ClaimedAt,
	}
}

// Values is the part of the store this needs: the server table, read and
// written whole by key.
type Values interface {
	ServerValue(ctx context.Context, key string) (string, error)
	SetServerValue(ctx context.Context, key, value string) error
}

// Service owns the link and the verifier built from it.
//
// Safe for concurrent use: a request verifies while another refreshes keys.
type Service struct {
	Values Values
	Log    *slog.Logger

	// HTTP talks to the account service. Nil uses a client with a timeout,
	// because a hung service must not hold a claim open forever.
	HTTP *http.Client

	// Now is the clock, for tests.
	Now func() time.Time

	mu       sync.Mutex
	loaded   bool
	link     *Link
	verifier *cloudauth.Verifier
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	// Short: every call here is one small JSON round trip, and a claim is a
	// person waiting at a form.
	return &http.Client{Timeout: 15 * time.Second}
}

// Link reports the stored link, or nil when this server has not been claimed.
func (s *Service) Link(ctx context.Context) (*Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(ctx)
}

// load reads the link once and keeps it. Called with the lock held.
func (s *Service) load(ctx context.Context) (*Link, error) {
	if s.loaded {
		return s.link, nil
	}
	raw, err := s.Values.ServerValue(ctx, LinkKey)
	if err != nil {
		return nil, fmt.Errorf("read cloud link: %w", err)
	}
	s.loaded = true
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var link Link
	if err := json.Unmarshal([]byte(raw), &link); err != nil {
		// A link that cannot be parsed is treated as absent rather than
		// fatal: an unclaimed server still serves its library, and the
		// administrator can claim it again.
		s.logger().Error("stored cloud link is unreadable; treating this server as unclaimed", "error", err)
		return nil, nil
	}
	s.link = &link
	s.verifier = verifierFor(&link, s.Now)
	return s.link, nil
}

func verifierFor(link *Link, now func() time.Time) *cloudauth.Verifier {
	v := &cloudauth.Verifier{Issuer: link.Issuer, Audience: link.ServerID, Now: now}
	// A link with no usable key still verifies nothing, which is the right
	// answer until a refresh lands.
	if err := v.SetKeys(link.Keys); err != nil {
		return v
	}
	return v
}

// save writes the link and rebuilds the verifier. Called with the lock held.
func (s *Service) save(ctx context.Context, link *Link) error {
	encoded, err := json.Marshal(link)
	if err != nil {
		return fmt.Errorf("encode cloud link: %w", err)
	}
	if err := s.Values.SetServerValue(ctx, LinkKey, string(encoded)); err != nil {
		return fmt.Errorf("store cloud link: %w", err)
	}
	s.loaded = true
	s.link = link
	s.verifier = verifierFor(link, s.Now)
	return nil
}

// Claim redeems a claim token at [issuer] and stores what comes back.
//
// The token is the whole of the authority -- an unclaimed server holds no
// secret to authenticate with -- so it is single-use and short-lived at the
// service's end, and this call either completes or leaves the server
// unclaimed. Nothing is written until the keys have been fetched too: a link
// with a secret and no keys would appear claimed while verifying nothing.
func (s *Service) Claim(ctx context.Context, issuer, token, name string) (*Link, error) {
	base, err := normaliseIssuer(issuer)
	if err != nil {
		return nil, err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("a claim token is required")
	}

	var redeemed struct {
		Server struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"server"`
		Secret string `json:"secret"`
	}
	if err := s.call(ctx, http.MethodPost, base+"/servers/claim", "", map[string]string{
		"token": token,
		"name":  name,
	}, &redeemed); err != nil {
		return nil, err
	}
	if redeemed.Server.ID == "" || redeemed.Secret == "" {
		return nil, errors.New("the account service answered a claim without a server id or secret")
	}

	keys, err := s.fetchKeys(ctx, base)
	if err != nil {
		// Deliberately fatal to the claim: see the doc comment.
		return nil, fmt.Errorf("claimed, but could not fetch the signing keys: %w", err)
	}

	link := &Link{
		Issuer:    base,
		ServerID:  redeemed.Server.ID,
		Secret:    redeemed.Secret,
		Name:      redeemed.Server.Name,
		Keys:      keys,
		ClaimedAt: s.now(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.save(ctx, link); err != nil {
		return nil, err
	}
	s.logger().Info("claimed this server with an account service", "issuer", base, "serverId", link.ServerID)
	return link, nil
}

// Unlink forgets the link.
//
// Local accounts a cloud identity created are left alone: they hold library
// grants the administrator made, and deleting them would quietly undo that
// work. Nothing can authenticate as them while there is no link.
func (s *Service) Unlink(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Values.SetServerValue(ctx, LinkKey, ""); err != nil {
		return fmt.Errorf("clear cloud link: %w", err)
	}
	s.loaded = true
	s.link = nil
	s.verifier = nil
	s.logger().Info("unlinked this server from its account service")
	return nil
}

// PublishAddresses tells the service where this server can be reached.
//
// The service stores them and hands them out; it never connects to them, so
// it never learns whether they work. A client tries them in order.
func (s *Service) PublishAddresses(ctx context.Context, addresses []string) error {
	s.mu.Lock()
	link, err := s.load(ctx)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if link == nil {
		return ErrNotLinked
	}
	cleaned := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if trimmed := strings.TrimSpace(address); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	return s.call(ctx, http.MethodPut, link.Issuer+"/servers/"+url.PathEscape(link.ServerID)+"/addresses",
		link.Secret, map[string]any{"addresses": cleaned}, nil)
}

// Verify checks an assertion and returns what it says.
//
// A key id this server does not hold is the one reason to go back to the
// service: rotation publishes the new key alongside the old one for longer
// than an assertion can live, so a refresh is a rotation catching up and
// never a signal that something is wrong. Anything else is refused without a
// round trip, so an unreachable service cannot stop this server verifying the
// assertions it already understands.
func (s *Service) Verify(ctx context.Context, assertion string) (*cloudauth.Claims, error) {
	s.mu.Lock()
	link, err := s.load(ctx)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if link == nil {
		s.mu.Unlock()
		return nil, ErrNotLinked
	}
	verifier := s.verifier
	known := verifier != nil && verifier.Knows(cloudauth.KeyID(assertion))
	s.mu.Unlock()

	if !known {
		if err := s.RefreshKeys(ctx); err != nil {
			// Not fatal: the assertion is about to be refused for the key it
			// names, which is the right answer, and saying why is the log's
			// job rather than the caller's.
			s.logger().Warn("could not refresh the account service's keys", "error", err)
		}
		s.mu.Lock()
		verifier = s.verifier
		s.mu.Unlock()
	}
	if verifier == nil {
		return nil, ErrNotLinked
	}
	return verifier.Verify(assertion)
}

// RefreshKeys fetches the published keys and pins what it saw.
func (s *Service) RefreshKeys(ctx context.Context) error {
	s.mu.Lock()
	link, err := s.load(ctx)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if link == nil {
		return ErrNotLinked
	}

	keys, err := s.fetchKeys(ctx, link.Issuer)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-read under the lock: a concurrent claim or unlink may have replaced
	// the link while this fetch was in flight, and pinning the old issuer's
	// keys onto the new one would be worse than not refreshing at all.
	current, err := s.load(ctx)
	if err != nil {
		return err
	}
	if current == nil || current.Issuer != link.Issuer || current.ServerID != link.ServerID {
		return ErrNotLinked
	}
	updated := *current
	updated.Keys = keys
	return s.save(ctx, &updated)
}

func (s *Service) fetchKeys(ctx context.Context, base string) ([]cloudauth.Key, error) {
	var published struct {
		Keys []cloudauth.Key `json:"keys"`
	}
	if err := s.call(ctx, http.MethodGet, base+"/.well-known/bask-keys.json", "", nil, &published); err != nil {
		return nil, err
	}
	if len(published.Keys) == 0 {
		return nil, errors.New("the account service published no keys")
	}
	// Checked here rather than at first use: a key this cannot parse is a
	// claim that should fail loudly, not an assertion that fails later for no
	// apparent reason.
	probe := &cloudauth.Verifier{}
	if err := probe.SetKeys(published.Keys); err != nil {
		return nil, fmt.Errorf("the account service published a key this cannot use: %w", err)
	}
	return published.Keys, nil
}

// call makes one JSON request. A non-2xx answer carries the service's own
// message where it sent one, because "that claim token is not good any more"
// is what the administrator needs to read.
func (s *Service) call(ctx context.Context, method, endpoint, bearer string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = strings.NewReader(string(encoded))
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	request.Header.Set("Accept", "application/json")

	response, err := s.client().Do(request)
	if err != nil {
		return fmt.Errorf("reach the account service: %w", err)
	}
	defer response.Body.Close()

	// Bounded: this is a small JSON API, and an endpoint that is not one must
	// not be able to feed this process a stream.
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read the account service's answer: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("the account service refused: %s", refusal(response.StatusCode, payload))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("the account service's answer was not the JSON expected: %w", err)
	}
	return nil
}

func refusal(status int, payload []byte) string {
	var said struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(payload, &said); err == nil && said.Error != "" {
		return said.Error
	}
	return fmt.Sprintf("HTTP %d", status)
}

// normaliseIssuer makes the issuer exactly what an assertion's `iss` will say,
// so the comparison in [cloudauth.Verifier] is between two spellings of the
// same thing rather than between a URL and a URL with a slash on it.
func normaliseIssuer(issuer string) (string, error) {
	trimmed := strings.TrimSpace(issuer)
	if trimmed == "" {
		return "", errors.New("an account service address is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("that is not an address: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", errors.New("the account service address needs an http or https scheme")
	}
	if parsed.Host == "" {
		return "", errors.New("the account service address has no host in it")
	}
	// Path and query dropped: every route is at the root, and an issuer that
	// carried a path would never match what the service signs.
	return parsed.Scheme + "://" + parsed.Host, nil
}
