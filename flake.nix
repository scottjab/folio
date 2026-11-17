{
  description = "folio: a self-hosted, tailnet-native markdown notes app";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    # go.mod requires go >= 1.27.4. go_1_27 in a floating nixpkgs is only as
    # new as its latest patch release, so a stale lock (or a consumer that
    # follows its own nixpkgs) silently ships an older go and fails the build
    # with a confusing go.mod error. Pin the toolchain to a revision that is
    # known to provide go 1.27.4 and bring it in as its own dependency.
    go-nixpkgs.url = "github:NixOS/nixpkgs/b349185bb79c7cf7ad8ea8c097931730bf473c71";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, go-nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        goPkgs = import go-nixpkgs { inherit system; };
        inherit (nixpkgs) lib;
        go = goPkgs.go_1_27;
        buildGoModule = pkgs.buildGoModule.override { inherit go; };

        # Release number, bumped when cutting a release.
        version = "0.1.4";

        # What the binary reports. Nix flakes cannot see git tags or run git,
        # so there is no git-describe here; instead we append the source's own
        # metadata, which is enough to trace any build back to a commit. A
        # tarball build has neither rev nor dirtyRev and just gets the release
        # number.
        sourceRev = self.rev or self.dirtyRev or null;
        buildVersion = if sourceRev == null
          then version
          else "${version}-${self.lastModifiedDate}-${lib.substring 0 12 sourceRev}";

        frontend = pkgs.buildNpmPackage {
          pname = "folio-frontend";
          inherit version;
          src = ./web;
          npmDepsHash = "sha256-QxRBxXaq+HDMFS0ANllJEVRiKwBBUvD7QgSdQU5mIjM=";
          dontNpmInstall = true;
          installPhase = ''
            runHook preInstall
            mkdir -p $out
            cp -r dist $out/dist
            runHook postInstall
          '';
        };

        folio = buildGoModule {
          pname = "folio";
          inherit version;
          src = ./.;
          vendorHash = "sha256-qHRGfuXufZouALjgJ+H/FG26e2mHvd8Jzh3ea3Zad9k=";

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

          ldflags = [ "-s" "-w" "-X" "main.version=${buildVersion}" ];

          subPackages = [ "cmd/folio" ];

          meta = with pkgs.lib; {
            description = "Self-hosted, tailnet-native markdown notes app with WYSIWYG editing and MCP";
            mainProgram = "folio";
            platforms = platforms.unix;
          };
        };
      in
      {
        packages = {
          inherit frontend folio;
          default = folio;
        };

        apps.default = flake-utils.lib.mkApp { drv = folio; };

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
            # The tailnet integration tests (internal/e2e) run the real
            # headscale control server as a subprocess. Tests skip politely
            # when this is absent.
            pkgs.headscale
            pkgs.jq
            # For `make icons`, which re-renders the app's PNG icons from their
            # SVG sources. Not part of any build: the PNGs are committed.
            pkgs.imagemagick
          ];

          shellHook = ''
            export GOTOOLCHAIN=local
            # stderr, so `nix develop -c cmd` output can be captured cleanly.
            echo "folio dev shell: $(go version)" >&2
          '';
        };

        checks = {
          # `nix flake check` builds the binary, which runs `go test ./...` as
          # part of buildGoModule's check phase.
          build = folio;
          inherit frontend;

          # Instantiates the NixOS module for real. Without this nothing ever
          # evaluates it, and a broken option type or an ExecStart that does not
          # resolve would only surface on someone's machine at deploy time.
          nixos-module = (nixpkgs.lib.nixosSystem {
            inherit system;
            modules = [
              self.nixosModules.default
              ({ lib, ... }: {
                boot.loader.grub.enable = false;
                fileSystems."/" = { device = "/dev/null"; fsType = "ext4"; };
                system.stateVersion = "25.05";
                nixpkgs.hostPlatform = system;

                services.folio = {
                  enable = true;
                  hostname = "notes";
                  authKeyFile = "/run/secrets/folio-authkey";
                  agents = [{ tag = "tag:notes-agent"; actAs = "you@github"; }];
                  settings.cacheTTL = "60s";
                };
              })
            ];
          }).config.system.build.toplevel;
        }
        # Boots a machine and starts the service. Evaluating the module proves
        # the options are well formed; only running it proves the systemd
        # hardening does not stop folio dead, which is the failure that would
        # otherwise turn up on a real deploy.
        // lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          nixos-vm = pkgs.testers.runNixOSTest {
            name = "folio-service";

            nodes = {
              # The default: state under /var/lib/folio.
              machine = { ... }: {
                imports = [ self.nixosModules.default ];
                services.folio = {
                  enable = true;
                  hostname = "notes";
                  logLevel = "debug";
                };
                virtualisation.memorySize = 2048;
              };

              # An overridden state directory, on a path whose parent does not
              # exist either, so the module has to create the lot.
              custom = { ... }: {
                imports = [ self.nixosModules.default ];
                services.folio = {
                  enable = true;
                  hostname = "notes";
                  stateDir = "/srv/notes/folio-data";
                };
                virtualisation.memorySize = 2048;
              };
            };

            testScript = ''
              machine.wait_for_unit("folio.service")

              # There is no tailnet in a test VM, so folio will not get past
              # joining it. Reaching that point is the assertion: the binary ran
              # under the sandbox, read its config, and found its state
              # directory, which is everything the module is responsible for.
              machine.wait_until_succeeds(
                  "journalctl -u folio.service | grep -q 'connecting to the tailnet'"
              )

              # The state directory is folio's own, and private.
              machine.succeed("test -d /var/lib/folio")
              machine.succeed(
                  "stat -c '%U:%G %a' /var/lib/folio | grep -qx 'folio:folio 700'"
              )

              # A seccomp filter that is too tight shows up as SIGSYS rather
              # than as a clean error, so check for it specifically.
              machine.fail("journalctl -u folio.service | grep -qiE 'bad system call|SIGSYS'")

              # And it should not have died on a sandbox denial either.
              machine.fail("journalctl -u folio.service | grep -qi 'permission denied'")
            '';
          };
        };
      })
    // {
      nixosModules.default = import ./nix/module.nix { inherit self; };
      nixosModules.folio = self.nixosModules.default;
    };
}
