# Understanding conservative inference reservations

This example uses a synthetic OpenAI-compatible text profile with a
128,000-token input window, a 4,096-token output ceiling and zero configured
Bifrost logical retries. It does not install a production policy or price.

The pinned SDK still permits one guarded encrypted-reasoning fallback, and each
logical attempt can enter fasthttp's bounded transport retry loop. Harbor therefore
reserves `(0 + 2) × 5 × (128000 + 4096) = 1,320,960` tokens for one completion
request. A conversation turn can make several completion requests; each must
fit the same immutable task allocation before its provider dispatch.

If only the final response reports 35 tokens, 35 become known usage and
1,320,925 remain unknown held capacity. The larger reservation is an upper bound,
not a claim that those tokens were consumed. A cancelled request without a complete
usage witness retains its entire 1,320,960-token reservation.

For two interrupted completion requests followed by one successful 35-token
response, the allocation contains 3,962,845 unknown held tokens and 35 known
tokens. Another completion request requires total capacity of at least 5,283,840
tokens. A 2,000,000-token allocation admits only one such envelope; observed
output alone does not create room for a second. An `attempt_count` of three
identifies three accounting envelopes, not the number of physical HTTP requests.

This is five times the earlier incomplete transport reservation. Existing task
ceilings never grow automatically, and a repaired runtime can refuse additional
work that no longer fits. New qualification fixtures fund their finite sequences
before the initiating Start; they never top up an active task. Historical
pre-repair executions cannot acquire a retroactive hard-cap guarantee.

Unknown holds may remain indefinitely, including after irreversible allocation
closure. Exact actual settlement needs evidence covering every possible physical
attempt. A timer, task terminality, a logical retry counter or a successful final
response cannot supply that evidence. Monetary reservations additionally require
the exact immutable inclusive pricing manifest and retain their own unknown
liability; this token example is not an invoice or a cost estimate.
