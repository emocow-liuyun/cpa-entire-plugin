# OAuth review — 2026-10-31

Reviewed against CPA `auth_provider.go`, `rpc_client.go`, and `auth_callbacks.go`, alongside the Entire refresh flow.

## Fixed in this review

- Auth parsing now preserves the credential's original JSON and operator-owned disabled flag, prefix, proxy URL, label and extension fields. Previously reconstruction could reset these values. Added regression test.
- When a credential initially lacks an access token, a refresh persistence failure now propagates instead of being hidden by a successful balance read.
- Existing renewal code requires a verified persistent target before rotating and reuses a newer valid token after acquiring the account lock.

## Verified facts, not findings

- The referenced CPA host explicitly restores missing FileName, Prefix, Label and ProxyURL from the existing auth record (`auth_provider.go` lines 430–440). Empty refresh response fields alone do not prove identity loss.
- The RPC adapter supplies `host_callback_id` for auth login and refresh. JSON omission of HTTPClient does not by itself mean the plugin bypasses the host transport.

## Remaining limitations

- `host.auth.save` in the inspected host uses `os.WriteFile`; it is not a transactional or power-loss-atomic persistence primitive. The plugin reports save failures, but recovery of an upstream refresh rotation after a failed save still requires further work.
- Host-driven `auth.refresh` releases the plugin lock before the host finishes persisting its response. A simultaneous quota refresh may still encounter a stale snapshot across this boundary. The current quota-only installation does not establish a general multi-process or all-host-scheduler guarantee.
- Metadata-only credentials without a resolvable persistent auth index are not renewed through quota reads. No refresh grant is intentionally spent without a verified save target.
- The whole system has not undergone a comprehensive security audit. Passing tests are not proof that all vulnerabilities are eliminated.

## Validation

`go test -race ./...`, `go vet ./...`, and the c-shared build passed for the quota module after these fixes. Previously failing real accounts were refreshed and checked in CPA UI at $100 each without deleting credentials. No paid Agent task was started for this audit.
