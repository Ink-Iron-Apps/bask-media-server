package tmdb

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/metadata"
)

// Who made a thing and who is in it.
//
// One request, not two. TMDB's detail endpoints take append_to_response, so
// /movie/{id}?append_to_response=credits returns the production companies
// and the whole cast and crew together — and the studio is only on the
// detail, while the credits are only on their own endpoint, so asking for
// them separately would be two requests for one answer.
//
// Fetched only when the library asked for it. This is the expensive half of
// enrichment: a request per matched title on top of the match itself, which
// over a cold library of a thousand films is a thousand requests nobody
// needs if they do not read cast lists.

// crewJobs maps TMDB's job titles to the roles BMS keeps.
//
// By job rather than by department, because the departments are wrong for
// this purpose: "Writing" holds the writer and also the story editor, while
// "Production" holds the producer and also the casting assistant. Only the
// jobs a client draws a line for are kept, and everything else is dropped
// rather than filed under a role it does not belong to.
var crewJobs = map[string]domain.CreditRole{
	"director":              domain.CreditDirector,
	"writer":                domain.CreditWriter,
	"screenplay":            domain.CreditWriter,
	"story":                 domain.CreditWriter,
	"teleplay":              domain.CreditWriter,
	"author":                domain.CreditWriter,
	"novel":                 domain.CreditWriter,
	"producer":              domain.CreditProducer,
	"executive producer":    domain.CreditProducer,
	"co-producer":           domain.CreditProducer,
	"associate producer":    domain.CreditProducer,
	"co-executive producer": domain.CreditProducer,
}

type creditsResponse struct {
	Cast []struct {
		Name        string `json:"name"`
		Character   string `json:"character"`
		Order       int    `json:"order"`
		ProfilePath string `json:"profile_path"`
	} `json:"cast"`
	Crew []struct {
		Name        string `json:"name"`
		Job         string `json:"job"`
		ProfilePath string `json:"profile_path"`
	} `json:"crew"`
}

type detailResponse struct {
	ProductionCompanies []struct {
		Name string `json:"name"`
	} `json:"production_companies"`

	// Networks is television's answer to a production company, and the one
	// a client shows for a series.
	Networks []struct {
		Name string `json:"name"`
	} `json:"networks"`

	Credits creditsResponse `json:"credits"`

	// AggregateCredits is what a series has instead: a cast assembled across
	// every episode, where each person carries a list of roles rather than
	// one character. Asked for alongside credits because a series' plain
	// credits endpoint is often nearly empty.
	AggregateCredits struct {
		Cast []struct {
			Name        string `json:"name"`
			Order       int    `json:"order"`
			ProfilePath string `json:"profile_path"`
			Roles       []struct {
				Character string `json:"character"`
			} `json:"roles"`
		} `json:"cast"`
		Crew []struct {
			Name string `json:"name"`
			Jobs []struct {
				Job string `json:"job"`
			} `json:"jobs"`
			ProfilePath string `json:"profile_path"`
		} `json:"crew"`
	} `json:"aggregate_credits"`
}

// maxCast is how many of a cast to keep.
//
// A client shows the first six or eight. A blockbuster's full cast runs to
// hundreds, and keeping them all would be a joining table larger than the
// library for names nobody reads. TMDB's order field is billing order, so
// the ones kept are the ones worth keeping.
const maxCast = 20

// People fetches the credits and the studio for one title.
//
// Returns no error for a title the provider has nothing about: an empty
// answer is an answer, and an enrichment pass must not fail an item over a
// cast list.
func (c *Client) People(ctx context.Context, want metadata.ArtQuery) (metadata.Credits, error) {
	if want.ProviderID == "" {
		return metadata.Credits{}, nil
	}

	var (
		out    detailResponse
		path   string
		params = url.Values{}
	)
	if want.Kind == domain.KindShow {
		path = "/tv/" + want.ProviderID
		params.Set("append_to_response", "aggregate_credits")
	} else {
		path = "/movie/" + want.ProviderID
		params.Set("append_to_response", "credits")
	}
	if err := c.get(ctx, path, params, &out); err != nil {
		if errors.Is(err, errNotFound) {
			return metadata.Credits{}, nil
		}
		return metadata.Credits{}, err
	}

	credits := metadata.Credits{Studio: studioOf(out)}
	if want.Kind == domain.KindShow {
		credits.People = seriesPeople(out)
	} else {
		credits.People = filmPeople(out.Credits)
	}
	return credits, nil
}

// studioOf is who made it: the first production company for a film, the
// first network for a series.
//
// The first, not all of them. A film routinely lists six companies of which
// one is the name anybody recognises, and TMDB puts them in order of
// involvement; a client draws one line.
func studioOf(out detailResponse) string {
	for _, network := range out.Networks {
		if name := strings.TrimSpace(network.Name); name != "" {
			return name
		}
	}
	for _, company := range out.ProductionCompanies {
		if name := strings.TrimSpace(company.Name); name != "" {
			return name
		}
	}
	return ""
}

func filmPeople(credits creditsResponse) []metadata.Person {
	var out []metadata.Person
	for _, member := range credits.Crew {
		role, wanted := crewJobs[strings.ToLower(strings.TrimSpace(member.Job))]
		if !wanted {
			continue
		}
		out = append(out, metadata.Person{
			Name: member.Name, Role: role, ThumbURL: profileURL(member.ProfilePath),
		})
	}

	cast := credits.Cast
	sort.SliceStable(cast, func(i, j int) bool { return cast[i].Order < cast[j].Order })
	for i, member := range cast {
		if i == maxCast {
			break
		}
		out = append(out, metadata.Person{
			Name: member.Name, Role: domain.CreditActor, Part: member.Character,
			Position: member.Order, ThumbURL: profileURL(member.ProfilePath),
		})
	}
	return out
}

// seriesPeople reads a series' aggregate credits, where a person carries a
// list of roles across every episode rather than one character.
func seriesPeople(out detailResponse) []metadata.Person {
	var people []metadata.Person
	for _, member := range out.AggregateCredits.Crew {
		for _, job := range member.Jobs {
			role, wanted := crewJobs[strings.ToLower(strings.TrimSpace(job.Job))]
			if !wanted {
				continue
			}
			people = append(people, metadata.Person{
				Name: member.Name, Role: role, ThumbURL: profileURL(member.ProfilePath),
			})
			break
		}
	}

	cast := out.AggregateCredits.Cast
	sort.SliceStable(cast, func(i, j int) bool { return cast[i].Order < cast[j].Order })
	for i, member := range cast {
		if i == maxCast {
			break
		}
		// The first role is the one they are known for in it: the list is
		// every character across every episode, and a client draws one.
		part := ""
		if len(member.Roles) > 0 {
			part = member.Roles[0].Character
		}
		people = append(people, metadata.Person{
			Name: member.Name, Role: domain.CreditActor, Part: part,
			Position: member.Order, ThumbURL: profileURL(member.ProfilePath),
		})
	}

	// A series whose aggregate credits are empty falls back to its plain
	// ones, which is the case for a show nobody has filled in properly.
	if len(people) == 0 {
		return filmPeople(out.Credits)
	}
	return people
}

// profileBase is the size a person's photograph is fetched at. w185 is what
// a client draws in a cast row; anything larger is bytes nobody sees.
const profileBase = "https://image.tmdb.org/t/p/w185"

func profileURL(path string) string {
	if path == "" {
		return ""
	}
	return profileBase + path
}
