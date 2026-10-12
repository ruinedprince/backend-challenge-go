package app

import "testing"

func TestRetryVisibilityTimeoutUsesBoundedExponentialBackoff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		receiveCount string
		want         int32
	}{
		{"1", 30},
		{"2", 60},
		{"3", 120},
		{"4", 240},
		{"5", 300},
		{"invalid", 30},
	}
	for _, testCase := range cases {
		t.Run(testCase.receiveCount, func(t *testing.T) {
			if got := retryVisibilityTimeout(testCase.receiveCount); got != testCase.want {
				t.Fatalf("retryVisibilityTimeout(%q) = %d, want %d", testCase.receiveCount, got, testCase.want)
			}
		})
	}
}
