package stream

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	ErrRangeNotSatisfiable = errors.New("range not satisfiable")
	rangeRegex             = regexp.MustCompile(`^bytes=(\d*)-(\d*)$`)
)

// ByteRange defines inclusive byte start, end, and total length.
type ByteRange struct {
	Start  int64
	End    int64
	Length int64
}

// ParseRange parses an HTTP Range header string into byte boundaries.
func ParseRange(rangeHeader string, totalSize int64) (*ByteRange, error) {
	rangeHeader = strings.TrimSpace(rangeHeader)
	if rangeHeader == "" {
		return nil, nil
	}

	matches := rangeRegex.FindStringSubmatch(rangeHeader)
	if len(matches) != 3 {
		return nil, fmt.Errorf("invalid range format: %s", rangeHeader)
	}

	startStr := strings.TrimSpace(matches[1])
	endStr := strings.TrimSpace(matches[2])

	if startStr == "" && endStr == "" {
		return nil, fmt.Errorf("empty range boundaries")
	}

	var start, end int64

	if startStr == "" {
		// Suffix range: bytes=-500 (last 500 bytes)
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid range offset: %s", endStr)
		}
		start = totalSize - n
		if start < 0 {
			start = 0
		}
		end = totalSize - 1
	} else {
		var err error
		start, err = strconv.ParseInt(startStr, 10, 64)
		if err != nil || start < 0 {
			return nil, fmt.Errorf("invalid range start: %s", startStr)
		}

		if endStr == "" {
			// Open-ended range: bytes=500-
			end = totalSize - 1
		} else {
			// Closed range: bytes=500-1000
			end, err = strconv.ParseInt(endStr, 10, 64)
			if err != nil || end < start {
				return nil, fmt.Errorf("invalid range end: %s", endStr)
			}
			if end >= totalSize {
				end = totalSize - 1
			}
		}
	}

	if totalSize > 0 && start >= totalSize {
		return nil, ErrRangeNotSatisfiable
	}

	return &ByteRange{
		Start:  start,
		End:    end,
		Length: end - start + 1,
	}, nil
}

