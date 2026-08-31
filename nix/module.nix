# NixOS module for folio.
#
# Everything folio needs is expressible in its JSON config file, so this module
# generates that file and keeps ExecStart to a single argument. That means there
# is exactly one place a setting can come from, and no chance of the flags and
# the file disagreeing.
{ self }:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.folio;
  format = pkgs.formats.json { };

  # Explicit options first, then settings, so the freeform escape hatch wins.
  # folio parses its config with RejectUnknownMembers, so a typo here is a loud
  # failure at startup rather than a setting that quietly did nothing.
  configFile = format.generate "folio.json" (
    {
      inherit (cfg) hostname logLevel watchExternal;
      stateDir = cfg.stateDir;
      addr = cfg.addr;
    }
    // lib.optionalAttrs (cfg.agents != [ ]) { inherit (cfg) agents; }
    // cfg.settings
  );
in
{
  options.services.folio = {
    enable = lib.mkEnableOption "folio, self-hosted markdown notes on a tailnet";

    package = lib.mkPackageOption self.packages.${pkgs.stdenv.hostPlatform.system} "folio" {
      default = [ "default" ];
    };

    hostname = lib.mkOption {
      type = lib.types.str;
      default = "folio";
      description = ''
        Tailnet node name. folio is then served at
        `https://<hostname>.<your-tailnet>.ts.net`.

        The tailnet needs MagicDNS and HTTPS certificates enabled, both under DNS
        in the Tailscale admin console, or the TLS listener has nothing to serve.
      '';
    };

    addr = lib.mkOption {
      type = lib.types.str;
      default = ":443";
      description = ''
        Address to listen on inside the tailnet. This is a tsnet address, not a
        host one: nothing is bound on the machine's own interfaces, so there is
        no firewall hole to open.
      '';
    };

    stateDir = lib.mkOption {
      type = lib.types.path;
      default = "/var/lib/folio";
      description = ''
        Holds the SQLite index, the tailnet node's keys, and one directory of
        markdown per user.

        The markdown is the source of truth and is what you want in a backup. The
        database beside it can be rebuilt at any time with `folio index rebuild`,
        though the share grants it holds cannot, so back up the lot.
      '';
    };

    logLevel = lib.mkOption {
      type = lib.types.enum [ "debug" "info" "warn" "error" ];
      default = "info";
      description = "Verbosity. `debug` includes tsnet's own chatter.";
    };

    watchExternal = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Watch the vaults for changes made outside folio, so editing a note in
        Obsidian or pulling one in with git updates the search index and any open
        browser tab.
      '';
    };

    authKeyFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      example = "/run/secrets/folio-authkey";
      description = ''
        File containing a Tailscale auth key, needed only to register the node on
        first start. Afterwards folio reuses the node key in its state directory.

        The file is passed through systemd's `LoadCredential`, so the key is
        readable only by this unit and never appears in its environment or in
        /proc.
      '';
    };

    agents = lib.mkOption {
      type = lib.types.listOf (lib.types.submodule {
        options = {
          tag = lib.mkOption {
            type = lib.types.strMatching "tag:.+";
            example = "tag:notes-agent";
            description = "Tailnet ACL tag carried by the node the agent runs on.";
          };
          actAs = lib.mkOption {
            type = lib.types.str;
            example = "you@github";
            description = "Tailnet login the agent acts as.";
          };
        };
      });
      default = [ ];
      example = [{ tag = "tag:notes-agent"; actAs = "you@github"; }];
      description = ''
        Lets an AI agent on a tagged node act as a named user over MCP.

        A tagged node has no human behind it, so folio refuses it by default
        rather than guessing whose notes it should see. A mapping here is the
        explicit grant. The named user must already have opened folio at least
        once: an agent can borrow an identity but never create one.
      '';
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "folio";
      description = "User the service runs as, and owner of the vault files.";
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "folio";
      description = ''
        Group owning the vault files. Adding your own account to this group is
        the least fiddly way to get at the markdown directly, for a backup job or
        to point a desktop Obsidian at it over a share.
      '';
    };

    settings = lib.mkOption {
      inherit (format) type;
      default = { };
      example = { cacheTTL = "60s"; };
      description = ''
        Written verbatim into folio's JSON config file, and merged over the
        options above, so anything here wins.

        This is the escape hatch for settings that have no option yet. folio
        rejects unknown keys outright, so a typo stops the service at startup
        with a message naming the key rather than being ignored.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    users.users = lib.mkIf (cfg.user == "folio") {
      folio = {
        isSystemUser = true;
        group = cfg.group;
        home = cfg.stateDir;
        description = "folio notes server";
      };
    };
    users.groups = lib.mkIf (cfg.group == "folio") { folio = { }; };

    # A static user rather than DynamicUser, deliberately. folio's whole premise
    # is that your notes are ordinary markdown files you can also open in
    # Obsidian or hand to a backup job, and a rotating uid under
    # /var/lib/private makes that needlessly awkward.
    systemd.tmpfiles.rules = [
      "d '${cfg.stateDir}' 0700 ${cfg.user} ${cfg.group} - -"
    ];

    systemd.services.folio = {
      description = "folio, self-hosted markdown notes on a tailnet";
      documentation = [ "https://github.com/scottjab/folio" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      wantedBy = [ "multi-user.target" ];

      # These belong in [Unit], not [Service]. Setting them through
      # serviceConfig puts them in the wrong section, where systemd ignores
      # them, and a tailnet that is slow to come up would then trip the default
      # start limit and give up for good.
      startLimitBurst = 5;
      startLimitIntervalSec = 300;

      serviceConfig = {
        ExecStart = "${lib.getExe cfg.package} serve --config ${configFile}";
        User = cfg.user;
        Group = cfg.group;

        Restart = "on-failure";
        RestartSec = 5;

        LoadCredential = lib.optional (cfg.authKeyFile != null)
          "ts_authkey:${cfg.authKeyFile}";

        # tsnet does its networking in userspace, with no TUN device, so the
        # service needs no capabilities at all.
        CapabilityBoundingSet = [ "" ];
        AmbientCapabilities = [ "" ];
        NoNewPrivileges = true;

        ProtectSystem = "strict";
        ReadWritePaths = [ cfg.stateDir ];
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectClock = true;
        ProtectControlGroups = true;
        ProtectHostname = true;
        ProtectKernelLogs = true;
        ProtectKernelModules = true;
        ProtectKernelTunables = true;
        ProtectProc = "invisible";
        ProcSubset = "pid";
        RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" "AF_NETLINK" ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [ "@system-service" "~@privileged" "~@resources" ];
        UMask = "0077";
      };
    };
  };
}
