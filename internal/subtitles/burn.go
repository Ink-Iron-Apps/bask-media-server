package subtitles

import (
	"sort"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/playback"
)

// Working out what to tell ffmpeg to draw.
//
// The awkward part is an index. A subtitle row carries the stream's own
// number inside the container, which is what `-map 0:5` takes — and ffmpeg's
// subtitles filter instead counts subtitle streams, so the fifth stream of a
// file might be its first subtitle. The two are not interchangeable and
// confusing them draws the wrong language, or nothing at all.
//
// So: the bitmap path uses the container's own numbering, because it maps the
// stream into a filtergraph. The text path counts subtitle streams, because
// it uses the filter. Both are worked out here, from the rows a surface
// already has in hand.

// BurnIn describes how to draw one subtitle into the picture.
//
// all is every subtitle known for the item, which is what makes the text
// index computable: it is the chosen row's position among the embedded ones,
// in stream order.
func BurnIn(all []domain.Subtitle, chosen domain.Subtitle) *playback.BurnIn {
	if chosen.External {
		// A sidecar is a file with one subtitle in it, so there is no index
		// to count: the filter reads the file.
		return &playback.BurnIn{Path: chosen.Path, Index: -1, Picture: !chosen.Text()}
	}
	burn := &playback.BurnIn{Path: chosen.Path, Picture: !chosen.Text()}
	if burn.Picture {
		// Composited, so it is mapped by the number the container gives it.
		burn.Index = chosen.StreamIndex
		return burn
	}
	burn.Index = textIndex(all, chosen)
	return burn
}

// textIndex is the chosen subtitle's position among the embedded subtitles of
// the same file, which is what the subtitles filter's si= means.
func textIndex(all []domain.Subtitle, chosen domain.Subtitle) int {
	inside := make([]domain.Subtitle, 0, len(all))
	for _, sub := range all {
		if !sub.External && sub.Path == chosen.Path {
			inside = append(inside, sub)
		}
	}
	sort.Slice(inside, func(i, j int) bool { return inside[i].StreamIndex < inside[j].StreamIndex })
	for position, sub := range inside {
		if sub.ID == chosen.ID {
			return position
		}
	}
	// Not in the list it should be in. Zero is the first subtitle, which is
	// the least wrong answer available and what a client with no idea asks
	// for anyway.
	return 0
}
