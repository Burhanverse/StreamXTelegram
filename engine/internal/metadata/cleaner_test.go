package metadata

import (
	"testing"
)

func TestSlugify(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Queen", "queen"},
		{"AC/DC", "ac_dc"},
		{"Simon & Garfunkel", "simon_and_garfunkel"},
		{"Ed Sheeran ÷ Tour", "ed_sheeran_divide_tour"},
		{"   Multiple    Spaces   ", "multiple_spaces"},
		{"Beyoncé", "beyonce"}, // Diacritics removed
		{"", ""},
	}

	for _, tt := range tests {
		got := Slugify(tt.input)
		if got != tt.expected {
			t.Errorf("Slugify(%q) = %q; want %q", tt.input, got, tt.expected)
		}
	}
}

func TestArtistAndAlbumID(t *testing.T) {
	if got := ArtistID("Pink Floyd"); got != "artist_pink_floyd" {
		t.Errorf("ArtistID mismatch: %s", got)
	}

	if got := AlbumID("The Dark Side of the Moon", 1973); got != "album_the_dark_side_of_the_moon_1973" {
		t.Errorf("AlbumID mismatch with year: %s", got)
	}

	if got := AlbumID("Greatest Hits", 0); got != "album_greatest_hits" {
		t.Errorf("AlbumID mismatch without year: %s", got)
	}
}

func TestNormalizePunctuation(t *testing.T) {
	input := "Queen，Freddie Mercury；Brian May：Rock／Opera"
	expected := "Queen, Freddie Mercury; Brian May: Rock / Opera"
	got := NormalizePunctuation(input)
	if got != expected {
		t.Errorf("NormalizePunctuation mismatch: got %q, want %q", got, expected)
	}
}

func TestStripNoise(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			"Bohemian Rhapsody [320kbps] (Official Video)",
			"Bohemian Rhapsody",
		},
		{
			"Starboy (feat. Daft Punk) [FLAC] @music_hub_channel",
			"Starboy (feat. Daft Punk)",
		},
		{
			"Hotel California (Remastered 2013) (Live)",
			"Hotel California",
		},
		{
			"Track Name (Unclosed bracket",
			"Track Name Unclosed bracket",
		},
		{
			"01. Bohemian Rhapsody",
			"Bohemian Rhapsody",
		},
		{
			"01/12 - Hotel California",
			"Hotel California",
		},
		{
			"02) Stairway to Heaven [FLAC]",
			"Stairway to Heaven",
		},
	}

	for _, tt := range tests {
		got := StripNoise(tt.input)
		if got != tt.expected {
			t.Errorf("StripNoise(%q) = %q; want %q", tt.input, got, tt.expected)
		}
	}
}

func TestCleanDuplicatedPhrase(t *testing.T) {
	// Duplicated halves with comma
	s := "Shaan, Kailash Kher, Shaan, Kailash Kher"
	cleaned, changed := CleanDuplicatedPhrase(s)
	if !changed || cleaned != "Shaan, Kailash Kher" {
		t.Errorf("CleanDuplicatedPhrase failed: got %q (changed: %t)", cleaned, changed)
	}

	// Non-duplicated
	s2 := "Queen, David Bowie"
	cleaned2, changed2 := CleanDuplicatedPhrase(s2)
	if changed2 || cleaned2 != s2 {
		t.Errorf("CleanDuplicatedPhrase falsely altered %q: got %q", s2, cleaned2)
	}
}

