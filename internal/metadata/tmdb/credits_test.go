package tmdb_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/metadata"
	"github.com/inknironapps/bask-media-server/internal/metadata/tmdb"
)

// creditsFake answers the detail endpoints, counting requests so one request
// per title can be shown rather than two.
type creditsFake struct{ calls int }

func newCreditsFake(t *testing.T) *creditsFake {
	t.Helper()
	f := &creditsFake{}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /movie/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		// The studio is only on the detail and the credits only on their own
		// endpoint, so append_to_response is what makes this one request.
		if r.URL.Query().Get("append_to_response") != "credits" {
			t.Errorf("credits were not appended: %q", r.URL.RawQuery)
		}
		writeJSON(w, map[string]any{
			"production_companies": []map[string]any{
				{"name": "FilmNation"}, {"name": "Lava Bear"},
			},
			"credits": map[string]any{
				"cast": []map[string]any{
					{"name": "Jeremy Renner", "character": "Ian Donnelly", "order": 1, "profile_path": "/jr.jpg"},
					{"name": "Amy Adams", "character": "Louise Banks", "order": 0, "profile_path": "/aa.jpg"},
				},
				"crew": []map[string]any{
					{"name": "Denis Villeneuve", "job": "Director", "profile_path": "/dv.jpg"},
					{"name": "Eric Heisserer", "job": "Screenplay"},
					{"name": "Shawn Levy", "job": "Producer"},
					// Neither a job nor a department this server keeps.
					{"name": "Jóhann Jóhannsson", "job": "Original Music Composer"},
					{"name": "Somebody", "job": "Casting Assistant"},
				},
			},
		})
	})

	mux.HandleFunc("GET /tv/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		if r.URL.Query().Get("append_to_response") != "aggregate_credits" {
			t.Errorf("aggregate credits were not appended: %q", r.URL.RawQuery)
		}
		writeJSON(w, map[string]any{
			"networks": []map[string]any{{"name": "BBC One"}},
			"aggregate_credits": map[string]any{
				"cast": []map[string]any{
					{"name": "Richard Harrington", "order": 0, "roles": []map[string]any{
						{"character": "Tom Mathias"}, {"character": "Himself"},
					}},
				},
				"crew": []map[string]any{
					{"name": "Ed Thomas", "jobs": []map[string]any{{"job": "Director"}}},
				},
			},
		})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	was := tmdb.BaseURL
	tmdb.BaseURL = server.URL
	t.Cleanup(func() { tmdb.BaseURL = was })
	return f
}

func TestAFilmsCreditsComeInOneRequest(t *testing.T) {
	f := newCreditsFake(t)
	client, err := tmdb.New("a-key", 2)
	if err != nil {
		t.Fatal(err)
	}

	credits, err := client.People(context.Background(), metadata.ArtQuery{
		Kind: domain.KindMovie, Title: "Arrival", Year: 2016, ProviderID: "329865",
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatalf("%d requests for one title", f.calls)
	}

	// The first production company, not all of them: a film lists six of
	// which one is the name anybody recognises, and a client draws one line.
	if credits.Studio != "FilmNation" {
		t.Fatalf("studio = %q", credits.Studio)
	}

	roles := map[string]domain.CreditRole{}
	parts := map[string]string{}
	for _, person := range credits.People {
		roles[person.Name] = person.Role
		parts[person.Name] = person.Part
	}
	if roles["Denis Villeneuve"] != domain.CreditDirector {
		t.Fatalf("Villeneuve = %q", roles["Denis Villeneuve"])
	}
	// Screenplay is a writing job, read by job rather than by department:
	// "Writing" also holds the story editor.
	if roles["Eric Heisserer"] != domain.CreditWriter {
		t.Fatalf("Heisserer = %q", roles["Eric Heisserer"])
	}
	if roles["Shawn Levy"] != domain.CreditProducer {
		t.Fatalf("Levy = %q", roles["Shawn Levy"])
	}
	// A job no client draws a line for is dropped rather than filed under a
	// role it does not belong to.
	if _, found := roles["Jóhann Jóhannsson"]; found {
		t.Fatal("a composer was stored as something else")
	}
	if _, found := roles["Somebody"]; found {
		t.Fatal("a casting assistant was stored as a producer")
	}

	if parts["Amy Adams"] != "Louise Banks" {
		t.Fatalf("part = %q", parts["Amy Adams"])
	}

	// The cast in billing order, whatever order the response listed them in.
	var cast []string
	for _, person := range credits.People {
		if person.Role == domain.CreditActor {
			cast = append(cast, person.Name)
		}
	}
	if len(cast) != 2 || cast[0] != "Amy Adams" {
		t.Fatalf("cast = %v", cast)
	}

	// And a photograph to fetch, at the size a cast row draws.
	for _, person := range credits.People {
		if person.Name == "Amy Adams" && person.ThumbURL == "" {
			t.Fatal("no photograph for the lead")
		}
	}
}

func TestASeriesUsesItsAggregateCredits(t *testing.T) {
	newCreditsFake(t)
	client, _ := tmdb.New("a-key", 2)

	credits, err := client.People(context.Background(), metadata.ArtQuery{
		Kind: domain.KindShow, Title: "Hinterland", ProviderID: "62688",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A network, which is television's answer to a production company.
	if credits.Studio != "BBC One" {
		t.Fatalf("studio = %q", credits.Studio)
	}

	var cast, crew int
	for _, person := range credits.People {
		switch person.Role {
		case domain.CreditActor:
			cast++
			// The first of a person's roles across every episode, because a
			// client draws one.
			if person.Part != "Tom Mathias" {
				t.Fatalf("part = %q", person.Part)
			}
		default:
			crew++
		}
	}
	if cast != 1 || crew != 1 {
		t.Fatalf("cast %d, crew %d", cast, crew)
	}
}

func TestNoProviderIDMeansNoRequest(t *testing.T) {
	f := newCreditsFake(t)
	client, _ := tmdb.New("a-key", 2)

	credits, err := client.People(context.Background(), metadata.ArtQuery{
		Kind: domain.KindMovie, Title: "Arrival",
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 {
		t.Fatalf("%d requests with no id to ask about", f.calls)
	}
	if credits.Studio != "" || len(credits.People) != 0 {
		t.Fatalf("credits = %+v", credits)
	}
}
