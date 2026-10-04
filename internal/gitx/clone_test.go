package gitx

import "testing"

func TestIsRemote(t *testing.T) {
	for _, tt := range []struct {
		s       string
		want    bool
		refused bool
	}{
		{"https://github.com/you/backup.git", true, false},
		{"https://github.com/you/backup", true, false},
		{"HTTPS://github.com/you/backup.git", true, false},
		{"https://ghp_token@github.com/you/backup.git", true, false},
		{"ssh://git@github.com/you/backup.git", true, false},
		{"SSH://github.com:22/you/backup.git", true, false},
		{"git@github.com:you/backup.git", true, false},
		{"git@github.com:/srv/backup.git", true, false},
		{"http://you:s3cret@github.com/you/backup.git", true, true},
		{"file:///srv/backup.git", true, true},
		{"git://github.com/you/backup.git", true, true},
		{"https://", false, false},
		{"~/backup", false, false},
		{"/home/you/backup", false, false},
		{"backup", false, false},
		{"./a@b:c", false, false},
		{"a/b@c:d", false, false},
		{"git@github.com:", false, false},
		{"C:\\Users\\you\\backup", false, false},
		{"C://Users/you/backup", false, false},
		{"", false, false},
	} {
		got, err := IsRemote(tt.s)
		if got != tt.want || (err != nil) != tt.refused {
			t.Errorf("IsRemote(%q) = %v, %v; want %v, refused %v", tt.s, got, err, tt.want, tt.refused)
		}
	}
	_, err := IsRemote("http://you:s3cret@github.com/you/backup.git")
	if err == nil || err.Error() != "salt cannot download a backup from http://github.com/you/backup.git. Give an https or ssh URL, such as https://github.com/you/backup.git or git@github.com:you/backup.git, or the path of a folder" {
		t.Fatalf("IsRemote(http URL) error = %v", err)
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
