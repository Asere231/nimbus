# Milestone 23: Key Vault keys and certificates

## Goal

A Nimbus vault holds secrets and nothing else. `internal/azure/keyvault/
handler.go` answers **501 NotSupported** for `keys`, `storage` and `rng`,
and for certificates except the CONTACTS list — which exists only because
azurerm reads it on every vault read.

So `azurerm_key_vault_key` and `azurerm_key_vault_certificate` cannot be
applied at all, and the half of Key Vault that is about *cryptography*
rather than *storage* is absent.

## Why this is worth a milestone

A secret is a string you put in and get back. A KEY is the opposite: the
private half never leaves, and the service performs operations with it —
sign, verify, wrap, unwrap, encrypt, decrypt. That distinction is the
entire reason Key Vault and KMS exist, and it is the thing a learner most
needs to meet, because it is what makes "the key never leaves the vault"
a real property rather than a slogan.

It is also **squarely inside the stdlib rule**: `crypto/rsa`,
`crypto/ecdsa`, `crypto/x509` and `encoding/pem` can do all of it with no
dependency. There is no vendor image to lean on and none is needed, which
makes this the opposite problem from Azure Functions — all the difficulty
is protocol fidelity, and that is what this project is good at.

## Measure first

- **What does azurerm send for a KEY?** `azurerm_key_vault_key` with
  `key_type = "RSA"`, `key_size = 2048` and an `key_opts` list. Find out
  the exact create path (`PUT /keys/{name}/create`), what it reads back,
  and which fields it compares — the "derive, don't generate" rule
  decides whether a plan comes back clean.
- **A key's JWK is the response shape**, not PEM: `kty`, `n`, `e` for
  RSA, `crv`/`x`/`y` for EC, all base64url. The private material is NEVER
  in a response. Get a real shape before inventing one.
- **What does a CERTIFICATE create actually do?** It is a three-part
  object — a certificate, a key and a secret, all under one name, all
  addressable separately. `azurerm_key_vault_certificate` with a
  `certificate_policy` asks the vault to GENERATE one (self-signed, with
  an issuer of `Self`), which means Nimbus signs it. Measure whether the
  provider polls an operation while it is "in progress".
- **Does anything READ a certificate back as PEM or PFX?** The secret
  under the same name returns the full chain plus private key, which is
  how most clients actually consume one. That indirection is the
  surprising part and worth confirming rather than assuming.
- **What does `az keyvault key` do** that azurerm does not — `encrypt`,
  `decrypt`, `sign`, `verify`, `wrapKey`, `unwrapKey` are colon-ish verbs
  on the key's URL and are the operations that make a key a key.

## Build

1. **Keys**: create (RSA and EC), get, list, list versions, delete,
   soft-delete and purge — mirroring how secrets already behave, since
   that machinery exists and was proven in Milestone 4.
2. **Cryptographic operations**: `sign`/`verify` at minimum, then
   `encrypt`/`decrypt` and `wrapKey`/`unwrapKey`. The private half stays
   in the store and never appears in a response — a test should assert
   that directly, because it is the property the whole service exists for.
3. **Certificates**: create with a self-signed policy, get, list, and the
   three-way addressing (certificate, key, secret under one name).
4. **Whatever is not implemented keeps answering 501 and NAMES itself**,
   as it does today. `storage` and `rng` are probably out of scope and
   should say so.

## Done criteria

- [ ] `azurerm_key_vault_key` applies, plans clean and destroys.
- [ ] `azurerm_key_vault_certificate` applies, plans clean and destroys.
- [ ] A signature made by the vault VERIFIES against the public key the
      vault published — checked with `crypto`, not by trusting the
      response.
- [ ] The private key never appears in any response body; asserted by a
      test rather than by inspection.
- [ ] `az keyvault key create/show/list` and `az keyvault certificate
      create/show` work.
- [ ] The existing secrets behaviour, including soft-delete and purge, is
      unchanged — `examples/terraform/azure-app` still applies.
- [ ] `docs/ROADMAP.md` loses "Only secrets are emulated in Key Vault"
      and states what is now emulated and what still answers 501.
- [ ] Full suite and CI green on all three legs.

## Ask the owner

- **How far into cryptographic operations to go.** `sign`/`verify` alone
  proves the property that matters. The full set (`encrypt`, `decrypt`,
  `wrapKey`, `unwrapKey`) is more surface for the same lesson. Recommend
  starting with sign/verify and adding the rest only if a real client
  needs them.
- **Whether to support importing a key** (`azurerm_key_vault_key` has no
  import, but `az keyvault key import` does) or generation only.
  Recommend generation only, stated.
- **Certificate issuers.** Real Key Vault integrates with DigiCert and
  GlobalSign. Self-signed is the only one that can work offline;
  recommend refusing the others by name rather than pretending.

## What NOT to do

- Do not reach for a third-party crypto library. `crypto/rsa`,
  `crypto/ecdsa` and `crypto/x509` cover this, and the stdlib-only rule
  is not negotiable.
- Do not return private material "because it is only a local emulator".
  The whole teaching value is that the private half does not leave, and a
  shortcut here would teach the opposite of the truth — the same argument
  that rejected serving Azure Functions' HTTP trigger ourselves.
