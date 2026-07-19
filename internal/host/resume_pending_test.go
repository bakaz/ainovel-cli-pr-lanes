package host

import "testing"

func TestIsPlainResumeSteer(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "plain", text: "继续", want: true},
		{name: "surrounding whitespace", text: " \n继续\t", want: true},
		{name: "constraint", text: "继续，但先修改第46章", want: false},
		{name: "longer phrase", text: "继续创作", want: false},
		{name: "empty", text: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPlainResumeSteer(tt.text); got != tt.want {
				t.Fatalf("isPlainResumeSteer(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}
