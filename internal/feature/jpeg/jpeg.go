// Package jpeg provides lightweight JPEG fragment detection and
// adjacency scoring for file carving.
//
// Design:
//   - The original cluster data is NOT retained in memory.
//   - Scan() extracts a compact Feature from each cluster.
//   - IsLikelyJPEG() performs a loose candidate filter.
//   - ScoreNext(a, b) estimates whether cluster b is likely to follow a.
//
// This package does NOT prove that two clusters belong to the same JPEG.
// It only provides evidence for a higher-level carving algorithm.
package jpeg

import "math"

// IsJPEGHeader reports whether data starts with a JPEG SOI followed by
// a marker prefix:
//
//	FF D8 FF
//
// This should only be used for the beginning of a JPEG file.
// For later fragments, the absence of this header is normal.
func IsJPEGHeader(data []byte) bool {
	return len(data) >= 3 &&
		data[0] == 0xFF &&
		data[1] == 0xD8 &&
		data[2] == 0xFF
}

// Feature is a compact summary of one cluster.
//
// It is intentionally small because millions of clusters may need to be
// indexed simultaneously. The original cluster bytes are not retained.
//
// Fields are divided into three groups:
//
//  1. JPEG-likeness:
//     FFCount, StuffedCount, RestartCount, Entropy
//
//  2. JPEG structure:
//     SOIOffset, EOIOffset, RST information
//
//  3. Cluster-boundary information:
//     TailFF, HeadMarker
//
// The offsets are relative to the beginning of this cluster.
type Feature struct {
	// Basic block information.
	BlockSize int

	// JPEG entropy-stream statistics.
	FFCount      int
	StuffedCount int // FF 00
	RestartCount int // FF D0 ~ FF D7
	Entropy      float64

	// JPEG markers found inside the cluster.
	//
	// -1 means the marker does not occur in this cluster.
	SOIOffset int // FF D8
	EOIOffset int // FF D9

	// Restart-marker information.
	//
	// FirstRST / LastRST are restart phases 0~7.
	// The phase sequence normally follows:
	//
	//     RST0 -> RST1 -> ... -> RST7 -> RST0
	//
	FirstRST int8
	LastRST  int8

	// Absolute offsets of the first and last restart markers.
	//
	// These are useful when comparing the end of A with the beginning
	// of B without retaining the cluster itself.
	FirstRSTOffset int
	LastRSTOffset  int

	// Distance between the first two restart markers in this cluster.
	//
	// 0 means fewer than two restart markers were observed.
	RSTGap int

	// Boundary information.
	//
	// TailFF means the final byte is FF. The following byte may be in the
	// next cluster and could therefore complete either FF 00 or FF marker.
	TailFF bool

	// HeadMarker means the cluster starts with FF followed by a non-zero
	// byte. This can indicate that the cluster begins exactly at a JPEG
	// marker boundary.
	HeadMarker bool
}

// Scan extracts all useful JPEG features in one pass.
//
// The caller should normally read one cluster into a temporary buffer:
//
//	data -> Scan(data) -> discard(data)
//
// Therefore the scanner does not require the entire disk to be resident
// in memory.
func Scan(data []byte) Feature {
	f := Feature{
		BlockSize:      len(data),
		SOIOffset:      -1,
		EOIOffset:      -1,
		FirstRST:       -1,
		LastRST:        -1,
		FirstRSTOffset: -1,
		LastRSTOffset:  -1,
	}

	if len(data) == 0 {
		return f
	}

	var freq [256]int

	// Offset of the previous RST marker.
	prevRSTOffset := -1

	for i := 0; i < len(data); i++ {
		b := data[i]
		freq[b]++

		if b != 0xFF {
			continue
		}

		f.FFCount++

		// FF is the final byte of this cluster.
		// The next byte belongs to the next cluster.
		if i+1 >= len(data) {
			continue
		}

		next := data[i+1]

		switch {
		case next == 0x00:
			// JPEG entropy-coded data uses FF 00 to represent a literal FF.
			f.StuffedCount++

		case next == 0xD8:
			// SOI.
			if f.SOIOffset < 0 {
				f.SOIOffset = i
			}

		case next == 0xD9:
			// EOI.
			if f.EOIOffset < 0 {
				f.EOIOffset = i
			}

		case next >= 0xD0 && next <= 0xD7:
			// Restart marker.
			phase := int8(next - 0xD0)

			if f.FirstRST < 0 {
				f.FirstRST = phase
				f.FirstRSTOffset = i
			}

			if prevRSTOffset >= 0 && f.RSTGap == 0 {
				// Keep the first observed RST interval as the
				// representative interval for this cluster.
				f.RSTGap = i - prevRSTOffset
			}

			f.LastRST = phase
			f.LastRSTOffset = i
			f.RestartCount++

			prevRSTOffset = i
		}
	}

	// Boundary state.
	f.TailFF = data[len(data)-1] == 0xFF

	f.HeadMarker =
		data[0] == 0xFF &&
			(len(data) < 2 || data[1] != 0x00)

	f.Entropy = shannonEntropy(freq[:], len(data))

	return f
}

