// Package e2e runs folio's identity path against a real tailnet.
//
// Everything else in this repository tests the tailnet seam with fakes: the
// identity tests hand [identity.Resolver] a two-line WhoIs stub, and the HTTP
// tests call the API over loopback. Those tests answer "given a peer, what
// happens", which is the right question for most bugs. They cannot answer a
// different one: when a real HTTP request crosses a real WireGuard connection
// from a node that a real control server registered, does folio end up knowing
// who is on the other end? That is the one seam with no unit-testable
// contract, because its contract is the tailnet itself.
//
// So this package runs the real thing, hermetically:
//
//   - headscale (the self-hosted control server, as a subprocess) is the
//     control plane, on a loopback socket;
//   - a local DERP relay and STUN server (tailscale's own test packages) keep
//     all traffic on the machine even if direct connections cannot form;
//   - tsnet nodes join it with preauth keys, the same way a production node
//     joins any control server;
//   - folio itself boots through [internal/tsserve] over the plain tailnet
//     listener, because headscale cannot provision ts.net certificates.
//
// The tests then talk to folio through [internal/client], the client the TUI
// uses, so the request path is the production one end to end: client process,
// netstack, WireGuard, tsnet, WhoIs, resolver, API, store, vault, disk.
//
// Nothing here mocks a tailscale type. If a test in this package fails, the
// failure is in folio's real tailnet behaviour or in the harness itself. The
// dev shell ships the headscale binary; without it, or under -short, every
// test in here skips itself.
package e2e
