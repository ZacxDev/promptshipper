# NixOS module: services.promptshipper.{stt,clip}.
#
#   imports = [ promptshipper.nixosModules.default ];
#   services.promptshipper.clip = {
#     enable = true;
#     tokensFile = "/run/secrets/promptshipper-clip.env";   # CLIPSTORE_TOKEN=...
#   };
#   services.promptshipper.stt = {
#     enable = true;
#     tokensFile = "/run/secrets/promptshipper-stt.env";    # STT_API_TOKEN=...
#   };
#
# Both bind to 127.0.0.1 by default. 🔴 Neither has TLS: bind to a private
# interface (a Tailscale/VPN address) or put a TLS reverse proxy in front;
# never open these ports to the internet.
self:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.promptshipper;
  inherit (lib)
    mkEnableOption
    mkIf
    mkMerge
    mkOption
    types
    ;
  defaultPackages = self.packages.${pkgs.stdenv.hostPlatform.system};

  # A runtime path as a STRING, never a Nix path: a path literal
  # (`./tokens.env`) would be copied into the world-readable Nix store.
  secretPath = types.strMatching "/.*" // {
    description = "absolute path, as a string (not a Nix path literal)";
  };

  # Shared sandboxing. Each service writes only its StateDirectory.
  hardening = {
    DynamicUser = true;
    UMask = "0077";
    NoNewPrivileges = true;
    ProtectSystem = "strict";
    ProtectHome = true;
    PrivateTmp = true;
    ProtectKernelTunables = true;
    ProtectKernelModules = true;
    ProtectKernelLogs = true;
    ProtectControlGroups = true;
    ProtectClock = true;
    ProtectHostname = true;
    RestrictNamespaces = true;
    RestrictRealtime = true;
    RestrictSUIDSGID = true;
    LockPersonality = true;
    SystemCallArchitectures = "native";
    RestrictAddressFamilies = [
      "AF_INET"
      "AF_INET6"
      "AF_UNIX"
    ];
    CapabilityBoundingSet = "";
    Restart = "on-failure";
    RestartSec = "5s";
  };

  common = name: defaultPort: {
    enable = mkEnableOption "the promptshipper ${name} server";
    package = mkOption {
      type = types.package;
      default = defaultPackages.${name};
      defaultText = lib.literalExpression "promptshipper.packages.\${system}.${name}";
      description = "The ${name} server package.";
    };
    host = mkOption {
      type = types.str;
      default = "127.0.0.1";
      example = "100.64.0.1";
      description = "Address to listen on. Use a private (VPN/LAN) address, never a public one.";
    };
    port = mkOption {
      type = types.port;
      default = defaultPort;
      description = "TCP port to listen on.";
    };
    openFirewall = mkOption {
      type = types.bool;
      default = false;
      description = "Open `port` in the firewall.";
    };
  };
in
{
  options.services.promptshipper = {
    stt = common "stt" 8118 // {
      tokensFile = mkOption {
        type = types.nullOr secretPath;
        default = null;
        example = "/run/secrets/promptshipper-stt.env";
        description = ''
          systemd EnvironmentFile holding `STT_API_TOKEN=<token>` and,
          optionally, `STT_API_TOKENS=name:token,...`. Read by systemd at
          start; never copied into the Nix store. Required.
        '';
      };
      backend = mkOption {
        type = types.enum [
          "faster-whisper"
          "openai"
          "fake"
          "cohere"
        ];
        default = "faster-whisper";
        description = "STT_BACKEND. `cohere` needs a package with torch/transformers (not provided here).";
      };
      model = mkOption {
        type = types.nullOr types.str;
        default = null;
        example = "base";
        description = "STT_MODEL; null = the backend's default (faster-whisper: small, downloaded on first start).";
      };
      snippetsDir = mkOption {
        type = types.nullOr types.path;
        default = null;
        description = ''
          A directory containing `snippets.json` (the snippet catalog),
          bind-mounted read-only at /etc/snip for the service, where the
          server looks for it. Edits to the file are live.
        '';
      };
      environment = mkOption {
        type = types.attrsOf types.str;
        default = { };
        example = {
          STT_NO_SPEECH_MAX = "0.6";
        };
        description = "Extra environment (any STT_* variable; see server/stt/README.md). Not for secrets.";
      };
    };

    clip = common "clip" 8139 // {
      tokensFile = mkOption {
        type = types.nullOr secretPath;
        default = null;
        example = "/run/secrets/promptshipper-clip.env";
        description = ''
          systemd EnvironmentFile holding `CLIPSTORE_TOKEN=<token>` (at least
          24 bytes; clip refuses to start otherwise). Never copied into the
          Nix store. Required.
        '';
      };
    };
  };

  config = mkMerge [
    (mkIf cfg.stt.enable {
      assertions = [
        {
          assertion = cfg.stt.tokensFile != null;
          message = "services.promptshipper.stt.tokensFile must be set (an EnvironmentFile with STT_API_TOKEN=...).";
        }
      ];
      networking.firewall.allowedTCPPorts = lib.optional cfg.stt.openFirewall cfg.stt.port;
      systemd.services.promptshipper-stt = {
        description = "promptshipper stt server";
        wantedBy = [ "multi-user.target" ];
        wants = [ "network-online.target" ];
        after = [ "network-online.target" ];
        environment = {
          STT_BACKEND = cfg.stt.backend;
          HOME = "/var/lib/promptshipper-stt";
          # The model cache. First start downloads the model here.
          HF_HOME = "/var/lib/promptshipper-stt/hf";
          PYTHONUNBUFFERED = "1";
        }
        // lib.optionalAttrs (cfg.stt.model != null) { STT_MODEL = cfg.stt.model; }
        // cfg.stt.environment;
        serviceConfig = hardening // {
          ExecStart = "${lib.getExe cfg.stt.package} --host ${cfg.stt.host} --port ${toString cfg.stt.port}";
          EnvironmentFile = cfg.stt.tokensFile;
          StateDirectory = "promptshipper-stt";
          StateDirectoryMode = "0700";
          WorkingDirectory = "/var/lib/promptshipper-stt";
          BindReadOnlyPaths = lib.optional (cfg.stt.snippetsDir != null) "${cfg.stt.snippetsDir}:/etc/snip";
        };
      };
    })

    (mkIf cfg.clip.enable {
      assertions = [
        {
          assertion = cfg.clip.tokensFile != null;
          message = "services.promptshipper.clip.tokensFile must be set (an EnvironmentFile with CLIPSTORE_TOKEN=...).";
        }
      ];
      networking.firewall.allowedTCPPorts = lib.optional cfg.clip.openFirewall cfg.clip.port;
      systemd.services.promptshipper-clip = {
        description = "promptshipper clip server";
        wantedBy = [ "multi-user.target" ];
        wants = [ "network-online.target" ];
        after = [ "network-online.target" ];
        serviceConfig = hardening // {
          # 🔴 One process per database file (single SQLite writer). systemd
          # runs exactly one; do not point a second service at this directory.
          ExecStart = "${lib.getExe cfg.clip.package} -addr ${cfg.clip.host}:${toString cfg.clip.port} -db /var/lib/promptshipper-clip/clipstore.db";
          EnvironmentFile = cfg.clip.tokensFile;
          StateDirectory = "promptshipper-clip";
          StateDirectoryMode = "0700";
          MemoryDenyWriteExecute = true;
          PrivateDevices = true;
          # A bad token is exit 2: retrying cannot fix it.
          RestartPreventExitStatus = 2;
        };
      };
    })
  ];
}