// HasRSTRhythm reports whether the cluster contains enough restart markers
// to provide an internal restart interval estimate.
func HasRSTRhythm(f Feature) bool {
	return f.RestartCount >= 2 && f.RSTGap > 0
}

// IsLikelyJPEG performs a loose candidate filter for non-header fragments.
//
// This is deliberately NOT a JPEG validator.
//
// The main signal is FF 00 stuffing density. Random compressed data can
// occasionally contain FF 00, so this only selects candidates for later
// fragment analysis.
//
// The threshold is intentionally conservative enough to avoid requiring
// restart markers, because many JPEG encoders do not enable restart
// intervals.
func IsLikelyJPEG(f Feature) bool {
	if f.BlockSize == 0 {
		return false
	}

	return f.StuffedCount >= 16 &&
		float64(f.StuffedCount)/float64(f.BlockSize) >= 1.0/2048.0
}

// IsJPEGStart reports whether this cluster contains a JPEG SOI.
//
// This is useful when identifying the first cluster of a recovered file.
func IsJPEGStart(f Feature) bool {
	return f.SOIOffset >= 0
}

// IsJPEGEnd reports whether this cluster contains a JPEG EOI.
//
// This is useful when identifying the final cluster of a recovered file.
func IsJPEGEnd(f Feature) bool {
	return f.EOIOffset >= 0
}

// RSTPhaseNext reports whether b's first restart marker is the phase that
// normally follows a's last restart marker.
//
// JPEG restart markers normally cycle:
//
//	RST0 -> RST1 -> ... -> RST7 -> RST0
//
// The result is false when either cluster has no usable RST information.
func RSTPhaseNext(a, b Feature) bool {
	if a.LastRST < 0 || b.FirstRST < 0 {
		return false
	}

	expected := (a.LastRST + 1) & 7
	return b.FirstRST == expected
}

// RSTGapRatio measures how similar the restart intervals of two clusters are.
//
// Returns:
//
//   - 0 when there is insufficient information.
//   - 1 when the intervals are identical.
//   - values approaching 0 when they are very different.
func RSTGapRatio(a, b Feature) float64 {
	if a.RSTGap <= 0 || b.RSTGap <= 0 {
		return 0
	}

	x := float64(a.RSTGap)
	y := float64(b.RSTGap)

	minGap := math.Min(x, y)
	maxGap := math.Max(x, y)

	if maxGap == 0 {
		return 0
	}

	return minGap / maxGap
}

// StuffedDensity returns the density of FF 00 pairs in this cluster.
func StuffedDensity(f Feature) float64 {
	if f.BlockSize <= 0 {
		return 0
	}

	return float64(f.StuffedCount) / float64(f.BlockSize)
}

// FF density is kept separate from StuffedDensity because it can provide
// a weaker but sometimes useful fallback signal.
func FFDensity(f Feature) float64 {
	if f.BlockSize <= 0 {
		return 0
	}

	return float64(f.FFCount) / float64(f.BlockSize)
}

