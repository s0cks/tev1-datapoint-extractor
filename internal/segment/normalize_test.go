package segment

import "testing"

func TestNormalizeText(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"empty", "", ""},
		{"line wraps", "Station Chief Larry\nDevlin received a\n  cable", "Station Chief Larry Devlin received a cable"},
		{"zero width joiners", "Agency\u202f(\u200dCIA\u200d) Station", "Agency (CIA) Station"},
		{"narrow nbsp and nbsp", "a\u202fb\u00a0c", "a b c"},
		{"space before period", "Republic of the Congo .", "Republic of the Congo."},
		{"space before comma and semicolon", "one , two ; three", "one, two; three"},
		{"leading and trailing space dropped", "  x  \n", "x"},
		{"soft hyphen", "psycho\u00adlogical", "psychological"},
		{"unicode kept", "Léopoldville — 日本語", "Léopoldville — 日本語"},
		{"only invisible", "\u200b\u200d", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeText(tt.in); got != tt.want {
				t.Errorf("NormalizeText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
