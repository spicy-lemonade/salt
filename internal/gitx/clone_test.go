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
		{"HTTPS://you:s3cret@github.com/x", "HTTPS://github.com/x"},
		{"ssh://you:s3cret@host/x", "ssh://host/x"},
		// An @ in the path is not a password.
		{"https://github.com/you/b@ckup.git", "https://github.com/you/b@ckup.git"},
		// A password holding @ is removed whole.
		{"https://you:p@ss@github.com/x", "https://github.com/x"},
		// A URL net/url would refuse still loses its credentials.
		{"https://you:s3cret@github.com:bad port/x", "https://github.com:bad port/x"},
		{"git@github.com:you/backup.git", "git@github.com:you/backup.git"},
		{"/home/you/backup", "/home/you/backup"},
		{"/home/a@b/backup", "/home/a@b/backup"},
		// Every URL in a message loses its credentials.
		{"from 'https://a:b@x/1' to https://c@y/2.", "from 'https://x/1' to https://y/2."},
	} {
		if got := RedactURL(tt.s); got != tt.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tt.s, got, tt.want)
		}
	}
}

func TestHideCredentials(t *testing.T) {
	for _, tt := range []struct{ out, remote, want string }{
		// A token given as the user name is removed from the URLs git shows.
		{
			"fatal: unable to access 'https://ghp_token@github.com/me/b.git/': 403",
			"https://ghp_token@github.com/me/b.git",
			"fatal: unable to access 'https://github.com/me/b.git/': 403",
		},
		// A user name with no password is kept outside URLs, so the path
		// still reads normally.
		{
			"fatal: repository 'https://github.com/you/backup.git/' not found",
			"https://you@github.com/you/backup.git",
			"fatal: repository 'https://github.com/you/backup.git/' not found",
		},
		// A password is hidden wherever it appears.
		{"remote: wrong password s3cret for you", "https://you:s3cret@host/x", "remote: wrong password *** for you"},
		{"remote: wrong password p@ss, p%40ss", "https://you:p%40ss@host/x", "remote: wrong password ***, ***"},
		{"https://you:p@ss@host/x: p@ss", "https://you:p@ss@host/x", "https://host/x: ***"},
		// Nothing to hide.
		{"fatal: could not read from remote", "git@github.com:you/backup.git", "fatal: could not read from remote"},
		{"fatal: x", "https://github.com/you/backup.git", "fatal: x"},
		{"fatal: x", "https://you:@host/x", "fatal: x"},
	} {
		if got := hideCredentials(tt.out, tt.remote); got != tt.want {
			t.Errorf("hideCredentials(%q, %q) = %q, want %q", tt.out, tt.remote, got, tt.want)
		}
	}
}
