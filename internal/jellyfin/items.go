package jellyfin

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// item is one row in Jellyfin's vocabulary: a BaseItemDto.
//
// One struct for every kind, with omitempty doing the work, because that is
// how the API itself is shaped — a client reads the fields that matter for
// the Type it asked for and ignores the rest.
type item struct {
	ID       string `json:"Id"`
	ServerID string `json:"ServerId,omitempty"`
	Name     string `json:"Name"`
	SortName string `json:"SortName,omitempty"`

	// Type is what this is: Movie, Series, Season, Episode, MusicArtist,
	// MusicAlbum, Audio, PhotoAlbum, Photo. MediaType is how it is consumed:
	// Video, Audio or Photo, and empty for a folder.
	Type      string `json:"Type"`
	MediaType string `json:"MediaType,omitempty"`
	IsFolder  bool   `json:"IsFolder"`

	ParentID string `json:"ParentId,omitempty"`

	// The series and season above an episode, and the album and artist above
	// a track, carried on every row: a client draws a list of episodes with
	// the show's name on each one and must not ask again per row.
	SeriesID              string   `json:"SeriesId,omitempty"`
	SeriesName            string   `json:"SeriesName,omitempty"`
	SeriesPrimaryImageTag string   `json:"SeriesPrimaryImageTag,omitempty"`
	SeasonID              string   `json:"SeasonId,omitempty"`
	SeasonName            string   `json:"SeasonName,omitempty"`
	AlbumID               string   `json:"AlbumId,omitempty"`
	Album                 string   `json:"Album,omitempty"`
	AlbumArtist           string   `json:"AlbumArtist,omitempty"`
	AlbumPrimaryImageTag  string   `json:"AlbumPrimaryImageTag,omitempty"`
	Artists               []string `json:"Artists,omitempty"`

	IndexNumber       *int `json:"IndexNumber,omitempty"`
	ParentIndexNumber *int `json:"ParentIndexNumber,omitempty"`

	// OfficialRating is the content rating as the board wrote it, which is
	// what a client draws beside the title and what its own parental
	// controls read.
	OfficialRating string `json:"OfficialRating,omitempty"`

	ProductionYear int    `json:"ProductionYear,omitempty"`
	Overview       string `json:"Overview,omitempty"`
	PremiereDate   string `json:"PremiereDate,omitempty"`
	DateCreated    string `json:"DateCreated,omitempty"`

	// RunTimeTicks is Jellyfin's unit of time: hundreds of nanoseconds.
	RunTimeTicks int64 `json:"RunTimeTicks,omitempty"`

	// ImageTags names the pictures this item has. The tag is what a client
	// puts in the image URL and in its own cache key, so it has to change
	// when the picture does — and never when it does not.
	ImageTags         map[string]string `json:"ImageTags,omitempty"`
	BackdropImageTags []string          `json:"BackdropImageTags,omitempty"`

	// PlaylistItemID identifies one line of a playlist rather than the thing
	// on it, because the same track twice is two lines and a client removing
	// or moving one has to be able to say which.
	PlaylistItemID string `json:"PlaylistItemId,omitempty"`

	// ExtraType is which sort of extra this is, for the rows a client asks
	// for under an item: Trailer, BehindTheScenes, DeletedScene and so on.
	ExtraType string `json:"ExtraType,omitempty"`

	// The counts tell a client there is a row worth drawing before it asks
	// for the rows themselves, which is the difference between a trailer
	// button that is there and one that appears a second later.
	SpecialFeatureCount int `json:"SpecialFeatureCount,omitempty"`
	LocalTrailerCount   int `json:"LocalTrailerCount,omitempty"`

	ChildCount         int `json:"ChildCount,omitempty"`
	RecursiveItemCount int `json:"RecursiveItemCount,omitempty"`

	UserData *userData `json:"UserData,omitempty"`

	Container    string        `json:"Container,omitempty"`
	Path         string        `json:"Path,omitempty"`
	Width        int           `json:"Width,omitempty"`
	Height       int           `json:"Height,omitempty"`
	MediaSources []mediaSource `json:"MediaSources,omitempty"`
	MediaStreams []mediaStream `json:"MediaStreams,omitempty"`

	// CollectionType is set only on a library, which is the one kind of row
	// that is a library rather than something in one.
	CollectionType string `json:"CollectionType,omitempty"`

	LocationType string `json:"LocationType,omitempty"`
}

