package jellyfin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// publicInfo is how a client decides an address is a server.
//
// Answered without a token on purpose: this is the request somebody's phone
// makes while they are still typing the address in, before anybody has a
// credential. It says what this is and nothing about what is in it.
func (s *Server) publicInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"Id":                     s.serverID(),
		"ServerName":             s.friendlyName(r.Context()),
		"Version":                s.version(),
		"ProductName":            "Bask Media Server",
		"OperatingSystem":        "",
		"LocalAddress":           "",
		"StartupWizardCompleted": true,
	})
}

// systemInfo is the same answer with the fields a signed-in client expects.
func (s *Server) systemInfo(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"Id":                     s.serverID(),
		"ServerName":             s.friendlyName(r.Context()),
		"Version":                s.version(),
		"ProductName":            "Bask Media Server",
		"StartupWizardCompleted": true,
		"HasUpdateAvailable":     false,
		"IsShuttingDown":         false,
		"SupportsLibraryMonitor": true,
		"CanSelfRestart":         false,
		"CanLaunchWebBrowser":    false,
		"OperatingSystem":        "",
		"HasPendingRestart":      false,
		"IsAdministrator":        user != nil && user.Admin,
	})
}

// quickConnectDisabled says this server has no Quick Connect.
//
// Jellyfin clients ask before offering the button, and a 404 here is read by
// some of them as "this is not a Jellyfin server" rather than "that feature
// is off". The honest answer is the one that keeps the button hidden.
func (s *Server) quickConnectDisabled(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, false)
}

// authenticateByName signs a client in with a username and a token.
//
// Jellyfin's own servers keep passwords; BMS keeps tokens, and this is where
// the two meet: the password field carries the token the admin interface
// issued for that device. No second secret is invented for the same account,
// and a token stays what it has always been in BMS — the thing you can revoke
// for one television without touching anybody else.
func (s *Server) authenticateByName(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"Username"`
		Pw       string `json:"Pw"`
		Password string `json:"Password"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	token := body.Pw
	if token == "" {
		// Older clients, and Emby's dialect, send `Password` instead.
		token = body.Password
	}
	if strings.TrimSpace(body.Username) == "" || strings.TrimSpace(token) == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	user, err := s.DB.UserForToken(r.Context(), token)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err != nil {
		s.fail(w, "look up token", err)
		return
	}
	// The token decides which account this is; the username has to agree with
	// it, or a token would sign in as whoever was typed above it.
	if !strings.EqualFold(strings.TrimSpace(body.Username), user.Username) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"User":        s.userDTO(*user),
		"SessionInfo": map[string]any{"UserId": itemID(user.ID), "ServerId": s.serverID()},
		"AccessToken": token,
		"ServerId":    s.serverID(),
	})
}

// userDTO is one account in Jellyfin's shape.
func (s *Server) userDTO(user domain.User) map[string]any {
	return map[string]any{
		"Id":                        itemID(user.ID),
		"Name":                      user.Username,
		"ServerId":                  s.serverID(),
		"HasPassword":               true,
		"HasConfiguredPassword":     true,
		"HasConfiguredEasyPassword": false,
		"EnableAutoLogin":           false,
		"Configuration": map[string]any{
			"PlayDefaultAudioTrack":  true,
			"DisplayMissingEpisodes": false,
		},
		"Policy": map[string]any{
			"IsAdministrator":                user.Admin,
			"IsHidden":                       false,
			"IsDisabled":                     false,
			"EnableMediaPlayback":            true,
			"EnableAudioPlaybackTranscoding": true,
			"EnableVideoPlaybackTranscoding": true,
			"EnablePlaybackRemuxing":         true,
			"EnableContentDownloading":       true,
			"EnableAllFolders":               true,
			"EnableRemoteAccess":             true,
		},
	}
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, s.userDTO(*user))
}

// userByID answers for the signed-in account only.
//
// BMS has no notion of one account reading another's: a token is an account,
// and the id in the path is checked against it rather than trusted.
func (s *Server) userByID(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if id, _, err := parseID(r.PathValue("user")); err != nil || id != user.ID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	writeJSON(w, http.StatusOK, s.userDTO(*user))
}
