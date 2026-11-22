// Package opensubtitles finds a subtitle for a file that has none.
//
// The one external service in this server besides the metadata provider, and
// it is off until somebody saves a key: nothing about anybody's library
// leaves the machine by default. When it is on, a search sends the film's
// title and the file's hash — that is what the service matches against, and
// it is worth knowing that it goes out.
//
// The hash is the important part. OpenSubtitles' own hash says a subtitle was
// authored against a file with exactly these bytes, so it cannot drift; a
// merely high-scoring subtitle for another release of the same film can be a
// minute out by the end, which is worse than no subtitle at all. So a hash
// match is marked as exact and everything else is offered as a guess.
package opensubtitles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BaseURL is the service. A variable so tests can point it at a fake, which
// is how this package is tested without a key and without the network.
var BaseURL = "https://api.opensubtitles.com/api/v1"

// userAgent identifies this client to the service, which requires one and
// rejects a request without it.
//
// The server's name and version, and nothing about who is running it: a
// subtitle search should not tell a third party whose library it is.
const userAgent = "BaskMediaServer v1"

// ErrNoKey means nobody has configured a key, so this is switched off.
var ErrNoKey = errors.New("no OpenSubtitles key: save one in the admin interface")

// ErrQuota means the service says this key has had its downloads for today.
// Distinct from a failure, because the answer is to wait rather than retry.
var ErrQuota = errors.New("this OpenSubtitles key has no downloads left today")

// Client talks to the service.
type Client struct {
	// Key is the API key. Empty means every call reports ErrNoKey.
	Key string

	// HTTP is the client used. Nil means one with a timeout, because a
	// subtitle search must not be able to hang a playback request.
	HTTP *http.Client
}

// Candidate is one subtitle the service offered.
type Candidate struct {
	// FileID is what a download asks for.
	FileID int64

	// Language is the two-letter code the service uses.
	Language string

	// Release is what the subtitle was authored against, which is the line
	// somebody chooses by when there is no exact match.
	Release string

	// Name is the subtitle file's own name.
	Name string

	// Exact means the hash matched: this subtitle was made for this file.
	Exact bool

	// HearingImpaired and Forced are what the subtitle is for.
	HearingImpaired bool
	Forced          bool

	// Downloads and Rating are the service's own popularity numbers, which
	// are the only ordering available when nothing matched by hash.
	Downloads int
	Rating    float64
}

// Query is what to look for.
type Query struct {
	// Title and Year are the film or series, as the library knows them.
	Title string
	Year  int

	// Season and Episode place an episode. Zero for a film.
	Season  int
	Episode int

	// Hash and Size are the file's own identity, which is what makes an
	// exact match possible. Empty hash searches by title alone.
	Hash string
	Size int64

	// Languages are the two-letter codes to look for, in order of
	// preference. Empty means English.
	Languages []string

	// HearingImpaired and Forced narrow the search to those kinds.
	HearingImpaired bool
	Forced          bool
}

// Search asks the service what it has.
//
// Ordered with the exact matches first, because that is the only ordering
// that matters: a hash match is authored against this file and anything else
// is a guess, however popular.
func (c *Client) Search(ctx context.Context, q Query) ([]Candidate, error) {
	if c.Key == "" {
		return nil, ErrNoKey
	}

	params := url.Values{}
	if q.Title != "" {
		params.Set("query", q.Title)
	}
	if q.Year > 0 {
		params.Set("year", strconv.Itoa(q.Year))
	}
	if q.Season > 0 {
		params.Set("season_number", strconv.Itoa(q.Season))
	}
	if q.Episode > 0 {
		params.Set("episode_number", strconv.Itoa(q.Episode))
	}
	if q.Hash != "" {
		params.Set("moviehash", q.Hash)
	}
	languages := q.Languages
	if len(languages) == 0 {
		languages = []string{"en"}
	}
	params.Set("languages", strings.Join(languages, ","))
	if q.HearingImpaired {
		params.Set("hearing_impaired", "only")
	}
	if q.Forced {
		params.Set("foreign_parts_only", "only")
	}

	var answer struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				Language         string  `json:"language"`
				DownloadCount    int     `json:"download_count"`
				Ratings          float64 `json:"ratings"`
				HearingImpaired  bool    `json:"hearing_impaired"`
				ForeignPartsOnly bool    `json:"foreign_parts_only"`
				MovieHashMatch   bool    `json:"moviehash_match"`
				Release          string  `json:"release"`
				Files            []struct {
					FileID   int64  `json:"file_id"`
					FileName string `json:"file_name"`
				} `json:"files"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.call(ctx, http.MethodGet, "/subtitles?"+params.Encode(), nil, &answer); err != nil {
		return nil, err
	}

	out := make([]Candidate, 0, len(answer.Data))
	for _, row := range answer.Data {
		if len(row.Attributes.Files) == 0 {
			// Nothing to download, so nothing to offer.
			continue
		}
		file := row.Attributes.Files[0]
		out = append(out, Candidate{
			FileID:          file.FileID,
			Language:        strings.ToLower(row.Attributes.Language),
			Release:         row.Attributes.Release,
			Name:            file.FileName,
			Exact:           row.Attributes.MovieHashMatch,
			HearingImpaired: row.Attributes.HearingImpaired,
			Forced:          row.Attributes.ForeignPartsOnly,
			Downloads:       row.Attributes.DownloadCount,
			Rating:          row.Attributes.Ratings,
		})
	}
	// Exact matches first, then by how many people have used the thing.
	sortCandidates(out)
	return out, nil
}

// Download returns the subtitle's bytes.
//
// Two requests, because the service works that way: one to turn a file id
// into a link, one to fetch it. The first is what counts against the key's
// daily allowance, which is why nothing here downloads speculatively.
func (c *Client) Download(ctx context.Context, fileID int64) ([]byte, string, error) {
	if c.Key == "" {
		return nil, "", ErrNoKey
	}

	body, err := json.Marshal(map[string]any{"file_id": fileID})
	if err != nil {
		return nil, "", err
	}
	var answer struct {
		Link      string `json:"link"`
		FileName  string `json:"file_name"`
		Remaining int    `json:"remaining"`
	}
	if err := c.call(ctx, http.MethodPost, "/download", body, &answer); err != nil {
		return nil, "", err
	}
	if answer.Link == "" {
		return nil, "", errors.New("the service gave no link to download")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, answer.Link, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	res, err := c.client().Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch subtitle: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch subtitle: %s", res.Status)
	}
	// Capped, because a subtitle is tens of kilobytes and this is a link
	// somebody else chose: see rule 1 of docs/RESOURCE-BUDGET.md.
	text, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, "", fmt.Errorf("fetch subtitle: %w", err)
	}
	return text, answer.FileName, nil
}

// call makes one request to the service and decodes the answer.
func (c *Client) call(ctx context.Context, method, path string, body []byte, into any) error {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Api-Key", c.Key)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("opensubtitles: %w", err)
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("opensubtitles refused the key: %s", res.Status)
	case http.StatusTooManyRequests, http.StatusPaymentRequired:
		return ErrQuota
	default:
		return fmt.Errorf("opensubtitles: %s", res.Status)
	}

	// Capped for the same reason the download is.
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("opensubtitles: %w", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("opensubtitles answered with something unexpected: %w", err)
	}
	return nil
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// A subtitle search is never worth hanging a request for.
	return &http.Client{Timeout: 20 * time.Second}
}
