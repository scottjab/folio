# folio on a real tailnet: the NixOS module and the shipped binary, joined to a
# headscale control server, with tailscaled peers making requests across
# WireGuard. This is the one seam unit tests cannot reach: that a request from
# a node a real control server registered arrives with a source address WhoIs
# can answer, under the unit's real systemd sandbox.
#
# The control node is headscale's NixOS test kit (its nix/README.md has the
# contract): control at http://headscale, DERP and STUN on the same node,
# MagicDNS on `tailnet`, `hs-authkey USER` to mint keys and `hs-join KEY` on
# the peers. headscale cannot issue ts.net certificates, so folio serves plain
# HTTP on :80 inside the tailnet.
#
# Needs KVM: `nix build .#checks.x86_64-linux.tailnet -L`, or for a shell,
# `nix build .#checks.x86_64-linux.tailnet.driverInteractive && ./result/bin/nixos-test-driver`.
{ pkgs, self, headscale }:

let
  # Users have no email under the kit, so a policy names them as `user@`.
  # Folio sees them as the bare login, e.g. "alice".
  policy = pkgs.writeText "policy.json" (builtins.toJSON {
    tagOwners = {
      "tag:agent" = [ "alice@" ];
      "tag:unmapped" = [ "ops@" ];
    };
    acls = [{ action = "accept"; src = [ "*" ]; dst = [ "*:*" ]; }];
  });

  peer = {
    imports = [ headscale.nixosModules.testkit-peer ];
    environment.systemPackages = [ pkgs.curl pkgs.jq ];
  };
