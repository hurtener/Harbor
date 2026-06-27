// cmd/harbor/cmd_token.go — the `harbor token` bring-your-own-issuer
// subcommand.
//
// `harbor token` is the no-IdP on-ramp: it lets an operator who runs no
// external identity provider self-issue the JWTs `harbor serve` verifies,
// using a signing key they manage. It has two verbs:
//
//   - `harbor token keygen` — generate an asymmetric keypair, write the
//     private key (PEM, mode 0600) and the matching public JWK Set
//     (jwks.json). The operator points `harbor serve`'s
//     `identity.jwks_file` at the JWK Set.
//   - `harbor token mint` — mint a Harbor JWT for an identity triple,
//     signed with that key. The token's `iss` / `aud` MUST equal the
//     operator's `identity.issuer` / `identity.audience`, because the
//     verifier hard-rejects a mismatch.
//
// `harbor serve` is unchanged. It accepts a `harbor token`-minted JWT for
// exactly one reason: the operator configured `jwks_file` to trust the
// key — identical to pointing at an external IdP. There is no mint path
// inside serve; serve still mints nothing.
//
// Grade: these tokens are signed by a key the operator manages —
// single-issuer / eval / small-production posture. For multi-user SSO,
// graduate to a real IdP (see the production-identity setup guide).

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Flag names for `harbor token`, declared as constants so the command
// bodies, tests, and help text reference one spelling.
const (
	flagTokenOut      = "out"
	flagTokenAlg      = "alg"
	flagTokenForce    = "force"
	flagTokenKey      = "key"
	flagTokenTenant   = "tenant"
	flagTokenUser     = "user"
	flagTokenSession  = "session"
	flagTokenIssuer   = "issuer"
	flagTokenAudience = "audience"
	flagTokenKID      = "kid"
	flagTokenScopes   = "scopes"
	flagTokenTTL      = "ttl"
)

// defaultTokenTTL is the `harbor token mint` expiry when --ttl is
// omitted. Short by design (least privilege): a self-issued token is
// long-lived only if the operator opts in.
const defaultTokenTTL = time.Hour

// privateKeyFile / jwksSetFile are the fixed filenames keygen writes
// under --out.
const (
	privateKeyFile = "private.pem"
	jwksSetFile    = "jwks.json"
)

// Stable CLI error codes for `harbor token`. Each value is a fixed
// string a smoke test asserts against.
const (
	// CodeTokenKeyExists fires when keygen would overwrite an existing
	// private key without --force.
	CodeTokenKeyExists = "token_key_exists"
	// CodeTokenUnknownAlg fires when --alg is not ES256 / RS256, or a
	// loaded key resolves to an unsupported algorithm.
	CodeTokenUnknownAlg = "token_unknown_alg"
	// CodeTokenKeygenFailed is the catch-all for keygen write / encode
	// failures.
	CodeTokenKeygenFailed = "token_keygen_failed" //nolint:gosec // G101 false positive: a CLI error-code string, not a credential
	// CodeTokenKeyLoad fires when mint cannot read / parse --key.
	CodeTokenKeyLoad = "token_key_load_failed" //nolint:gosec // G101 false positive: a CLI error-code string, not a credential
	// CodeTokenMissingField fires when a mandatory mint field
	// (tenant/user/session/issuer/audience) is empty, or --ttl is
	// non-positive.
	CodeTokenMissingField = "token_missing_field"
	// CodeTokenMintFailed is the catch-all for signing failures.
	CodeTokenMintFailed = "token_mint_failed" //nolint:gosec // G101 false positive: a CLI error-code string, not a credential
)

