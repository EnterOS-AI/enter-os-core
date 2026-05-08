# Secrets Key Custody

How the encryption keys that protect Molecule workspace secrets are managed, where each key lives, and what an attacker who compromises one layer can or cannot read.

This document exists because the platform repo (`workspace-server`) reads `SECRETS_ENCRYPTION_KEY` from its process env, which on its own looks like "encryption-at-rest theater." The full custody chain runs through the control plane (`molecule-controlplane`) where AWS KMS holds the key material at rest. Anyone reading only the platform repo sees half the picture.

## Two modes

The control plane's `internal/crypto.Envelope` ships in two modes, picked at boot from env:

| Mode | Trigger | At-rest format | Recommended for |
|------|---------|----------------|-----------------|
| **KMS envelope** | `KMS_KEY_ARN` set | Per-blob KMS-wrapped DEK + AES-256-GCM ciphertext | Production, multi-tenant SaaS |
| **Static key** | Only `SECRETS_ENCRYPTION_KEY` set | AES-256-GCM with one process-wide key | Dev, self-hosted single-tenant |

`Envelope.Decrypt` is dual-mode — it can read either format on the way out, so a deployment can flip from static-key to KMS envelope without re-encrypting historical rows. Code: `molecule-controlplane/internal/crypto/kms.go`.

## KMS envelope flow

When `KMS_KEY_ARN` is configured, every secret write looks like:

1. CP calls `kms.GenerateDataKey(KeyId=KMS_KEY_ARN, KeySpec=AES_256)` → returns `{Plaintext, CiphertextBlob}`.
2. CP encrypts the secret with AES-256-GCM using `Plaintext` as the key.
3. CP discards `Plaintext` from memory; persists the blob:

   ```
   [0x02 prefix][uint16 BE: encrypted_dek_len][encrypted_dek][nonce(12)][ct+tag]
   ```

   The `0x02` byte distinguishes v2 (KMS-wrapped) blobs from legacy static-key blobs.

4. To read: CP calls `kms.Decrypt(CiphertextBlob)` → recovers the AES key → unwraps the GCM ciphertext.

KMS calls cost ~$0.03 per 10k requests. We do not cache DEKs — provisioning rate is orders below steady-state reads, and not caching keeps key rotation reasoning simple.

## What lives where

| Layer | Key custody | Plaintext key in memory? |
|-------|-------------|--------------------------|
| AWS KMS | KMS-resident, never leaves the HSM | No (hardware) |
| `molecule-controlplane` process | KMS client + IAM role | Briefly per-secret-op only |
| CP database (`database_url_encrypted`, tenant secrets) | KMS-wrapped blobs | Never |
| Per-tenant `workspace-server` env (`SECRETS_ENCRYPTION_KEY`) | Provisioned at tenant boot by CP | Yes, for the tenant's process lifetime |
| Tenant Postgres (`workspace_secrets.value`) | AES-256-GCM with the tenant's key | Never |

The "plaintext in tenant memory" row is the standard envelope-encryption trade-off: a DEK has to be unwrapped somewhere to be used. The blast radius of compromising one tenant's process is one tenant's secrets — not the whole fleet.

## Threat model

| Attacker capability | Can they read tenant secrets? |
|---------------------|-------------------------------|
| Reads CP database backup | No — KMS unwrap requires IAM-scoped `kms:Decrypt` |
| Steals `KMS_KEY_ARN` value | No — ARN alone does nothing without IAM access |
| Compromises CP IAM role | Yes — can `kms:Decrypt` any wrapped DEK |
| Reads tenant Postgres (one tenant) | No — `SECRETS_ENCRYPTION_KEY` lives only in the tenant's own EC2 process env, not in DB |
| Compromises one tenant's EC2 | Yes for that tenant's secrets, no for any other tenant |
| Compromises CP host | Game over (CP can provision arbitrary tenants) |

The two boundaries the design protects:

- **DB-only compromise (incl. backups)** → secrets remain encrypted; attacker needs separate access to either KMS (prod) or CP env (dev).
- **One-tenant compromise** → blast radius limited to that tenant; no cross-tenant key reuse.

## Rotation

- **Tenant key rotation** (per-tenant `SECRETS_ENCRYPTION_KEY`): re-encrypt the tenant's `workspace_secrets` rows under a new key, then swap the env var. Static-key mode requires this for all rotation; KMS mode only requires it on suspected key compromise.
- **KMS CMK rotation**: AWS KMS handles annual automatic rotation of the customer master key. Re-wrapping data keys is unnecessary because each `Decrypt` call routes through the current CMK version automatically (KMS keeps prior versions for decrypt-only).

## Audit / compliance posture

For SOC2 / ISO 27001 / customer security questionnaires:

- **Key custody**: AWS KMS (FIPS 140-2 Level 3 HSM-backed)
- **Key isolation**: per-tenant DEK; no shared keys across tenants
- **Access control**: IAM-scoped `kms:Decrypt`, audited via CloudTrail
- **At-rest encryption**: AES-256-GCM (NIST-approved, authenticated)
- **In-transit encryption**: TLS 1.2+ for KMS, CP-to-tenant, tenant-to-DB
- **Rotation**: AWS-managed CMK rotation annually; manual DEK rotation on incident

## Pointers

- KMS envelope code: [`molecule-controlplane/internal/crypto/kms.go`](https://git.moleculesai.app/molecule-ai/molecule-controlplane/src/branch/main/internal/crypto/kms.go)
- Static-key fallback: [`molecule-controlplane/internal/crypto/aes.go`](https://git.moleculesai.app/molecule-ai/molecule-controlplane/src/branch/main/internal/crypto/aes.go)
- Tenant secrets handler: [`workspace-server/internal/crypto/aes.go`](../../workspace-server/internal/crypto/aes.go)
- Tenant secrets schema: [database-schema.md](./database-schema.md#workspace_secrets)
