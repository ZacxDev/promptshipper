{
  description = "promptshipper — self-hosted speech-to-text and a cross-device clipboard for your AI agents";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: lib.genAttrs systems (system: f system nixpkgs.legacyPackages.${system});

      # ONE version for everything: the chart's appVersion, which is also the
      # image tag release.yml publishes. Read, not restated.
      version = builtins.head (
        builtins.match ''.*appVersion: "([^"]+)".*'' (
          builtins.readFile ./deploy/helm/promptshipper/Chart.yaml
        )
      );

      # The Python environments. `sttPython` is what the server runs on;
      # `sttTestPython` is the suite's, which uses the fake backend and so
      # needs no model library (mirrors server/stt/requirements-dev.txt).
      # ⚠ Versions are nixpkgs', not the pins in requirements.txt.
      sttPythonPkgs =
        p: with p; [
          fastapi
          uvicorn
          python-multipart
          numpy
          httpx
        ];
      sttTestPythonPkgs = p: (sttPythonPkgs p) ++ [ p.pytest ];
    in
    {
      packages = forAllSystems (
        system: pkgs:
        let
          sttPython = pkgs.python3.withPackages (p: (sttPythonPkgs p) ++ [ p.faster-whisper ]);
        in
        {
          # The clip server. The binary keeps its historical name, `clipstore`.
          # buildGoModule's checkPhase runs `go test ./...`, so building this
          # IS running clip's unit tests (see checks.clip).
          clip = pkgs.buildGoModule {
            pname = "promptshipper-clip";
            inherit version;
            src = lib.fileset.toSource {
              root = ./server/clip;
              fileset = lib.fileset.unions [
                ./server/clip/go.mod
                ./server/clip/go.sum
                (lib.fileset.fileFilter (f: f.hasExt "go") ./server/clip)
              ];
            };
            vendorHash = "sha256-ETqFiOYVBd4yU+g/FjP8ItE3yK+8E03QXWoEItS4yAE=";
            # Pure-Go SQLite: built exactly as the image is (server/clip/Dockerfile).
            env.CGO_ENABLED = "0";
            ldflags = [
              "-s"
              "-w"
              "-X main.buildVersion=${version}"
            ];
            postInstall = ''
              mv $out/bin/clip $out/bin/clipstore
            '';
            meta = {
              description = "promptshipper clip server (cross-device clipboard, SQLite FTS5)";
              license = lib.licenses.mit;
              mainProgram = "clipstore";
            };
          };

          # The stt server: the app's source plus a Python env and ffmpeg (a
          # hard runtime dependency: every request shells out to it).
          # `promptshipper-stt [--host H] [--port P]` is uvicorn; the snippet
          # catalog path is fixed at /etc/snip/snippets.json.
          stt = pkgs.stdenvNoCC.mkDerivation {
            pname = "promptshipper-stt";
            inherit version;
            src = lib.fileset.toSource {
              root = ./server/stt;
              fileset = lib.fileset.unions [
                ./server/stt/app.py
                ./server/stt/check_tokens.py
                ./server/stt/backends
              ];
            };
            nativeBuildInputs = [ pkgs.makeWrapper ];
            installPhase = ''
              runHook preInstall
              share=$out/share/promptshipper-stt
              mkdir -p $share $out/bin
              cp -r app.py check_tokens.py backends $share/
              makeWrapper ${sttPython}/bin/python $out/bin/promptshipper-stt \
                --add-flags "-m uvicorn app:app --app-dir $share" \
                --prefix PATH : ${lib.makeBinPath [ pkgs.ffmpeg-headless ]}
              makeWrapper ${sttPython}/bin/python $out/bin/promptshipper-stt-check-tokens \
                --add-flags "$share/check_tokens.py"
              runHook postInstall
            '';
            meta = {
              description = "promptshipper stt server (OpenAI-compatible transcription + snippet catalog)";
              license = lib.licenses.mit;
              mainProgram = "promptshipper-stt";
            };
          };

          # ── sibling packages go here ───────────────────────────────────
          # TODO(desktop/snip): `snip` package — being built on its own branch.
          # TODO(server/mcp):   `mcp` package  — being built on its own branch.
          # Add them as attributes of this set; `default` stays a server.
          # ───────────────────────────────────────────────────────────────

          default = self.packages.${system}.clip;
        }
      );

      checks = forAllSystems (
        system: pkgs: {
          # clip's `go test ./...` runs in the package's checkPhase.
          clip = self.packages.${system}.clip;

          stt-tests =
            pkgs.runCommand "promptshipper-stt-tests"
              {
                nativeBuildInputs = [
                  (pkgs.python3.withPackages sttTestPythonPkgs)
                  pkgs.ffmpeg-headless
                ];
              }
              ''
                cp -r ${./server/stt} src
                chmod -R u+w src
                cd src
                export HOME=$TMPDIR
                python -m pytest -q -p no:cacheprovider
                touch $out
              '';

          # The wrapper starts and finds its own modules (no model needed).
          stt-wrapper = pkgs.runCommand "promptshipper-stt-wrapper" { } ''
            ${self.packages.${system}.stt}/bin/promptshipper-stt --help > /dev/null
            STT_API_TOKEN=0123456789abcdef0123456789 \
              ${self.packages.${system}.stt}/bin/promptshipper-stt-check-tokens
            touch $out
          '';

          # The NixOS module, in a VM: both services start, authenticate,
          # persist across a restart and serve the snippet catalog.
          module = import ./nix/module-test.nix { inherit self pkgs; };
        }
      );

      devShells = forAllSystems (
        system: pkgs: {
          default = pkgs.mkShell {
            packages = [
              pkgs.go
              (pkgs.python3.withPackages (p: (sttTestPythonPkgs p) ++ [ p.pyyaml ]))
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

      nixosModules.default = import ./nix/module.nix self;
      nixosModules.promptshipper = self.nixosModules.default;

      formatter = forAllSystems (_: pkgs: pkgs.nixfmt-rfc-style);
    };
}
