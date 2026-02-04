{
  description = "Citadel CLI - AceTeam Sovereign Compute Fabric agent";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            # Go toolchain
            go_1_24
            gotools
            gopls
            go-tools

            # Build essentials
            git
            gnumake

            # Docker and container tools
            docker
            docker-compose

            # Utilities used by build script
            tree

            # Note: Tailscale is intentionally not included here as it's
            # typically installed system-wide and runs as a daemon.
            # Install via: brew install tailscale (macOS) or system package manager

            # Documentation site (Docusaurus)
            nodejs_22
            pnpm

            # Common development tools
            curl
            wget
            jq
          ];

          shellHook = ''
            echo "🏰 Citadel CLI development environment"
            echo ""
            echo "Available tools:"
            echo "  go version: $(go version)"
            echo "  docker: $(docker --version 2>/dev/null || echo 'not available (may need Docker daemon running)')"
            echo "  docker-compose: $(docker-compose --version 2>/dev/null || echo 'not available')"
            echo ""
            echo "Quick start:"
            echo "  ./build.sh          - Build release binaries"
            echo "  go build -o citadel - Quick local build"
            echo "  go test ./...       - Run tests"
            echo "  go run . status     - Check node status"
            echo ""

            # Set up Go environment
            export GOPATH="$HOME/go"
            export PATH="$GOPATH/bin:$PATH"

            # Enable Go modules
            export GO111MODULE=on
          '';
        };

        # Optional: Define a package build for citadel
        packages.default = pkgs.buildGoModule {
          pname = "citadel-cli";
          version = self.rev or "dev";
          src = ./.;

          vendorHash = null; # Set to the correct hash after first build, or use vendorHash = pkgs.lib.fakeSha256;

          ldflags = [
            "-X github.com/aceteam-ai/citadel-cli/cmd.Version=${self.rev or "dev"}"
          ];

          meta = with pkgs.lib; {
            description = "Citadel CLI - AceTeam Sovereign Compute Fabric agent";
            homepage = "https://github.com/aceteam-ai/citadel-cli";
            license = licenses.mit;
            maintainers = [ ];
            platforms = platforms.linux ++ platforms.darwin;
          };
        };
      }
    );
}
