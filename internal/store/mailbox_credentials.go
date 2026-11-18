package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
)

const mailboxCredentialEnvelopePrefix = "enc:v1:"
const mailboxCredentialEnvelopeNamespace = "enc:"

var ErrCredentialEncryptionNotConfigured = errors.New("mailbox credential encryption is not configured")

type mailboxCredentialCipher struct {
	keys []cipher.AEAD
}

// ConfigureCredentialEncryption installs the dedicated mailbox-credential keyring.
// Keys are standard base64 encodings of exactly 32 random bytes. The active key
// is used for writes; old keys are accepted only for reads and explicit rotation.
func (p *Postgres) ConfigureCredentialEncryption(active string, old []string) error {
	encoded := make([]string, 0, 1+len(old))
	encoded = append(encoded, active)
	encoded = append(encoded, old...)
	keys := make([]cipher.AEAD, 0, len(encoded))
	for i, value := range encoded {
		if strings.TrimSpace(value) == "" {
			if i == 0 {
				return fmt.Errorf("configure mailbox credential encryption: active key is required")
			}
			return fmt.Errorf("configure mailbox credential encryption: old key %d is empty", i)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
		if err != nil || len(raw) != 32 {
			return fmt.Errorf("configure mailbox credential encryption: key %d must be standard base64 encoding of 32 bytes", i)
		}
		block, err := aes.NewCipher(raw)
		if err != nil {
			return fmt.Errorf("configure mailbox credential encryption: initialize key %d", i)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return fmt.Errorf("configure mailbox credential encryption: initialize key %d envelope", i)
		}
		keys = append(keys, aead)
	}
	p.credentialCipher = &mailboxCredentialCipher{keys: keys}
	return nil
}

func mailboxCredentialAAD(mailboxID string) []byte {
	return []byte("agent-emails/mailbox-password/v1\x00" + strings.TrimSpace(mailboxID))
}

func (p *Postgres) encryptMailboxCredential(mailboxID, plaintext string) (string, error) {
	if p.credentialCipher == nil || len(p.credentialCipher.keys) == 0 {
		return "", ErrCredentialEncryptionNotConfigured
	}
	aead := p.credentialCipher.keys[0]
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("encrypt mailbox credential: generate nonce: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), mailboxCredentialAAD(mailboxID))
	return mailboxCredentialEnvelopePrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

func (p *Postgres) decryptMailboxCredential(mailboxID, envelope string) (string, int, error) {
	if p.credentialCipher == nil || len(p.credentialCipher.keys) == 0 {
		return "", -1, ErrCredentialEncryptionNotConfigured
	}
	if !strings.HasPrefix(envelope, mailboxCredentialEnvelopePrefix) {
		return "", -1, errors.New("mailbox credential is not encrypted")
	}
	payload, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(envelope, mailboxCredentialEnvelopePrefix))
	if err != nil {
		return "", -1, errors.New("mailbox credential envelope is invalid")
	}
	for i, aead := range p.credentialCipher.keys {
		if len(payload) < aead.NonceSize() {
			break
		}
		nonce, ciphertext := payload[:aead.NonceSize()], payload[aead.NonceSize():]
		plaintext, openErr := aead.Open(nil, nonce, ciphertext, mailboxCredentialAAD(mailboxID))
		if openErr == nil {
			return string(plaintext), i, nil
		}
	}
	return "", -1, errors.New("mailbox credential cannot be decrypted")
}

// EncryptLegacyMailboxCredentials explicitly upgrades plaintext credentials and
// rotates old-key envelopes to the active key in one transaction. Normal reads
// never accept plaintext credentials.
func (p *Postgres) EncryptLegacyMailboxCredentials(ctx context.Context) (int, error) {
	return p.EncryptLegacyMailboxCredentialsWithPlaintextOverrides(ctx, nil)
}

// EncryptLegacyMailboxCredentialsWithPlaintextOverrides explicitly identifies
// legacy plaintext rows whose passwords collide with the reserved enc: envelope
// namespace. Overrides are accepted only for existing rows that cannot already
// be decrypted as valid v1 envelopes.
func (p *Postgres) EncryptLegacyMailboxCredentialsWithPlaintextOverrides(ctx context.Context, plaintextMailboxIDs []string) (int, error) {
	if p.credentialCipher == nil || len(p.credentialCipher.keys) == 0 {
		return 0, ErrCredentialEncryptionNotConfigured
	}
	overrides := make(map[string]struct{}, len(plaintextMailboxIDs))
	for _, mailboxID := range plaintextMailboxIDs {
		mailboxID = strings.TrimSpace(mailboxID)
		if mailboxID == "" {
			return 0, errors.New("plaintext mailbox override ID cannot be empty")
		}
		parsed, err := uuid.Parse(mailboxID)
		if err != nil {
			return 0, fmt.Errorf("plaintext mailbox override ID %s is invalid", mailboxID)
		}
		mailboxID = parsed.String()
		if _, duplicate := overrides[mailboxID]; duplicate {
			return 0, fmt.Errorf("plaintext mailbox override ID %s is duplicated", mailboxID)
		}
		overrides[mailboxID] = struct{}{}
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT id::text,password_encrypted FROM mailboxes ORDER BY id FOR UPDATE`)
	if err != nil {
		return 0, err
	}
	type credentialRow struct{ id, stored string }
	credentials := []credentialRow{}
	for rows.Next() {
		var row credentialRow
		if err := rows.Scan(&row.id, &row.stored); err != nil {
			rows.Close()
			return 0, err
		}
		credentials = append(credentials, row)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	foundOverrides := make(map[string]struct{}, len(overrides))

	updated := 0
	for _, row := range credentials {
		plaintext := row.stored
		needsWrite := true
		_, overridden := overrides[row.id]
		if overridden {
			foundOverrides[row.id] = struct{}{}
			if !strings.HasPrefix(row.stored, mailboxCredentialEnvelopeNamespace) {
				return 0, fmt.Errorf("plaintext mailbox override %s does not have a reserved envelope prefix collision", row.id)
			}
			if strings.HasPrefix(row.stored, mailboxCredentialEnvelopePrefix) {
				if _, _, decryptErr := p.decryptMailboxCredential(row.id, row.stored); decryptErr == nil {
					return 0, fmt.Errorf("plaintext mailbox override %s already contains a valid encrypted credential", row.id)
				}
			}
		} else if strings.HasPrefix(row.stored, mailboxCredentialEnvelopeNamespace) && !strings.HasPrefix(row.stored, mailboxCredentialEnvelopePrefix) {
			return 0, fmt.Errorf("validate encrypted mailbox credential for mailbox %s: unsupported envelope version", row.id)
		} else if strings.HasPrefix(row.stored, mailboxCredentialEnvelopePrefix) {
			var keyIndex int
			plaintext, keyIndex, err = p.decryptMailboxCredential(row.id, row.stored)
			if err != nil {
				return 0, fmt.Errorf("validate encrypted mailbox credential for mailbox %s: %w", row.id, err)
			}
			needsWrite = keyIndex != 0
		}
		if !needsWrite {
			continue
		}
		envelope, err := p.encryptMailboxCredential(row.id, plaintext)
		if err != nil {
			return 0, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE mailboxes SET password_encrypted=$1 WHERE id=$2`, envelope, row.id)
		if err != nil {
			return 0, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if count != 1 {
			return 0, sql.ErrNoRows
		}
		updated++
	}
	for mailboxID := range overrides {
		if _, found := foundOverrides[mailboxID]; !found {
			return 0, fmt.Errorf("plaintext mailbox override %s does not match a mailbox", mailboxID)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return updated, nil
}
