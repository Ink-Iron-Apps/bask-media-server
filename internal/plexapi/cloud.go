package plexapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/cloud"
	"github.com/inknironapps/bask-media-server/internal/cloudauth"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// AssertionHeader is where a client may present an account-service assertion
// explicitly.
//
// It is also accepted in X-Plex-Token, because a client that speaks this
// dialect already sends that header on every request and in the query string
// of a stream URL. The two cannot be confused: an assertion is a compact JWS,
// and a token this server issued is a single opaque string with no dots in it.
const AssertionHeader = "X-Bask-Assertion"

// errAwaitingApproval is a verified identity with no permission yet. Separate
// from every other refusal because it is the one the viewer can do nothing
// about and the administrator can fix in a click.
var errAwaitingApproval = errors.New("this account is waiting for the server's administrator")

// looksLikeAnAssertion reports whether presented is shaped like a compact JWS.
//
// Shape only, and deliberately: this decides which credential path to take,
// not whether anything is trusted. A string that looks like one and is not
// gets refused by the verifier, which is the same answer as a bad token.
func looksLikeAnAssertion(presented string) bool {
	if presented == "" {
		return false
	}
	parts := strings.Split(presented, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
	}
	return true
}

// cloudCredential verifies an assertion and resolves the local account it maps
// to.
//
// The mapping is this machine's, not the service's: `sub` names a person, and
// which local account that is -- and what it may reach -- is decided here. A
// subject nobody has seen before gets an account created for it, inactive, so
// the administrator has something to grant libraries to; it authenticates as
// nothing until they do.
func (s *Server) cloudCredential(ctx context.Context, assertion string) (*domain.Credential, error) {
	if s.Cloud == nil {
		return nil, cloud.ErrNotLinked
	}
	claims, err := s.Cloud.Verify(ctx, assertion)
	if err != nil {
		return nil, err
	}

	user, err := s.Store.UserForCloudSubject(ctx, claims.Subject)
	switch {
	case errors.Is(err, store.ErrNotFound):
		created, createErr := s.Store.CreateCloudUser(ctx, claims.Subject, claims.Name, claims.Email)
		if createErr != nil {
			return nil, createErr
		}
		s.logger().Info("a cloud account presented itself for the first time and is awaiting approval",
			"account", created.Username, "subject", claims.Subject)
		return nil, errAwaitingApproval
	case err != nil:
		return nil, err
	}
	if !user.Active {
		return nil, errAwaitingApproval
	}
	return s.Store.CredentialForUser(ctx, user.ID)
}

// refuseCloud answers a refusal from the assertion path.
//
// An unverified assertion is a 401, the same as a bad token: the client should
// ask the account service for a new one. A verified identity with no
// permission is a 403 with something a person can read, because re-signing-in
// would not help and a client that was told 401 would loop trying.
func (s *Server) refuseCloud(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errAwaitingApproval):
		http.Error(w, "this account is waiting for the server's administrator to allow it", http.StatusForbidden)
	case errors.Is(err, cloudauth.ErrNotVerified), errors.Is(err, cloud.ErrNotLinked), errors.Is(err, store.ErrNotFound):
		w.Header().Set("X-Plex-Protocol", "1.0")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	default:
		s.logger().Error("cloud assertion check failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
