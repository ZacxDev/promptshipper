# VM test for nix/module.nix. Exercises the real units: both services start
# under their sandboxing, refuse a missing token, accept the right one, keep
# clips across a restart, and stt serves the bind-mounted snippet catalog.
# The tokens here are throwaway test values, written into the VM's store.
{ self, pkgs }:
let
  sttToken = "stt-vm-test-token-0123456789abcdef";
  clipToken = "clip-vm-test-token-0123456789abcdef";
  snippets = pkgs.writeTextDir "snippets.json" (
    builtins.toJSON [
      {
        label = "VM test snippet";
        body = "hello from the module test";
        search_terms = [ "vm" ];
        category = "test";
      }
    ]
  );
in
pkgs.testers.runNixOSTest {
  name = "promptshipper-module";
  nodes.machine = {
    imports = [ self.nixosModules.default ];
    virtualisation.memorySize = 2048;
    environment.etc."ps-test/stt.env".text = "STT_API_TOKEN=${sttToken}\n";
    environment.etc."ps-test/clip.env".text = "CLIPSTORE_TOKEN=${clipToken}\n";
    environment.systemPackages = [ pkgs.curl ];
    services.promptshipper.clip = {
      enable = true;
      tokensFile = "/etc/ps-test/clip.env";
    };
    services.promptshipper.stt = {
      enable = true;
      backend = "fake";
      port = 18118;
      tokensFile = "/etc/ps-test/stt.env";
      snippetsDir = snippets;
    };
  };
  testScript = ''
    clip = "http://127.0.0.1:8139"
    stt = "http://127.0.0.1:18118"
    clip_auth = "-H 'Authorization: Bearer ${clipToken}'"
    stt_auth = "-H 'Authorization: Bearer ${sttToken}'"

    machine.wait_for_unit("promptshipper-clip.service")
    machine.wait_for_open_port(8139)
    machine.succeed(f"curl -sf {clip}/healthz | grep -q '\"status\":\"ok\"'")
    # Without the token: 401, never data.
    code = machine.succeed(f"curl -s -o /dev/null -w '%{{http_code}}' {clip}/v1/clips").strip()
    assert code == "401", code
    machine.succeed(
        f"curl -sf -X POST {clip_auth} -H 'Content-Type: application/json' "
        f"-d '{{\"body\":\"persisted across restart\",\"device\":\"vm\"}}' {clip}/v1/clips"
    )
    machine.succeed("systemctl restart promptshipper-clip.service")
    machine.wait_for_open_port(8139)
    machine.succeed(f"curl -sf {clip_auth} '{clip}/v1/clips?q=restart' | grep -q 'persisted across restart'")
    # The database is in the state directory, not somewhere ephemeral.
    machine.succeed("test -s /var/lib/private/promptshipper-clip/clipstore.db")

    machine.wait_for_unit("promptshipper-stt.service")
    machine.wait_for_open_port(18118)
    machine.wait_until_succeeds(f"curl -sf {stt}/healthz | grep -q '\"backend\":\"fake\"'", timeout=120)
    code = machine.succeed(f"curl -s -o /dev/null -w '%{{http_code}}' {stt}/v1/snippets").strip()
    assert code == "401", code
    machine.succeed(f"curl -sf {stt_auth} {stt}/v1/snippets | grep -q 'hello from the module test'")
  '';
}