// newTokenCmd builds the `token` parent command and binds its two verbs.
func newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "self-issue JWTs harbor serve verifies (bring-your-own-issuer)",
		Long: `Self-issue the JWTs ` + "`harbor serve`" + ` verifies, using a signing key you
manage — the on-ramp for an operator with no external identity provider.

  harbor token keygen   Generate a signing keypair + public JWK Set
  harbor token mint     Mint a Harbor JWT signed with that key

` + "`harbor serve`" + ` is unchanged: it accepts a minted token only because you
point ` + "`identity.jwks_file`" + ` at the emitted JWK Set — exactly as you would
point it at an external provider. serve still mints nothing itself.

These tokens are signed by a key you manage: protect the private key.
This is a single-issuer / eval / small-production posture; for multi-user
SSO, graduate to a real identity provider.`,
		Args: cobra.NoArgs,
		// No RunE: invoking `harbor token` with no verb prints help and
		// exits non-zero (cobra default for a parent with subcommands).
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}
	cmd.AddCommand(newTokenKeygenCmd())
	cmd.AddCommand(newTokenMintCmd())
	return cmd
}

// newTokenKeygenCmd builds `harbor token keygen`.
func newTokenKeygenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "generate an asymmetric signing keypair + public JWK Set",
		Long: `Generate an asymmetric keypair and write it under --out:

  private.pem   the signing key (PEM, mode 0600) — KEEP THIS PRIVATE
  jwks.json     the public JWK Set (RFC 7517) to load via identity.jwks_file

The JWK Set's key id (kid) is the RFC 7638 JWK thumbprint of the key —
content-derived, stable, never a hardcoded constant. mint stamps the same
kid by default so the verifier resolves the key.

Examples:
  harbor token keygen --out ./harbor-keys
  harbor token keygen --out ./harbor-keys --alg RS256
  harbor token keygen --out ./harbor-keys --force`,
		Args: cobra.NoArgs,
		RunE: runTokenKeygen,
	}
	cmd.Flags().String(flagTokenOut, "", "output directory for private.pem + jwks.json (required)")
	cmd.Flags().String(flagTokenAlg, algES256, "signing algorithm: ES256 (default) or RS256")
	cmd.Flags().Bool(flagTokenForce, false, "overwrite an existing private.pem")
	return cmd
}

// newTokenMintCmd builds `harbor token mint`.
func newTokenMintCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mint",
		Short: "mint a Harbor JWT signed with a keygen-produced key",
		Long: `Mint a Harbor JWT for an identity triple, signed with --key.

--issuer and --audience are MANDATORY and MUST equal your serve config's
identity.issuer / identity.audience — the verifier hard-rejects an
iss/aud mismatch with 401.

Least privilege: no scopes are granted unless --scopes is passed. The
default TTL is short (1h); raise it with --ttl only when you must.

The signed token is written to stdout (capture it); nothing else is.

Examples:
  harbor token mint --key ./harbor-keys/private.pem \
    --tenant acme --user alice --session sess-1 \
    --issuer https://issuer.example.com --audience harbor
  harbor token mint --key ./harbor-keys/private.pem \
    --tenant acme --user ops --session sess-2 \
    --issuer https://issuer.example.com --audience harbor \
    --scopes admin,console:fleet --ttl 8h`,
		Args: cobra.NoArgs,
		RunE: runTokenMint,
	}
	cmd.Flags().String(flagTokenKey, "", "path to the private.pem produced by `harbor token keygen` (required)")
	cmd.Flags().String(flagTokenTenant, "", "tenant id claim (required)")
	cmd.Flags().String(flagTokenUser, "", "user id claim (required)")
	cmd.Flags().String(flagTokenSession, "", "session id claim (required)")
	cmd.Flags().String(flagTokenIssuer, "", "iss claim — MUST equal identity.issuer (required)")
	cmd.Flags().String(flagTokenAudience, "", "aud claim — MUST equal identity.audience (required)")
	cmd.Flags().String(flagTokenKID, "", "key id header (defaults to the key's JWK thumbprint)")
	cmd.Flags().StringSlice(flagTokenScopes, nil, "scopes to grant (default: none — least privilege)")
	cmd.Flags().Duration(flagTokenTTL, defaultTokenTTL, "token lifetime")
	return cmd
}

