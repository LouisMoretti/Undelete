package business

import "testing"

func TestOwnerAllowed(t *testing.T) {
	tests := []struct {
		name    string
		allowed []int64
		ownerID int64
		want    bool
	}{
		{name: "open onboarding admits everyone", allowed: nil, ownerID: 42, want: true},
		{name: "empty allowlist admits everyone", allowed: []int64{}, ownerID: 42, want: true},
		{name: "listed owner admitted", allowed: []int64{42}, ownerID: 42, want: true},
		{name: "unlisted owner refused", allowed: []int64{42}, ownerID: 99, want: false},
		{name: "several owners admitted", allowed: []int64{42, 99, 7}, ownerID: 99, want: true},
		{name: "unlisted owner refused against a list", allowed: []int64{42, 99, 7}, ownerID: 8, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := NewService(nil, nil, nil, tt.allowed, testLogger())
			if got := service.ownerAllowed(tt.ownerID); got != tt.want {
				t.Fatalf("ownerAllowed(%d) = %t, expected %t", tt.ownerID, got, tt.want)
			}
		})
	}
}
