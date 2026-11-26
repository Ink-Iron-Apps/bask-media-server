package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/cloud"
)

// The Bask account service, as the administrator sees it: whether this server
// is claimed, claiming it, letting it go, and telling the service where it can
// be reached.
//
// Everything here is optional. A server nobody claims serves its library
// exactly as it always has, and this page says so rather than hiding.

type cloudView struct {
	// Supported is false when this build has no account-service support
	// wired in at all, which is a different thing from being unclaimed.
	Supported bool `json:"supported"`
	Linked    bool `json:"linked"`

	// Link is absent while unclaimed. It never carries the server's secret.
	Link *cloud.PublicLink `json:"link,omitempty"`

	// Waiting is how many accounts have presented a verified identity and are
	// waiting to be allowed. The number the administrator is here to act on.
	Waiting int `json:"waiting"`
}

func (s *Server) cloudStatus(w http.ResponseWriter, r *http.Request) {
	out := cloudView{Supported: s.Cloud != nil}
	if s.Cloud == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}

	link, err := s.Cloud.Link(r.Context())
	if err != nil {
		s.fail(w, "read the cloud link", err)
		return
	}
	if link != nil {
		public := link.Public()
		out.Linked = true
		out.Link = &public
	}

	users, err := s.DB.Users(r.Context())
	if err != nil {
		s.fail(w, "list users", err)
		return
	}
	for _, user := range users {
		if user.FromCloud() && !user.Active {
			out.Waiting++
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) claimCloud(w http.ResponseWriter, r *http.Request) {
	if s.Cloud == nil {
		writeError(w, http.StatusNotImplemented, "this server was built without account-service support")
		return
	}
	var body struct {
		Issuer string `json:"issuer"`
		Token  string `json:"token"`
		Name   string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = s.friendlyName(r)
	}

	link, err := s.Cloud.Claim(r.Context(), body.Issuer, body.Token, name)
	if err != nil {
		// The service's own words, and a 400 rather than a 500: an expired
		// claim token is the administrator's to fix by minting another, and a
		// 500 would send them looking at this server's logs instead.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, link.Public())
}

func (s *Server) unlinkCloud(w http.ResponseWriter, r *http.Request) {
	if s.Cloud == nil {
		writeError(w, http.StatusNotImplemented, "this server was built without account-service support")
		return
	}
	if err := s.Cloud.Unlink(r.Context()); err != nil {
		s.fail(w, "unlink the account service", err)
		return
	}
	// The accounts a cloud identity created are left alone on purpose: they
	// hold library grants somebody made, and deleting them would quietly undo
	// that work. Nothing can authenticate as them while there is no link.
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) publishCloudAddresses(w http.ResponseWriter, r *http.Request) {
	if s.Cloud == nil {
		writeError(w, http.StatusNotImplemented, "this server was built without account-service support")
		return
	}
	var body struct {
		Addresses []string `json:"addresses"`
	}
	if !decode(w, r, &body) {
		return
	}
	err := s.Cloud.PublishAddresses(r.Context(), body.Addresses)
	if errors.Is(err, cloud.ErrNotLinked) {
		writeError(w, http.StatusConflict, "claim this server before publishing where it can be reached")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
