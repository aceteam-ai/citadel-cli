# typed: false
# frozen_string_literal: true

# Homebrew formula for Citadel CLI
# Install with: brew install aceteam-ai/tap/citadel
class Citadel < Formula
  desc "CLI agent for the AceTeam Sovereign Compute Fabric"
  homepage "https://aceteam.ai"
  license "Apache-2.0"
  version "2.1.1"

  on_macos do
    on_arm do
      url "https://github.com/aceteam-ai/citadel-cli/releases/download/v#{version}/citadel_v#{version}_darwin_arm64.tar.gz"
      sha256 "478ab100b2391371926e9666c70867547a22a61027150974395dfe4b9bcfbf09"
    end
    on_intel do
      url "https://github.com/aceteam-ai/citadel-cli/releases/download/v#{version}/citadel_v#{version}_darwin_amd64.tar.gz"
      sha256 "befe63822f6634a2c0142e2c12defa9fa520bdbef10551221e3070bd7290aabe"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/aceteam-ai/citadel-cli/releases/download/v#{version}/citadel_v#{version}_linux_arm64.tar.gz"
      sha256 "b7bd2294ab83d5d0178996fa0fba52b5fe1080f92c20ab1c88e12e5332e9131e"
    end
    on_intel do
      url "https://github.com/aceteam-ai/citadel-cli/releases/download/v#{version}/citadel_v#{version}_linux_amd64.tar.gz"
      sha256 "44534713bc319f0601cb30479befe9d61f93ede4822e6d8c2c55228651a44171"
    end
  end

  def install
    bin.install "citadel"
    man1.install "citadel.1" if File.exist?("citadel.1")
  end

  test do
    assert_match "citadel version", shell_output("#{bin}/citadel version")
  end
end