// tokenKeygenJSON is the wire shape `harbor token keygen --json` emits.
type tokenKeygenJSON struct {
	PrivateKey string `json:"private_key"`
	JWKS       string `json:"jwks"`
	KID        string `json:"kid"`
	Alg        string `json:"alg"`
}

// tokenMintJSON is the wire shape `harbor token mint --json` emits.
type tokenMintJSON struct {
	Token     string `json:"token"`
	KID       string `json:"kid"`
	Alg       string `json:"alg"`
	ExpiresAt string `json:"expires_at"`
}

// runTokenKeygen is the cobra RunE for `harbor token keygen`.
func runTokenKeygen(cmd *cobra.Command, _ []string) error {
	outDir, _ := cmd.Flags().GetString(flagTokenOut) //nolint:errcheck // flag statically registered; lookup cannot fail
	alg, _ := cmd.Flags().GetString(flagTokenAlg)    //nolint:errcheck // flag statically registered; lookup cannot fail
	force, _ := cmd.Flags().GetBool(flagTokenForce)  //nolint:errcheck // flag statically registered; lookup cannot fail

	if strings.TrimSpace(outDir) == "" {
		return emitCLIError(cmd, CLIError{
			Subcommand: "token keygen",
			Message:    "--out is required",
			Code:       CodeTokenMissingField,
			Hint:       "pass --out <dir> to write private.pem + jwks.json",
		})
	}
	alg = strings.ToUpper(strings.TrimSpace(alg))

	signer, err := generateSigningKey(alg)
	if err != nil {
		return emitCLIError(cmd, keygenErrorToCLIError(err))
	}

	if err := ensureKeyDir(outDir); err != nil {
		return emitCLIError(cmd, CLIError{
			Subcommand: "token keygen",
			Message:    err.Error(),
			Code:       CodeTokenKeygenFailed,
			Hint:       "ensure --out is writable",
		})
	}

	privPath := filepath.Join(outDir, privateKeyFile)
	jwksPath := filepath.Join(outDir, jwksSetFile)

	if err := writePrivatePEM(privPath, signer, force); err != nil {
		return emitCLIError(cmd, keygenErrorToCLIError(err))
	}

	jwk, kid, err := publicJWK(signer, alg)
	if err != nil {
		return emitCLIError(cmd, keygenErrorToCLIError(err))
	}
	if err := writeJWKSet(jwksPath, jwk); err != nil {
		return emitCLIError(cmd, keygenErrorToCLIError(err))
	}

	// Stderr warning — fail-loud reminder, never the key bytes.
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"keep %s out of version control — it is your signing key (mode 0600)\n", privPath)

	if resolveJSONMode(cmd) {
		return writeJSONLine(cmd, tokenKeygenJSON{
			PrivateKey: privPath,
			JWKS:       jwksPath,
			KID:        kid,
			Alg:        alg,
		})
	}
	if !resolveQuietMode(cmd) {
		out := cmd.OutOrStdout()
		_, _ = fmt.Fprintf(out, "generated %s keypair\n", alg)
		_, _ = fmt.Fprintf(out, "  private key: %s (mode 0600)\n", privPath)
		_, _ = fmt.Fprintf(out, "  JWK Set:     %s\n", jwksPath)
		_, _ = fmt.Fprintf(out, "  kid:         %s\n", kid)
		_, _ = fmt.Fprintf(out, "point `harbor serve`'s identity.jwks_file at %s\n", jwksPath)
	}
	return nil
}

