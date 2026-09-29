package guard

import "testing"

func TestCheck(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{"unset", map[string]string{}, false},
		{"empty", map[string]string{EnvActive: ""}, false},
		{"set", map[string]string{EnvActive: "1"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(k string) (string, bool) { v, ok := tt.env[k]; return v, ok }
			if err := Check(lookup); (err != nil) != tt.wantErr {
				t.Fatalf("Check() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
