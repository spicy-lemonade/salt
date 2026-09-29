# Template for Formula/salt.rb in spicy-lemonade/homebrew-tap.
# The release workflow fills in __VERSION__ and __SHA256__ and pushes it.
class Salt < Formula
  desc "Encrypts AI agent memory backups before they reach Git"
  homepage "https://github.com/spicy-lemonade/salt"
  url "https://github.com/spicy-lemonade/salt/archive/refs/tags/v__VERSION__.tar.gz"
  sha256 "__SHA256__"
  license "MIT"

  depends_on "go" => :build

  def install
    ENV["CGO_ENABLED"] = "0"
    system "go", "build", *std_go_args(ldflags: "-X main.version=#{version}"), "./cmd/salt"
  end

  test do
    assert_match "salt #{version}", shell_output("#{bin}/salt version")
  end
end