// runTokenMint is the cobra RunE for `harbor token mint`.
func runTokenMint(cmd *cobra.Command, _ []string) error {
	keyPath, _ := cmd.Flags().GetString(flagTokenKey)        //nolint:errcheck // flag statically registered; lookup cannot fail
	tenant, _ := cmd.Flags().GetString(flagTokenTenant)      //nolint:errcheck // flag statically registered; lookup cannot fail
	user, _ := cmd.Flags().GetString(flagTokenUser)          //nolint:errcheck // flag statically registered; lookup cannot fail
	session, _ := cmd.Flags().GetString(flagTokenSession)    //nolint:errcheck // flag statically registered; lookup cannot fail
	issuer, _ := cmd.Flags().GetString(flagTokenIssuer)      //nolint:errcheck // flag statically registered; lookup cannot fail
	audience, _ := cmd.Flags().GetString(flagTokenAudience)  //nolint:errcheck // flag statically registered; lookup cannot fail
	kidOverride, _ := cmd.Flags().GetString(flagTokenKID)    //nolint:errcheck // flag statically registered; lookup cannot fail
	scopes, _ := cmd.Flags().GetStringSlice(flagTokenScopes) //nolint:errcheck // flag statically registered; lookup cannot fail
	ttl, _ := cmd.Flags().GetDuration(flagTokenTTL)          //nolint:errcheck // flag statically registered; lookup cannot fail

	if missing := firstMissingMintField(keyPath, tenant, user, session, issuer, audience); missing != "" {
		return emitCLIError(cmd, CLIError{
			Subcommand: "token mint",
			Message:    fmt.Sprintf("--%s is required", missing),
			Code:       CodeTokenMissingField,
			Hint:       "--key, --tenant, --user, --session, --issuer and --audience are all mandatory",
		})
	}
	if ttl <= 0 {
		return emitCLIError(cmd, CLIError{
			Subcommand: "token mint",
			Message:    fmt.Sprintf("--ttl must be positive, got %s", ttl),
			Code:       CodeTokenMissingField,
			Hint:       "pass a positive duration, e.g. --ttl 1h",
		})
	}

	signer, err := loadPrivatePEM(keyPath)
	if err != nil {
		return emitCLIError(cmd, CLIError{
			Subcommand: "token mint",
			Message:    err.Error(),
			Code:       CodeTokenKeyLoad,
			Hint:       "pass --key pointing at a private.pem produced by `harbor token keygen`",
		})
	}
	alg, err := algForKey(signer)
	if err != nil {
		return emitCLIError(cmd, CLIError{
			Subcommand: "token mint",
			Message:    err.Error(),
			Code:       CodeTokenUnknownAlg,
			Hint:       "the key must be an ES256/384/512 or RS256 asymmetric key",
		})
	}

	// kid defaults to the key's JWK thumbprint so it matches the kid
	// keygen stamped into jwks.json.
	kid := strings.TrimSpace(kidOverride)
	if kid == "" {
		_, thumb, err := publicJWK(signer, alg)
		if err != nil {
			return emitCLIError(cmd, CLIError{
				Subcommand: "token mint",
				Message:    err.Error(),
				Code:       CodeTokenMintFailed,
				Hint:       "could not derive the key thumbprint; pass --kid explicitly",
			})
		}
		kid = thumb
	}

	now := time.Now()
	// Drop empty / whitespace scope entries so `--scopes ""` does not
	// inject a blank scope.
	scopes = cleanScopes(scopes)
	claims := harborClaims(now, ttl, issuer, audience, tenant, user, session, scopes)

	signed, err := mintJWT(signer, alg, kid, claims)
	if err != nil {
		return emitCLIError(cmd, CLIError{
			Subcommand: "token mint",
			Message:    err.Error(),
			Code:       CodeTokenMintFailed,
			Hint:       "signing failed; re-check the key file",
		})
	}

	expiresAt := now.Add(ttl)
	// Stderr: echo the lifetime + least-privilege posture (never the
	// token, never the key).
	scopeNote := "no scopes (least privilege)"
	if len(scopes) > 0 {
		scopeNote = "scopes: " + strings.Join(scopes, ",")
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"minted %s token: expires %s (ttl %s), %s\n",
		alg, expiresAt.Format(time.RFC3339), ttl, scopeNote)

	if resolveJSONMode(cmd) {
		return writeJSONLine(cmd, tokenMintJSON{
			Token:     signed,
			KID:       kid,
			Alg:       alg,
			ExpiresAt: expiresAt.Format(time.RFC3339),
		})
	}
	// Default: the raw token on stdout, one line, so `TOKEN=$(harbor
	// token mint ...)` captures it cleanly.
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), signed)
	return nil
}

