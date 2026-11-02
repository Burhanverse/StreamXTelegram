package stream

import (
	"errors"
	"testing"
)

func TestParseRange(t *testing.T) {
	const totalSize = 1000

	tests := []struct {
		name       string
		header     string
		wantStart  int64
		wantEnd    int64
		wantLen    int64
		wantErr    bool
		wantErrIs  error
	}{
		{
			name:      "empty header",
			header:    "",
			wantStart: 0, wantEnd: 0, wantLen: 0,
		},
		{
			name:      "open-ended from 0",
			header:    "bytes=0-",
			wantStart: 0, wantEnd: 999, wantLen: 1000,
		},
		{
			name:      "open-ended from 500",
			header:    "bytes=500-",
			wantStart: 500, wantEnd: 999, wantLen: 500,
		},
		{
			name:      "closed range",
			header:    "bytes=100-200",
			wantStart: 100, wantEnd: 200, wantLen: 101,
		},
		{
			name:      "suffix range",
			header:    "bytes=-200",
			wantStart: 800, wantEnd: 999, wantLen: 200,
		},
		{
			name:      "suffix range exceeding total",
			header:    "bytes=-2000",
			wantStart: 0, wantEnd: 999, wantLen: 1000,
		},
		{
			name:      "closed range exceeding total",
			header:    "bytes=900-2000",
			wantStart: 900, wantEnd: 999, wantLen: 100,
		},
		{
			name:      "range start at total (unsatisfiable)",
			header:    "bytes=1000-",
			wantErr:   true,
			wantErrIs: ErrRangeNotSatisfiable,
		},
		{
			name:      "range start past total (unsatisfiable)",
			header:    "bytes=1500-",
			wantErr:   true,
			wantErrIs: ErrRangeNotSatisfiable,
		},
		{
			name:      "invalid end before start",
			header:    "bytes=500-400",
			wantErr:   true,
		},
		{
			name:      "invalid format",
			header:    "characters=0-100",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRange(tt.header, totalSize)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseRange(%q) expected error, got nil", tt.header)
				}
				if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("ParseRange(%q) expected error %v, got %v", tt.header, tt.wantErrIs, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRange(%q) unexpected error: %v", tt.header, err)
			}
			if tt.header == "" {
				if got != nil {
					t.Fatalf("ParseRange(%q) expected nil, got %v", tt.header, got)
				}
				return
			}
			if got.Start != tt.wantStart || got.End != tt.wantEnd || got.Length != tt.wantLen {
				t.Errorf("ParseRange(%q) = {%d, %d, %d}, want {%d, %d, %d}",
					tt.header, got.Start, got.End, got.Length, tt.wantStart, tt.wantEnd, tt.wantLen)
			}
		})
	}
}

