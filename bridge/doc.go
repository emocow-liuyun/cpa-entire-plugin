// Package bridge is a dependency-light client for the Entire Agent control
// plane, written to sit underneath a CPA (CLIProxyAPI) executor plugin.
//
// It covers one narrow path and nothing else:
//
//	GET  {core}  /me/billing                       account billing snapshot
//	GET  {cell}  /agents/models                    harness + model catalogue
//	GET  {cell}  /{host}/{project}/agents/{id}     agent config + JSON etag
//	PUT  {cell}  /{host}/{project}/agents/{id}     full config replace (If-Match)
//	POST {cell}  /{host}/{project}/agents/{id}/runs  start run (Idempotency-Key)
//	GET  {cell}  /{host}/{project}/runs/{runId}    poll to a terminal status
//	POST {cell}  /{host}/{project}/runs/{runId}/cancel  best-effort cancel
//
// Explicit non-goals, so no caller mistakes this for a general gateway:
//
//   - No token streaming. A run's text is read once, after the terminal
//     status; there is no true SSE channel and none is simulated.
//   - No tool calls. The cloud agent runs its own tools; nothing is routed
//     back to the API client as tool_calls.
//   - Not a drop-in replacement for a Chat Completions / Responses / Messages
//     endpoint. RunCreate carries a Trail/Change target, not messages.
//
// Two safety properties are structural rather than conventional:
//
//   - The bearer is stored in a private field and is unreachable from outside
//     the package except through Config.Bearer, which only reports whether a
//     token is set. No error, log line or formatted output produced here
//     includes it; errors built from HTTP bodies pass through redactErrorBody.
//   - Every request URL is checked against an allowlist of https origins
//     (host:port) before the transport runs, so a tampered base URL or a
//     relative path cannot redirect the bearer to another host.
package bridge
