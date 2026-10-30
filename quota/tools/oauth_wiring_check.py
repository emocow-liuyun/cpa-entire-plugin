#!/usr/bin/env python3
"""Integration checklist for the Entire auth provider capability.

Reports which of the three wiring edits still have to land in plugin.go before
the auth RPCs (auth.identifier / auth.parse / auth.login.start /
auth.login.poll / auth.refresh) are reachable through the plugin dispatcher.

The auth code lives entirely in oauth.go and is self-contained: its tests call
handleAuthMethod directly, so they pass whether or not the dispatcher is wired.
"""
from pathlib import Path
import sys

HERE = Path(__file__).resolve().parent.parent

DISPATCH_ANCHOR = '"unknown method: "+trimForMessage(method), httpStatusForMethod(method))'
DISPATCH_INSERT = """
	case methodAuthIdentifier, methodAuthParse, methodAuthLoginStart,
		methodAuthLoginPoll, methodAuthRefresh:
		return handleAuthMethod(method, rawRequest)"""

CAPS_ANCHOR = "Capabilities: registrationCapabilities{QuotaProvider: true},"
CAPS_INSERT = "Capabilities: registrationCapabilities{QuotaProvider: true, AuthProvider: true},"

STRUCT_ANCHOR = """type registrationCapabilities struct {
	QuotaProvider bool `json:"quota_provider"`
}"""
STRUCT_INSERT = """type registrationCapabilities struct {
	QuotaProvider bool `json:"quota_provider"`
	AuthProvider  bool `json:"auth_provider"`
}"""

CHECKS = (
    (DISPATCH_ANCHOR, "handleMethod dispatch (default arm)"),
    (CAPS_ANCHOR, "buildRegistration capability literal"),
    (STRUCT_ANCHOR, "registrationCapabilities struct"),
)


def main() -> int:
    oauth = HERE / "oauth.go"
    tests = HERE / "oauth_test.go"
    plugin = HERE / "plugin.go"
    if not oauth.is_file():
        sys.exit("oauth.go is missing")
    print("oauth.go:", oauth.stat().st_size, "bytes")
    print("oauth_test.go:", tests.stat().st_size, "bytes")
    print()

    source = plugin.read_text(encoding="utf-8")
    wired = "handleAuthMethod(method, rawRequest)" in source and "AuthProvider: true" in source
    if wired:
        print("Wiring applied: handleMethod routes auth.* and auth_provider is declared.")
        return 0

    pending = [label for anchor, label in CHECKS if anchor in source]
    if pending:
        print("Pending wiring in plugin.go: " + ", ".join(pending))
    else:
        print("Pending wiring in plugin.go: every anchor was already edited;")
        print("verify the three edits below are present in your plugin.go.")
    print("Until these edits land the auth RPCs are unreachable; the auth tests")
    print("call handleAuthMethod directly and pass regardless.")
    print()
    print("1) registrationCapabilities struct ->")
    print(STRUCT_INSERT)
    print("\n2) buildRegistration capability literal ->")
    print("   " + CAPS_INSERT)
    print("\n3) handleMethod switch, before the default arm ->")
    print(DISPATCH_INSERT)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
