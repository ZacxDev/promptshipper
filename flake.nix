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
      # ── desktop packages go here ─────────────────────────────────────────
      # TODO(desktop/snip): `packages.<system>.snip`  — lands with PR #3/#4.
      # TODO(server/mcp):   `packages.<system>.mcp`   — lands with PR #3/#4.
      # TODO(home-manager): `homeManagerModules.default` (snip + mcp on the
      #                     desktop), next to `packages`, once both exist.
      # Do not add server packages or NixOS server modules here (decided
      # 2026-11-20): compose and the chart are the server install paths.
      # ─────────────────────────────────────────────────────────────────────

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