// ensureKeyDir creates outDir owner-only (mode 0700) — the private key's
// parent must not be world-/group-listable. It tightens to 0700 only a
// directory it CREATED: a pre-existing directory the operator pointed
// --out at (e.g. a shared ~/.harbor) is left as-is, so keygen never
// silently re-permissions someone else's directory. The key file itself
// is always written 0600, so its secrecy does not depend on the dir mode.
func ensureKeyDir(outDir string) error {
	created := false
	if _, statErr := os.Stat(outDir); errors.Is(statErr, os.ErrNotExist) {
		created = true
	} else if statErr != nil {
		return fmt.Errorf("token: stat %s: %w", outDir, statErr)
	}
	if err := os.MkdirAll(outDir, keyDirMode); err != nil {
		return fmt.Errorf("token: create %s: %w", outDir, err)
	}
	// MkdirAll's mode is umask-masked, so a freshly-created dir still needs
	// an explicit Chmod to guarantee owner-only — but only one we created.
	if created {
		if err := os.Chmod(outDir, keyDirMode); err != nil {
			return fmt.Errorf("token: chmod %s: %w", outDir, err)
		}
	}
	return nil
}

// writeJSONLine marshals v and writes it as a single line + newline to
// stdout — the --json wire shape. Marshal failure surfaces loudly (no
// silent degradation).
func writeJSONLine(cmd *cobra.Command, v any) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("token: marshal json result: %w", err)
	}
	buf = append(buf, '\n')
	if _, err := cmd.OutOrStdout().Write(buf); err != nil {
		return fmt.Errorf("token: write json result: %w", err)
	}
	return nil
}

// firstMissingMintField returns the flag name of the first empty
// mandatory mint field, or "" when all are present.
func firstMissingMintField(key, tenant, user, session, issuer, audience string) string {
	switch {
	case strings.TrimSpace(key) == "":
		return flagTokenKey
	case strings.TrimSpace(tenant) == "":
		return flagTokenTenant
	case strings.TrimSpace(user) == "":
		return flagTokenUser
	case strings.TrimSpace(session) == "":
		return flagTokenSession
	case strings.TrimSpace(issuer) == "":
		return flagTokenIssuer
	case strings.TrimSpace(audience) == "":
		return flagTokenAudience
	default:
		return ""
	}
}

// cleanScopes trims and drops empty scope entries.
func cleanScopes(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// keygenErrorToCLIError maps a keygen crypto sentinel onto a CLIError.
func keygenErrorToCLIError(err error) CLIError {
	switch {
	case errors.Is(err, ErrTokenKeyExists):
		return CLIError{
			Subcommand: "token keygen",
			Message:    err.Error(),
			Code:       CodeTokenKeyExists,
			Hint:       "pass --force to overwrite, or pick a fresh --out directory",
		}
	case errors.Is(err, ErrTokenUnknownAlg):
		return CLIError{
			Subcommand: "token keygen",
			Message:    err.Error(),
			Code:       CodeTokenUnknownAlg,
			Hint:       "use --alg ES256 or --alg RS256",
		}
	default:
		return CLIError{
			Subcommand: "token keygen",
			Message:    err.Error(),
			Code:       CodeTokenKeygenFailed,
			Hint:       "see the wrapped error for the failing path",
		}
	}
}
