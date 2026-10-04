package gitx

import "testing"

func TestIsRemote(t *testing.T) {
	for _, tt := range []struct {
		s    string
		want bool
	}{
		{"https://github.com/you/backup.git", true},
		{"https://github.com/you/backup", true},
		{"HTTPS://github.com/you/backup.git", true},
		{"https://ghp_token@github.com/you/backup.git", true},
		{"git@github.com:you/backup.git", true},
		{"git@github.com:/srv/backup.git", true},
		{"https://", false},
		{"http://github.com/you/backup.git", false},
		{"ssh://git@github.com/you/backup.git", false},
		{"file:///srv/backup.git", false},
		{"~/backup", false},
		{"/home/you/backup", false},
		{"backup", false},
		{"./a@b:c", false},
		{"a/b@c:d", false},
		{"git@github.com:", false},
		{"C:\\Users\\you\\backup", false},
		{"", false},
	} {
		if got := IsRemote(tt.s); got != tt.want {
			t.Errorf("IsRemote(%q) = %v, want %v", tt.s, got, tt.want)
		}
	}
}

func TestRedactURL(t *testing.T) {
	for _, tt := range []struct{ s, want string }{
		{"https://github.com/you/backup.git", "https://github.com/you/backup.git"},
		{"https://ghp_token@github.com/you/backup.git", "https://github.com/you/backup.git"},
		{"https://you:s3cret@github.com/you/backup.git", "https://github.com/you/backup.git"},
		{"HTTPS://you:s3cret@github.com/x", "https://github.com/x"},
		// An @ in the path is not a password.
		{"https://github.com/you/b@ckup.git", "https://github.com/you/b@ckup.git"},
		// A password holding @ is removed whole.
		{"https://you:p@ss@github.com/x", "https://github.com/x"},
		// A URL net/url would refuse still loses its credentials.
		{"https://you:s3cret@github.com:bad port/x", "https://github.com:bad port/x"},
		{"git@github.com:you/backup.git", "git@github.com:you/backup.git"},
		{"/home/you/backup", "/home/you/backup"},
	} {
		if got := RedactURL(tt.s); got != tt.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tt.s, got, tt.want)
		}
	}
}
