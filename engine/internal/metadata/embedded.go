package metadata

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"musix/engine/internal/logger"

	"github.com/tommyo123/mtag"
)

var logMeta = logger.New("metadata")

// ErrUnsupportedContainer indicates the audio stream is in an unrecognized or unsupported container format.
var ErrUnsupportedContainer = errors.New("unsupported audio container format")

// ExtractEmbeddedMetadata opens any supported audio container (FLAC, MP3, M4A, OGG, etc.) via mtag,
// extracts authoritative tags and stream audio properties into FLACMetadata, and writes embedded cover
// art to disk in artworkDir (<trackID>.orig and <trackID>.jpg).
func ExtractEmbeddedMetadata(
	ctx context.Context,
	trackID string,
	reader RangeReader,
	size int64,
	artworkDir string,
) (*FLACMetadata, bool, error) {
	if reader == nil {
		return nil, false, errors.New("range reader is nil")
	}

	if ctx != nil && ctx.Err() != nil {
		return nil, false, ctx.Err()
	}

	// If size is unknown or non-positive, fall back to header-based FLAC extraction
	if size <= 0 {
		return ExtractFLACMetadata(ctx, trackID, reader, artworkDir)
	}

	ra := newRangeReaderAt(ctx, trackID, reader, size)

	// Open with mtag using lightweight read-only options.
	// WithSkipPictures is mandatory to avoid loading full image payloads into memory during initial tag scan.
	f, err := mtag.OpenSource(
		ra,
		size,
		mtag.WithReadOnly(),
		mtag.WithSkipV1(),
		mtag.WithSkipPictures(),
	)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || (ctx != nil && ctx.Err() != nil) {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("%w: %v", ErrUnsupportedContainer, err)
	}
	defer f.Close()

	if f.Container() == mtag.ContainerUnknown {
		return nil, false, ErrUnsupportedContainer
	}

	meta := &FLACMetadata{
		Title:       strings.TrimSpace(f.Title()),
		Artist:      strings.TrimSpace(f.Artist()),
		Album:       strings.TrimSpace(f.Album()),
		AlbumArtist: strings.TrimSpace(f.AlbumArtist()),
		Year:        f.Year(),
		TrackNumber: f.Track(),
		DiscNumber:  f.Disc(),
		Genre:       strings.TrimSpace(f.Genre()),
		Composer:    strings.TrimSpace(f.Composer()),
	}

	ap := f.AudioProperties()
	meta.DurationSec = int(ap.Duration.Seconds())
	meta.SampleRate = ap.SampleRate
	meta.Channels = ap.Channels

	// Multi-value artist extraction
	artistValues := f.CustomValues("ARTIST")
	if len(artistValues) == 0 {
		artistValues = f.CustomValues("PERFORMER")
	}
	for _, a := range artistValues {
		a = strings.TrimSpace(a)
		if a != "" {
			found := false
			for _, existing := range meta.Artists {
				if strings.EqualFold(existing, a) {
					found = true
					break
				}
			}
			if !found {
				meta.Artists = append(meta.Artists, a)
			}
		}
	}
	if len(meta.Artists) == 0 && meta.Artist != "" {
		meta.Artists = []string{meta.Artist}
	}
	if len(meta.Artists) > 0 {
		meta.Artist = strings.Join(meta.Artists, ", ")
	}

	// Artwork extraction
	hasArt := false

	if f.Container() == mtag.ContainerFLAC {
		// For FLAC: mtag does not expose picture byte offsets.
		// Use lightweight header-only block walker over the cached rangeReaderAt
		// to stream the picture to disk with bounded memory.
		flacTags, picInfo, flacErr := ExtractFLACTags(ctx, trackID, ra)
		if flacErr == nil && flacTags != nil {
			if meta.DurationSec == 0 && flacTags.DurationSec > 0 {
				meta.DurationSec = flacTags.DurationSec
			}
			if meta.SampleRate == 0 && flacTags.SampleRate > 0 {
				meta.SampleRate = flacTags.SampleRate
			}
			if meta.Channels == 0 && flacTags.Channels > 0 {
				meta.Channels = flacTags.Channels
			}
			if len(meta.Artists) == 0 && len(flacTags.Artists) > 0 {
				meta.Artists = flacTags.Artists
				meta.Artist = flacTags.Artist
			}
		}

		if picInfo != nil && artworkDir != "" {
			artWritten, streamErr := StreamFLACPicture(ctx, trackID, ra, picInfo, artworkDir)
			if streamErr != nil {
				logMeta.Warnf("Failed to stream FLAC picture for track %s: %v", trackID, streamErr)
			}
			if artWritten {
				hasArt = true
				meta.HasPicture = true
				meta.PictureType = picInfo.Type
				meta.PictureMime = picInfo.Mime
				meta.PictureOffset = picInfo.Offset
				meta.PictureLength = picInfo.Length
			}
		}
	} else if artworkDir != "" {
		// For MP3, M4A, OGG: reopen without WithSkipPictures to inspect image summaries and payload
		fPic, err := mtag.OpenSource(ra, size, mtag.WithReadOnly(), mtag.WithSkipV1())
		if err != nil {
			logMeta.Warnf("Failed to reopen audio container with pictures for track %s: %v", trackID, err)
		} else {
			defer fPic.Close()
			summaries := fPic.ImageSummaries()
			if len(summaries) > 0 {
				var bestSummary *mtag.ImageSummary
				for i := range summaries {
					s := &summaries[i]
					if bestSummary == nil || (bestSummary.Type != mtag.PictureCoverFront && s.Type == mtag.PictureCoverFront) {
						bestSummary = s
					}
				}

				// Cap picture size at 8 MB to protect memory
				const maxCoverBytes = 8 * 1024 * 1024
				if bestSummary != nil && bestSummary.Size > 0 && bestSummary.Size <= maxCoverBytes {
					images := fPic.Images()
					var bestPic *mtag.Picture
					for i := range images {
						p := &images[i]
						if bestPic == nil || (bestPic.Type != mtag.PictureCoverFront && p.Type == mtag.PictureCoverFront) {
							bestPic = p
						}
					}

					if bestPic != nil && len(bestPic.Data) > 0 {
						if err := os.MkdirAll(artworkDir, 0755); err == nil {
							origPath := filepath.Join(artworkDir, trackID+".orig")
							thumbPath := filepath.Join(artworkDir, trackID+".jpg")

							if err := os.WriteFile(origPath, bestPic.Data, 0644); err == nil {
								if isValidImage(origPath) {
									if dsErr := DownscaleImageToJPEG(origPath, thumbPath, MaxThumbnailDimension); dsErr == nil {
										hasArt = true
										meta.HasPicture = true
										meta.PictureMime = bestPic.MIME
										meta.PictureLength = int64(len(bestPic.Data))
										meta.PictureType = uint32(bestPic.Type)
									} else if copyErr := copyFile(origPath, thumbPath); copyErr == nil {
										hasArt = true
										meta.HasPicture = true
										meta.PictureMime = bestPic.MIME
										meta.PictureLength = int64(len(bestPic.Data))
									}
								} else {
									_ = os.Remove(origPath)
								}
							}
						}
					}
				}
			}
		}
	}

	return meta, hasArt, nil
}
