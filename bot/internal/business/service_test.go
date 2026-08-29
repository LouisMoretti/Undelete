package business

import "testing"

func TestOwnerAllowed(t *testing.T) {
	tests := []struct {
		name        string
		ownerFilter int64
		ownerID     int64
		want        bool
	}{
		{name: "filtre désactivé", ownerFilter: 0, ownerID: 42, want: true},
		{name: "owner autorisé", ownerFilter: 42, ownerID: 42, want: true},
		{name: "owner étranger", ownerFilter: 42, ownerID: 99, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := &Service{ownerFilter: tt.ownerFilter}
			if got := service.ownerAllowed(tt.ownerID); got != tt.want {
				t.Fatalf("ownerAllowed(%d) = %t, attendu %t", tt.ownerID, got, tt.want)
			}
		})
	}
}
