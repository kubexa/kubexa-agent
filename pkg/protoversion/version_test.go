package protoversion

import "testing"

func TestValidate(t *testing.T) {
	t.Parallel()

	if err := Validate("v1"); err != nil {
		t.Fatalf("Validate(v1) = %v", err)
	}
	if err := Validate("V1"); err != nil {
		t.Fatalf("Validate(V1) = %v", err)
	}
	if err := Validate("v2"); err == nil {
		t.Fatal("expected error for unsupported v2")
	}
	if err := Validate(""); err == nil {
		t.Fatal("expected error for empty version")
	}
}

func TestNegotiate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		preferred string
		remote    []string
		local     []string
		want      string
		wantErr   bool
	}{
		{
			name:      "preferred match",
			preferred: "v1",
			remote:    []string{"v1"},
			local:     []string{"v1"},
			want:      "v1",
		},
		{
			name:      "legacy empty remote uses v1",
			preferred: "v1",
			remote:    nil,
			local:     []string{"v1"},
			want:      "v1",
		},
		{
			name:      "pick first mutual",
			preferred: "v2",
			remote:    []string{"v2", "v1"},
			local:     []string{"v1"},
			want:      "v1",
		},
		{
			name:      "no overlap",
			preferred: "v2",
			remote:    []string{"v2"},
			local:     []string{"v1"},
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Negotiate(tt.preferred, tt.remote, tt.local)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Negotiate() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("Negotiate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateGatewayResponse(t *testing.T) {
	t.Parallel()

	if err := ValidateGatewayResponse("", nil); err != nil {
		t.Fatalf("legacy empty response = %v", err)
	}
	if err := ValidateGatewayResponse("v1", []string{"v1"}); err != nil {
		t.Fatalf("v1 response = %v", err)
	}
	if err := ValidateGatewayResponse("v2", []string{"v2"}); err == nil {
		t.Fatal("expected unsupported negotiated version to fail")
	}
}
