# Governed generic credential refusal during discovery

A signed capability broker may refuse generic user minting with HTTP 403,
OAuth `error: "access_denied"`, and the exact bounded reason
`governed_app_operation_required`. This is a failed caller request, not evidence
that the shared source or its last successful catalog failed.

The exchange driver recognizes this only on the configured signed broker's
complete valid response, outside preparation and bound operation exchange. It
returns `auth.GovernedAppMintRequiredError`, preserving permanent
`ErrCredentialRejected` classification. Discovery refresh and probe still fail
honestly for that caller, but retain shared health, counters, timestamp and
catalog generation. Retained state reports the last successful discovery; it
does not replace current authorization for an App resource or callback.

Other 403 responses, invalid clients, malformed responses, current publisher
refusals, credential-source failures, network and schema failures retain their
existing behavior. No reason is inferred from an error description or status
alone. This extension grants no identity or data authority and changes no
public Protocol request or version.

The broker producer must emit the reason only for its exact governed generic
mint refusal after existing authentication and policy checks. Older brokers omit
it and retain existing failure behavior; older readers ignore it and retain
existing failure behavior. The availability correction requires both producer
and consumer updates.