// AdjacentScore estimates whether b is likely to immediately follow a.
//
// The score is directional:
//
//	ScoreNext(a, b)
//
// means "how plausible is A -> B?"
//
// It deliberately does NOT return a boolean. Fragment recovery is uncertain,
// and several weak pieces of evidence are better combined into a score.
//
// Rough interpretation:
//
//	>= 8   strong candidate
//	5~8    good candidate
//	2~5    possible
//	< 2    weak
//	< 0    strong conflict
//
// These thresholds are starting points only and should be tuned against
// recovered JPEGs.
func ScoreNext(a, b Feature) float64 {
	if a.BlockSize == 0 || b.BlockSize == 0 {
		return -math.MaxFloat64
	}

	score := 0.0

	// ------------------------------------------------------------
	// 1. Hard-ish structural conflicts.
	// ------------------------------------------------------------

	// If A contains EOI, it normally terminates a JPEG.
	// Therefore another ordinary JPEG fragment should not follow it.
	if a.EOIOffset >= 0 {
		return -100
	}

	// If B contains SOI, it is normally the beginning of a JPEG.
	// It therefore should not normally follow an ordinary middle fragment.
	if b.SOIOffset >= 0 {
		score -= 20
	}

	// ------------------------------------------------------------
	// 2. JPEG boundary evidence.
	// ------------------------------------------------------------

	// A ending in FF is interesting because the next byte may be in B.
	//
	// The strongest case is:
	//
	//     A: ... FF
	//     B: 00 ...
	//
	// However, we intentionally do not require B[0] here because Feature
	// does not retain the raw first byte. The actual bytes can be checked
	// later when this edge becomes a high-confidence candidate.
	if a.TailFF {
		score += 0.5
	}

	// B starting at a marker boundary is weak positive evidence that the
	// cluster may begin at a meaningful JPEG boundary.
	if b.HeadMarker {
		score += 0.25
	}

	// ------------------------------------------------------------
	// 3. Restart phase continuity.
	// ------------------------------------------------------------

	if a.LastRST >= 0 && b.FirstRST >= 0 {
		if RSTPhaseNext(a, b) {
			score += 4.0
		} else {
			// Wrong phase is meaningful negative evidence.
			score -= 3.0
		}
	}

	// ------------------------------------------------------------
	// 4. Restart interval similarity.
	// ------------------------------------------------------------

	if a.RSTGap > 0 && b.RSTGap > 0 {
		ratio := RSTGapRatio(a, b)

		switch {
		case ratio >= 0.95:
			score += 3.0
		case ratio >= 0.80:
			score += 2.0
		case ratio >= 0.60:
			score += 0.75
		default:
			score -= 1.5
		}
	}

	// ------------------------------------------------------------
	// 5. JPEG entropy-stream similarity.
	// ------------------------------------------------------------

	// Similar FF 00 density is useful, but deliberately weak.
	// Two unrelated compressed streams can have similar statistics.
	da := StuffedDensity(a)
	db := StuffedDensity(b)

	if da > 0 && db > 0 {
		ratio := math.Min(da, db) / math.Max(da, db)

		switch {
		case ratio >= 0.80:
			score += 1.0
		case ratio >= 0.60:
			score += 0.5
		}
	}

	// Entropy is also only weak evidence.
	//
	// Do not require exact equality: JPEG blocks can have substantially
	// different local image content.
	if a.Entropy > 0 && b.Entropy > 0 {
		diff := math.Abs(a.Entropy - b.Entropy)

		switch {
		case diff < 0.20:
			score += 0.5
		case diff < 0.50:
			score += 0.25
		}
	}

	return score
}

// HasStrongRSTEvidence reports whether A -> B has both restart-phase
// continuity and compatible restart intervals.
func HasStrongRSTEvidence(a, b Feature) bool {
	return a.LastRST >= 0 &&
		b.FirstRST >= 0 &&
		RSTPhaseNext(a, b) &&
		RSTGapRatio(a, b) >= 0.80
}

func shannonEntropy(freq []int, total int) float64 {
	if total == 0 {
		return 0
	}

	entropy := 0.0

	for _, c := range freq {
		if c == 0 {
			continue
		}

		p := float64(c) / float64(total)
		entropy -= p * math.Log2(p)
	}

	return entropy
}
