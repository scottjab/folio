{
  description = "tsnotes: a self-hosted, tailnet-native markdown notes app";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        go = pkgs.go_1_27;
        buildGoModule = pkgs.buildGoModule.override { inherit go; };

        version = "0.1.0";

        frontend = pkgs.buildNpmPackage {
          pname = "tsnotes-frontend";
          inherit version;
          src = ./web;
          npmDepsHash = "sha256-/WLnpjafrP/tZTz9SJ+8gCk2E+0ZInZf6mtt4uDpcvM=";
          dontNpmInstall = true;
          installPhase = ''
            runHook preInstall
            mkdir -p $out
            cp -r dist $out/dist
            runHook postInstall
          '';
        };

        tsnotes = buildGoModule {
          pname = "tsnotes";
          inherit version;
          src = ./.;
          vendorHash = "sha256-T1iT2WyBYW58ghM/OkDpIx9JoKpRV4/EGyYY3q0MidM=";

          env.CGO_ENABLED = 0;

          # The Go build embeds internal/web/dist. A placeholder is committed so
          # `go test ./...` works without node; here we swap in the real bundle.
          preBuild = ''
            cp -r ${frontend}/dist/. internal/web/dist/
            chmod -R u+w internal/web/dist
            # Fail here rather than shipping a binary whose UI is a stub page.
            test -f internal/web/dist/app.js \
              || (echo "frontend bundle missing after copy" >&2; exit 1)
          '';

          ldflags = [ "-s" "-w" "-X" "main.version=${version}" ];

          subPackages = [ "cmd/tsnotes" ];

          meta = with pkgs.lib; {
            description = "Self-hosted, tailnet-native markdown notes app with WYSIWYG editing and MCP";
            mainProgram = "tsnotes";
            platforms = platforms.unix;
          };
        };
      in
      {
        packages = {
          inherit frontend tsnotes;
          default = tsnotes;
        };

        apps.default = flake-utils.lib.mkApp { drv = tsnotes; };

        devShells.default = pkgs.mkShell {
          packages = [
            go
            pkgs.gopls
            pkgs.gotools
            pkgs.go-tools
            pkgs.golangci-lint
            pkgs.delve
            pkgs.nodejs_24
            pkgs.esbuild
            pkgs.prefetch-npm-deps
            pkgs.sqlite
            pkgs.tailscale
            pkgs.jq
          ];

          shellHook = ''
            export GOTOOLCHAIN=local
            echo "tsnotes dev shell: $(go version)"
          '';
        };

        checks = {
          # `nix flake check` builds the binary, which runs `go test ./...` as
          # part of buildGoModule's check phase.
          build = tsnotes;
          inherit frontend;
        };
      })
    // {
      nixosModules.default = { config, lib, pkgs, ... }:
        let cfg = config.services.tsnotes;
        in {
          options.services.tsnotes = {
            enable = lib.mkEnableOption "tsnotes, a tailnet-native markdown notes app";

            package = lib.mkOption {
              type = lib.types.package;
              default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
              description = "The tsnotes package to run.";
            };

            hostname = lib.mkOption {
              type = lib.types.str;
              default = "tsnotes";
              description = ''
                Tailnet node name. The app is served at
                https://<hostname>.<your-tailnet>.ts.net, which needs MagicDNS and
                HTTPS certificates enabled for the tailnet.
              '';
            };

            authKeyFile = lib.mkOption {
              type = lib.types.nullOr lib.types.path;
              default = null;
              example = "/run/secrets/tsnotes-authkey";
              description = ''
                File holding a Tailscale auth key, needed only on first run. It is
                passed through systemd's LoadCredential so it never appears in the
                unit's environment.
              '';
            };

            settings = lib.mkOption {
              type = lib.types.attrs;
              default = { };
              example = {
                agents = [{ tag = "tag:notes-agent"; actAs = "you@github"; }];
              };
              description = "Contents of the JSON config file. See internal/config.";
            };
          };

          config = lib.mkIf cfg.enable {
            systemd.services.tsnotes = {
              description = "tsnotes";
              after = [ "network-online.target" ];
              wants = [ "network-online.target" ];
              wantedBy = [ "multi-user.target" ];

              serviceConfig = {
                ExecStart = lib.escapeShellArgs ([
                  "${cfg.package}/bin/tsnotes" "serve"
                  "--hostname" cfg.hostname
                  "--state" "/var/lib/tsnotes"
                ] ++ lib.optionals (cfg.settings != { }) [
                  "--config" (pkgs.writers.writeJSON "tsnotes.json" cfg.settings)
                ]);

                DynamicUser = true;
                StateDirectory = "tsnotes";
                StateDirectoryMode = "0700";
                Restart = "on-failure";
                RestartSec = 5;

                LoadCredential = lib.optional (cfg.authKeyFile != null)
                  "ts_authkey:${cfg.authKeyFile}";

                # tsnotes reads and writes exactly one directory and talks to the
                # tailnet. Nothing else needs to be reachable from it.
                AmbientCapabilities = [ "CAP_NET_ADMIN" ];
                CapabilityBoundingSet = [ "CAP_NET_ADMIN" ];
                NoNewPrivileges = true;
                PrivateDevices = true;
                PrivateTmp = true;
                ProtectClock = true;
                ProtectControlGroups = true;
                ProtectHome = true;
                ProtectHostname = true;
                ProtectKernelLogs = true;
                ProtectKernelModules = true;
                ProtectKernelTunables = true;
                ProtectProc = "invisible";
                ProtectSystem = "strict";
                RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" "AF_NETLINK" ];
                RestrictNamespaces = true;
                RestrictRealtime = true;
                RestrictSUIDSGID = true;
                SystemCallArchitectures = "native";
                SystemCallFilter = [ "@system-service" "~@privileged" "~@resources" ];
                LockPersonality = true;
                MemoryDenyWriteExecute = true;
              };
            };
          };
        };
    };
}
