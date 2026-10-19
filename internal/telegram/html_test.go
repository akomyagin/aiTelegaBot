package telegram

import "testing"

func TestEscapeHTML(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Обычный текст", "Обычный текст"},
		{"empty", "", ""},
		{"ampersand", "A & B", "A &amp; B"},
		{"angle brackets", "<b>bold</b>", "&lt;b&gt;bold&lt;/b&gt;"},
		{"placeholder from hotfix", "<url|@channel>", "&lt;url|@channel&gt;"},
		{"ampersand first", "&<>", "&amp;&lt;&gt;"},
		{"quotes untouched", `"x" 'y'`, `"x" 'y'`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EscapeHTML(tt.in); got != tt.want {
				t.Errorf("EscapeHTML(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
