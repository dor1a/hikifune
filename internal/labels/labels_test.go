package labels

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name      string
		labels    map[string]string
		protected bool
		wantErr   bool
	}{
		{"no labels", nil, false, false},
		{"other labels only", map[string]string{"com.docker.compose.service": "web"}, false, false},
		{"true", map[string]string{Protected: "true"}, true, false},
		{"TRUE", map[string]string{Protected: "TRUE"}, true, false},
		{"1", map[string]string{Protected: "1"}, true, false},
		{"false", map[string]string{Protected: "false"}, false, false},
		{"0", map[string]string{Protected: "0"}, false, false},
		{"empty value", map[string]string{Protected: ""}, true, false},
		{"typo", map[string]string{Protected: "ture"}, true, true},
		{"yes", map[string]string{Protected: "yes"}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Parse(tt.labels)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if p.Protected != tt.protected {
				t.Fatalf("Protected = %v, want %v", p.Protected, tt.protected)
			}
		})
	}
}
