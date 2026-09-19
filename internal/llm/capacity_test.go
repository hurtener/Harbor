package llm

import (
	"errors"
	"math"
	"testing"
)

func TestRequestInputLimit(t *testing.T) {
	t.Parallel()
	positive, zero, negative, ceiling := 200, 0, -1, 950
	tests := []struct {
		name                    string
		window                  int
		reserve                 float64
		explicit, defaultOutput *int
		want, output            int
		invalid                 bool
	}{
		{name: "unknown output stays unknown", window: 1000, reserve: .05, want: 950},
		{name: "explicit output", window: 1000, reserve: .05, explicit: &positive, want: 750, output: 200},
		{name: "profile default", window: 1000, reserve: .05, defaultOutput: &positive, want: 750, output: 200},
		{name: "explicit wins", window: 1000, reserve: .05, explicit: &positive, defaultOutput: &ceiling, want: 750, output: 200},
		{name: "entire remaining window", window: 1000, reserve: .05, explicit: &ceiling, output: 950},
		{name: "fractional margin rounds up", window: 101, reserve: .05, want: 95},
		{name: "max int zero reserve", window: math.MaxInt, want: math.MaxInt},
		{name: "max int near one", window: math.MaxInt, reserve: math.Nextafter(1, 0), want: 1023},
		{name: "zero output", window: 1000, explicit: &zero, invalid: true},
		{name: "negative output", window: 1000, explicit: &negative, invalid: true},
		{name: "invalid default", window: 1000, defaultOutput: &zero, invalid: true},
		{name: "unknown window", invalid: true},
		{name: "negative reserve", window: 1000, reserve: -.1, invalid: true},
		{name: "full reserve", window: 1000, reserve: 1, invalid: true},
		{name: "nan", window: 1000, reserve: math.NaN(), invalid: true},
		{name: "infinity", window: 1000, reserve: math.Inf(1), invalid: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			limit, output, err := requestInputLimit(CompleteRequest{MaxTokens: tc.explicit}, ModelProfile{ContextWindowTokens: tc.window, DefaultMaxTokens: tc.defaultOutput}, tc.reserve)
			if tc.invalid {
				if !errors.Is(err, ErrInvalidConfig) {
					t.Fatalf("error=%v, want invalid config", err)
				}
				return
			}
			if err != nil || limit != tc.want || output != tc.output {
				t.Fatalf("limit/output/error=%d/%d/%v, want %d/%d/nil", limit, output, err, tc.want, tc.output)
			}
		})
	}
}