func TestCleanArtistAndTitle(t *testing.T) {
	// Stuttered artist should be deduplicated
	artist := "A.R. Rahman, Shreya Ghoshal, A.R. Rahman, Shreya Ghoshal"
	cleanA, changedA := CleanArtist(artist)
	if !changedA || cleanA != "A.R. Rahman, Shreya Ghoshal" {
		t.Errorf("CleanArtist failed: got %q (changed: %t)", cleanA, changedA)
	}

	// Legitimate intentional repetitive title without artist stutter must be preserved
	titleRepetitive := "Tonight, Tonight"
	cleanT, changedT := CleanTitle(titleRepetitive, "The Smashing Pumpkins")
	if changedT || cleanT != "Tonight, Tonight" {
		t.Errorf("CleanTitle altered legitimate title: got %q (changed: %t)", cleanT, changedT)
	}

	// Long repetitive stuttered title should be deduplicated
	longStutter := "Mangal Bhavan Amangal Haari, Mangal Bhavan Amangal Haari"
	cleanLong, changedLong := CleanTitle(longStutter, "Ravindra Jain")
	if !changedLong || cleanLong != "Mangal Bhavan Amangal Haari" {
		t.Errorf("CleanTitle failed on long stutter: got %q (changed: %t)", cleanLong, changedLong)
	}

	// Full CleanMetadata
	cTitle, cArtist := CleanMetadata("Comfortably Numb [320kbps] @rock_tracks", "Pink Floyd @channels")
	if cTitle != "Comfortably Numb" || cArtist != "Pink Floyd" {
		t.Errorf("CleanMetadata failed: title=%q, artist=%q", cTitle, cArtist)
	}
}

func TestExtractTitleAndArtist(t *testing.T) {
	tests := []struct {
		name         string
		rawTitle     string
		rawArtist    string
		rawFileName  string
		wantTitle    string
		wantArtist   string
	}{
		{
			name:        "User example: full audio attr and filename",
			rawTitle:    "Tu Hai Ki Nahi",
			rawArtist:   "Ankit Tiwari, Abhendra Kumar Upadhyay",
			rawFileName: "Tu Hai Ki Nahi - Ankit Tiwari, Abhendra Kumar Upadhyay.flac",
			wantTitle:   "Tu Hai Ki Nahi",
			wantArtist:  "Ankit Tiwari, Abhendra Kumar Upadhyay",
		},
		{
			name:        "User example: missing artist, title from audio attr, artist in filename",
			rawTitle:    "Tu Hai Ki Nahi",
			rawArtist:   "Unknown Artist",
			rawFileName: "Tu Hai Ki Nahi - Ankit Tiwari, Abhendra Kumar Upadhyay.flac",
			wantTitle:   "Tu Hai Ki Nahi",
			wantArtist:  "Ankit Tiwari, Abhendra Kumar Upadhyay",
		},
		{
			name:        "User example: uploaded as raw document (empty title & artist)",
			rawTitle:    "",
			rawArtist:   "",
			rawFileName: "Tu Hai Ki Nahi - Ankit Tiwari, Abhendra Kumar Upadhyay.flac",
			wantTitle:   "Tu Hai Ki Nahi",
			wantArtist:  "Ankit Tiwari, Abhendra Kumar Upadhyay",
		},
		{
			name:        "Raw title has both title and artist, artist is Unknown",
			rawTitle:    "Tu Hai Ki Nahi - Ankit Tiwari, Abhendra Kumar Upadhyay",
			rawArtist:   "Unknown Artist",
			rawFileName: "Tu Hai Ki Nahi - Ankit Tiwari, Abhendra Kumar Upadhyay.flac",
			wantTitle:   "Tu Hai Ki Nahi",
			wantArtist:  "Ankit Tiwari, Abhendra Kumar Upadhyay",
		},
		{
			name:        "Track with numeric prefix in filename",
			rawTitle:    "01. Tu Hai Ki Nahi",
			rawArtist:   "",
			rawFileName: "01. Tu Hai Ki Nahi - Ankit Tiwari.mp3",
			wantTitle:   "Tu Hai Ki Nahi",
			wantArtist:  "Ankit Tiwari",
		},
		{
			name:        "Filename with underscores",
			rawTitle:    "",
			rawArtist:   "",
			rawFileName: "Tum_Se_Hi_-_Mohit_Chauhan.mp3",
			wantTitle:   "Tum Se Hi",
			wantArtist:  "Mohit Chauhan",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTitle, gotArtist := ExtractTitleAndArtist(tt.rawTitle, tt.rawArtist, tt.rawFileName)
			if gotTitle != tt.wantTitle {
				t.Errorf("ExtractTitleAndArtist title = %q, want %q", gotTitle, tt.wantTitle)
			}
			if gotArtist != tt.wantArtist {
				t.Errorf("ExtractTitleAndArtist artist = %q, want %q", gotArtist, tt.wantArtist)
			}
		})
	}
}
