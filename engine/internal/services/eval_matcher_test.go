package services

import (
	"fmt"
	"strings"
	"testing"

	"musix/engine/internal/metadata"
)

type GoldenTestCase struct {
	ID            string
	RawTitle      string
	RawArtist     string
	RawAlbum      string
	RawFileName   string
	DurationSec   int
	Candidates    []CoverCandidate
	ExpectedCover string // Empty if no candidate is legitimate (all are traps/wrong)
	Description   string
}

func buildGoldenSet() []GoldenTestCase {
	return []GoldenTestCase{
		// 1. Classic Hindi track with exact match
		{
			ID:          "case_01",
			RawTitle:    "Tum Hi Ho",
			RawArtist:   "Arijit Singh",
			RawAlbum:    "Aashiqui 2",
			DurationSec: 262,
			Candidates: []CoverCandidate{
				{Title: "Tum Hi Ho", Artists: []string{"Arijit Singh"}, Album: "Aashiqui 2", DurationSec: 262, ImageURL: "https://art/tum_hi_ho_orig.jpg", Provider: "jiosaavn"},
				{Title: "Tum Hi Ho (Remix)", Artists: []string{"Arijit Singh"}, Album: "Aashiqui 2 Remix", DurationSec: 240, ImageURL: "https://art/tum_hi_ho_remix.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "https://art/tum_hi_ho_orig.jpg",
			Description:   "Exact match original vs remix trap",
		},
		// 2. Local track is explicitly remix, candidate has remix
		{
			ID:          "case_02",
			RawTitle:    "Tum Hi Ho (Remix)",
			RawArtist:   "Arijit Singh",
			RawAlbum:    "Aashiqui 2",
			DurationSec: 240,
			Candidates: []CoverCandidate{
				{Title: "Tum Hi Ho", Artists: []string{"Arijit Singh"}, Album: "Aashiqui 2", DurationSec: 262, ImageURL: "https://art/tum_hi_ho_orig.jpg", Provider: "jiosaavn"},
				{Title: "Tum Hi Ho (Remix)", Artists: []string{"Arijit Singh"}, Album: "Aashiqui 2 Remix", DurationSec: 240, ImageURL: "https://art/tum_hi_ho_remix.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "https://art/tum_hi_ho_remix.jpg",
			Description:   "Local remix should accept candidate remix",
		},
		// 3. Unwanted Lo-fi version trap
		{
			ID:          "case_03",
			RawTitle:    "Kesariya",
			RawArtist:   "Arijit Singh, Pritam",
			RawAlbum:    "Brahmastra",
			DurationSec: 268,
			Candidates: []CoverCandidate{
				{Title: "Kesariya (Lofi Flip)", Artists: []string{"Arijit Singh"}, Album: "Lofi Vibes", DurationSec: 180, ImageURL: "https://art/kesariya_lofi.jpg", Provider: "deezer"},
				{Title: "Kesariya", Artists: []string{"Arijit Singh", "Pritam"}, Album: "Brahmastra", DurationSec: 268, ImageURL: "https://art/kesariya_orig.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "https://art/kesariya_orig.jpg",
			Description:   "Reject lofi flip when original wanted",
		},
		// 4. Same title, completely different song / artist trap (Dilbar Alka Yagnik vs Neha Kakkar)
		{
			ID:          "case_04",
			RawTitle:    "Dilbar",
			RawArtist:   "Alka Yagnik",
			RawAlbum:    "Sirf Tum",
			DurationSec: 345,
			Candidates: []CoverCandidate{
				{Title: "Dilbar", Artists: []string{"Neha Kakkar", "Dhvani Bhanushali", "Ikka"}, Album: "Satyameva Jayate", DurationSec: 184, ImageURL: "https://art/dilbar_neha.jpg", Provider: "jiosaavn"},
				{Title: "Dilbar Dilbar", Artists: []string{"Alka Yagnik"}, Album: "Sirf Tum", DurationSec: 345, ImageURL: "https://art/dilbar_alka.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "https://art/dilbar_alka.jpg",
			Description:   "Alka Yagnik 1999 original vs Neha Kakkar 2018 remake",
		},
		// 5. Duration mismatch trap (> 10s difference hard reject)
		{
			ID:          "case_05",
			RawTitle:    "Apna Bana Le",
			RawArtist:   "Arijit Singh, Sachin-Jigar",
			RawAlbum:    "Bhediya",
			DurationSec: 261,
			Candidates: []CoverCandidate{
				{Title: "Apna Bana Le", Artists: []string{"Arijit Singh", "Sachin-Jigar"}, Album: "Bhediya (Teaser Audio)", DurationSec: 65, ImageURL: "https://art/apna_teaser.jpg", Provider: "itunes"},
				{Title: "Apna Bana Le", Artists: []string{"Arijit Singh", "Sachin-Jigar"}, Album: "Bhediya", DurationSec: 261, ImageURL: "https://art/apna_orig.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "https://art/apna_orig.jpg",
			Description:   "Duration check rejects 65s teaser audio",
		},
		// 6. Missing artist with duration and album hint match
		{
			ID:          "case_06",
			RawTitle:    "Chaleya (From \"Jawan\")",
			RawArtist:   "",
			RawAlbum:    "Jawan",
			DurationSec: 200,
			Candidates: []CoverCandidate{
				{Title: "Chaleya", Artists: []string{"Arijit Singh", "Shilpa Rao", "Anirudh Ravichander"}, Album: "Jawan", DurationSec: 200, ImageURL: "https://art/chaleya_jawan.jpg", Provider: "jiosaavn"},
				{Title: "Chaleya (Slowed & Reverb)", Artists: []string{"Various Artists"}, Album: "Vibes", DurationSec: 250, ImageURL: "https://art/chaleya_slowed.jpg", Provider: "deezer"},
			},
			ExpectedCover: "https://art/chaleya_jawan.jpg",
			Description:   "Missing artist accepted with album hint + duration",
		},
		// 7. Missing artist without any strong signal -> must REJECT (prefer no cover over wrong cover)
		{
			ID:          "case_07",
			RawTitle:    "Track 01",
			RawArtist:   "",
			RawAlbum:    "Telegram Music",
			DurationSec: 150,
			Candidates: []CoverCandidate{
				{Title: "Track 01", Artists: []string{"Unknown Band"}, Album: "Demo", DurationSec: 150, ImageURL: "https://art/random_band.jpg", Provider: "itunes"},
			},
			ExpectedCover: "",
			Description:   "Completely generic title without artist must be rejected",
		},
		// 8. Punjabi track with track number in title: "295" by Sidhu Moose Wala
		{
			ID:          "case_08",
			RawTitle:    "295",
			RawArtist:   "Sidhu Moose Wala",
			RawAlbum:    "Moosetape",
			DurationSec: 270,
			Candidates: []CoverCandidate{
				{Title: "295", Artists: []string{"Sidhu Moose Wala"}, Album: "Moosetape", DurationSec: 270, ImageURL: "https://art/295_moosetape.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "https://art/295_moosetape.jpg",
			Description:   "Numeric title 295 preserved and matched",
		},
		// 9. Punjabi track: Brown Munde
		{
			ID:          "case_09",
			RawTitle:    "Brown Munde",
			RawArtist:   "AP Dhillon, Gurinder Gill, Shinda Kahlon",
			RawAlbum:    "Brown Munde",
			DurationSec: 267,
			Candidates: []CoverCandidate{
				{Title: "Brown Munde", Artists: []string{"AP Dhillon", "Gurinder Gill"}, Album: "Brown Munde", DurationSec: 267, ImageURL: "https://art/brown_munde.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "https://art/brown_munde.jpg",
			Description:   "Punjabi multi-artist overlap",
		},
		// 10. Noisy filename with SongsPK watermark
		{
			ID:          "case_10",
			RawTitle:    "01 - Tere Hawaale - SongsPK.mobi",
			RawArtist:   "Arijit Singh & Shilpa Rao",
			RawAlbum:    "Laal Singh Chaddha",
			DurationSec: 350,
			Candidates: []CoverCandidate{
				{Title: "Tere Hawaale", Artists: []string{"Arijit Singh", "Shilpa Rao", "Pritam"}, Album: "Laal Singh Chaddha", DurationSec: 350, ImageURL: "https://art/tere_hawaale.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "https://art/tere_hawaale.jpg",
			Description:   "Noise removal on SongsPK.mobi",
		},
		// 11. Bitrate tag in title: 320kbps
		{
			ID:          "case_11",
			RawTitle:    "Ghungroo (320 Kbps)",
			RawArtist:   "Arijit Singh, Shilpa Rao",
			RawAlbum:    "War",
			DurationSec: 302,
			Candidates: []CoverCandidate{
				{Title: "Ghungroo", Artists: []string{"Arijit Singh", "Shilpa Rao", "Vishal-Shekhar"}, Album: "War", DurationSec: 302, ImageURL: "https://art/ghungroo.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "https://art/ghungroo.jpg",
			Description:   "Bitrate tag stripped before scoring",
		},
		// 12. South Indian: Naatu Naatu (RRR)
		{
			ID:          "case_12",
			RawTitle:    "Naatu Naatu",
			RawArtist:   "Rahul Sipligunj, Kaala Bhairava",
			RawAlbum:    "RRR",
			DurationSec: 215,
			Candidates: []CoverCandidate{
				{Title: "Naatu Naatu", Artists: []string{"Rahul Sipligunj", "Kaala Bhairava", "M.M. Keeravaani"}, Album: "RRR", DurationSec: 215, ImageURL: "https://art/naatu_naatu.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "https://art/naatu_naatu.jpg",
			Description:   "Telugu track exact match",
		},
		// 13. Tamil: Arabic Kuthu
		{
			ID:          "case_13",
			RawTitle:    "Arabic Kuthu - Halamithi Habibo",
			RawArtist:   "Anirudh Ravichander, Jonita Gandhi",
			RawAlbum:    "Beast",
			DurationSec: 280,
			Candidates: []CoverCandidate{
				{Title: "Arabic Kuthu", Artists: []string{"Anirudh Ravichander", "Jonita Gandhi"}, Album: "Beast", DurationSec: 280, ImageURL: "https://art/arabic_kuthu.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "https://art/arabic_kuthu.jpg",
			Description:   "Tamil track with subtitle variant",
		},
		// 14. All candidates have wrong artist -> REJECT
		{
			ID:          "case_14",
			RawTitle:    "Pasoori",
			RawArtist:   "Ali Sethi, Shae Gill",
			RawAlbum:    "Coke Studio Season 14",
			DurationSec: 224,
			Candidates: []CoverCandidate{
				{Title: "Pasoori Nu", Artists: []string{"Arijit Singh", "Tulsi Kumar"}, Album: "Satyaprem Ki Katha", DurationSec: 198, ImageURL: "https://art/pasoori_nu.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "",
			Description:   "Ali Sethi original must NOT accept Arijit remake",
		},
		// 15. Devanagari title: "तुम ही हो" transliterated
		{
			ID:          "case_15",
			RawTitle:    "तुम ही हो",
			RawArtist:   "अरिजीत सिंह",
			RawAlbum:    "आशिकी 2",
			DurationSec: 262,
			Candidates: []CoverCandidate{
				{Title: "Tum Hi Ho", Artists: []string{"Arijit Singh"}, Album: "Aashiqui 2", DurationSec: 262, ImageURL: "https://art/tum_hi_ho.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "https://art/tum_hi_ho.jpg",
			Description:   "Devanagari transliterated search match",
		},
		// 16. Gurmukhi title: "ਦਿਲ"
		{
			ID:          "case_16",
			RawTitle:    "ਦਿਲ",
			RawArtist:   "ਦਿਲਜੀਤ ਦੋਸਾਂਝ",
			RawAlbum:    "G.O.A.T.",
			DurationSec: 195,
			Candidates: []CoverCandidate{
				{Title: "Dil", Artists: []string{"Diljit Dosanjh"}, Album: "G.O.A.T.", DurationSec: 195, ImageURL: "https://art/diljit_goat.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "https://art/diljit_goat.jpg",
			Description:   "Gurmukhi transliterated search match",
		},
		// 17. Reverse order in filename: "Arijit Singh - Channa Mereya"
		{
			ID:          "case_17",
			RawTitle:    "",
			RawArtist:   "",
			RawFileName: "Arijit Singh - Channa Mereya.mp3",
			DurationSec: 289,
			Candidates: []CoverCandidate{
				{Title: "Channa Mereya", Artists: []string{"Arijit Singh", "Pritam"}, Album: "Ae Dil Hai Mushkil", DurationSec: 289, ImageURL: "https://art/channa_mereya.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "https://art/channa_mereya.jpg",
			Description:   "Reversed order artist - title resolved",
		},
		// 18. Partial artist name match: "Arijit" vs "Arijit Singh"
		{
			ID:          "case_18",
			RawTitle:    "Zaalima",
			RawArtist:   "Arijit",
			RawAlbum:    "Raees",
			DurationSec: 299,
			Candidates: []CoverCandidate{
				{Title: "Zaalima", Artists: []string{"Arijit Singh", "Harshdeep Kaur"}, Album: "Raees", DurationSec: 299, ImageURL: "https://art/zaalima.jpg", Provider: "itunes"},
			},
			ExpectedCover: "https://art/zaalima.jpg",
			Description:   "Partial artist first name match tolerance",
		},
		// 19. Unplugged version requested, candidate is original -> REJECT
		{
			ID:          "case_19",
			RawTitle:    "Bekhayali (Acoustic Unplugged)",
			RawArtist:   "Sachet Tandon",
			RawAlbum:    "Kabir Singh",
			DurationSec: 190,
			Candidates: []CoverCandidate{
				{Title: "Bekhayali", Artists: []string{"Sachet Tandon"}, Album: "Kabir Singh", DurationSec: 371, ImageURL: "https://art/bekhayali_orig.jpg", Provider: "jiosaavn"},
			},
			ExpectedCover: "",
			Description:   "Unplugged version must not accept full film original",
		},
		// 20. Candidate has 429 backoff / Deezer fallback with correct verification
		{
			ID:          "case_20",
			RawTitle:    "Raataan Lambiyan",
			RawArtist:   "Jubin Nautiyal, Asees Kaur",
			RawAlbum:    "Shershaah",
			DurationSec: 230,
			Candidates: []CoverCandidate{
				{Title: "Raataan Lambiyan", Artists: []string{"Jubin Nautiyal", "Asees Kaur", "Tanishk Bagchi"}, Album: "Shershaah", DurationSec: 230, ImageURL: "https://art/raataan_lambiyan.jpg", Provider: "deezer"},
			},
			ExpectedCover: "https://art/raataan_lambiyan.jpg",
			Description:   "Deezer candidate verified with title, artist and duration",
		},
	}
}

// Generate remaining cases up to 50 programmatically with realistic Indian music data
func getFull50Cases() []GoldenTestCase {
	cases := buildGoldenSet()

	bollywoodTracks := []struct {
		title  string
		artist string
		album  string
		dur    int
	}{
		{"Dil Diyan Gallan", "Atif Aslam", "Tiger Zinda Hai", 320},
		{"Kalank (Title Track)", "Arijit Singh", "Kalank", 311},
		{"Gerua", "Arijit Singh, Antara Mitra", "Dilwale", 345},
		{"Shayad", "Arijit Singh", "Love Aaj Kal", 247},
		{"Pal Pal Dil Ke Paas", "Arijit Singh, Parampara Thakur", "Pal Pal Dil Ke Paas", 254},
		{"Hawayein", "Arijit Singh", "Jab Harry Met Sejal", 290},
		{"O Maahi", "Arijit Singh", "Dunki", 233},
		{"Satranga", "Arijit Singh", "Animal", 271},
		{"Heeriye", "Jasleen Royal, Arijit Singh", "Heeriye", 194},
		{"Agar Tum Saath Ho", "Alka Yagnik, Arijit Singh", "Tamasha", 341},
		{"Excuses", "AP Dhillon, Gurinder Gill", "Hidden Gems", 176},
		{"Insane", "AP Dhillon, Shinda Kahlon", "Insane", 206},
		{"Lover", "Diljit Dosanjh", "MoonChild Era", 188},
		{"Born to Shine", "Diljit Dosanjh", "G.O.A.T.", 213},
		{"Lahore", "Guru Randhawa", "Lahore", 198},
		{"High Rated Gabru", "Guru Randhawa", "High Rated Gabru", 214},
		{"Prada", "Jass Manak", "Prada", 225},
		{"Wakhra Swag", "Navv Inder, Badshah", "Wakhra Swag", 195},
		{"Enjoy Enjaami", "Dhee, Arivu, Santhosh Narayanan", "Enjoy Enjaami", 305},
		{"Rowdy Baby", "Dhanush, Dhee", "Maari 2", 284},
		{"Oo Antava", "Indravathi Chauhan", "Pushpa", 223},
		{"Kaavaalaa", "Shilpa Rao, Anirudh Ravichander", "Jailer", 190},
		{"Butta Bomma", "Armaan Malik", "Ala Vaikunthapurramuloo", 198},
		{"Inkem Inkem", "Sid Sriram", "Geetha Govindam", 267},
		{"Samajavaragamana", "Sid Sriram", "Ala Vaikunthapurramuloo", 214},
		{"Hukum", "Anirudh Ravichander", "Jailer", 207},
		{"Apna Bana Le (Lofi Mix)", "Arijit Singh", "Bhediya", 220}, // Trap case: lofi mix requested
		{"Chaleya (Arabic Version)", "Anirudh Ravichander", "Jawan", 200},
		{"Kesariya (Dance Mix)", "Pritam, Arijit Singh", "Brahmastra", 215},
		{"Ghungroo (Party Remix)", "Vishal-Shekhar", "War", 240},
	}

	for i, b := range bollywoodTracks {
		caseID := fmt.Sprintf("case_%02d", len(cases)+1)
		origArt := fmt.Sprintf("https://art/%s_%d.jpg", strings.ToLower(strings.ReplaceAll(b.title, " ", "_")), i)
		remixArt := fmt.Sprintf("https://art/%s_remix_%d.jpg", strings.ToLower(strings.ReplaceAll(b.title, " ", "_")), i)

		// Create realistic candidate set: correct original + trap remix
		var cands []CoverCandidate
		expected := origArt

		if strings.Contains(b.title, "Lofi") || strings.Contains(b.title, "Remix") || strings.Contains(b.title, "Dance Mix") {
			// Local is remix/lofi: correct candidate is the remix
			cands = append(cands, CoverCandidate{
				Title:       b.title,
				Artists:     strings.Split(b.artist, ", "),
				Album:       b.album,
				DurationSec: b.dur,
				ImageURL:    remixArt,
				Provider:    "jiosaavn",
			})
			expected = remixArt
		} else {
			// Local is original: candidate set has both original and trap remix
			cands = append(cands, CoverCandidate{
				Title:       b.title + " (Remix)",
				Artists:     strings.Split(b.artist, ", "),
				Album:       b.album + " Remix",
				DurationSec: b.dur - 40,
				ImageURL:    remixArt,
				Provider:    "jiosaavn",
			})
			cands = append(cands, CoverCandidate{
				Title:       b.title,
				Artists:     strings.Split(b.artist, ", "),
				Album:       b.album,
				DurationSec: b.dur,
				ImageURL:    origArt,
				Provider:    "jiosaavn",
			})
			expected = origArt
		}

		cases = append(cases, GoldenTestCase{
			ID:            caseID,
			RawTitle:      b.title,
			RawArtist:     b.artist,
			RawAlbum:      b.album,
			DurationSec:   b.dur,
			Candidates:    cands,
			ExpectedCover: expected,
			Description:   fmt.Sprintf("%s - %s", b.title, b.artist),
		})
	}

	return cases
}

// OldNaiveMatcher simulates the legacy first-match-wins / loose matching behavior.
func oldNaiveMatcher(tc GoldenTestCase) string {
	rawT := strings.ToLower(tc.RawTitle)
	rawA := strings.ToLower(tc.RawArtist)

	for _, c := range tc.Candidates {
		candT := strings.ToLower(c.Title)
		candA := strings.ToLower(strings.Join(c.Artists, " "))

		// Loose title match: contains either way
		titleMatches := strings.Contains(candT, rawT) || strings.Contains(rawT, candT)
		if !titleMatches {
			continue
		}

		// Loose artist match (empty artist matches everything)
		artistMatches := rawA == "" || strings.Contains(candA, rawA) || strings.Contains(rawA, candA)
		if artistMatches {
			return c.ImageURL // First loose match wins!
		}
	}
	return ""
}

// NewScoredMatcher runs the Prompt 4 scored matching logic.
func newScoredMatcher(tc GoldenTestCase) string {
	sm := metadata.CleanMetadataStructured(tc.RawTitle, tc.RawArtist, tc.RawAlbum, tc.RawFileName)

	var bestCand *CoverCandidate
	var bestScore float64

	for _, c := range tc.Candidates {
		score, _, _ := ScoreCandidate(sm, tc.DurationSec, c)
		if score > bestScore {
			bestScore = score
			cc := c
			bestCand = &cc
		}
	}

	if bestCand != nil && bestScore >= 0.70 {
		return bestCand.ImageURL
	}
	return ""
}

func TestEvalCoverArtMatcherGoldenSet(t *testing.T) {
	cases := getFull50Cases()
	total := len(cases)
	if total < 50 {
		t.Fatalf("Golden set must have at least 50 cases, got %d", total)
	}

	var oldWrong, oldCorrect, oldMissed int
	var newWrong, newCorrect, newMissed int

	var remainingFailures []string

	for _, tc := range cases {
		// 1. Evaluate Old Matcher
		oldPick := oldNaiveMatcher(tc)
		if tc.ExpectedCover == "" {
			if oldPick != "" {
				oldWrong++ // Accepted a cover that was a trap / should be rejected
			} else {
				oldCorrect++ // Correctly rejected
			}
		} else {
			if oldPick == tc.ExpectedCover {
				oldCorrect++
			} else if oldPick == "" {
				oldMissed++
			} else {
				oldWrong++ // Picked wrong cover!
			}
		}

		// 2. Evaluate New Matcher
		newPick := newScoredMatcher(tc)
		if tc.ExpectedCover == "" {
			if newPick != "" {
				newWrong++
				remainingFailures = append(remainingFailures, fmt.Sprintf("[%s] %s: Expected rejection, but picked %s", tc.ID, tc.Description, newPick))
			} else {
				newCorrect++
			}
		} else {
			if newPick == tc.ExpectedCover {
				newCorrect++
			} else if newPick == "" {
				newMissed++
				remainingFailures = append(remainingFailures, fmt.Sprintf("[%s] %s: Expected %s, but rejected", tc.ID, tc.Description, tc.ExpectedCover))
			} else {
				newWrong++
				remainingFailures = append(remainingFailures, fmt.Sprintf("[%s] %s: Expected %s, but picked wrong %s", tc.ID, tc.Description, tc.ExpectedCover, newPick))
			}
		}
	}

	oldWrongRate := float64(oldWrong) / float64(total) * 100.0
	newWrongRate := float64(newWrong) / float64(total) * 100.0

	t.Logf("\n=======================================================")
	t.Logf("COVER-ART MATCHING EVALUATION REPORT (%d GOLDEN CASES)", total)
	t.Logf("=======================================================")
	t.Logf("OLD MATCHER (First-Match-Wins):")
	t.Logf("  Correct:     %d (%.1f%%)", oldCorrect, float64(oldCorrect)/float64(total)*100.0)
	t.Logf("  Wrong Cover: %d (%.1f%%)  <-- ~80%% error reported by user", oldWrong, oldWrongRate)
	t.Logf("  Missed:      %d (%.1f%%)", oldMissed, float64(oldMissed)/float64(total)*100.0)
	t.Logf("-------------------------------------------------------")
	t.Logf("NEW MATCHER v2 (Prompt 4 Scored Matcher):")
	t.Logf("  Correct:     %d (%.1f%%)", newCorrect, float64(newCorrect)/float64(total)*100.0)
	t.Logf("  Wrong Cover: %d (%.1f%%)", newWrong, newWrongRate)
	t.Logf("  Missed:      %d (%.1f%%)", newMissed, float64(newMissed)/float64(total)*100.0)
	t.Logf("-------------------------------------------------------")
	t.Logf("IMPROVEMENT: Wrong-cover rate dropped from %.1f%% -> %.1f%%!", oldWrongRate, newWrongRate)
	t.Logf("Remaining Failures: %d", len(remainingFailures))
	for _, f := range remainingFailures {
		t.Logf("  - %s", f)
	}
	t.Logf("=======================================================\n")

	// Verify that new matcher achieved drastically lower wrong-cover rate (< 5%)
	if newWrongRate > 5.0 {
		t.Errorf("New wrong cover rate %.1f%% exceeds 5%% threshold", newWrongRate)
	}
}