// userData is where this viewer is in this item. Per account, which is the
// point of having accounts.
type userData struct {
	PlaybackPositionTicks int64   `json:"PlaybackPositionTicks"`
	PlayCount             int     `json:"PlayCount"`
	Played                bool    `json:"Played"`
	PlayedPercentage      float64 `json:"PlayedPercentage,omitempty"`
	IsFavorite            bool    `json:"IsFavorite"`
	Key                   string  `json:"Key,omitempty"`
}

// itemsReply is the envelope every list comes back in.
type itemsReply struct {
	Items            []item `json:"Items"`
	TotalRecordCount int    `json:"TotalRecordCount"`
	StartIndex       int    `json:"StartIndex"`
}

const (
	ticksPerSecond = int64(time.Second / 100) // 100-nanosecond units
)

func ticks(d time.Duration) int64 { return d.Nanoseconds() / 100 }

func durationOfTicks(t int64) time.Duration { return time.Duration(t) * 100 * time.Nanosecond }

// kindNames maps what BMS knows to what a Jellyfin client switches on.
func typeOf(kind domain.Kind) string {
	switch kind {
	case domain.KindMovie:
		return "Movie"
	case domain.KindShow:
		return "Series"
	case domain.KindSeason:
		return "Season"
	case domain.KindEpisode:
		return "Episode"
	case domain.KindArtist:
		return "MusicArtist"
	case domain.KindAlbum:
		return "MusicAlbum"
	case domain.KindTrack:
		return "Audio"
	case domain.KindPhotoAlbum:
		return "PhotoAlbum"
	case domain.KindPhoto:
		return "Photo"
	case domain.KindExtra:
		// Overwritten per extra with its own sort — a trailer is a "Trailer"
		// to a client — so this is only what an extra of no stated sort is.
		return "Video"
	default:
		return "Folder"
	}
}

// mediaTypeOf is how the thing is consumed, which is what decides whether a
// client reaches for a video surface, an audio one, or an image viewer.
func mediaTypeOf(kind domain.Kind) string {
	switch kind {
	case domain.KindMovie, domain.KindEpisode, domain.KindExtra:
		return "Video"
	case domain.KindTrack:
		return "Audio"
	case domain.KindPhoto:
		return "Photo"
	default:
		return ""
	}
}

func isFolder(kind domain.Kind) bool { return mediaTypeOf(kind) == "" }

// collectionType is what a library holds, in Jellyfin's words. "homevideos"
// is what it calls a photo library, which is also where its clients put the
// photo grid.
func collectionType(kind domain.SectionKind) string {
	switch kind {
	case domain.SectionShows:
		return "tvshows"
	case domain.SectionMusic:
		return "music"
	case domain.SectionPhotos:
		return "homevideos"
	default:
		return "movies"
	}
}

// view renders one library.
func (s *Server) view(section domain.Section) item {
	return item{
		ID:             sectionID(section.ID),
		ServerID:       s.serverID(),
		Name:           section.Title,
		SortName:       strings.ToLower(section.Title),
		Type:           "CollectionFolder",
		IsFolder:       true,
		CollectionType: collectionType(section.Kind),
		LocationType:   "FileSystem",
		ImageTags:      map[string]string{},
	}
}

// related is what a page of rows needs to know about what is above them: the
// containers' names and pictures, read once for the page rather than once per
// row.
type related struct {
	parents map[int64]domain.Item
	counts  map[int64]int
	// favourites is which of this page's items the asking account marked.
	// One query for the page rather than one per row, the same way the
	// parents and the child counts are read.
	favourites map[int64]bool
}

