# promptshipper-mcp's nix package: the stdio MCP server that lets an agent read,
# search and push clips (server/mcp). The root flake calls this file with
# `src = ./server/mcp` and exposes it as `packages.<system>.mcp`. It is a
# DESKTOP tool (it runs beside the agent), which is why the desktop-only flake
# ships it while the clip and STT servers stay with compose and the chart.
{
  lib,
  buildGoModule,
  src,
  # main.go's buildVersion is "dev" in source and has no release line to read
  # (unlike snip's version.go), so the flake passes its own short revision and
  # the binary's --version prints "0-unstable-<rev>", naming the commit it was
  # built from.
  rev,
}:

buildGoModule {
  pname = "promptshipper-mcp";
  # 🔴 A CONSTANT, not the rev. buildGoModule names the vendored-modules
  # fixed-output derivation "<pname>-<version>-go-modules"; a per-commit version
  # would rename that FOD on every commit and re-fetch identical modules. The
  # rev reaches the binary only through ldflags below.
  version = "0-unstable";

  src = lib.cleanSource src;

  # 🔴 MEASURED by building with lib.fakeHash and taking the "got:" value from
  # the failure. NEVER hand-edited: change go.mod/go.sum and re-derive it.
  vendorHash = "sha256-NbuvbsmNbz3aKdjyyfpkbi52yRhXl473j3LvAUDn3L8=";

  env.CGO_ENABLED = "0";

  # 🔴 doCheck = false for the reason snip's package.nix gives: a red test
  # must never fail a consumer's switch. The tests run in CI (ci.yml, `mcp`),
  # including the seam test against the real clip server, which needs a
  # clipstore binary this build does not have.
  doCheck = false;

  ldflags = [
    "-s"
    "-w"
    "-X main.buildVersion=0-unstable-${rev}"
  ];

  # `go build` in this directory names the binary `mcp`; the README and every
  # client config call it promptshipper-mcp.
  postInstall = ''
    mv $out/bin/mcp $out/bin/promptshipper-mcp
  '';

  meta = with lib; {
    description = "MCP server for the promptshipper clip store (read, search, push clips)";
    mainProgram = "promptshipper-mcp";
    license = licenses.mit;
    platforms = platforms.linux;
  };
}