in
pkgs.testers.runNixOSTest {
  name = "folio-tailnet";

  nodes = {
    headscale = {
      imports = [ headscale.nixosModules.testkit ];
      services.headscale.settings.policy = { mode = "file"; path = policy; };
    };

    server = { lib, ... }: {
      imports = [ self.nixosModules.default ];
      services.folio = {
        enable = true;
        hostname = "notes";
        controlURL = "http://headscale";
        plainHTTP = true;
        addr = ":80";
        logLevel = "debug";
        # The test writes the key once headscale has minted it.
        authKeyFile = "/run/folio-authkey";
        agents = [{ tag = "tag:agent"; actAs = "alice"; }];
      };
      # Started by the test script, after the auth key exists.
      systemd.services.folio.wantedBy = lib.mkForce [ ];
      # There is no route out of the VM; don't make tsnet try to upload logs.
      systemd.services.folio.environment.TS_NO_LOGS_NO_SUPPORT = "1";
      virtualisation.memorySize = 2048;
    };

    # resolved lets tailscaled install split DNS for the MagicDNS domain, so
    # names like notes.tailnet resolve the way they would on a laptop.
    alice = { imports = [ peer ]; services.resolved.enable = true; };
    bob = peer;
    # Joins as an unmapped tagged node first, then as alice's agent.
    agent = peer;
  };

  testScript = ''
    import json
    import shlex

    # Each wait makes one attempt per second, capped at T seconds.
    T = 120

    def curl(node, method, url, body=None, headers=()):
        """Run curl on NODE; return (status, parsed JSON body or None)."""
        cmd = ["curl", "-sS", "-m", "10", "-X", method, "-o", "/tmp/body", "-w", "%{http_code}"]
        for h in headers:
            cmd += ["-H", h]
        if body is not None:
            cmd += ["-H", "Content-Type: application/json", "--data-binary", json.dumps(body)]
        status = int(node.succeed(shlex.join(cmd + [url])).strip())
        raw = node.succeed("cat /tmp/body")
        try:
            return status, json.loads(raw)
        except ValueError:
            return status, None

    def ok(node, method, path, body=None, headers=(), want=200):
        status, out = curl(node, method, base + path, body, headers)
        assert status == want, f"{method} {path} on {node.name}: {status}, want {want}: {out}"
        return out

    def first_me(node):
        """The first request from a fresh node races its map push; retry it."""
        node.wait_until_succeeds(f"curl -fsS -m 5 {base}/api/me", timeout=T)
        return ok(node, "GET", "/api/me")

    def code(node, method, path, body=None, headers=()):
        return curl(node, method, base + path, body, headers)[0]

    start_all()

    folio_key = headscale.succeed("hs-authkey folio").strip()
    alice_key = headscale.succeed("hs-authkey alice").strip()
    bob_key = headscale.succeed("hs-authkey bob").strip()
    agent_key = headscale.succeed("hs-authkey alice --tags tag:agent").strip()
    stray_key = headscale.succeed("hs-authkey ops --tags tag:unmapped").strip()

    server.wait_for_unit("multi-user.target")
    server.succeed(f"echo {folio_key} > /run/folio-authkey")
    server.systemctl("start folio.service")
    server.wait_until_succeeds("journalctl -u folio.service | grep -q 'folio is up'", timeout=T)

    alice.succeed(f"hs-join {alice_key}")
    bob.succeed(f"hs-join {bob_key}")
    notes_ip = alice.wait_until_succeeds("tailscale ip -4 notes", timeout=T).strip()
    base = f"http://{notes_ip}"

    with subtest("identity: the tailnet says who alice is"):
        me = first_me(alice)
        assert me["login"] == "alice", me
        assert not me["isAgent"], me
        vault = me["vault"]
        assert vault, "no vault provisioned"

    with subtest("a note written over the tailnet lands on disk and in the index"):
        made = ok(alice, "POST", f"/api/vaults/{vault}/notes",
                  {"path": "Welcome.md", "content": "# Welcome\n\nHello from alice."}, want=201)
        note = ok(alice, "GET", f"/api/vaults/{vault}/notes/Welcome.md")
        assert "Hello from alice." in note["content"], note
        assert "id: " in note["content"], "identity was not stamped into frontmatter"
        assert note["ownerLogin"] == "alice", note
        server.succeed(f"grep -qF 'Hello from alice.' /var/lib/folio/vaults/{vault}/Welcome.md")

        hits = ok(alice, "GET", "/api/search?q=Hello")["hits"]
        assert any(h["path"] == "Welcome.md" and h["ownerLogin"] == "alice" for h in hits), hits

    with subtest("writes are compare-and-swap on the content hash"):
        ok(alice, "PUT", f"/api/vaults/{vault}/notes/Welcome.md", {"content": "# Edited"},
           headers=[f'If-Match: "{made["sha256"]}"'])
        assert code(alice, "PUT", f"/api/vaults/{vault}/notes/Welcome.md", {"content": "# Stale"},
                    headers=['If-Match: "not-the-real-hash"']) == 409

    with subtest("sharing: grant, permission boundary, revoke"):
        ok(alice, "POST", f"/api/vaults/{vault}/notes",
           {"path": "Project/Notes.md", "content": "# Project"}, want=201)
        first_me(bob)
        path = f"/api/vaults/{vault}/notes/Project/Notes.md"

        vaults = ok(bob, "GET", "/api/vaults")["vaults"]
        assert not any(v["ownerLogin"] == "alice" for v in vaults), vaults
        assert code(bob, "GET", path) == 403

        ok(alice, "POST", "/api/shares",
           {"path": "Project/Notes.md", "grantee": "bob", "perm": "read", "isFolder": False}, want=201)
        vaults = ok(bob, "GET", "/api/vaults")["vaults"]
        assert any(v["ownerLogin"] == "alice" and v["vault"] == vault for v in vaults), vaults
        shared = ok(bob, "GET", path)
        assert shared["perm"] == "read", shared
        assert code(bob, "PUT", path, {"content": "# Overwritten"},
                    headers=[f'If-Match: "{shared["sha256"]}"']) == 403

        ok(alice, "POST", "/api/shares",
           {"path": "Project/Notes.md", "grantee": "bob", "perm": "write", "isFolder": False}, want=201)
        written = ok(bob, "PUT", path, {"content": "# Project\n\nBob was here."},
                     headers=[f'If-Match: "{shared["sha256"]}"'])
        assert ok(alice, "GET", path)["sha256"] == written["sha256"]

        share_ids = [s["id"] for s in ok(alice, "GET", "/api/shares")["shares"]]
        assert share_ids, "alice has nothing to revoke"
        for sid in share_ids:
            ok(alice, "DELETE", f"/api/shares/{sid}", want=204)
        assert code(bob, "GET", path) in (403, 404)

    with subtest("the event stream carries bob's edit to alice"):
        ok(alice, "POST", f"/api/vaults/{vault}/notes", {"path": "Live.md", "content": "# Live"}, want=201)
        ok(alice, "POST", "/api/shares",
           {"path": "Live.md", "grantee": "bob", "perm": "write", "isFolder": False}, want=201)
        alice.succeed(
            "systemd-run --unit=watch sh -c "
            + shlex.quote(f"${pkgs.curl}/bin/curl -sSN -H 'Accept: text/event-stream' -D /tmp/ev.h -o /tmp/ev {base}/api/events")
        )
        # Listening before bob writes, or the test races the subscription.
        alice.wait_until_succeeds("grep -q '^HTTP/1.1 200' /tmp/ev.h", timeout=T)
        live = ok(bob, "GET", f"/api/vaults/{vault}/notes/Live.md")
        ok(bob, "PUT", f"/api/vaults/{vault}/notes/Live.md", {"content": "# Live\n\nBob."},
           headers=[f'If-Match: "{live["sha256"]}"'])
        alice.wait_until_succeeds(
            "grep -A1 '^event: note.updated' /tmp/ev | grep -qF '\"path\":\"Live.md\"'", timeout=T
        )
        alice.succeed("systemctl stop watch")

    with subtest("CSRF guard holds across the tailnet"):
        new = lambda p: {"path": p, "content": "# csrf"}
        notes = f"/api/vaults/{vault}/notes"
        assert code(alice, "POST", notes, new("Blocked.md"), ["Origin: https://evil.example.com"]) == 403
        assert code(alice, "POST", notes, new("OwnOrigin.md"), [f"Origin: {base}"]) == 201
        assert code(alice, "POST", notes, new("NoOrigin.md")) == 201

    with subtest("MagicDNS: the suffix is advertised and the name resolves"):
        suffix = alice.succeed("tailscale status --json | jq -r .CurrentTailnet.MagicDNSSuffix").strip()
        assert suffix.rstrip(".") == "tailnet", suffix
        out = json.loads(alice.wait_until_succeeds("curl -fsS -m 5 http://notes.tailnet/api/me", timeout=T))
        assert out["login"] == "alice", out

    with subtest("a tagged node nobody mapped is refused with 403"):
        agent.succeed(f"hs-join {stray_key}")
        agent.wait_until_succeeds(f"test $(curl -s -m 5 -o /dev/null -w '%{{http_code}}' {base}/api/me) = 403", timeout=T)

    with subtest("a mapped tagged node acts as alice, flagged as an agent"):
        agent.succeed("tailscale logout")
        agent.succeed(f"hs-join {agent_key}")
        agent.wait_until_succeeds(f"curl -fsS -m 5 {base}/api/me", timeout=T)
        ame = ok(agent, "GET", "/api/me")
        assert ame["isAgent"] and ame["login"] == "alice", ame
        ok(agent, "POST", f"/api/vaults/{vault}/notes", {"path": "From-agent.md", "content": "# From the agent"}, want=201)
        server.succeed(f"grep -qF 'From the agent' /var/lib/folio/vaults/{vault}/From-agent.md")

    with subtest("the sandbox did not get in the way"):
        server.fail("journalctl -u folio.service | grep -qiE 'bad system call|SIGSYS'")
  '';
}