func (r related) of(id *int64) (domain.Item, bool) {
	if id == nil {
		return domain.Item{}, false
	}
	parent, found := r.parents[*id]
	return parent, found
}

// relatives reads the containers above a page of items, and how many things
// are inside the folders on it.
func (s *Server) relatives(ctx context.Context, items []domain.Item, user *domain.User) related {
	out := related{}

	if user != nil && len(items) > 0 {
		marked := make([]int64, 0, len(items))
		for _, it := range items {
			marked = append(marked, it.ID)
		}
		if set, err := s.db(ctx).FavouriteSet(ctx, user.ID, marked); err == nil {
			out.favourites = set
		} else {
			s.logger().Warn("could not read favourites", "error", err)
		}
	}

	var wanted []int64
	for _, it := range items {
		if it.ParentID != nil {
			wanted = append(wanted, *it.ParentID)
		}
	}
	if len(wanted) > 0 {
		parents, err := s.db(ctx).ItemsByIDs(ctx, wanted)
		if err != nil {
			s.logger().Warn("could not read parents", "error", err)
		} else {
			out.parents = parents
			var grand []int64
			for _, parent := range parents {
				if parent.ParentID != nil {
					grand = append(grand, *parent.ParentID)
				}
			}
			if len(grand) > 0 {
				if more, err := s.db(ctx).ItemsByIDs(ctx, grand); err == nil {
					for id, item := range more {
						out.parents[id] = item
					}
				}
			}
		}
	}

	var folders []int64
	for _, it := range items {
		if isFolder(it.Kind) {
			folders = append(folders, it.ID)
		}
	}
	if len(folders) > 0 {
		if counts, err := s.db(ctx).ChildCounts(ctx, folders); err == nil {
			out.counts = counts
		} else {
			s.logger().Warn("could not count children", "error", err)
		}
	}
	return out
}

