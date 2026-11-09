{
  description = "AgentFeedback service and client";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/4fe25e12eb9f4c744966dd3de5a05246cf098601";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in {
      packages = forAllSystems (system:
        let pkgs = nixpkgs.legacyPackages.${system};
        in {
          default = pkgs.buildGo127Module rec {
            pname = "agentfeedback";
            # Updated to the release version before tagging; untagged development is explicit.
            version = "4.4.0-dev";
            src = self;
            vendorHash = "sha256-EVgoHoT7zeFcXhHm4F48qx8UFdj0eBVsEf1HdIWEFNA=";
            subPackages = [ "cmd/agentfeedback" ];
            ldflags = [ "-s" "-w" "-X main.version=${version}" "-X main.commit=${self.rev or self.dirtyRev or "unknown"}" ];
            meta = {
              description = "Write-once feedback from AI coding agents";
              homepage = "https://github.com/AgentFeedback/agentfeedback";
              license = pkgs.lib.licenses.mit;
              mainProgram = "agentfeedback";
              platforms = systems;
            };
          };
        });
      checks = forAllSystems (system: { package = self.packages.${system}.default; });
    };
}
