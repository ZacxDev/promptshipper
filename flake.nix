{
  description = "promptshipper — self-hosted speech-to-text and a cross-device clipboard for your AI agents";

  # v1 role of this flake: the DESKTOP side (snip, the MCP server and a
  # home-manager module) plus a dev shell. The servers are deployed with
  # docker compose (deploy/compose) or the Helm chart (deploy/helm); their test
  # suites run in .github/workflows/ci.yml, not here.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      # Only systems CI actually evaluates (.github/workflows/chart.yml).
      systems = [ "x86_64-linux" ];
      forAllSystems = f: lib.genAttrs systems (system: f system nixpkgs.legacyPackages.${system});
    in
    {
      # ── desktop packages ─────────────────────────────────────────────────
      # snip (desktop/snip/package.nix) and the MCP server
      # (server/mcp/package.nix). CI builds both and runs them (chart.yml,
      # `flake`); `nix flake check` alone only evaluates packages.
      # TODO(home-manager): `homeManagerModules.default` (snip + mcp on the
      #                     desktop), next to `packages`.
      # Do not add server packages or NixOS server modules here (decided
      # 2026-11-20): compose and the chart are the server install paths.
      # ─────────────────────────────────────────────────────────────────────
      packages = forAllSystems (
        _: pkgs: {
          snip = pkgs.callPackage ./desktop/snip/package.nix { src = ./desktop/snip; };
          mcp = pkgs.callPackage ./server/mcp/package.nix {
            src = ./server/mcp;
            version = "0-unstable-" + (self.shortRev or self.dirtyShortRev or "unknown");
          };
        }
      );

      apps = forAllSystems (
        system: _: {
          snip = {
            type = "app";
            program = nixpkgs.lib.getExe self.packages.${system}.snip;
            meta.description = "hotkey -> fuzzy snippet picker -> paste";
          };
          mcp = {
            type = "app";
            program = nixpkgs.lib.getExe self.packages.${system}.mcp;
            meta.description = "MCP server for the promptshipper clip store";
          };
        }
      );

      devShells = forAllSystems (
        _: pkgs: {
          default = pkgs.mkShell {
            packages = [
              pkgs.go
              (pkgs.python3.withPackages (
                p: with p; [
                  fastapi
                  uvicorn
                  python-multipart
                  numpy
                  httpx
                  pytest
                  pyyaml
                ]
              ))
              pkgs.ffmpeg-headless
              pkgs.sqlite
              pkgs.kubernetes-helm
              pkgs.kubeconform
              pkgs.shellcheck
            ];
            env.CGO_ENABLED = "0";
          };
        }
      );

      formatter = forAllSystems (_: pkgs: pkgs.nixfmt);
    };
}