// render turns one library row into one Jellyfin row.
func (s *Server) render(it domain.Item, rel related, progress domain.Progress) item {
	out := item{
		ID:             itemID(it.ID),
		ServerID:       s.serverID(),
		Name:           it.Title,
		SortName:       it.SortTitle,
		Type:           typeOf(it.Kind),
		MediaType:      mediaTypeOf(it.Kind),
		IsFolder:       isFolder(it.Kind),
		ProductionYear: it.Year,
		Overview:       it.Summary,
		RunTimeTicks:   ticks(it.Duration),
		LocationType:   "FileSystem",
	}
	out.OfficialRating = it.Rating
	if it.Kind == domain.KindSeason && out.Name == "" {
		out.Name = "Season " + strconv.Itoa(it.Index)
	}
	if it.Index > 0 {
		index := it.Index
		out.IndexNumber = &index
	}
	if it.Disc > 0 {
		disc := it.Disc
		out.ParentIndexNumber = &disc
	}
	if !it.AddedAt.IsZero() {
		out.DateCreated = it.AddedAt.UTC().Format(time.RFC3339)
	}
	if !it.TakenAt.IsZero() {
		// A photograph's date is when it was taken, which is what a client
		// groups a camera roll by.
		out.PremiereDate = it.TakenAt.UTC().Format(time.RFC3339)
	}
	if n := rel.counts[it.ID]; n > 0 {
		out.ChildCount, out.RecursiveItemCount = n, n
	}
	if tag := s.imageTag(it); tag != "" {
		out.ImageTags = map[string]string{"Primary": tag}
	}
	if it.ArtKey != "" {
		out.BackdropImageTags = []string{shortTag(it.ArtKey)}
	}

	if parent, found := rel.of(it.ParentID); found {
		out.ParentID = itemID(parent.ID)
		switch it.Kind {
		case domain.KindEpisode:
			out.SeasonID, out.SeasonName = itemID(parent.ID), parent.Title
			if out.SeasonName == "" {
				out.SeasonName = "Season " + strconv.Itoa(parent.Index)
			}
			if parent.Index > 0 {
				season := parent.Index
				out.ParentIndexNumber = &season
			}
			if show, found := rel.of(parent.ParentID); found {
				out.SeriesID, out.SeriesName = itemID(show.ID), show.Title
				out.SeriesPrimaryImageTag = s.imageTag(show)
			}
		case domain.KindSeason:
			out.SeriesID, out.SeriesName = itemID(parent.ID), parent.Title
			out.SeriesPrimaryImageTag = s.imageTag(parent)
		case domain.KindTrack:
			out.AlbumID, out.Album = itemID(parent.ID), parent.Title
			out.AlbumPrimaryImageTag = s.imageTag(parent)
			performer := it.Performer
			if artist, found := rel.of(parent.ParentID); found {
				out.AlbumArtist = artist.Title
				if performer == "" {
					performer = artist.Title
				}
			}
			if performer != "" {
				out.Artists = []string{performer}
			}
			// A track with no cover of its own shows the album's, which is
			// where the cover actually is.
			if out.ImageTags == nil && out.AlbumPrimaryImageTag != "" {
				out.ImageTags = map[string]string{"Primary": out.AlbumPrimaryImageTag}
			}
		case domain.KindAlbum:
			out.AlbumArtist = parent.Title
			out.Artists = []string{parent.Title}
		}
	} else if it.ParentID != nil {
		out.ParentID = itemID(*it.ParentID)
	} else {
		// Nothing above it in the library means the library itself is above
		// it, which is what a client walks back up to.
		out.ParentID = sectionID(it.SectionID)
	}

	if len(it.Parts) > 0 {
		// Every file, not only the first. A film with a Theatrical and a
		// Director's Cut is one item with two files, and offering one of
		// them is the same as not having the other: a client's version
		// picker is built from this list, and a list of one has nothing to
		// pick. The first is still what the top-level fields describe,
		// because that is what a client plays when it does not choose.
		part := it.Parts[0]
		out.Container = part.Container
		out.Path = part.Path
		if it.Kind == domain.KindPhoto {
			out.Width, out.Height = displaySize(part)
		}
		out.MediaSources = make([]mediaSource, 0, len(it.Parts))
		for _, each := range it.Parts {
			out.MediaSources = append(out.MediaSources, s.mediaSource(it, each))
		}
	}

	// User data on anything that can carry an opinion, not only on the things
	// that play: a show or an album can be a favourite, and a client that is
	// sent no user data for one draws an empty star it cannot fill.
	favourite := rel.favourites[it.ID]
	if it.Playable() || favourite {
		out.UserData = &userData{
			PlaybackPositionTicks: ticks(progress.Offset),
			PlayCount:             progress.ViewCount,
			Played:                progress.Watched,
			IsFavorite:            favourite,
			Key:                   itemID(it.ID),
		}
		if it.Duration > 0 && progress.Offset > 0 {
			out.UserData.PlayedPercentage = float64(progress.Offset) / float64(it.Duration) * 100
		}
	}
	return out
}

// imageTag is the cache key for an item's picture.
//
// A photograph and an album of them have no stored artwork — the picture is
// the file, or one of the files inside — so their tag is derived from the
// item instead, and changes when a rescan moves it.
func (s *Server) imageTag(it domain.Item) string {
	if it.PosterKey != "" {
		return shortTag(it.PosterKey)
	}
	if it.Kind == domain.KindPhoto || it.Kind == domain.KindPhotoAlbum {
		return shortTag(itemID(it.ID) + strconv.FormatInt(it.UpdatedAt.Unix(), 10))
	}
	return ""
}

func shortTag(key string) string {
	if len(key) > 32 {
		return key[:32]
	}
	return key
}

// displaySize is a photograph's dimensions as they are meant to be seen: the
// stored size with a quarter turn applied where its EXIF asks for one.
func displaySize(p domain.Part) (width, height int) {
	if p.Orientation >= 5 && p.Orientation <= 8 {
		return p.Height, p.Width
	}
	return p.Width, p.Height
}
