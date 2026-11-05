package metadata

import (
	"testing"
)

func TestPrompt3_IndianMusicMetadataNormalization40Cases(t *testing.T) {
	tests := []struct {
		name            string
		rawTitle        string
		rawArtist       string
		rawFileName     string
		wantTitle       string
		wantArtists     []string
		wantAlbumHint   string
		wantVersionTag  string
		wantScript      string
	}{
		// 1-5: Hindi film songs with site watermarks
		{
			name:        "1. SongsPK watermark",
			rawTitle:    "Tum Hi Ho [SongsPK.cc]",
			rawArtist:   "Arijit Singh",
			rawFileName: "01 - Tum Hi Ho - Arijit Singh [SongsPK].mp3",
			wantTitle:   "Tum Hi Ho",
			wantArtists: []string{"Arijit Singh"},
		},
		{
			name:        "2. PagalWorld watermark with bitrate",
			rawTitle:    "Kesariya (PagalWorld.com) 320kbps",
			rawArtist:   "Pritam, Arijit Singh",
			rawFileName: "Kesariya - Brahmastra [PagalWorld].mp3",
			wantTitle:   "Kesariya",
			wantArtists: []string{"Pritam", "Arijit Singh"},
		},
		{
			name:        "3. DJMaza tag",
			rawTitle:    "Ghungroo [DJMaza.Info]",
			rawArtist:   "Vishal-Shekhar, Arijit Singh, Shilpa Rao",
			rawFileName: "Ghungroo - War (DJMaza).mp3",
			wantTitle:   "Ghungroo",
			wantArtists: []string{"Vishal-Shekhar", "Arijit Singh", "Shilpa Rao"},
		},
		{
			name:        "4. Mr-Jatt tag",
			rawTitle:    "Apna Bana Le [Mr-Jatt]",
			rawArtist:   "Arijit Singh, Sachin-Jigar",
			rawFileName: "Apna Bana Le - Bhediya [Mr-Jatt.com].mp3",
			wantTitle:   "Apna Bana Le",
			wantArtists: []string{"Arijit Singh", "Sachin-Jigar"},
		},
		{
			name:        "5. PagalFree tag",
			rawTitle:    "Tere Hawaale - PagalFree",
			rawArtist:   "Arijit Singh & Shilpa Rao",
			rawFileName: "Tere Hawaale - Laal Singh Chaddha.mp3",
			wantTitle:   "Tere Hawaale",
			wantArtists: []string{"Arijit Singh", "Shilpa Rao"},
		},

		// 6-10: Bitrate variants
		{
			name:        "6. 128 kbps variant",
			rawTitle:    "Chaleya 128kbps",
			rawArtist:   "Anirudh Ravichander, Arijit Singh",
			rawFileName: "Chaleya - Jawan 128 kbps.mp3",
			wantTitle:   "Chaleya",
			wantArtists: []string{"Anirudh Ravichander", "Arijit Singh"},
		},
		{
			name:        "7. 192Kbps variant",
			rawTitle:    "O Maahi [192Kbps]",
			rawArtist:   "Pritam, Arijit Singh",
			rawFileName: "O Maahi - Dunki 192kbps.mp3",
			wantTitle:   "O Maahi",
			wantArtists: []string{"Pritam", "Arijit Singh"},
		},
		{
			name:        "8. Dash 320 at end",
			rawTitle:    "Lutt Putt Gaya - 320",
			rawArtist:   "Pritam, Arijit Singh",
			rawFileName: "Lutt Putt Gaya - Dunki - 320.mp3",
			wantTitle:   "Lutt Putt Gaya",
			wantArtists: []string{"Pritam", "Arijit Singh"},
		},
		{
			name:        "9. 256 kbps in brackets",
			rawTitle:    "Heeriye (256kbps)",
			rawArtist:   "Jasleen Royal ft. Arijit Singh",
			rawFileName: "Heeriye (256kbps).mp3",
			wantTitle:   "Heeriye",
			wantArtists: []string{"Jasleen Royal", "Arijit Singh"},
		},
		{
			name:        "10. 320 in filename only",
			rawTitle:    "Satranga",
			rawArtist:   "Arijit Singh, Shreyas Puranik",
			rawFileName: "Satranga_Animal_320kbps.mp3",
			wantTitle:   "Satranga",
			wantArtists: []string{"Arijit Singh", "Shreyas Puranik"},
		},

		// 11-15: Punjabi songs
		{
			name:        "11. Sidhu Moose Wala Punjabi track",
			rawTitle:    "295 [DjPunjab]",
			rawArtist:   "Sidhu Moose Wala",
			rawFileName: "Sidhu Moose Wala - 295.mp3",
			wantTitle:   "295",
			wantArtists: []string{"Sidhu Moose Wala"},
		},
		{
			name:        "12. Karan Aujla Punjabi track",
			rawTitle:    "Softly",
			rawArtist:   "Karan Aujla, Ikky",
			rawFileName: "Softly - Karan Aujla (Mr-Jatt).mp3",
			wantTitle:   "Softly",
			wantArtists: []string{"Karan Aujla", "Ikky"},
		},
		{
			name:        "13. Diljit Dosanjh Punjabi track",
			rawTitle:    "Lover [Pendujatt]",
			rawArtist:   "Diljit Dosanjh",
			rawFileName: "Diljit Dosanjh - Lover.mp3",
			wantTitle:   "Lover",
			wantArtists: []string{"Diljit Dosanjh"},
		},
		{
			name:        "14. AP Dhillon Punjabi track",
			rawTitle:    "With You",
			rawArtist:   "AP Dhillon",
			rawFileName: "AP Dhillon - With You.mp3",
			wantTitle:   "With You",
			wantArtists: []string{"AP Dhillon"},
		},
		{
			name:        "15. Gurmukhi Punjabi title",
			rawTitle:    "ਮਾਝਾ ਬਲਾਕ",
			rawArtist:   "Sidhu Moose Wala",
			rawFileName: "Sidhu Moose Wala - Majha Block.mp3",
			wantTitle:   "ਮਾਝਾ ਬਲਾਕ",
			wantArtists: []string{"Sidhu Moose Wala"},
			wantScript:  "gurmukhi",
		},

		// 16-20: Tamil / Telugu music
		{
			name:        "16. Masstamilan Tamil track",
			rawTitle:    "Naa Ready [Masstamilan]",
			rawArtist:   "Anirudh Ravichander, Vijay, Asal Kolaar",
			rawFileName: "Naa Ready - Leo [Masstamilan.dev].mp3",
			wantTitle:   "Naa Ready",
			wantArtists: []string{"Anirudh Ravichander", "Vijay", "Asal Kolaar"},
		},
		{
			name:        "17. NaaSongs Telugu track",
			rawTitle:    "Kurchi Madathapetti [NaaSongs]",
			rawArtist:   "Thaman S, Sahithi Chaganti",
			rawFileName: "Kurchi Madathapetti - Guntur Kaaram (NaaSongs).mp3",
			wantTitle:   "Kurchi Madathapetti",
			wantArtists: []string{"Thaman S", "Sahithi Chaganti"},
		},
		{
			name:        "18. Kuttyweb Tamil track",
			rawTitle:    "Arabic Kuthu - Kuttyweb",
			rawArtist:   "Anirudh Ravichander, Jonita Gandhi",
			rawFileName: "Arabic Kuthu - Beast.mp3",
			wantTitle:   "Arabic Kuthu",
			wantArtists: []string{"Anirudh Ravichander", "Jonita Gandhi"},
		},
		{
			name:        "19. Sensongs Telugu track",
			rawTitle:    "Oo Antava [Sensongs]",
			rawArtist:   "Devi Sri Prasad, Indravathi Chauhan",
			rawFileName: "Oo Antava - Pushpa.mp3",
			wantTitle:   "Oo Antava",
			wantArtists: []string{"Devi Sri Prasad", "Indravathi Chauhan"},
		},
		{
			name:        "20. Tamil script title",
			rawTitle:    "ஹுகும்",
			rawArtist:   "Anirudh Ravichander",
			rawFileName: "Hukum - Jailer.mp3",
			wantTitle:   "ஹுகும்",
			wantArtists: []string{"Anirudh Ravichander"},
			wantScript:  "tamil",
		},

		// 21-25: Movie hint extraction (From "Movie")
		{
			name:          "21. From in title parentheses",
			rawTitle:      "Zaalima (From \"Raees\")",
			rawArtist:     "Arijit Singh, Harshdeep Kaur",
			rawFileName:   "Zaalima.mp3",
			wantTitle:     "Zaalima",
			wantArtists:   []string{"Arijit Singh", "Harshdeep Kaur"},
			wantAlbumHint: "Raees",
		},
		{
			name:          "22. From in title brackets",
			rawTitle:      "Raataan Lambiyan [From \"Shershaah\"]",
			rawArtist:     "Tanishk Bagchi, Jubin Nautiyal, Asees Kaur",
			rawFileName:   "Raataan Lambiyan.mp3",
			wantTitle:     "Raataan Lambiyan",
			wantArtists:   []string{"Tanishk Bagchi", "Jubin Nautiyal", "Asees Kaur"},
			wantAlbumHint: "Shershaah",
		},
		{
			name:          "23. From The Movie in title",
			rawTitle:      "Chaleya (From The Movie \"Jawan\")",
			rawArtist:     "Anirudh Ravichander, Arijit Singh, Shilpa Rao",
			rawFileName:   "Chaleya.mp3",
			wantTitle:     "Chaleya",
			wantArtists:   []string{"Anirudh Ravichander", "Arijit Singh", "Shilpa Rao"},
			wantAlbumHint: "Jawan",
		},
		{
			name:          "24. Film hint in filename",
			rawTitle:      "Namo Namo",
			rawArtist:     "Amit Trivedi",
			rawFileName:   "Namo Namo (From \"Kedarnath\").mp3",
			wantTitle:     "Namo Namo",
			wantArtists:   []string{"Amit Trivedi"},
			wantAlbumHint: "Kedarnath",
		},
		{
			name:          "25. From with single quotes",
			rawTitle:      "Gerua (From 'Dilwale')",
			rawArtist:     "Pritam, Arijit Singh, Antara Mitra",
			rawFileName:   "Gerua.mp3",
			wantTitle:     "Gerua",
			wantArtists:   []string{"Pritam", "Arijit Singh", "Antara Mitra"},
			wantAlbumHint: "Dilwale",
		},

		// 26-30: Version tags (remix, lofi, slowed, unplugged)
		{
			name:           "26. Remix tag",
			rawTitle:       "Tum Hi Ho (Remix)",
			rawArtist:      "DJ Chetas, Arijit Singh",
			rawFileName:    "Tum Hi Ho Remix.mp3",
			wantTitle:      "Tum Hi Ho",
			wantArtists:    []string{"DJ Chetas", "Arijit Singh"},
			wantVersionTag: "remix",
		},
		{
			name:           "27. Lofi tag",
			rawTitle:       "Kesariya (Lofi Mix)",
			rawArtist:      "Arijit Singh",
			rawFileName:    "Kesariya Lofi.mp3",
			wantTitle:      "Kesariya",
			wantArtists:    []string{"Arijit Singh"},
			wantVersionTag: "lofi",
		},
		{
			name:           "28. Slowed and Reverb tag",
			rawTitle:       "Pasoori (Slowed + Reverb)",
			rawArtist:      "Ali Sethi, Shae Gill",
			rawFileName:    "Pasoori Slowed Reverb.mp3",
			wantTitle:      "Pasoori",
			wantArtists:    []string{"Ali Sethi", "Shae Gill"},
			wantVersionTag: "slowed",
		},
		{
			name:           "29. Unplugged version",
			rawTitle:       "Samjhawan (Unplugged)",
			rawArtist:      "Alia Bhatt",
			rawFileName:    "Samjhawan Unplugged.mp3",
			wantTitle:      "Samjhawan",
			wantArtists:    []string{"Alia Bhatt"},
			wantVersionTag: "unplugged",
		},
		{
			name:           "30. Acoustic cover",
			rawTitle:       "Ilahi (Acoustic Cover)",
			rawArtist:      "Arijit Singh",
			rawFileName:    "Ilahi Acoustic Cover.mp3",
			wantTitle:      "Ilahi",
			wantArtists:    []string{"Arijit Singh"},
			wantVersionTag: "cover",
		},

		// 31-35: Reversed filename order (Artist - Title vs Title - Artist)
		{
			name:        "31. Reversed: Arijit Singh - Title",
			rawTitle:    "",
			rawArtist:   "",
			rawFileName: "Arijit Singh - Tum Hi Ho.mp3",
			wantTitle:   "Tum Hi Ho",
			wantArtists: []string{"Arijit Singh"},
		},
		{
			name:        "32. Reversed: Diljit Dosanjh - Title",
			rawTitle:    "",
			rawArtist:   "",
			rawFileName: "Diljit Dosanjh - Lover.mp3",
			wantTitle:   "Lover",
			wantArtists: []string{"Diljit Dosanjh"},
		},
		{
			name:        "33. Standard: Title - Shreya Ghoshal",
			rawTitle:    "",
			rawArtist:   "",
			rawFileName: "Sunn Raha Hai - Shreya Ghoshal.mp3",
			wantTitle:   "Sunn Raha Hai",
			wantArtists: []string{"Shreya Ghoshal"},
		},
		{
			name:        "34. Reversed: Atif Aslam - Title",
			rawTitle:    "",
			rawArtist:   "",
			rawFileName: "Atif Aslam - Jeene Laga Hoon.mp3",
			wantTitle:   "Jeene Laga Hoon",
			wantArtists: []string{"Atif Aslam"},
		},
		{
			name:        "35. Reversed: Sidhu Moose Wala - Title",
			rawTitle:    "",
			rawArtist:   "",
			rawFileName: "Sidhu Moose Wala - So High.mp3",
			wantTitle:   "So High",
			wantArtists: []string{"Sidhu Moose Wala"},
		},

		// 36-40: Devanagari script transliteration and zero-width characters
		{
			name:        "36. Devanagari Title: तुम ही हो",
			rawTitle:    "तुम ही हो",
			rawArtist:   "अरिजीत सिंह",
			rawFileName: "Tum Hi Ho.mp3",
			wantTitle:   "तुम ही हो",
			wantArtists: []string{"अरिजीत सिंह"},
			wantScript:  "devanagari",
		},
		{
			name:        "37. Devanagari Title: केसरिया",
			rawTitle:    "केसरिया",
			rawArtist:   "Arijit Singh",
			rawFileName: "Kesariya.mp3",
			wantTitle:   "केसरिया",
			wantArtists: []string{"Arijit Singh"},
			wantScript:  "devanagari",
		},
		{
			name:        "38. Zero-width character stripping",
			rawTitle:    "Pehle\u200B Bhi\u200C Main\u200D",
			rawArtist:   "Vishal Mishra",
			rawFileName: "Pehle Bhi Main - Animal.mp3",
			wantTitle:   "Pehle Bhi Main",
			wantArtists: []string{"Vishal Mishra"},
		},
		{
			name:        "39. Full Video Song descriptor removal",
			rawTitle:    "Lungi Dance (Full Video Song)",
			rawArtist:   "Yo Yo Honey Singh",
			rawFileName: "Lungi Dance [SongsPK].mp3",
			wantTitle:   "Lungi Dance",
			wantArtists: []string{"Yo Yo Honey Singh"},
		},
		{
			name:        "40. Unknown Artist placeholder dropped",
			rawTitle:    "Instrumental Beat",
			rawArtist:   "Unknown Artist",
			rawFileName: "beat.mp3",
			wantTitle:   "Instrumental Beat",
			wantArtists: nil, // empty slice, never placeholder
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sm := ParseStructuredMetadata(tt.rawTitle, tt.rawArtist, tt.rawFileName)

			if sm.Title != tt.wantTitle {
				t.Errorf("Title = %q, want %q", sm.Title, tt.wantTitle)
			}

			if len(tt.wantArtists) == 0 {
				if len(sm.Artists) != 0 {
					t.Errorf("Expected empty artists, got %v", sm.Artists)
				}
			} else {
				if len(sm.Artists) != len(tt.wantArtists) {
					t.Errorf("Artists length = %d (%v), want %d (%v)", len(sm.Artists), sm.Artists, len(tt.wantArtists), tt.wantArtists)
				} else {
					for i, a := range tt.wantArtists {
						if sm.Artists[i] != a {
							t.Errorf("Artist[%d] = %q, want %q", i, sm.Artists[i], a)
						}
					}
				}
			}

			if tt.wantAlbumHint != "" && sm.AlbumHint != tt.wantAlbumHint {
				t.Errorf("AlbumHint = %q, want %q", sm.AlbumHint, tt.wantAlbumHint)
			}

			if tt.wantVersionTag != "" {
				found := false
				for _, vt := range sm.VersionTags {
					if vt == tt.wantVersionTag {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected version tag %q in %v", tt.wantVersionTag, sm.VersionTags)
				}
			}

			if tt.wantScript != "" && sm.LanguageScript != tt.wantScript {
				t.Errorf("LanguageScript = %q, want %q", sm.LanguageScript, tt.wantScript)
			}

			// Ensure Devanagari titles transliterate to non-empty Latin SearchTitle
			if sm.LanguageScript == "devanagari" && sm.SearchTitle == sm.Title {
				t.Errorf("Devanagari title was not transliterated: %q", sm.SearchTitle)
			}
		})
	}
}

